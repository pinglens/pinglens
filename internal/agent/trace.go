package agent

import (
	"context"
	"net/netip"
	"sync"
	"time"

	"github.com/pinglens/pinglens/internal/protocol"
	"github.com/pinglens/pinglens/internal/traceroute"
)

// tracer runs one continuous trace; *traceroute.Prober implements it.
type tracer interface {
	Supports(addr netip.Addr) bool
	Trace(ctx context.Context, host string, opts traceroute.Options, cb traceroute.Callbacks)
}

// hopNamer annotates hop addresses; *traceroute.HopNames implements it.
type hopNamer interface {
	Get(addr netip.Addr) (traceroute.HopName, bool)
}

const (
	defaultTraceInterval = time.Second
	defaultTraceMaxTTL   = 30
	defaultTraceReport   = 5 * time.Second
)

// traceSettings turns the job's trace settings into engine options and the
// report interval, filling in defaults for anything unset.
func traceSettings(cfg protocol.TraceConfig) (traceroute.Options, time.Duration) {
	opts := traceroute.Options{
		Interval: time.Duration(cfg.IntervalMillis) * time.Millisecond,
		Timeout:  time.Duration(cfg.TimeoutMillis) * time.Millisecond,
		MaxTTL:   cfg.MaxTTL,
	}
	if opts.Interval <= 0 {
		opts.Interval = defaultTraceInterval
	}
	if opts.Timeout <= 0 || opts.Timeout >= opts.Interval {
		opts.Timeout = opts.Interval * 9 / 10
	}
	if opts.MaxTTL <= 0 || opts.MaxTTL > traceroute.MaxTTLLimit {
		opts.MaxTTL = defaultTraceMaxTTL
	}
	report := time.Duration(cfg.ReportMillis) * time.Millisecond
	if report <= 0 {
		report = defaultTraceReport
	}
	return opts, report
}

// traceLoop traces addr for the life of ctx and posts the rolling hop table
// every report interval and once more when the trace ends.
func traceLoop(ctx context.Context, tr tracer, names hopNamer, job protocol.ProbeJob, addr netip.Addr, event func(protocol.AgentEvent) bool) {
	resolved := addr.String()
	post := func(report protocol.TraceReport) bool {
		return event(protocol.AgentEvent{Type: "trace", Target: job.Target, Resolved: resolved, Trace: &report})
	}
	if !tr.Supports(addr) {
		post(protocol.TraceReport{Error: "this agent cannot trace " + addrFamily(addr) + " targets"})
		return
	}
	opts, every := traceSettings(job.Trace)
	table := &hopTable{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The address was resolved and checked against the allowed ranges
		// already; tracing the literal keeps the engine from resolving again.
		tr.Trace(ctx, resolved, opts, table.callbacks())
	}()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			<-done
			if report, ok := table.report(names); ok {
				post(report)
			}
			return
		case <-ticker.C:
			if report, ok := table.report(names); ok && !post(report) {
				return
			}
		}
	}
}

func addrFamily(addr netip.Addr) string {
	if addr.Unmap().Is4() {
		return "IPv4"
	}
	return "IPv6"
}

// hopTable accumulates probe outcomes per TTL since the job started.
type hopTable struct {
	mu      sync.Mutex
	hops    [traceroute.MaxTTLLimit + 1]hopStats
	destTTL int
}

type hopStats struct {
	sent, lost int
	replies    int
	last, sum  time.Duration
	best       time.Duration
	worst      time.Duration
	// addr is the address that answered most recently, so a path change
	// shows the new hop.
	addr netip.Addr
}

func (t *hopTable) callbacks() traceroute.Callbacks {
	return traceroute.Callbacks{
		OnReply: func(r traceroute.Reply) {
			t.mu.Lock()
			defer t.mu.Unlock()
			h := &t.hops[r.TTL]
			h.sent++
			h.replies++
			h.last = r.RTT
			h.sum += r.RTT
			if h.replies == 1 || r.RTT < h.best {
				h.best = r.RTT
			}
			if r.RTT > h.worst {
				h.worst = r.RTT
			}
			h.addr = r.From
		},
		OnTimeout: func(ttl int) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.hops[ttl].sent++
			t.hops[ttl].lost++
		},
		OnDestTTL: func(n int) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.destTTL = n
		},
	}
}

// report renders the table as mtr-style rows: TTL 1 up to the destination's
// TTL, or up to the farthest TTL with a probe outcome while the destination
// is unreached. A TTL that never answered has no address and 100% loss.
// ok is false until some probe has an outcome.
func (t *hopTable) report(names hopNamer) (protocol.TraceReport, bool) {
	t.mu.Lock()
	hops := t.hops
	limit := t.destTTL
	t.mu.Unlock()
	if limit == 0 {
		for ttl := len(hops) - 1; ttl > 0; ttl-- {
			if hops[ttl].sent > 0 {
				limit = ttl
				break
			}
		}
	}
	if limit == 0 {
		return protocol.TraceReport{}, false
	}
	rows := make([]protocol.TraceRow, 0, limit)
	for ttl := 1; ttl <= limit; ttl++ {
		h := hops[ttl]
		row := protocol.TraceRow{Hop: ttl, Sent: h.sent}
		if h.sent > 0 {
			row.LossPct = 100 * float64(h.lost) / float64(h.sent)
		}
		if h.replies > 0 {
			row.LastMillis = millis(h.last)
			row.AvgMillis = millis(h.sum / time.Duration(h.replies))
			row.BestMillis = millis(h.best)
			row.WorstMillis = millis(h.worst)
		}
		if h.addr.IsValid() {
			row.Address = h.addr.String()
			// The first reports may go out before the names are looked up;
			// later reports carry them.
			name, _ := names.Get(h.addr)
			row.Host, row.ASN, row.ASName = name.Host, name.ASN, name.ASName
		}
		rows = append(rows, row)
	}
	return protocol.TraceReport{Rows: rows}, true
}
