// Package config holds KWatch's runtime configuration.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Config is the full runtime configuration for the KWatch service.
type Config struct {
	// Brokers is the list of Kafka bootstrap addresses.
	Brokers []string

	// Topics limits monitoring to these topics. Empty means every topic
	// the cluster reports, minus Kafka's internal ones.
	Topics []string

	// Groups limits monitoring to these consumer groups. Empty means every
	// group the cluster reports.
	Groups []string

	// PollInterval is how often lag is collected from the cluster.
	PollInterval time.Duration

	// PollTimeout bounds a single collection round.
	PollTimeout time.Duration

	// ListenAddr is the address the HTTP server binds to. It serves
	// /metrics, /healthz and /api/lag.
	ListenAddr string
}

// Default returns the configuration used when nothing is set in the environment.
func Default() Config {
	return Config{
		Brokers:      []string{"localhost:9092"},
		PollInterval: 15 * time.Second,
		PollTimeout:  10 * time.Second,
		ListenAddr:   ":9090",
	}
}

// FromEnv builds a Config from KWATCH_* environment variables, falling back to
// Default for anything unset. Lookup is injected so it can be tested without
// touching the real environment.
func FromEnv(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	cfg := Default()

	if v, ok := lookup("KWATCH_BROKERS"); ok {
		cfg.Brokers = SplitList(v)
		if len(cfg.Brokers) == 0 {
			return cfg, fmt.Errorf("KWATCH_BROKERS is set but empty")
		}
	}
	if v, ok := lookup("KWATCH_TOPICS"); ok {
		cfg.Topics = SplitList(v)
	}
	if v, ok := lookup("KWATCH_GROUPS"); ok {
		cfg.Groups = SplitList(v)
	}
	if v, ok := lookup("KWATCH_LISTEN_ADDR"); ok {
		cfg.ListenAddr = v
	}

	for _, d := range []struct {
		key string
		dst *time.Duration
	}{
		{"KWATCH_POLL_INTERVAL", &cfg.PollInterval},
		{"KWATCH_POLL_TIMEOUT", &cfg.PollTimeout},
	} {
		v, ok := lookup(d.key)
		if !ok {
			continue
		}
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("%s: %w", d.key, err)
		}
		if parsed <= 0 {
			return cfg, fmt.Errorf("%s must be positive, got %s", d.key, v)
		}
		*d.dst = parsed
	}

	return cfg, cfg.Validate()
}

// Validate reports whether the configuration can be used to start the service.
func (c Config) Validate() error {
	if len(c.Brokers) == 0 {
		return fmt.Errorf("at least one broker is required")
	}
	if c.ListenAddr == "" {
		return fmt.Errorf("listen address is required")
	}
	if c.PollInterval <= 0 {
		return fmt.Errorf("poll interval must be positive")
	}
	if c.PollTimeout <= 0 {
		return fmt.Errorf("poll timeout must be positive")
	}
	if c.PollTimeout > c.PollInterval {
		return fmt.Errorf("poll timeout (%s) must not exceed poll interval (%s)", c.PollTimeout, c.PollInterval)
	}
	return nil
}

// SplitList parses a comma-separated list, dropping blank entries and
// surrounding whitespace.
func SplitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
