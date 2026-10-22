package agent

import (
	"context"
	"net/netip"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pinglens/pinglens/internal/protocol"
	"github.com/pinglens/pinglens/internal/traceroute"
)

var (
	hop1    = netip.MustParseAddr("192.0.2.1")
	hop1New = netip.MustParseAddr("192.0.2.11")
	dest    = netip.MustParseAddr("198.51.100.9")
)

func routable(netip.Addr) error { return nil }

func TestHopTableReport(t *testing.T) {
	table := &hopTable{}
	cb := table.callbacks()
	if _, ok := table.report(fakeNames{}); ok {
		t.Fatal("reported before any probe outcome")
	}
	cb.OnReply(traceroute.Reply{TTL: 1, From: hop1, RTT: time.Millisecond})
	cb.OnReply(traceroute.Reply{TTL: 1, From: hop1, RTT: 3 * time.Millisecond})
	// The path changes at the first hop: the row shows the new address.
	cb.OnReply(traceroute.Reply{TTL: 1, From: hop1New, RTT: 2 * time.Millisecond})
	cb.OnTimeout(2)
	cb.OnTimeout(2)
	cb.OnReply(traceroute.Reply{TTL: 3, From: dest, RTT: 10 * time.Millisecond, Dest: true})
	cb.OnTimeout(3)
	// A TTL past the destination is not reported.
	cb.OnTimeout(4)
	cb.OnDestTTL(3)

	report, ok := table.report(fakeNames{hop1New: {Host: "gw.example.net", ASN: "AS64500", ASName: "EXAMPLE"}})
	if !ok {
		t.Fatal("no report after probe outcomes")
	}
	want := []protocol.TraceRow{
		{Hop: 1, Address: "192.0.2.11", Host: "gw.example.net", ASN: "AS64500", ASName: "EXAMPLE", Sent: 3, LastMillis: 2, AvgMillis: 2, BestMillis: 1, WorstMillis: 3},
		{Hop: 2, Sent: 2, LossPct: 100},
		{Hop: 3, Address: "198.51.100.9", Sent: 2, LossPct: 50, LastMillis: 10, AvgMillis: 10, BestMillis: 10, WorstMillis: 10},
	}
	if len(report.Rows) != len(want) {
		t.Fatalf("rows = %+v, want %+v", report.Rows, want)
	}
	for i := range want {
		if report.Rows[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i+1, report.Rows[i], want[i])
		}
	}
}

// TestHopTableReportUnreached covers a destination that has not answered
// while the engine keeps probing out to the maximum TTL: rows end one TTL
// past the farthest that answered, as mtr shows them, never past the
// farthest TTL probed.
func TestHopTableReportUnreached(t *testing.T) {
	for name, tc := range map[string]struct {
		answered []int
		probed   int
		wantRows int
	}{
		// Hop 2 is silent but hop 3 answers, so hop 2 stays; the path goes
		// silent after hop 3, as from DFW toward a blackholed address.
		"silent after hop 3":         {[]int{1, 3}, 30, 4},
		"nothing answers":            {nil, 30, 1},
		"last probed TTL answered":   {[]int{1, 2, 3}, 3, 3},
		"no outcomes past the reply": {[]int{1}, 1, 1},
	} {
		t.Run(name, func(t *testing.T) {
			table := &hopTable{}
			cb := table.callbacks()
			for ttl := 1; ttl <= tc.probed; ttl++ {
				if slices.Contains(tc.answered, ttl) {
					cb.OnReply(traceroute.Reply{TTL: ttl, From: hop1, RTT: time.Millisecond})
				} else {
					cb.OnTimeout(ttl)
				}
			}
			cb.OnDestTTL(0)
			report, _ := table.report(fakeNames{})
			if len(report.Rows) != tc.wantRows {
				t.Fatalf("got %d rows %+v, want %d", len(report.Rows), report.Rows, tc.wantRows)
			}
			if last := report.Rows[len(report.Rows)-1]; !slices.Contains(tc.answered, last.Hop) && last.LossPct != 100 {
				t.Fatalf("last row %+v, want the first silent TTL at 100%% loss", last)
			}
		})
	}
}

func TestTraceLoopReportsUnsupportedFamily(t *testing.T) {
	var events []protocol.AgentEvent
	traceLoop(context.Background(), &fakeTracer{supports: false}, fakeNames{}, routable, protocol.ProbeJob{Target: "example.net"},
		netip.MustParseAddr("2001:db8::9"), func(e protocol.AgentEvent) bool { events = append(events, e); return true })
	if len(events) != 1 || events[0].Type != "trace" || events[0].Trace.Error != "this agent cannot trace IPv6 targets" {
		t.Fatalf("events = %+v", events)
	}
}

// TestTraceLoopReportsNoRoute covers a family whose socket opens but which
// has no route, such as IPv6 on loopback only: the trace reports why it
// cannot run instead of starting and losing every probe.
func TestTraceLoopReportsNoRoute(t *testing.T) {
	tr := &fakeTracer{supports: true}
	var events []protocol.AgentEvent
	traceLoop(context.Background(), tr, fakeNames{}, func(netip.Addr) error { return syscall.ENETUNREACH }, protocol.ProbeJob{Target: "example.net"},
		netip.MustParseAddr("2001:db8::9"), func(e protocol.AgentEvent) bool { events = append(events, e); return true })
	if len(events) != 1 || events[0].Trace.Error != "cannot trace 2001:db8::9: network is unreachable" {
		t.Fatalf("events = %+v", events)
	}
	if tr.host != "" {
		t.Fatal("traced a target without a route")
	}
}

// TestTraceLoopReports covers the periodic reports and the final one when
// the job ends, both carrying the engine's outcomes.
func TestTraceLoopReports(t *testing.T) {
	tr := &fakeTracer{supports: true}
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var reports []protocol.TraceReport
	done := make(chan struct{})
	go func() {
		defer close(done)
		traceLoop(ctx, tr, fakeNames{}, routable, protocol.ProbeJob{Target: "example.net", Trace: protocol.TraceConfig{ReportMillis: 20}}, dest,
			func(e protocol.AgentEvent) bool {
				mu.Lock()
				defer mu.Unlock()
				if e.Type == "trace" && e.Resolved == dest.String() {
					reports = append(reports, *e.Trace)
				}
				return true
			})
	}()
	cb := tr.started(t)
	cb.OnReply(traceroute.Reply{TTL: 1, From: dest, RTT: time.Millisecond, Dest: true})
	cb.OnDestTTL(1)
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(reports)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no periodic report")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cb.OnReply(traceroute.Reply{TTL: 1, From: dest, RTT: 3 * time.Millisecond, Dest: true})
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	last := reports[len(reports)-1]
	if len(last.Rows) != 1 || last.Rows[0].Sent != 2 || last.Rows[0].WorstMillis != 3 {
		t.Fatalf("final report = %+v, want the reply sent after the last periodic report", last)
	}
	if tr.host != dest.String() || tr.opts != (traceroute.Options{Interval: time.Second, Timeout: 900 * time.Millisecond, MaxTTL: 30}) {
		t.Fatalf("traced %q with %+v, want the resolved literal and the default options", tr.host, tr.opts)
	}
}

type fakeNames map[netip.Addr]traceroute.HopName

func (f fakeNames) Get(addr netip.Addr) (traceroute.HopName, bool) {
	name, ok := f[addr]
	return name, ok
}

// fakeTracer records the trace it was asked for and hands the test its
// callbacks; the trace runs until ctx ends.
type fakeTracer struct {
	supports bool
	mu       sync.Mutex
	host     string
	opts     traceroute.Options
	cb       chan traceroute.Callbacks
}

func (f *fakeTracer) Supports(netip.Addr) bool { return f.supports }

func (f *fakeTracer) Trace(ctx context.Context, host string, opts traceroute.Options, cb traceroute.Callbacks) {
	f.mu.Lock()
	f.host, f.opts = host, opts
	ch := f.channel()
	f.mu.Unlock()
	ch <- cb
	<-ctx.Done()
}

func (f *fakeTracer) channel() chan traceroute.Callbacks {
	if f.cb == nil {
		f.cb = make(chan traceroute.Callbacks, 1)
	}
	return f.cb
}

func (f *fakeTracer) started(t *testing.T) traceroute.Callbacks {
	t.Helper()
	f.mu.Lock()
	ch := f.channel()
	f.mu.Unlock()
	select {
	case cb := <-ch:
		return cb
	case <-time.After(5 * time.Second):
		t.Fatal("trace never started")
		return traceroute.Callbacks{}
	}
}
