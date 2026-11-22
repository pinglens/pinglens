package exporter

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/pinglens/pinglens/internal/traceroute"
)

// TestHealthCountsConfigReloads covers each reload result, an unchanged
// file counting nothing, and the last-success timestamp following applied
// loads only.
func TestHealthCountsConfigReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeFile(t, path, "targets:\n  - host: 1.1.1.1\n")
	before := time.Now()
	clock := time.Unix(1_700_000_000, 0)
	e, reg := startExporter(t, path, newFakeTracer(), func() time.Time { return clock })
	const reloads = "pinglens_exporter_config_reloads_total"
	const lastSuccess = "pinglens_exporter_config_last_reload_success_timestamp_seconds"
	check := func(step string, applied, invalid, failed float64) []*dto.MetricFamily {
		t.Helper()
		families := gather(t, reg)
		for result, want := range map[string]float64{"applied": applied, "invalid": invalid, "error": failed} {
			if got := value(t, families, reloads, map[string]string{"result": result}); got != want {
				t.Fatalf("%s: %s{result=%q} = %v, want %v", step, reloads, result, got, want)
			}
		}
		return families
	}

	families := check("startup", 0, 0, 0)
	if got := value(t, families, lastSuccess, nil); got < float64(before.Unix()) {
		t.Fatalf("startup: last success %v, want the initial load's time", got)
	}
	e.reload()
	check("unchanged", 0, 0, 0)

	writeFile(t, path, "targets: []\n")
	e.reload()
	e.reload()
	check("invalid file, read twice", 0, 1, 0)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	e.reload()
	families = check("missing file", 0, 1, 1)
	if got := value(t, families, lastSuccess, nil); got == float64(clock.Unix()) {
		t.Fatal("a failed reload set the last success timestamp")
	}

	writeFile(t, path, "targets:\n  - host: 1.1.1.1\n  - host: 8.8.8.8\n")
	e.reload()
	families = check("changed file", 1, 1, 1)
	if got := value(t, families, lastSuccess, nil); got != float64(clock.Unix()) {
		t.Fatalf("last success = %v, want %v", got, float64(clock.Unix()))
	}
	if got := value(t, families, "pinglens_exporter_targets", nil); got != 2 {
		t.Fatalf("targets = %v after the reload, want 2", got)
	}
}

// TestHealthTargetGaugesAndOutcomes covers the target gauges following each
// target's latest state, and the resolution and probe outcome counters
// summing over targets.
func TestHealthTargetGaugesAndOutcomes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeFile(t, path, "targets:\n  - host: 192.0.2.1\n  - host: 192.0.2.2\n  - host: example.net\n")
	tracer := newFakeTracer()
	_, reg := startExporter(t, path, tracer, nil)
	tracer.wait(t, "192.0.2.1", "192.0.2.2", "example.net")

	// Discovered: three outcomes at TTL 1, and two replies.
	found := tracer.callbacks("192.0.2.1")
	found.OnTargetUp(true)
	for range 3 {
		found.OnTimeout(1)
	}
	found.OnReply(traceroute.Reply{TTL: 2, From: netip.MustParseAddr("10.0.0.2"), RTT: time.Millisecond})
	found.OnReply(traceroute.Reply{TTL: 3, From: netip.MustParseAddr("192.0.2.1"), RTT: time.Millisecond, Dest: true})
	// Resolved, still discovering.
	slow := tracer.callbacks("192.0.2.2")
	slow.OnTargetUp(true)
	slow.OnTimeout(1)
	slow.OnTimeout(1)
	// Failing to resolve.
	failing := tracer.callbacks("example.net")
	failing.OnTargetUp(false)
	failing.OnTargetUp(false)

	want := func(families []*dto.MetricFamily, name string, labels map[string]string, want float64) {
		t.Helper()
		if got := value(t, families, name, labels); got != want {
			t.Fatalf("%s%v = %v, want %v", name, labels, got, want)
		}
	}
	families := gather(t, reg)
	want(families, "pinglens_exporter_targets", nil, 3)
	want(families, "pinglens_exporter_targets_discovering", nil, 2)
	want(families, "pinglens_exporter_targets_unresolved", nil, 1)
	want(families, "pinglens_exporter_target_resolutions_total", map[string]string{"result": "success"}, 2)
	want(families, "pinglens_exporter_target_resolutions_total", map[string]string{"result": "failure"}, 2)
	want(families, "pinglens_exporter_probe_outcomes_total", map[string]string{"outcome": "reply"}, 2)
	want(families, "pinglens_exporter_probe_outcomes_total", map[string]string{"outcome": "timeout"}, 5)
	want(families, "pinglens_exporter_hop_series", nil, 2)

	failing.OnTargetUp(true)
	families = gather(t, reg)
	want(families, "pinglens_exporter_targets_unresolved", nil, 0)
	want(families, "pinglens_exporter_target_resolutions_total", map[string]string{"result": "success"}, 3)
}

// TestHealthPassesProberAndHopNameStats covers the prober and hop name
// counters, with every label value exported from zero.
func TestHealthPassesProberAndHopNameStats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeFile(t, path, "targets:\n  - host: 1.1.1.1\n")
	tracer := newFakeTracer()
	tracer.stats = traceroute.ProberStats{ReceiveErrors: 1, Unmatched: 7, FamilyUnavailable: 2}
	pending := &atomic.Int64{}
	pending.Store(1)
	names := fakeNames{pending: pending, size: 4, lookups: traceroute.LookupStats{
		PTR:    traceroute.LookupResults{Success: 3, NotFound: 1},
		ASN:    traceroute.LookupResults{Error: 2},
		ASName: traceroute.LookupResults{Success: 1},
	}}
	e, err := New(slog.New(slog.DiscardHandler), path, tracer, names)
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(e)

	const expected = `
# HELP pinglens_exporter_hop_name_lookups_pending Hop addresses whose name lookups have not all settled.
# TYPE pinglens_exporter_hop_name_lookups_pending gauge
pinglens_exporter_hop_name_lookups_pending 1
# HELP pinglens_exporter_hop_name_lookups_total Completed hop name lookups by kind and result: success, not_found (NXDOMAIN, NODATA or an unusable answer) or error.
# TYPE pinglens_exporter_hop_name_lookups_total counter
pinglens_exporter_hop_name_lookups_total{kind="as_name",result="error"} 0
pinglens_exporter_hop_name_lookups_total{kind="as_name",result="not_found"} 0
pinglens_exporter_hop_name_lookups_total{kind="as_name",result="success"} 1
pinglens_exporter_hop_name_lookups_total{kind="asn",result="error"} 2
pinglens_exporter_hop_name_lookups_total{kind="asn",result="not_found"} 0
pinglens_exporter_hop_name_lookups_total{kind="asn",result="success"} 0
pinglens_exporter_hop_name_lookups_total{kind="ptr",result="error"} 0
pinglens_exporter_hop_name_lookups_total{kind="ptr",result="not_found"} 1
pinglens_exporter_hop_name_lookups_total{kind="ptr",result="success"} 3
# HELP pinglens_exporter_hop_names Hop addresses in the name cache.
# TYPE pinglens_exporter_hop_names gauge
pinglens_exporter_hop_names 4
# HELP pinglens_exporter_icmp_receive_errors_total Failed reads from the raw ICMP sockets.
# TYPE pinglens_exporter_icmp_receive_errors_total counter
pinglens_exporter_icmp_receive_errors_total 1
# HELP pinglens_exporter_icmp_unmatched_total Received ICMP messages that answer no pending probe: other ICMP traffic, and late or duplicate replies.
# TYPE pinglens_exporter_icmp_unmatched_total counter
pinglens_exporter_icmp_unmatched_total 7
# HELP pinglens_exporter_traces_failed_total Traces that could not start: invalid_options, family_unavailable (an IP literal of a family without a socket) or no_echo_id.
# TYPE pinglens_exporter_traces_failed_total counter
pinglens_exporter_traces_failed_total{reason="family_unavailable"} 2
pinglens_exporter_traces_failed_total{reason="invalid_options"} 0
pinglens_exporter_traces_failed_total{reason="no_echo_id"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"pinglens_exporter_hop_name_lookups_pending", "pinglens_exporter_hop_name_lookups_total", "pinglens_exporter_hop_names",
		"pinglens_exporter_icmp_receive_errors_total", "pinglens_exporter_icmp_unmatched_total", "pinglens_exporter_traces_failed_total",
	); err != nil {
		t.Fatal(err)
	}
}

// TestHealthReadyWarmupAndCollectDuration covers ready and warmup_seconds
// following the warm latch, and the collect duration appearing once a
// previous Collect finished.
func TestHealthReadyWarmupAndCollectDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeFile(t, path, "targets:\n  - host: 1.1.1.1\n")
	e, err := New(slog.New(slog.DiscardHandler), path, newFakeTracer(), fakeNames{})
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(e)
	const (
		ready    = "pinglens_exporter_ready"
		warmup   = "pinglens_exporter_warmup_seconds"
		duration = "pinglens_exporter_last_collect_duration_seconds"
	)

	families := gather(t, reg)
	if got := value(t, families, ready, nil); got != 0 {
		t.Fatalf("ready = %v before warm-up, want 0", got)
	}
	for _, name := range []string{warmup, duration} {
		if n := len(matching(families, name, nil)); n != 0 {
			t.Fatalf("%s exported on the first scrape before warm-up", name)
		}
	}

	start := time.Unix(1_700_000_000, 0)
	e.started = start
	e.now = func() time.Time { return start.Add(42 * time.Second) }
	if !e.checkWarm() {
		t.Fatal("exporter without targets or pending names did not warm up")
	}
	families = gather(t, reg)
	if got := value(t, families, ready, nil); got != 1 {
		t.Fatalf("ready = %v after warm-up, want 1", got)
	}
	if got := value(t, families, warmup, nil); got != 42 {
		t.Fatalf("warmup = %v, want 42", got)
	}
	if got := value(t, families, duration, nil); got < 0 {
		t.Fatalf("last collect duration = %v", got)
	}
}

// TestHandlerServesHealthAndRuntimeMetrics covers the /metrics registry:
// trace and health metrics, build info and the Go and process collectors.
func TestHandlerServesHealthAndRuntimeMetrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeFile(t, path, "targets:\n  - host: 1.1.1.1\n")
	e, err := New(slog.New(slog.DiscardHandler), path, newFakeTracer(), fakeNames{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(e.Handler())
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	buildInfo := `pinglens_exporter_build_info{goversion="` + runtime.Version() + `",`
	for _, want := range []string{metricHop + "{", buildInfo, "pinglens_exporter_targets 0", "go_goroutines ", "process_cpu_seconds_total "} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
}

// value returns the counter or gauge value of the one series of family name
// whose labels include want.
func value(t *testing.T, families []*dto.MetricFamily, name string, want map[string]string) float64 {
	t.Helper()
	m := find(t, families, name, want)
	if c := m.GetCounter(); c != nil {
		return c.GetValue()
	}
	return m.GetGauge().GetValue()
}
