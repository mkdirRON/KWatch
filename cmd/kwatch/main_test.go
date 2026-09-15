package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/mkdirRON/KWatch/internal/lag"
	"github.com/mkdirRON/KWatch/internal/metrics"
)

func testMux(t *testing.T) (*http.ServeMux, *snapshotStore) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	store := &snapshotStore{}

	snap := lag.Snapshot{
		Entries: []lag.Entry{{
			Group: "order_consumer_1", Topic: "orders", Partition: 0,
			CommittedOffset: 90, EndOffset: 100, Lag: 10,
		}},
		Partitions: []lag.PartitionOffset{{Topic: "orders", Partition: 0, EndOffset: 100}},
	}
	m.Observe(snap)
	store.set(snap)

	return newMux(reg, store), store
}

func get(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, rec.Code)
	}
	return rec
}

func TestMetricsEndpoint(t *testing.T) {
	mux, _ := testMux(t)
	body := get(t, mux, "/metrics").Body.String()

	for _, want := range []string{
		`kwatch_consumer_group_lag{group="order_consumer_1",partition="0",topic="orders"} 10`,
		`kwatch_topic_partition_end_offset{partition="0",topic="orders"} 100`,
		"kwatch_up 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}

func TestHealthzEndpoint(t *testing.T) {
	mux, _ := testMux(t)
	if got := get(t, mux, "/healthz").Body.String(); got != "ok\n" {
		t.Errorf("/healthz = %q, want \"ok\\n\"", got)
	}
}

func TestLagAPIEndpoint(t *testing.T) {
	mux, _ := testMux(t)
	var snap lag.Snapshot
	if err := json.Unmarshal(get(t, mux, "/api/lag").Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode /api/lag: %v", err)
	}
	if len(snap.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(snap.Entries))
	}
	if snap.Entries[0].Lag != 10 {
		t.Errorf("lag = %d, want 10", snap.Entries[0].Lag)
	}
}

func TestLagAPIBeforeFirstCollection(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics.New(reg)
	mux := newMux(reg, &snapshotStore{})

	var snap lag.Snapshot
	if err := json.Unmarshal(get(t, mux, "/api/lag").Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode /api/lag: %v", err)
	}
	if len(snap.Entries) != 0 {
		t.Errorf("entries = %d, want 0 before the first collection", len(snap.Entries))
	}
}
