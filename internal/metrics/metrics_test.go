package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/mkdirRON/KWatch/internal/lag"
)

func snapshot(entries ...lag.Entry) lag.Snapshot {
	return lag.Snapshot{
		Entries:     entries,
		Partitions:  []lag.PartitionOffset{{Topic: "orders", Partition: 0, EndOffset: 100}},
		CollectedAt: time.Unix(1700000000, 0),
		Duration:    250 * time.Millisecond,
	}
}

func TestObservePublishesLag(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	m.Observe(snapshot(lag.Entry{
		Group: "order_consumer_1", Topic: "orders", Partition: 0,
		CommittedOffset: 90, EndOffset: 100, Lag: 10,
	}))

	want := `
# HELP kwatch_consumer_group_lag Messages a consumer group is behind the end of a topic partition.
# TYPE kwatch_consumer_group_lag gauge
kwatch_consumer_group_lag{group="order_consumer_1",partition="0",topic="orders"} 10
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "kwatch_consumer_group_lag"); err != nil {
		t.Error(err)
	}
	if got := testutil.ToFloat64(m.up); got != 1 {
		t.Errorf("kwatch_up = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.scrapeDuration); got != 0.25 {
		t.Errorf("scrape duration = %v, want 0.25", got)
	}
}

func TestObserveDropsStaleSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	m.Observe(snapshot(
		lag.Entry{Group: "g1", Topic: "orders", Partition: 0, Lag: 10},
		lag.Entry{Group: "gone", Topic: "orders", Partition: 1, Lag: 5},
	))
	if got := testutil.CollectAndCount(m.lag); got != 2 {
		t.Fatalf("series = %d, want 2", got)
	}

	// The second group disappears; its series must not linger at a stale value.
	m.Observe(snapshot(lag.Entry{Group: "g1", Topic: "orders", Partition: 0, Lag: 12}))
	if got := testutil.CollectAndCount(m.lag); got != 1 {
		t.Errorf("series = %d, want 1 after the group went away", got)
	}
}

func TestObserveErrorMarksDown(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	m.Observe(snapshot(lag.Entry{Group: "g1", Topic: "orders", Partition: 0, Lag: 10}))
	m.ObserveError(100 * time.Millisecond)

	if got := testutil.ToFloat64(m.up); got != 0 {
		t.Errorf("kwatch_up = %v, want 0", got)
	}
	if got := testutil.ToFloat64(m.scrapeErrors); got != 1 {
		t.Errorf("scrape errors = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.scrapesTotal); got != 2 {
		t.Errorf("scrapes total = %v, want 2", got)
	}
	// The last known lag stays published; kwatch_up is what alerting reads.
	if got := testutil.CollectAndCount(m.lag); got != 1 {
		t.Errorf("series = %d, want the last known value retained", got)
	}
}
