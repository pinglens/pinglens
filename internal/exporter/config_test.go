package exporter

import (
	"strings"
	"testing"
	"time"
)

func TestParseConfigDefaultsAndLabels(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
targets:
  - host: 1.1.1.1
    ping_group: default
    asn: 13335
  - host: one.one.one.one
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Interval != 2500*time.Millisecond || cfg.Timeout != 2*time.Second || cfg.MaxTTL != 30 {
		t.Fatalf("defaults = %s %s %d", cfg.Interval, cfg.Timeout, cfg.MaxTTL)
	}
	if got := cfg.Targets[0].Labels; len(got) != 2 || got["ping_group"] != "default" || got["asn"] != "13335" {
		t.Fatalf("labels = %v", got)
	}
	if got := cfg.Targets[1]; got.Host != "one.one.one.one" || len(got.Labels) != 0 {
		t.Fatalf("second target = %+v", got)
	}
}

func TestParseConfigRejects(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"duplicate host": {"targets:\n  - host: 1.1.1.1\n  - host: 1.1.1.1\n", "duplicate host"},
		"reserved label": {"targets:\n  - host: 1.1.1.1\n    ttl: \"3\"\n", `"ttl" is reserved`},
		"dunder label":   {"targets:\n  - host: 1.1.1.1\n    __meta: x\n", "invalid label name"},
		"bad label":      {"targets:\n  - host: 1.1.1.1\n    bad-name: x\n", "invalid label name"},
		"timeout":        {"interval: 2s\ntimeout: 2s\ntargets:\n  - host: 1.1.1.1\n", "shorter than interval"},
		"interval":       {"interval: 500ms\ntimeout: 100ms\ntargets:\n  - host: 1.1.1.1\n", "shorter than 1s"},
		"max ttl":        {"max_ttl: 32\ntargets:\n  - host: 1.1.1.1\n", "max_ttl"},
		"ipv6":           {"targets:\n  - host: 2606:4700:4700::1111\n", "only IPv4"},
		"no targets":     {"max_ttl: 5\n", "no targets"},
		"unknown key":    {"intervall: 2s\ntargets:\n  - host: 1.1.1.1\n", "intervall"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
