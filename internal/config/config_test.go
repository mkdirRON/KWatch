package config

import (
	"testing"
	"time"
)

func env(pairs map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := pairs[key]
		return v, ok
	}
}

func TestFromEnvDefaults(t *testing.T) {
	cfg, err := FromEnv(env(nil))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if got, want := len(cfg.Brokers), 1; got != want {
		t.Fatalf("brokers = %d, want %d", got, want)
	}
	if cfg.Brokers[0] != "localhost:9092" {
		t.Errorf("broker = %q, want localhost:9092", cfg.Brokers[0])
	}
	if cfg.PollInterval != 15*time.Second {
		t.Errorf("poll interval = %s, want 15s", cfg.PollInterval)
	}
	if cfg.Topics != nil || cfg.Groups != nil {
		t.Errorf("filters = %v/%v, want both unset", cfg.Topics, cfg.Groups)
	}
}

func TestFromEnvOverrides(t *testing.T) {
	cfg, err := FromEnv(env(map[string]string{
		"KWATCH_BROKERS":       "kafka-1:9092, kafka-2:9092",
		"KWATCH_TOPICS":        "orders,payments",
		"KWATCH_GROUPS":        "order_consumer_1",
		"KWATCH_POLL_INTERVAL": "30s",
		"KWATCH_POLL_TIMEOUT":  "5s",
		"KWATCH_LISTEN_ADDR":   ":8080",
	}))
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if len(cfg.Brokers) != 2 || cfg.Brokers[1] != "kafka-2:9092" {
		t.Errorf("brokers = %v, want both addresses trimmed", cfg.Brokers)
	}
	if len(cfg.Topics) != 2 || len(cfg.Groups) != 1 {
		t.Errorf("filters = %v/%v", cfg.Topics, cfg.Groups)
	}
	if cfg.PollInterval != 30*time.Second || cfg.PollTimeout != 5*time.Second {
		t.Errorf("durations = %s/%s", cfg.PollInterval, cfg.PollTimeout)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("listen = %q", cfg.ListenAddr)
	}
}

func TestFromEnvRejectsBadInput(t *testing.T) {
	cases := map[string]map[string]string{
		"empty brokers":     {"KWATCH_BROKERS": " , "},
		"bad duration":      {"KWATCH_POLL_INTERVAL": "soon"},
		"negative duration": {"KWATCH_POLL_INTERVAL": "-5s"},
		"timeout > interval": {
			"KWATCH_POLL_INTERVAL": "5s",
			"KWATCH_POLL_TIMEOUT":  "30s",
		},
	}
	for name, vars := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := FromEnv(env(vars)); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestSplitList(t *testing.T) {
	got := SplitList(" a , ,b,c ")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("SplitList = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SplitList[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
