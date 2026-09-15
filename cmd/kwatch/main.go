// Command kwatch monitors Kafka consumer group lag and exposes it to
// Prometheus so teams are alerted before consumers fall behind.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/segmentio/kafka-go"

	"github.com/mkdirRON/KWatch/internal/config"
	"github.com/mkdirRON/KWatch/internal/lag"
	"github.com/mkdirRON/KWatch/internal/metrics"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("kwatch: %v", err)
	}
}

func run() error {
	cfg, err := config.FromEnv(os.LookupEnv)
	if err != nil {
		return err
	}
	// Flags override the environment so a local run can point at another
	// cluster without exporting anything.
	brokers := flag.String("brokers", strings.Join(cfg.Brokers, ","), "comma-separated Kafka bootstrap addresses")
	listen := flag.String("listen", cfg.ListenAddr, "address for the metrics HTTP server")
	interval := flag.Duration("poll-interval", cfg.PollInterval, "how often to collect lag")
	flag.Parse()

	cfg.Brokers = config.SplitList(*brokers)
	cfg.ListenAddr = *listen
	cfg.PollInterval = *interval
	if cfg.PollTimeout > cfg.PollInterval {
		cfg.PollTimeout = cfg.PollInterval
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	client := &kafka.Client{
		Addr:    kafka.TCP(cfg.Brokers...),
		Timeout: cfg.PollTimeout,
	}
	collector := lag.New(client, cfg.Topics, cfg.Groups)

	registry := prometheus.NewRegistry()
	m := metrics.New(registry)
	snapshots := &snapshotStore{}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           newMux(registry, snapshots),
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("kwatch: serving metrics on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	log.Printf("kwatch: polling %v every %s", cfg.Brokers, cfg.PollInterval)
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		poll(ctx, collector, m, snapshots, cfg.PollInterval, cfg.PollTimeout)
	}()

	select {
	case err := <-serveErr:
		stop()
		<-pollDone
		return err
	case <-ctx.Done():
		log.Print("kwatch: shutting down")
	}

	<-pollDone
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// poll collects lag on a ticker until ctx is cancelled, collecting once up
// front so metrics are populated before the first interval elapses.
func poll(ctx context.Context, c *lag.Collector, m *metrics.Metrics, store *snapshotStore, interval, timeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		collectOnce(ctx, c, m, store, timeout)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func collectOnce(ctx context.Context, c *lag.Collector, m *metrics.Metrics, store *snapshotStore, timeout time.Duration) {
	start := time.Now()
	roundCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	snap, err := c.Collect(roundCtx)
	if err != nil {
		if ctx.Err() != nil {
			return // Shutting down, not a collection failure.
		}
		m.ObserveError(time.Since(start))
		log.Printf("kwatch: collection failed: %v", err)
		return
	}
	m.Observe(snap)
	store.set(snap)
}

// snapshotStore holds the most recent snapshot for the /api/lag endpoint.
type snapshotStore struct {
	mu   sync.RWMutex
	snap lag.Snapshot
}

func (s *snapshotStore) set(snap lag.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = snap
}

func (s *snapshotStore) get() lag.Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap
}

func newMux(registry *prometheus.Registry, store *snapshotStore) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	// /api/lag serves the last snapshot as JSON, which is handier than
	// reading the Prometheus text format when debugging by hand.
	mux.HandleFunc("/api/lag", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(store.get()); err != nil {
			log.Printf("kwatch: encode /api/lag: %v", err)
		}
	})

	return mux
}
