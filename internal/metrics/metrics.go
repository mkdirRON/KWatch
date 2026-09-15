// Package metrics exposes KWatch's collection results to Prometheus.
package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/mkdirRON/KWatch/internal/lag"
)

// Metrics holds the Prometheus collectors published by KWatch.
type Metrics struct {
	lag             *prometheus.GaugeVec
	committedOffset *prometheus.GaugeVec
	endOffset       *prometheus.GaugeVec

	up             prometheus.Gauge
	scrapesTotal   prometheus.Counter
	scrapeErrors   prometheus.Counter
	scrapeDuration prometheus.Gauge
	lastScrapeTime prometheus.Gauge
}

// New registers KWatch's collectors with reg and returns them.
func New(reg prometheus.Registerer) *Metrics {
	groupLabels := []string{"group", "topic", "partition"}

	m := &Metrics{
		lag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kwatch_consumer_group_lag",
			Help: "Messages a consumer group is behind the end of a topic partition.",
		}, groupLabels),
		committedOffset: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kwatch_consumer_group_committed_offset",
			Help: "Last offset committed by a consumer group on a topic partition.",
		}, groupLabels),
		endOffset: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kwatch_topic_partition_end_offset",
			Help: "Log end offset of a topic partition.",
		}, []string{"topic", "partition"}),
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kwatch_up",
			Help: "1 if the last collection round reached the Kafka cluster, 0 otherwise.",
		}),
		scrapesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kwatch_scrapes_total",
			Help: "Collection rounds attempted.",
		}),
		scrapeErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kwatch_scrape_errors_total",
			Help: "Collection rounds that failed.",
		}),
		scrapeDuration: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kwatch_scrape_duration_seconds",
			Help: "Duration of the last collection round.",
		}),
		lastScrapeTime: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kwatch_last_scrape_timestamp_seconds",
			Help: "Unix time of the last successful collection round.",
		}),
	}

	reg.MustRegister(
		m.lag, m.committedOffset, m.endOffset,
		m.up, m.scrapesTotal, m.scrapeErrors, m.scrapeDuration, m.lastScrapeTime,
	)
	return m
}

// Observe publishes a successful collection round.
//
// The gauge vectors are reset first so that series for partitions or groups
// that have gone away stop being exported instead of freezing at a stale value.
func (m *Metrics) Observe(snap lag.Snapshot) {
	m.lag.Reset()
	m.committedOffset.Reset()
	m.endOffset.Reset()

	for _, e := range snap.Entries {
		labels := prometheus.Labels{
			"group":     e.Group,
			"topic":     e.Topic,
			"partition": strconv.Itoa(e.Partition),
		}
		m.lag.With(labels).Set(float64(e.Lag))
		m.committedOffset.With(labels).Set(float64(e.CommittedOffset))
	}
	for _, p := range snap.Partitions {
		m.endOffset.With(prometheus.Labels{
			"topic":     p.Topic,
			"partition": strconv.Itoa(p.Partition),
		}).Set(float64(p.EndOffset))
	}

	m.scrapesTotal.Inc()
	m.up.Set(1)
	m.scrapeDuration.Set(snap.Duration.Seconds())
	m.lastScrapeTime.Set(float64(snap.CollectedAt.Add(snap.Duration).Unix()))
}

// ObserveError records a failed collection round. Previously published lag
// series are left in place; kwatch_up going to 0 is what alerting keys on.
func (m *Metrics) ObserveError(took time.Duration) {
	m.scrapesTotal.Inc()
	m.scrapeErrors.Inc()
	m.up.Set(0)
	m.scrapeDuration.Set(took.Seconds())
}
