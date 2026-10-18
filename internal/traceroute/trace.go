package traceroute

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// MaxTTLLimit is the largest TTL a probe can carry: the TTL occupies the
// low five bits of the echo sequence number.
const MaxTTLLimit = 31

const (
	// destMemoryRounds is how many rounds a destination reply at a TTL keeps
	// that TTL as the probing limit. A path that grows by a hop is found again
	// once every remembered reply is this old.
	destMemoryRounds = 10
	// ttlSpacing separates the probes of one round so routers that rate-limit
	// ICMP errors per source see a trickle rather than a burst.
	ttlSpacing      = 10 * time.Millisecond
	resolveInterval = 5 * time.Minute
	resolveRetry    = 30 * time.Second
	resolveTimeout  = 5 * time.Second
	ttlBits         = 5
	roundMask       = 0x7ff
	neverRound      = -1 << 40
)

var payload = []byte("pltrace\x00")

// Options set a trace's probe timing.
type Options struct {
	// Interval is the time between rounds; each round probes every TTL up to
	// the destination once.
	Interval time.Duration
	// Timeout is how long a probe may wait for its reply; later replies are
	// ignored and the probe counts as lost.
	Timeout time.Duration
	// MaxTTL is the farthest hop probed while the destination is unreached.
	MaxTTL int
}

// Reply is one answered probe.
type Reply struct {
	TTL  int
	From netip.Addr
	RTT  time.Duration
	// Dest is true when the destination itself answered rather than a router
	// on the way.
	Dest bool
}

// Callbacks receive a trace's per-probe events. Nil callbacks are skipped.
// OnReply runs on the prober's receive goroutine and the others on the
// trace's goroutine, so callbacks for one trace may run concurrently.
//
// Every probe ends in exactly one OnReply or OnTimeout, except probes
// discarded without an event: those beyond the destination (a lower TTL
// reached it in the probe's round or later) and those in flight when the
// target's address changes or the trace stops. OnReply plus OnTimeout per
// TTL is therefore the count of probes sent with a meaningful outcome.
//
// In a discovery round, which probes up to MaxTTL because no destination
// TTL is remembered, a destination reply is held until every lower TTL of
// the round has its outcome. Replies reordered in flight therefore cannot
// report a TTL past the destination, at the cost of delivering a held reply
// up to a round late when a lower hop never answers.
type Callbacks struct {
	// OnReply fires for each reply received within the timeout.
	OnReply func(Reply)
	// OnTimeout fires when an unanswered probe expires, at the start of the
	// first round after its timeout.
	OnTimeout func(ttl int)
	// OnDestTTL fires once per round with the TTL at which the destination
	// currently answers, or 0 while it is unreached.
	OnDestTTL func(n int)
	// OnTargetIP fires when the target first resolves and on every change of
	// its address. Pending probes are discarded at a change.
	OnTargetIP func(netip.Addr)
	// OnTargetUp reports whether the latest resolution succeeded. A failure
	// keeps probing the last address, if any.
	OnTargetUp func(bool)
}

// conn is the network seam: a raw ICMP socket in production, a simulated
// path in tests.
type conn interface {
	Send(b []byte, ttl int, dst netip.Addr) error
	Recv(b []byte) (n int, from netip.Addr, err error)
	Close() error
}

// Prober sends TTL-limited ICMP echo requests for any number of concurrent
// traces over one raw socket and routes the replies back to their trace by
// echo ID.
type Prober struct {
	logger  *slog.Logger
	conn    conn
	now     func() time.Time
	spacing time.Duration
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)

	mu        sync.RWMutex
	runners   map[uint16]*runner
	idBase    uint16
	nextIndex uint16

	done chan struct{}
}

// NewProber opens the raw ICMP socket and starts the receive loop. Opening
// the socket needs CAP_NET_RAW.
func NewProber(logger *slog.Logger) (*Prober, error) {
	c, err := listenRaw()
	if err != nil {
		return nil, fmt.Errorf("open raw ICMP socket (needs NET_RAW): %w", err)
	}
	p := newProber(logger, c)
	go p.receive()
	return p, nil
}

func newProber(logger *slog.Logger, c conn) *Prober {
	return &Prober{
		logger:  logger,
		conn:    c,
		now:     time.Now,
		spacing: ttlSpacing,
		resolve: resolveIPv4,
		runners: map[uint16]*runner{},
		idBase:  uint16(rand.N(1 << 16)),
		done:    make(chan struct{}),
	}
}

// Close closes the socket and waits for the receive loop to stop. Traces
// still running stop receiving replies.
func (p *Prober) Close() error {
	err := p.conn.Close()
	<-p.done
	return err
}

// Trace probes host continuously until ctx is cancelled. host is an IPv4
// literal or a hostname, re-resolved every five minutes (every 30 seconds
// while resolution fails). A hostname with several addresses keeps its
// current one while DNS still returns it, and otherwise takes the first.
func (p *Prober) Trace(ctx context.Context, host string, opts Options, cb Callbacks) {
	if opts.Interval <= 0 || opts.Timeout <= 0 {
		p.logger.Error("trace needs a positive interval and timeout", "target", host)
		return
	}
	literal, err := netip.ParseAddr(host)
	isLiteral := err == nil
	if isLiteral && !literal.Unmap().Is4() {
		p.logger.Error("trace targets must be IPv4", "target", host)
		return
	}
	r := p.newRunner(host, opts, cb)
	if !p.register(r) {
		p.logger.Error("no free ICMP echo ID", "target", host)
		return
	}
	defer p.unregister(r.id)
	if isLiteral {
		r.setIP(literal.Unmap())
		call(cb.OnTargetUp, true)
	}

	// Spread traces over the interval so their probes do not leave in bursts.
	start := time.NewTimer(rand.N(opts.Interval))
	select {
	case <-ctx.Done():
		start.Stop()
		return
	case <-start.C:
	}
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	var resolveAt time.Time
	for {
		if !isLiteral && !time.Now().Before(resolveAt) {
			resolveAt = time.Now().Add(r.resolve(ctx))
		}
		r.runRound(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Prober) newRunner(host string, opts Options, cb Callbacks) *runner {
	opts.MaxTTL = min(max(opts.MaxTTL, 1), MaxTTLLimit)
	r := &runner{prober: p, host: host, opts: opts, cb: cb, pending: map[uint16]pendingProbe{}, held: map[uint16]heldReply{}}
	r.forgetDestinations()
	return r
}

func (p *Prober) register(r *runner) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for range 1 << 16 {
		id := p.idBase + p.nextIndex
		p.nextIndex++
		if _, used := p.runners[id]; !used {
			r.id = id
			p.runners[id] = r
			return true
		}
	}
	return false
}

func (p *Prober) unregister(id uint16) {
	p.mu.Lock()
	delete(p.runners, id)
	p.mu.Unlock()
}

func (p *Prober) receive() {
	defer close(p.done)
	buf := make([]byte, 1500)
	for {
		n, from, err := p.conn.Recv(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			p.logger.Warn("ICMP receive failed", "error", err)
			time.Sleep(10 * time.Millisecond)
			continue
		}
		p.dispatch(buf[:n], from, p.now())
	}
}

// dispatch routes one received ICMP message to the trace owning its echo ID.
// A raw socket sees every ICMP packet in the network namespace, including
// other pingers', so anything that does not match a pending probe is dropped.
func (p *Prober) dispatch(b []byte, from netip.Addr, now time.Time) {
	id, seq, origDst, quoted, ok := parseReply(b)
	if !ok {
		return
	}
	p.mu.RLock()
	r := p.runners[id]
	p.mu.RUnlock()
	if r != nil {
		r.reply(seq, from.Unmap(), origDst, quoted, now)
	}
}

// parseReply extracts the echo ID and sequence of the probe an ICMP message
// answers: an echo reply carrying our payload directly, or a time-exceeded
// or unreachable error quoting the probe. quoted reports the latter, with
// the probe's original destination in origDst.
func parseReply(b []byte) (id, seq uint16, origDst netip.Addr, quoted, ok bool) {
	msg, err := icmp.ParseMessage(1, b)
	if err != nil {
		return 0, 0, netip.Addr{}, false, false
	}
	var data []byte
	switch body := msg.Body.(type) {
	case *icmp.Echo:
		if msg.Type != ipv4.ICMPTypeEchoReply || !bytes.Equal(body.Data, payload) {
			return 0, 0, netip.Addr{}, false, false
		}
		return uint16(body.ID), uint16(body.Seq), netip.Addr{}, false, true
	case *icmp.TimeExceeded:
		data = body.Data
	case *icmp.DstUnreach:
		data = body.Data
	default:
		return 0, 0, netip.Addr{}, false, false
	}
	if len(data) < ipv4.HeaderLen {
		return 0, 0, netip.Addr{}, false, false
	}
	ihl := int(data[0]&0x0f) * 4
	if ihl < ipv4.HeaderLen || len(data) < ihl+8 || data[ihl] != byte(ipv4.ICMPTypeEcho) {
		return 0, 0, netip.Addr{}, false, false
	}
	id = uint16(data[ihl+4])<<8 | uint16(data[ihl+5])
	seq = uint16(data[ihl+6])<<8 | uint16(data[ihl+7])
	origDst = netip.AddrFrom4([4]byte(data[16:20]))
	return id, seq, origDst, true, true
}

func encodeSeq(round, ttl int) uint16 {
	return uint16(round&roundMask)<<ttlBits | uint16(ttl)
}

func seqTTL(seq uint16) int {
	return int(seq & (1<<ttlBits - 1))
}

type pendingProbe struct {
	sent  time.Time
	round int
	// discovery marks probes of a round sent up to MaxTTL.
	discovery bool
}

// heldReply is a discovery-round destination reply waiting for the lower
// TTLs of its round.
type heldReply struct {
	reply Reply
	round int
}

// runner is one target's trace state.
type runner struct {
	prober *Prober
	id     uint16
	host   string
	opts   Options
	cb     Callbacks

	mu            sync.Mutex
	ip            netip.Addr
	round         int
	pending       map[uint16]pendingProbe
	held          map[uint16]heldReply
	lastDestRound [MaxTTLLimit + 1]int
}

// resolve looks up the target and returns the delay before the next lookup.
func (r *runner) resolve(ctx context.Context) time.Duration {
	lookupCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	addrs, err := r.prober.resolve(lookupCtx, r.host)
	if err == nil && len(addrs) == 0 {
		err = fmt.Errorf("no IPv4 address for %s", r.host)
	}
	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		r.prober.logger.Warn("resolve trace target", "target", r.host, "error", err)
		call(r.cb.OnTargetUp, false)
		return resolveRetry
	}
	// Round-robin DNS reorders its answers; switching between addresses of
	// one name would discard in-flight probes and the destination TTL.
	r.mu.Lock()
	current := r.ip
	r.mu.Unlock()
	if !slices.Contains(addrs, current) {
		r.setIP(addrs[0])
	}
	call(r.cb.OnTargetUp, true)
	return resolveInterval
}

// setIP switches the trace to addr. Probes in flight to the old address and
// remembered destination TTLs no longer apply and are discarded.
func (r *runner) setIP(addr netip.Addr) {
	r.mu.Lock()
	if addr == r.ip {
		r.mu.Unlock()
		return
	}
	r.ip = addr
	clear(r.pending)
	clear(r.held)
	r.forgetDestinations()
	r.mu.Unlock()
	call(r.cb.OnTargetIP, addr)
}

func (r *runner) forgetDestinations() {
	for i := range r.lastDestRound {
		r.lastDestRound[i] = neverRound
	}
}

// runRound expires stale probes, then probes TTL 1 up to the nearest TTL at
// which the destination answered in the last destMemoryRounds rounds, or up
// to MaxTTL when it has not.
func (r *runner) runRound(ctx context.Context) {
	now := r.prober.now()
	r.mu.Lock()
	var expired []int
	expiredRounds := map[int]bool{}
	for seq, probe := range r.pending {
		if now.Sub(probe.sent) > r.opts.Timeout {
			delete(r.pending, seq)
			expiredRounds[probe.round] = true
			if ttl := seqTTL(seq); !r.beyondDestLocked(probe.round, ttl) {
				expired = append(expired, ttl)
			}
		}
	}
	var released []Reply
	for round := range expiredRounds {
		released = append(released, r.settleLocked(round)...)
	}
	round := r.round
	r.round++
	limit, reached := r.opts.MaxTTL, false
	for ttl := 1; ttl <= r.opts.MaxTTL; ttl++ {
		if round-r.lastDestRound[ttl] < destMemoryRounds {
			limit, reached = ttl, true
			break
		}
	}
	ip := r.ip
	r.mu.Unlock()

	if r.cb.OnTimeout != nil {
		for _, ttl := range expired {
			r.cb.OnTimeout(ttl)
		}
	}
	for _, rep := range released {
		call(r.cb.OnReply, rep)
	}
	if reached {
		call(r.cb.OnDestTTL, limit)
	} else {
		call(r.cb.OnDestTTL, 0)
	}
	if !ip.IsValid() {
		return
	}

	for ttl := 1; ttl <= limit; ttl++ {
		if ttl > 1 && r.prober.spacing > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.prober.spacing):
			}
		}
		seq := encodeSeq(round, ttl)
		msg := icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: int(r.id), Seq: int(seq), Data: payload}}
		b, err := msg.Marshal(nil)
		if err != nil {
			r.prober.logger.Error("marshal ICMP echo", "error", err)
			return
		}
		r.mu.Lock()
		if r.ip != ip {
			// The address changed while this round was being sent.
			r.mu.Unlock()
			return
		}
		r.pending[seq] = pendingProbe{sent: r.prober.now(), round: round, discovery: !reached}
		r.mu.Unlock()
		if err := r.prober.conn.Send(b, ttl, ip); err != nil {
			r.prober.logger.Debug("send trace probe", "target", r.host, "ttl", ttl, "error", err)
		}
	}
}

// reply matches an answer to its pending probe. Late, duplicate and foreign
// answers are ignored; a late probe stays pending until runRound expires it.
// An echo reply must come from the target, and an error must quote a probe
// sent to it.
func (r *runner) reply(seq uint16, from, origDst netip.Addr, quoted bool, now time.Time) {
	r.mu.Lock()
	if !r.ip.IsValid() || (quoted && origDst != r.ip) || (!quoted && from != r.ip) {
		r.mu.Unlock()
		return
	}
	probe, ok := r.pending[seq]
	if !ok || now.Sub(probe.sent) > r.opts.Timeout {
		r.mu.Unlock()
		return
	}
	delete(r.pending, seq)
	ttl := seqTTL(seq)
	var deliver []Reply
	if !r.beyondDestLocked(probe.round, ttl) {
		dest := from == r.ip
		if dest && probe.round > r.lastDestRound[ttl] {
			r.lastDestRound[ttl] = probe.round
		}
		rep := Reply{TTL: ttl, From: from, RTT: now.Sub(probe.sent), Dest: dest}
		if dest && probe.discovery && r.lowerPendingLocked(probe.round, ttl) {
			r.held[seq] = heldReply{reply: rep, round: probe.round}
		} else {
			deliver = append(deliver, rep)
		}
	}
	// Any outcome in the round may settle its held replies.
	deliver = append(deliver, r.settleLocked(probe.round)...)
	r.mu.Unlock()
	for _, rep := range deliver {
		call(r.cb.OnReply, rep)
	}
}

// settleLocked resolves the held replies of round after one of its probes
// got an outcome: a reply past a lower destination answer is discarded, and
// one with no lower TTL still pending is returned for delivery, lowest TTL
// first.
func (r *runner) settleLocked(round int) []Reply {
	var released []Reply
	for seq, h := range r.held {
		if h.round != round {
			continue
		}
		switch {
		case r.beyondDestLocked(round, h.reply.TTL):
			delete(r.held, seq)
		case !r.lowerPendingLocked(round, h.reply.TTL):
			delete(r.held, seq)
			released = append(released, h.reply)
		}
	}
	slices.SortFunc(released, func(a, b Reply) int { return a.TTL - b.TTL })
	return released
}

// lowerPendingLocked reports whether a probe of round below ttl still awaits
// its outcome.
func (r *runner) lowerPendingLocked(round, ttl int) bool {
	for lower := 1; lower < ttl; lower++ {
		if probe, ok := r.pending[encodeSeq(round, lower)]; ok && probe.round == round {
			return true
		}
	}
	return false
}

// beyondDestLocked reports whether a probe at ttl in round lies past the
// destination: some lower TTL got a destination reply in that round or a
// later one. The first round, and every round after the destination TTL is
// forgotten, probes up to MaxTTL, and the destination answers every probe
// past its own hop; those answers say nothing about the path.
func (r *runner) beyondDestLocked(round, ttl int) bool {
	for lower := 1; lower < ttl; lower++ {
		if r.lastDestRound[lower] >= round {
			return true
		}
	}
	return false
}

func call[T any](fn func(T), v T) {
	if fn != nil {
		fn(v)
	}
}

func resolveIPv4(ctx context.Context, host string) ([]netip.Addr, error) {
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	for i := range addrs {
		addrs[i] = addrs[i].Unmap()
	}
	return addrs, err
}

// rawConn is a raw IPv4 ICMP socket.
type rawConn struct {
	mu sync.Mutex
	pc *icmp.PacketConn
}

func listenRaw() (*rawConn, error) {
	pc, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, err
	}
	return &rawConn{pc: pc}, nil
}

// Send writes one probe with the given TTL. x/net has no per-message TTL for
// IPv4 sends, so the socket TTL is set and used under one lock.
func (c *rawConn) Send(b []byte, ttl int, dst netip.Addr) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.pc.IPv4PacketConn().SetTTL(ttl); err != nil {
		return err
	}
	_, err := c.pc.WriteTo(b, &net.IPAddr{IP: dst.AsSlice()})
	return err
}

func (c *rawConn) Recv(b []byte) (int, netip.Addr, error) {
	n, peer, err := c.pc.ReadFrom(b)
	if err != nil {
		return 0, netip.Addr{}, err
	}
	ipAddr, ok := peer.(*net.IPAddr)
	if !ok {
		return 0, netip.Addr{}, fmt.Errorf("unexpected peer address %T", peer)
	}
	addr, _ := netip.AddrFromSlice(ipAddr.IP)
	return n, addr.Unmap(), nil
}

func (c *rawConn) Close() error {
	return c.pc.Close()
}
