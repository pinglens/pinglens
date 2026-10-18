// Package exporter publishes continuous traceroute results as Prometheus
// metrics: cumulative per-hop probe counters and RTT histograms from which
// loss and latency can be computed exactly for any window.
package exporter

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/pinglens/pinglens/internal/traceroute"
)

const (
	defaultInterval = 2500 * time.Millisecond
	defaultTimeout  = 2 * time.Second
	defaultMaxTTL   = 30
	minInterval     = time.Second
)

var (
	labelNamePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	// reservedLabels are set by the exporter or by Prometheus itself.
	reservedLabels = map[string]bool{
		"target": true, "target_ip": true, "ttl": true, "hop_ip": true, "hop_host": true,
		"hop_asn": true, "hop_as_name": true, "dest": true, "le": true,
		"cluster": true, "job": true, "instance": true,
	}
)

// Config is a validated exporter configuration file.
type Config struct {
	Interval time.Duration
	Timeout  time.Duration
	MaxTTL   int
	Targets  []Target
}

// Target is one traced host and the labels its series carry.
type Target struct {
	Host   string
	Labels map[string]string
}

func (c Config) options() traceroute.Options {
	return traceroute.Options{Interval: c.Interval, Timeout: c.Timeout, MaxTTL: c.MaxTTL}
}

type fileConfig struct {
	Interval *time.Duration      `yaml:"interval"`
	Timeout  *time.Duration      `yaml:"timeout"`
	MaxTTL   *int                `yaml:"max_ttl"`
	Targets  []map[string]string `yaml:"targets"`
}

// ParseConfig decodes and validates a configuration file. Any invalid entry
// rejects the whole file.
func ParseConfig(data []byte) (Config, error) {
	var raw fileConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	cfg := Config{Interval: defaultInterval, Timeout: defaultTimeout, MaxTTL: defaultMaxTTL}
	if raw.Interval != nil {
		cfg.Interval = *raw.Interval
	}
	if raw.Timeout != nil {
		cfg.Timeout = *raw.Timeout
	}
	if raw.MaxTTL != nil {
		cfg.MaxTTL = *raw.MaxTTL
	}
	if cfg.Interval < minInterval {
		return Config{}, fmt.Errorf("interval %s is shorter than %s", cfg.Interval, minInterval)
	}
	if cfg.Timeout <= 0 || cfg.Timeout >= cfg.Interval {
		return Config{}, fmt.Errorf("timeout %s must be positive and shorter than interval %s", cfg.Timeout, cfg.Interval)
	}
	if cfg.MaxTTL < 1 || cfg.MaxTTL > traceroute.MaxTTLLimit {
		return Config{}, fmt.Errorf("max_ttl %d is outside 1..%d", cfg.MaxTTL, traceroute.MaxTTLLimit)
	}
	if len(raw.Targets) == 0 {
		return Config{}, errors.New("no targets configured")
	}
	seen := map[string]bool{}
	for i, entry := range raw.Targets {
		t, err := parseTarget(entry)
		if err != nil {
			return Config{}, fmt.Errorf("target %d: %w", i+1, err)
		}
		if seen[t.Host] {
			return Config{}, fmt.Errorf("target %d: duplicate host %q", i+1, t.Host)
		}
		seen[t.Host] = true
		cfg.Targets = append(cfg.Targets, t)
	}
	return cfg, nil
}

func parseTarget(entry map[string]string) (Target, error) {
	host := strings.TrimSpace(entry["host"])
	if host == "" {
		return Target{}, errors.New("host is required")
	}
	if strings.ContainsAny(host, " \t/") {
		return Target{}, fmt.Errorf("invalid host %q", host)
	}
	if addr, err := netip.ParseAddr(host); err == nil && !addr.Is4() {
		return Target{}, fmt.Errorf("host %q: only IPv4 targets are supported", host)
	}
	labels := maps.Clone(entry)
	delete(labels, "host")
	for name := range labels {
		if !labelNamePattern.MatchString(name) || strings.HasPrefix(name, "__") {
			return Target{}, fmt.Errorf("host %q: invalid label name %q", host, name)
		}
		if reservedLabels[name] {
			return Target{}, fmt.Errorf("host %q: label name %q is reserved", host, name)
		}
	}
	return Target{Host: host, Labels: labels}, nil
}
