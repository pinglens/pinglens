package exporter

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/pinglens/pinglens/internal/traceroute"
)

func TestReloadAppliesTargetChangesInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeFile(t, path, `
targets:
  - host: 1.1.1.1
    ping_group: default
  - host: 8.8.8.8
    ping_group: default
`)
	tracer := newFakeTracer()
	e, reg := startExporter(t, path, tracer, nil)
	tracer.wait(t, "1.1.1.1", "8.8.8.8")

	one := tracer.callbacks("1.1.1.1")
	one.OnTargetIP(netip.MustParseAddr("1.1.1.1"))
	one.OnTargetUp(true)
	// Sent counts probe outcomes: two timeouts and one reply.
	for range 2 {
		one.OnTimeout(1)
	}
	one.OnReply(traceroute.Reply{TTL: 1, From: netip.MustParseAddr("10.0.0.1"), RTT: 3 * time.Millisecond})
	one.OnDestTTL(4)

	families := gather(t, reg)
	sent := find(t, families, metricSent, map[string]string{"target": "1.1.1.1", "ttl": "01"})
	if got := sent.GetCounter().GetValue(); got != 3 {
		t.Fatalf("sent = %v, want 3", got)
	}
	rtt := find(t, families, metricRTT, map[string]string{"target": "1.1.1.1", "ttl": "01", "hop_ip": "10.0.0.1", "dest": "false"})
	if h := rtt.GetHistogram(); h.GetSampleCount() != 1 || bucket(h, .0025) != 0 || bucket(h, .005) != 1 || bucket(h, 1) != 1 {
		t.Fatalf("histogram = %v", h)
	}
	hop := find(t, families, metricHop, map[string]string{"hop_ip": "10.0.0.1"})
	if got := labels(hop); len(got) != 4 || got["hop_host"] != "r1.example.net" || got["hop_asn"] != "AS64500" {
		t.Fatalf("hop_info labels = %v", got)
	}

	// Drop 8.8.8.8, add 9.9.9.9 with a label key no other target has, and
	// move 1.1.1.1 to another group.
	writeFile(t, path, `
targets:
  - host: 1.1.1.1
    ping_group: other
  - host: 9.9.9.9
    ping_group: default
    endpoint_name: Quad9
`)
	e.reload()
	tracer.wait(t, "9.9.9.9")
	if !tracer.stopped("8.8.8.8") {
		t.Fatal("removed target's trace is still running")
	}
	one.OnTimeout(1)

	families = gather(t, reg)
	for _, family := range families {
		for _, m := range family.GetMetric() {
			got := labels(m)
			if got["target"] == "8.8.8.8" {
				t.Fatalf("removed target still exported in %s", family.GetName())
			}
			if family.GetName() == metricHop {
				continue
			}
			if _, ok := got["endpoint_name"]; !ok {
				t.Fatalf("%s series %v lacks endpoint_name", family.GetName(), got)
			}
		}
	}
	find(t, families, metricTarget, map[string]string{"target": "9.9.9.9", "endpoint_name": "Quad9", "target_ip": ""})
	find(t, families, metricTarget, map[string]string{"target": "1.1.1.1", "endpoint_name": "", "target_ip": "1.1.1.1"})
	sent = find(t, families, metricSent, map[string]string{"target": "1.1.1.1", "ttl": "01", "ping_group": "other"})
	if got := sent.GetCounter().GetValue(); got != 4 {
		t.Fatalf("relabelled target's sent = %v, want 4 (no reset)", got)
	}

	// An invalid file keeps the running configuration.
	writeFile(t, path, "targets:\n  - host: 1.1.1.1\n    ttl: \"1\"\n")
	e.reload()
	find(t, gather(t, reg), metricTarget, map[string]string{"target": "9.9.9.9"})

	// A global setting change keeps the running settings; the chart rolls
	// the pod for it. Target changes in the same edit still apply.
	writeFile(t, path, "interval: 5s\ntargets:\n  - host: 1.1.1.1\n  - host: 8.8.4.4\n")
	e.reload()
	tracer.wait(t, "8.8.4.4")
	if got := tracer.options("8.8.4.4").Interval; got != defaultInterval {
		t.Fatalf("new target traced every %s, want the running %s", got, defaultInterval)
	}
	if !tracer.stopped("9.9.9.9") {
		t.Fatal("removed target's trace is still running")
	}
}

// TestPruneIdleDropsStaleSeries covers a path that changed: TTLs and hops
// without a probe outcome for staleAfter stop being exported, and a TTL that
// comes back restarts its counter.
func TestPruneIdleDropsStaleSeries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeFile(t, path, "targets:\n  - host: 1.1.1.1\n")
	clock := time.Unix(1_700_000_000, 0)
	tracer := newFakeTracer()
	e, reg := startExporter(t, path, tracer, func() time.Time { return clock })
	tracer.wait(t, "1.1.1.1")
	cb := tracer.callbacks("1.1.1.1")
	hop := func(ttl int, ip string) traceroute.Reply {
		return traceroute.Reply{TTL: ttl, From: netip.MustParseAddr(ip), RTT: time.Millisecond}
	}

	cb.OnReply(hop(1, "10.0.0.1"))
	cb.OnReply(hop(2, "10.0.0.2"))
	cb.OnTimeout(3)
	clock = clock.Add(staleAfter / 2)
	cb.OnReply(hop(1, "10.0.0.1"))
	clock = clock.Add(staleAfter/2 + time.Minute)
	e.pruneIdle()

	families := gather(t, reg)
	if got := find(t, families, metricSent, map[string]string{"ttl": "01"}).GetCounter().GetValue(); got != 2 {
		t.Fatalf("active TTL sent = %v, want 2", got)
	}
	find(t, families, metricRTT, map[string]string{"ttl": "01", "hop_ip": "10.0.0.1"})
	for _, gone := range []struct {
		name   string
		labels map[string]string
	}{
		{metricSent, map[string]string{"ttl": "02"}},
		{metricSent, map[string]string{"ttl": "03"}},
		{metricRTT, map[string]string{"ttl": "02"}},
	} {
		if n := len(matching(families, gone.name, gone.labels)); n != 0 {
			t.Fatalf("idle %s%v still exported", gone.name, gone.labels)
		}
	}

	cb.OnTimeout(3)
	if got := find(t, gather(t, reg), metricSent, map[string]string{"ttl": "03"}).GetCounter().GetValue(); got != 1 {
		t.Fatalf("returning TTL sent = %v, want 1", got)
	}
}

// TestInvalidSeriesDropped covers a label value client_golang rejects: the
// series is dropped and the rest of the scrape still succeeds, instead of
// Collect panicking inside Gather and killing the process.
func TestInvalidSeriesDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeFile(t, path, "targets:\n  - host: 1.1.1.1\n")
	e, err := New(slog.New(slog.DiscardHandler), path, newFakeTracer(), fakeNames{invalid: true})
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(e)

	families := gather(t, reg)
	find(t, families, metricHop, map[string]string{"hop_ip": "10.0.0.1"})
	if n := len(matching(families, metricHop, map[string]string{"hop_ip": "10.0.0.2"})); n != 0 {
		t.Fatal("series with an invalid label value was exported")
	}
}

// TestReadyzWaitsForPathsAndHopNames covers the warm-up a rolling update
// waits on: /readyz fails until traces had two rounds plus the timeout to
// discover their paths and no hop name is pending, then stays ready; a
// resolver that never settles delays it only until maxWarmup.
func TestReadyzWaitsForPathsAndHopNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	writeFile(t, path, "interval: 2s\ntimeout: 1s\ntargets:\n  - host: 1.1.1.1\n")
	start := time.Unix(1_700_000_000, 0)
	for name, steps := range map[string][]struct {
		at      time.Duration
		pending int64
		want    int
	}{
		"names settle": {
			{4 * time.Second, 0, http.StatusServiceUnavailable}, // before 2 rounds + timeout
			{5 * time.Second, 2, http.StatusServiceUnavailable},
			{6 * time.Second, 0, http.StatusOK},
			{7 * time.Second, 3, http.StatusOK}, // new hops later do not unready it
		},
		"names never settle": {
			{maxWarmup - time.Second, 5, http.StatusServiceUnavailable},
			{maxWarmup, 5, http.StatusOK},
		},
	} {
		t.Run(name, func(t *testing.T) {
			pending := &atomic.Int64{}
			e, err := New(slog.New(slog.DiscardHandler), path, newFakeTracer(), fakeNames{pending: pending})
			if err != nil {
				t.Fatal(err)
			}
			now := start
			e.now = func() time.Time { return now }
			e.started = start
			srv := httptest.NewServer(e.Handler())
			defer srv.Close()
			for _, step := range steps {
				now = start.Add(step.at)
				pending.Store(step.pending)
				if !e.warm.Load() {
					e.checkWarm()
				}
				resp, err := srv.Client().Get(srv.URL + "/readyz")
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != step.want {
					t.Fatalf("at %s with %d pending: /readyz %d, want %d", step.at, step.pending, resp.StatusCode, step.want)
				}
			}
		})
	}
}

func startExporter(t *testing.T, path string, tracer *fakeTracer, now func() time.Time) (*Exporter, *prometheus.Registry) {
	t.Helper()
	e, err := New(slog.New(slog.DiscardHandler), path, tracer, fakeNames{})
	if err != nil {
		t.Fatal(err)
	}
	if now != nil {
		e.now = now
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.Run(ctx, time.Hour)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	reg := prometheus.NewRegistry()
	reg.MustRegister(e)
	return e, reg
}

type fakeTracer struct {
	mu      sync.Mutex
	cbs     map[string]traceroute.Callbacks
	opts    map[string]traceroute.Options
	done    map[string]bool
	started chan string
}

func newFakeTracer() *fakeTracer {
	return &fakeTracer{
		cbs:     map[string]traceroute.Callbacks{},
		opts:    map[string]traceroute.Options{},
		done:    map[string]bool{},
		started: make(chan string, 16),
	}
}

func (f *fakeTracer) Trace(ctx context.Context, host string, opts traceroute.Options, cb traceroute.Callbacks) {
	f.mu.Lock()
	f.cbs[host] = cb
	f.opts[host] = opts
	f.mu.Unlock()
	f.started <- host
	<-ctx.Done()
	f.mu.Lock()
	f.done[host] = true
	f.mu.Unlock()
}

func (f *fakeTracer) options(host string) traceroute.Options {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opts[host]
}

func (f *fakeTracer) wait(t *testing.T, hosts ...string) {
	t.Helper()
	want := map[string]bool{}
	for _, h := range hosts {
		want[h] = true
	}
	for len(want) > 0 {
		select {
		case h := <-f.started:
			delete(want, h)
		case <-time.After(5 * time.Second):
			t.Fatalf("traces not started: %v", want)
		}
	}
}

func (f *fakeTracer) callbacks(host string) traceroute.Callbacks {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cbs[host]
}

func (f *fakeTracer) stopped(host string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.done[host]
}

// fakeNames knows one router; with invalid set it also holds an AS name
// that is not UTF-8. pending, when set, is what Pending reports.
type fakeNames struct {
	invalid bool
	pending *atomic.Int64
}

func (fakeNames) Get(netip.Addr) (traceroute.HopName, bool) { return traceroute.HopName{}, false }
func (fakeNames) Prune()                                    {}
func (f fakeNames) Pending() int {
	if f.pending == nil {
		return 0
	}
	return int(f.pending.Load())
}
func (f fakeNames) Range(fn func(netip.Addr, traceroute.HopName)) {
	fn(netip.MustParseAddr("10.0.0.1"), traceroute.HopName{Host: "r1.example.net", ASN: "AS64500", ASName: "EXAMPLE"})
	if f.invalid {
		fn(netip.MustParseAddr("10.0.0.2"), traceroute.HopName{ASN: "AS64501", ASName: "BAD \xff"})
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func gather(t *testing.T, reg *prometheus.Registry) []*dto.MetricFamily {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	return families
}

// find returns the one series of family name whose labels include want.
func find(t *testing.T, families []*dto.MetricFamily, name string, want map[string]string) *dto.Metric {
	t.Helper()
	found := matching(families, name, want)
	if len(found) != 1 {
		t.Fatalf("%s%v matched %d series, want 1", name, want, len(found))
	}
	return found[0]
}

// matching returns the series of family name whose labels include want.
func matching(families []*dto.MetricFamily, name string, want map[string]string) []*dto.Metric {
	var found []*dto.Metric
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
	metrics:
		for _, m := range family.GetMetric() {
			got := labels(m)
			for k, v := range want {
				if value, ok := got[k]; !ok || value != v {
					continue metrics
				}
			}
			found = append(found, m)
		}
	}
	return found
}

func labels(m *dto.Metric) map[string]string {
	out := map[string]string{}
	for _, pair := range m.GetLabel() {
		out[pair.GetName()] = pair.GetValue()
	}
	return out
}

func bucket(h *dto.Histogram, bound float64) uint64 {
	for _, b := range h.GetBucket() {
		if b.GetUpperBound() == bound {
			return b.GetCumulativeCount()
		}
	}
	return 0
}
