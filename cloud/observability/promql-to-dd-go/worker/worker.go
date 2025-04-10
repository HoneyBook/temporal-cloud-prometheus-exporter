package worker

import (
	"fmt"
	"log"
	"time"

	"github.com/DataDog/datadog-api-client-go/v2/api/datadogV2"
	promapi "github.com/prometheus/client_golang/api/prometheus/v1"

	"github.com/temporalio/promql-to-dd-go/datadog"
	"github.com/temporalio/promql-to-dd-go/prometheus"
)

type Worker struct {
	prometheus.Querier
	datadog.Submitter
	MetricPrefix  string
	Quantiles     []float64
	QueryInterval time.Duration
	StepDuration  time.Duration
	SleepDuration time.Duration
}

const (
	HistogramPromQL = "histogram_quantile(%.2f, sum(rate(%s[1m])) by (temporal_namespace,operation,le))"
	RatePromQL      = "rate(%s[1m])"
	RetryInterval   = 3 * time.Second
)

func (w *Worker) Run() {
	interrupt := interruptCh()
	ticker := time.NewTicker(w.SleepDuration)
	defer ticker.Stop()
	errs := make(chan error, 1)

	for {
		go w.do(errs)

		select {
		case err := <-errs:
			log.Println("Worker failed:", err)
			time.Sleep(RetryInterval)
		case <-ticker.C:
			continue
		case s := <-interrupt:
			log.Println("Worker has been stopped.", "Signal", s)
			return
		}
	}
}

func (w *Worker) QueryWindow() time.Duration {
	return time.Duration(w.QueryInterval.Seconds()*1.2) * time.Second // 20% range overlap between queries
}

func (w *Worker) do(errorChan chan<- error) {
	queryRange := w.calcRange()
	histograms, counters, err := w.ListMetrics(w.MetricPrefix)
	if err != nil {
		panic(err)
	}

	log.Printf("Querying Prometheus\n")
	log.Printf("Found %d histogram metrics: %v\n", len(histograms), histograms)
	log.Printf("Found %d counter metrics: %v\n", len(counters), counters)

	histogramSeries := []datadogV2.MetricSeries{}
	// histograms
	for _, quantile := range w.Quantiles {
		for _, bucketName := range histograms {
			promql := fmt.Sprintf(HistogramPromQL, quantile, bucketName)
			matrix, err := w.QueryMetrics(promql, queryRange)
			if err != nil {
				errorChan <- err
				return
			}
			histogramSeries = append(histogramSeries, PromHistogramToDatadogGauge(bucketName, quantile, matrix)...)
		}
	}
	log.Printf("Received %d histogram series\n", len(histogramSeries))

	// rates
	rateSeries := []datadogV2.MetricSeries{}
	// counts
	countSeries := []datadogV2.MetricSeries{}
	for _, counterName := range counters {
		// Query and submit rate metrics
		promql := fmt.Sprintf(RatePromQL, counterName)
		matrix, err := w.QueryMetrics(promql, queryRange)
		if err != nil {
			errorChan <- err
			return
		}
		rateSeries = append(rateSeries, PromCountToDatadogRate(counterName, matrix)...)

		// Query and submit raw count metrics
		matrix, err = w.QueryMetrics(counterName, queryRange)
		if err != nil {
			errorChan <- err
			return
		}
		countSeries = append(countSeries, PromCountToDatadogCount(counterName, matrix)...)
	}
	log.Printf("Received %d rate series\n", len(rateSeries))
	log.Printf("Received %d count series\n", len(countSeries))

	log.Printf("Submitting to Datadog\n")
	series := append(histogramSeries, rateSeries...)
	series = append(series, countSeries...)
	err = w.SubmitMetrics(series)
	if err != nil {
		errorChan <- err
		return
	}
	log.Printf("Submitted total of %d series\n", len(series))
	log.Printf("Awaits next tick (interval: %.0f seconds)\n", w.SleepDuration.Seconds())
}

func (w *Worker) calcRange() promapi.Range {
	end := time.Now().Unix() / 60 * 60 // round seconds
	star := end - int64(w.QueryWindow().Seconds())
	stepSeconds := int64(w.StepDuration.Seconds())

	// add padding
	star = ((star / stepSeconds) - 1) * stepSeconds
	end = ((end / stepSeconds) + 1) * stepSeconds

	return promapi.Range{
		Start: time.Unix(star, 0),
		End:   time.Unix(end, 0),
		Step:  w.StepDuration,
	}
}
