package exporter

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/pinglens/pinglens/internal/traceroute"
)

const (
	// staleAfter is how long a hop series, a TTL's sent counter or a hop
	// name survives without a probe outcome, bounding cardinality as paths
	// change.
	staleAfter = time.Hour
	gcInterval = 5 * time.Minute
	// maxWarmup bounds how long /readyz waits for traces to discover their
	// paths and for hop names, so a target that never resolves or a broken
	// resolver delays readiness rather than blocking it.
	maxWarmup = 5 * time.Minute
)

// Tracer runs one continuous trace until ctx is cancelled;
// *traceroute.Prober implements it.
type Tracer interface {
	Trace(ctx context.Context, host string, opts traceroute.Options, cb traceroute.Callbacks)
}

// HopNamer annotates hop addresses; *traceroute.HopNames implements it.
type HopNamer interface {
	Get(addr netip.Addr) (traceroute.HopName, bool)
	Range(fn func(netip.Addr, traceroute.HopName))
	Pending() int
	Prune()
}

// Exporter runs one trace per configured target and collects their state as
// Prometheus metrics. Target list changes in the configuration file apply in
// place without disturbing the counters of unchanged targets.
type Exporter struct {
	logger *slog.Logger
	path   string
	tracer Tracer
	names  HopNamer
	now    func() time.Time
	ready  atomic.Bool
	// warm latches once traces have discovered their paths and the hop
	// names they found are looked up; see checkWarm.
	warm    atomic.Bool
	started time.Time

	mu      sync.Mutex
	cfg     Config
	sum     [sha256.Size]byte
	targets map[string]*targetState
	ctx     context.Context
	wg      sync.WaitGroup
}

// New loads the configuration file at path. Traces start with Run.
func New(logger *slog.Logger, path string, tracer Tracer, names HopNamer) (*Exporter, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &Exporter{
		logger:  logger,
		path:    path,
		tracer:  tracer,
		names:   names,
		now:     time.Now,
		cfg:     cfg,
		sum:     sha256.Sum256(data),
		targets: map[string]*targetState{},
	}, nil
}

// Run starts every target's trace and re-reads the configuration file every
// reloadInterval until ctx is cancelled, then stops the traces.
func (e *Exporter) Run(ctx context.Context, reloadInterval time.Duration) {
	e.mu.Lock()
	e.ctx = ctx
	e.started = e.now()
	for _, t := range e.cfg.Targets {
		e.startLocked(t)
	}
	e.mu.Unlock()
	e.ready.Store(true)
	defer e.stopAll()

	reload := time.NewTicker(reloadInterval)
	defer reload.Stop()
	gc := time.NewTicker(gcInterval)
	defer gc.Stop()
	warmup := time.NewTicker(time.Second)
	defer warmup.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-reload.C:
			e.reload()
		case <-gc.C:
			e.names.Prune()
			e.pruneIdle()
		case <-warmup.C:
			if e.checkWarm() {
				warmup.Stop()
			}
		}
	}
}

// checkWarm latches warm once every trace has discovered its path and every
// hop name found so far has settled, or after maxWarmup. Until then /readyz
// fails, so a rolling update keeps the previous pod, with its names, serving
// while this one finds and looks up its hops.
func (e *Exporter) checkWarm() bool {
	e.mu.Lock()
	started := e.started
	e.mu.Unlock()
	elapsed := e.now().Sub(started)
	discovering := 0
	for _, state := range e.states() {
		if !state.discovered() {
			discovering++
		}
	}
	pending := e.names.Pending()
	if (discovering > 0 || pending > 0) && elapsed < maxWarmup {
		return false
	}
	e.warm.Store(true)
	e.logger.Info("warmed up", "after", elapsed.Round(time.Second), "discovering_targets", discovering, "pending_hop_names", pending)
	return true
}

// Handler serves /metrics from a registry holding only this exporter's
// metrics, /healthz once traces are running, and /readyz once warm.
func (e *Exporter) Handler() http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(e)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !e.ready.Load() {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !e.warm.Load() {
			http.Error(w, "warming up", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// reload applies a changed configuration file. An invalid file is logged and
// ignored; the running configuration stays. Target changes apply in place.
// interval, timeout and max_ttl only apply at startup: the chart rolls the
// pod when they change, so a running exporter keeps its settings rather
// than exiting and restarting a pod that is about to be replaced.
func (e *Exporter) reload() {
	data, err := os.ReadFile(e.path)
	if err != nil {
		e.logger.Error("read config", "path", e.path, "err", err)
		return
	}
	sum := sha256.Sum256(data)
	e.mu.Lock()
	if sum == e.sum {
		e.mu.Unlock()
		return
	}
	e.sum = sum
	cfg, err := ParseConfig(data)
	if err != nil {
		e.mu.Unlock()
		e.logger.Error("invalid config; keeping the running one", "path", e.path, "err", err)
		return
	}
	if cfg.options() != e.cfg.options() {
		e.logger.Warn("global settings changed; restart to apply them", "path", e.path,
			"interval", cfg.Interval, "timeout", cfg.Timeout, "max_ttl", cfg.MaxTTL)
		cfg.Interval, cfg.Timeout, cfg.MaxTTL = e.cfg.Interval, e.cfg.Timeout, e.cfg.MaxTTL
	}

	wanted := map[string]Target{}
	for _, t := range cfg.Targets {
		wanted[t.Host] = t
	}
	var removed []*targetState
	for host, state := range e.targets {
		if _, ok := wanted[host]; !ok {
			delete(e.targets, host)
			removed = append(removed, state)
		}
	}
	added, relabelled := 0, 0
	for _, t := range cfg.Targets {
		state, ok := e.targets[t.Host]
		if !ok {
			e.startLocked(t)
			added++
			continue
		}
		if state.setLabels(t.Labels) {
			relabelled++
		}
	}
	e.cfg = cfg
	e.mu.Unlock()

	for _, state := range removed {
		state.stop()
	}
	e.logger.Info("targets reloaded", "added", added, "removed", len(removed), "relabelled", relabelled)
}

func (e *Exporter) startLocked(t Target) {
	ctx, cancel := context.WithCancel(e.ctx)
	state := &targetState{
		host:   t.Host,
		labels: maps.Clone(t.Labels),
		hops:   map[hopKey]*hopSeries{},
		cancel: cancel,
		done:   make(chan struct{}),
	}
	e.targets[t.Host] = state
	opts := e.cfg.options()
	e.wg.Go(func() {
		defer close(state.done)
		e.tracer.Trace(ctx, t.Host, opts, state.callbacks(e.names, e.now))
	})
}

func (e *Exporter) stopAll() {
	e.mu.Lock()
	for _, state := range e.targets {
		state.cancel()
	}
	e.mu.Unlock()
	e.wg.Wait()
}

func (e *Exporter) pruneIdle() {
	cutoff := e.now().Add(-staleAfter)
	for _, state := range e.states() {
		state.pruneIdle(cutoff)
	}
}

func (e *Exporter) states() []*targetState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Collect(maps.Values(e.targets))
}

type hopKey struct {
	ttl  int
	ip   netip.Addr
	dest bool
}

type hopSeries struct {
	buckets [len(rttBuckets)]uint64
	count   uint64
	sum     float64
	last    time.Time
}

// targetState is one trace's cumulative counters, guarded by mu.
type targetState struct {
	host   string
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	labels map[string]string
	up     bool
	// resolveFailed is set while the latest resolution failed.
	resolveFailed bool
	ip            netip.Addr
	destTTL       int
	// sent counts probes per TTL once they are answered or time out, so it
	// and the reply counts always describe the same probes. Probes the
	// engine discards (past the destination, or in flight at an address
	// change) are never counted.
	sent     [traceroute.MaxTTLLimit + 1]uint64
	sentLast [traceroute.MaxTTLLimit + 1]time.Time
	hops     map[hopKey]*hopSeries
}

func (s *targetState) callbacks(names HopNamer, now func() time.Time) traceroute.Callbacks {
	return traceroute.Callbacks{
		OnReply: func(r traceroute.Reply) {
			names.Get(r.From)
			key := hopKey{ttl: r.TTL, ip: r.From, dest: r.Dest}
			seconds := r.RTT.Seconds()
			at := now()
			s.mu.Lock()
			s.countSentLocked(r.TTL, at)
			series := s.hops[key]
			if series == nil {
				series = &hopSeries{}
				s.hops[key] = series
			}
			for i, bound := range rttBuckets {
				if seconds <= bound {
					series.buckets[i]++
					break
				}
			}
			series.count++
			series.sum += seconds
			series.last = at
			s.mu.Unlock()
		},
		OnTimeout: func(ttl int) {
			at := now()
			s.mu.Lock()
			s.countSentLocked(ttl, at)
			s.mu.Unlock()
		},
		OnDestTTL: func(n int) {
			s.mu.Lock()
			s.destTTL = n
			s.mu.Unlock()
		},
		OnTargetIP: func(addr netip.Addr) {
			s.mu.Lock()
			s.ip = addr
			s.mu.Unlock()
		},
		OnTargetUp: func(up bool) {
			s.mu.Lock()
			s.up = up
			s.resolveFailed = !up
			s.mu.Unlock()
		},
	}
}

// discovered reports whether the trace has found its path. Every round ends
// with one outcome at TTL 1, so the third such outcome comes two intervals
// after round 1 began, past round 1's timeout: round 1 has seen every hop
// that answers and asked for its name. A target that failed to resolve and
// has no address has nothing to discover yet.
func (s *targetState) discovered() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent[1] >= 3 || (s.resolveFailed && !s.ip.IsValid())
}

func (s *targetState) countSentLocked(ttl int, at time.Time) {
	s.sent[ttl]++
	s.sentLast[ttl] = at
}

// setLabels swaps the target's labels and reports whether they changed.
func (s *targetState) setLabels(labels map[string]string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if maps.Equal(s.labels, labels) {
		return false
	}
	s.labels = maps.Clone(labels)
	return true
}

// pruneIdle drops the sent counters and hop series without a probe outcome
// since cutoff, such as TTLs past a destination that moved closer or routers
// no longer on the path. A TTL that becomes active again restarts from zero,
// which Prometheus reads as a counter reset.
func (s *targetState) pruneIdle(cutoff time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, series := range s.hops {
		if series.last.Before(cutoff) {
			delete(s.hops, key)
		}
	}
	for ttl, last := range s.sentLast {
		if s.sent[ttl] > 0 && last.Before(cutoff) {
			s.sent[ttl] = 0
		}
	}
}

// stop cancels the trace and waits for it to release its echo ID.
func (s *targetState) stop() {
	s.cancel()
	<-s.done
}
