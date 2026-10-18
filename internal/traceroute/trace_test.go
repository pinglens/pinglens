package traceroute

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

var (
	testSource  = netip.MustParseAddr("198.51.100.7")
	testDest    = netip.MustParseAddr("192.0.2.9")
	testSource6 = netip.MustParseAddr("2001:db8:ffff::7")
	testDest6   = netip.MustParseAddr("2001:db8::9")
)

func TestSeqEncoding(t *testing.T) {
	for _, tc := range []struct{ round, ttl int }{{0, 1}, {0, 31}, {2047, 1}, {2047, 31}, {1234, 17}} {
		seq := encodeSeq(tc.round, tc.ttl)
		if got := seqTTL(seq); got != tc.ttl {
			t.Fatalf("seqTTL(encodeSeq(%d, %d)) = %d", tc.round, tc.ttl, got)
		}
		if got := int(seq >> ttlBits); got != tc.round {
			t.Fatalf("round of encodeSeq(%d, %d) = %d", tc.round, tc.ttl, got)
		}
	}
	if encodeSeq(2048, 5) != encodeSeq(0, 5) || encodeSeq(2049, 31) != encodeSeq(1, 31) {
		t.Fatal("round counter does not wrap at 2048")
	}
}

func TestParseReplyQuotedEcho(t *testing.T) {
	probe := echoRequest(t, 0xbeef, encodeSeq(3, 7))
	reply := timeExceeded(t, probe, testDest)
	id, seq, origDst, quoted, ok := parseReply(reply, false)
	if !ok || !quoted || id != 0xbeef || seq != encodeSeq(3, 7) || origDst != testDest {
		t.Fatalf("parseReply = id %#x seq %#x dst %s quoted %v ok %v", id, seq, origDst, quoted, ok)
	}

	// RFC 792 only promises 8 bytes of the quoted datagram after its header;
	// anything shorter cannot name the probe.
	short := timeExceededRaw(t, append(ipHeader(testDest), probe[:4]...))
	if _, _, _, _, ok := parseReply(short, false); ok {
		t.Fatal("quote shorter than 8 bytes was accepted")
	}
	if _, _, _, _, ok := parseReply(probe, false); ok {
		t.Fatal("echo request was accepted as a reply")
	}
	if _, _, _, _, ok := parseReply(echoReply(t, 0xbeef, 9), false); !ok {
		t.Fatal("echo reply was rejected")
	}
	other := mustMarshal(&icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: 0xbeef, Seq: 9, Data: []byte("pinglens")}})
	if _, _, _, _, ok := parseReply(other, false); ok {
		t.Fatal("echo reply with another pinger's payload was accepted")
	}
}

func TestParseReplyQuotedEcho6(t *testing.T) {
	probe := mustMarshal(&icmp.Message{Type: ipv6.ICMPTypeEchoRequest, Body: &icmp.Echo{ID: 0xbeef, Seq: int(encodeSeq(3, 7)), Data: payload}})
	quote := append(ip6Header(testDest6), probe...)
	reply := mustMarshal(&icmp.Message{Type: ipv6.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: quote}})
	id, seq, origDst, quoted, ok := parseReply(reply, true)
	if !ok || !quoted || id != 0xbeef || seq != encodeSeq(3, 7) || origDst != testDest6 {
		t.Fatalf("parseReply = id %#x seq %#x dst %s quoted %v ok %v", id, seq, origDst, quoted, ok)
	}
	short := mustMarshal(&icmp.Message{Type: ipv6.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: append(ip6Header(testDest6), probe[:4]...)}})
	if _, _, _, _, ok := parseReply(short, true); ok {
		t.Fatal("quote shorter than 8 bytes was accepted")
	}
	echo := mustMarshal(&icmp.Message{Type: ipv6.ICMPTypeEchoReply, Body: &icmp.Echo{ID: 0xbeef, Seq: 9, Data: payload}})
	if _, _, _, _, ok := parseReply(echo, true); !ok {
		t.Fatal("ICMPv6 echo reply was rejected")
	}
	// An ICMPv4 message read as ICMPv6 must not match: type numbers differ.
	if _, _, _, _, ok := parseReply(echoReply(t, 0xbeef, 9), true); ok {
		t.Fatal("ICMPv4 echo reply was accepted on the IPv6 socket")
	}
}

// TestIPv6RoundsConvergeOnDestination traces an IPv6 destination at hop 4
// behind three IPv6 routers.
func TestIPv6RoundsConvergeOnDestination(t *testing.T) {
	path := newFakePath()
	path.routers = []netip.Addr{netip.MustParseAddr("2001:db8:1::1"), netip.MustParseAddr("2001:db8:1::2"), netip.MustParseAddr("2001:db8:1::3")}
	rec, r, clock := newTestRunnerAt(t, path, Options{Interval: 2500 * time.Millisecond, Timeout: 2 * time.Second, MaxTTL: 30}, testDest6)

	runRounds(r, clock, 3)

	assertReplies(t, rec.replies, map[hopKey]int{
		{1, "2001:db8:1::1", false}: 3,
		{2, "2001:db8:1::2", false}: 3,
		{3, "2001:db8:1::3", false}: 3,
		{4, "2001:db8::9", true}:    3,
	})
	if !slices.Equal(rec.destTTL, []int{0, 4, 4}) {
		t.Fatalf("destination TTLs %v, want [0 4 4]", rec.destTTL)
	}
}

// TestTraceRejectsUnavailableFamily covers a prober without an IPv6 socket.
func TestTraceRejectsUnavailableFamily(t *testing.T) {
	p := newProber(slog.New(slog.DiscardHandler), newFakePath(), nil)
	if p.Supports(testDest6) || !p.Supports(testDest) {
		t.Fatal("Supports does not follow the open sockets")
	}
	called := false
	done := make(chan struct{})
	go func() {
		p.Trace(context.Background(), testDest6.String(), Options{Interval: time.Second, Timeout: 500 * time.Millisecond, MaxTTL: 4},
			Callbacks{OnTargetIP: func(netip.Addr) { called = true }})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Trace of an unavailable family did not return")
	}
	if called {
		t.Fatal("Trace started on an unavailable family")
	}
}

func TestRoundsConvergeOnDestination(t *testing.T) {
	path := newFakePath()
	// The third router rate-limits its ICMP errors and answers every other
	// round.
	path.answers = func(ttl, round int) bool { return ttl != 3 || round%2 == 1 }
	rec, r, clock := newTestRunner(t, path, Options{Interval: 2500 * time.Millisecond, Timeout: 2 * time.Second, MaxTTL: 30})

	runRounds(r, clock, 20)

	for ttl := 1; ttl <= 4; ttl++ {
		if got := path.sent(ttl); got != 20 {
			t.Fatalf("TTL %d sent %d probes, want 20", ttl, got)
		}
	}
	for ttl := 5; ttl <= 30; ttl++ {
		if got := path.sent(ttl); got != 1 {
			t.Fatalf("TTL %d sent %d probes, want 1 (only before the destination answered)", ttl, got)
		}
	}
	// The destination also answers TTLs 5-30 in the first round; those
	// replies are past its hop and must not be reported.
	assertReplies(t, rec.replies, map[hopKey]int{
		{1, "10.0.0.1", false}: 20,
		{2, "10.0.0.2", false}: 20,
		{3, "10.0.0.3", false}: 10,
		{4, "192.0.2.9", true}: 20,
	})
	if got := rec.timeouts; len(got) != 1 || got[3] != 10 {
		t.Fatalf("timeouts = %v, want 10 at TTL 3 only", got)
	}
	if rec.destTTL[0] != 0 {
		t.Fatalf("first round reported destination TTL %d before any reply", rec.destTTL[0])
	}
	for i, n := range rec.destTTL[1:] {
		if n != 4 {
			t.Fatalf("round %d reported destination TTL %d, want 4", i+1, n)
		}
	}

	// The destination goes silent: the limit holds for destMemoryRounds
	// rounds after its last answer, then probing widens to MaxTTL again.
	path.destDown = true
	runRounds(r, clock, destMemoryRounds)
	if got := rec.destTTL[len(rec.destTTL)-2]; got != 4 {
		t.Fatalf("destination TTL %d nine rounds after the last answer, want 4", got)
	}
	if got := rec.destTTL[len(rec.destTTL)-1]; got != 0 {
		t.Fatalf("destination TTL %d ten rounds after the last answer, want 0", got)
	}
	if got := path.sent(30); got != 2 {
		t.Fatalf("TTL 30 sent %d probes, want 2 once the limit reset", got)
	}
}

// TestProbesBeyondDestinationDiscarded covers a destination that rate-limits
// its echo replies: past its hop, answered and unanswered probes alike are
// discarded without an event, so they count neither as replies nor as loss.
func TestProbesBeyondDestinationDiscarded(t *testing.T) {
	path := newFakePath()
	path.answers = func(ttl, _ int) bool { return ttl <= 10 }
	rec, r, clock := newTestRunner(t, path, Options{Interval: 2500 * time.Millisecond, Timeout: 2 * time.Second, MaxTTL: 30})

	runRounds(r, clock, 3)

	assertReplies(t, rec.replies, map[hopKey]int{
		{1, "10.0.0.1", false}: 3,
		{2, "10.0.0.2", false}: 3,
		{3, "10.0.0.3", false}: 3,
		{4, "192.0.2.9", true}: 3,
	})
	if len(rec.timeouts) != 0 {
		t.Fatalf("timeouts = %v, want none", rec.timeouts)
	}
}

// TestReorderedRepliesPastDestinationDiscarded delivers a discovery round's
// replies highest TTL first: the destination's answers at TTLs 5-30 arrive
// before the one at TTL 4 and must still not be reported.
func TestReorderedRepliesPastDestinationDiscarded(t *testing.T) {
	path := newFakePath()
	path.deferred = true
	rec, r, clock := newTestRunner(t, path, Options{Interval: 2500 * time.Millisecond, Timeout: 2 * time.Second, MaxTTL: 30})

	r.runRound(context.Background())
	path.deliver(true)
	clock.Advance(r.opts.Interval)
	r.runRound(context.Background())
	path.deliver(true)

	assertReplies(t, rec.replies, map[hopKey]int{
		{1, "10.0.0.1", false}: 2,
		{2, "10.0.0.2", false}: 2,
		{3, "10.0.0.3", false}: 2,
		{4, "192.0.2.9", true}: 2,
	})
	if len(rec.timeouts) != 0 || !slices.Equal(rec.destTTL, []int{0, 4}) {
		t.Fatalf("timeouts %v, destination TTLs %v; want none and [0 4]", rec.timeouts, rec.destTTL)
	}
}

// TestHeldReplyReleasedWhenLowerHopExpires covers a discovery round with a
// silent router: the destination reply waits for that hop's timeout, then
// is reported once.
func TestHeldReplyReleasedWhenLowerHopExpires(t *testing.T) {
	path := newFakePath()
	path.answers = func(ttl, _ int) bool { return ttl != 2 }
	rec, r, clock := newTestRunner(t, path, Options{Interval: 2500 * time.Millisecond, Timeout: 2 * time.Second, MaxTTL: 30})

	runRounds(r, clock, 1)
	if n := rec.replies[hopKey{4, "192.0.2.9", true}]; n != 0 {
		t.Fatalf("destination reply reported while TTL 2 was pending (%d)", n)
	}
	// Round 1 expires TTL 2, releasing round 0's reply; round 1 itself knows
	// the destination TTL and reports its reply at once.
	runRounds(r, clock, 1)
	assertReplies(t, rec.replies, map[hopKey]int{
		{1, "10.0.0.1", false}: 2,
		{3, "10.0.0.3", false}: 2,
		{4, "192.0.2.9", true}: 2,
	})
	if len(rec.timeouts) != 1 || rec.timeouts[2] != 1 {
		t.Fatalf("timeouts = %v, want one at TTL 2", rec.timeouts)
	}
}

func TestLateAndDuplicateRepliesIgnored(t *testing.T) {
	path := newFakePath()
	opts := Options{Interval: 2500 * time.Millisecond, Timeout: 2 * time.Second, MaxTTL: 4}
	path.rtt = func(ttl int) time.Duration {
		if ttl == 1 {
			return opts.Timeout + time.Millisecond
		}
		return 5 * time.Millisecond
	}
	path.duplicate = true
	rec, r, clock := newTestRunner(t, path, opts)

	runRounds(r, clock, 3)

	assertReplies(t, rec.replies, map[hopKey]int{
		{2, "10.0.0.2", false}: 3,
		{3, "10.0.0.3", false}: 3,
		{4, "192.0.2.9", true}: 3,
	})
	if got := rec.timeouts[1]; got != 2 {
		t.Fatalf("TTL 1 timeouts = %d, want 2 (third still pending)", got)
	}
}

func TestReplyForOtherDestinationIgnored(t *testing.T) {
	path := newFakePath()
	rec, r, clock := newTestRunner(t, path, Options{Interval: time.Second, Timeout: 500 * time.Millisecond, MaxTTL: 4})
	path.quoteDst = netip.MustParseAddr("192.0.2.200")

	// The routers' errors quote another destination and are dropped, so
	// TTLs 1-3 time out; the first round's destination reply is held until
	// then, and the second round's is reported at once.
	runRounds(r, clock, 2)

	assertReplies(t, rec.replies, map[hopKey]int{{4, "192.0.2.9", true}: 2})
	if want := map[int]int{1: 1, 2: 1, 3: 1}; !maps.Equal(rec.timeouts, want) {
		t.Fatalf("timeouts = %v, want %v", rec.timeouts, want)
	}
}

// TestEchoReplyFromOtherAddressIgnored covers an echo reply that matches a
// pending probe's ID and sequence but does not come from the target.
func TestEchoReplyFromOtherAddressIgnored(t *testing.T) {
	path := newFakePath()
	path.answers = func(int, int) bool { return false }
	rec, r, clock := newTestRunner(t, path, Options{Interval: time.Second, Timeout: 500 * time.Millisecond, MaxTTL: 1})
	r.runRound(context.Background())
	reply := echoReply(t, r.id, encodeSeq(0, 1))

	r.prober.dispatch(reply, netip.MustParseAddr("192.0.2.200"), clock.Now())
	assertReplies(t, rec.replies, map[hopKey]int{})
	r.prober.dispatch(reply, testDest, clock.Now())
	assertReplies(t, rec.replies, map[hopKey]int{{1, "192.0.2.9", true}: 1})
}

func TestResolveKeepsCurrentAddress(t *testing.T) {
	a, b := netip.MustParseAddr("192.0.2.9"), netip.MustParseAddr("192.0.2.10")
	path := newFakePath()
	path.answers = func(int, int) bool { return false }
	p := newProber(slog.New(slog.DiscardHandler), path, nil)
	p.spacing = 0
	var answer []netip.Addr
	var answerErr error
	p.resolve = func(context.Context, string) ([]netip.Addr, error) { return answer, answerErr }
	var ips []netip.Addr
	var up []bool
	r := p.newRunner("dest.test", Options{Interval: time.Second, Timeout: 500 * time.Millisecond, MaxTTL: 4}, Callbacks{
		OnTargetIP: func(addr netip.Addr) { ips = append(ips, addr) },
		OnTargetUp: func(v bool) { up = append(up, v) },
	})
	pending := func() int {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.pending)
	}

	answer = []netip.Addr{a, b}
	r.resolve(context.Background())
	r.runRound(context.Background())
	// DNS rotates the same addresses: the trace keeps its address and its
	// in-flight probes.
	answer = []netip.Addr{b, a}
	r.resolve(context.Background())
	if !slices.Equal(ips, []netip.Addr{a}) || pending() != 4 {
		t.Fatalf("after reorder: target IPs %v, %d pending, want [%s] and 4", ips, pending(), a)
	}
	// The address is gone from DNS: switch to the first answer.
	answer = []netip.Addr{b}
	r.resolve(context.Background())
	if !slices.Equal(ips, []netip.Addr{a, b}) || pending() != 0 {
		t.Fatalf("after change: target IPs %v, %d pending, want [%s %s] and 0", ips, pending(), a, b)
	}
	// A failed lookup reports the target down but keeps probing.
	answer, answerErr = nil, errors.New("SERVFAIL")
	if retry := r.resolve(context.Background()); retry != resolveRetry {
		t.Fatalf("retry after failure = %s, want %s", retry, resolveRetry)
	}
	r.mu.Lock()
	ip := r.ip
	r.mu.Unlock()
	if ip != b || !slices.Equal(up, []bool{true, true, true, false}) {
		t.Fatalf("after failure: probing %s, up %v", ip, up)
	}
}

func TestTraceThroughReceiveLoop(t *testing.T) {
	path := newFakePath()
	path.async = make(chan packet, 256)
	p := newProber(slog.New(slog.DiscardHandler), path, nil)
	p.spacing = 0
	p.resolve = func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{testDest}, nil }
	path.prober = p
	p.start()

	var mu sync.Mutex
	var targetIPs []netip.Addr
	var up []bool
	converged := make(chan struct{})
	var once sync.Once
	cb := Callbacks{
		OnTargetIP: func(a netip.Addr) { mu.Lock(); targetIPs = append(targetIPs, a); mu.Unlock() },
		OnTargetUp: func(v bool) { mu.Lock(); up = append(up, v); mu.Unlock() },
		OnDestTTL: func(n int) {
			if n == 4 {
				once.Do(func() { close(converged) })
			}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Trace(ctx, "dest.test", Options{Interval: 20 * time.Millisecond, Timeout: 15 * time.Millisecond, MaxTTL: 30}, cb)
		close(done)
	}()
	select {
	case <-converged:
	case <-time.After(5 * time.Second):
		t.Fatal("trace never converged on the destination TTL")
	}
	cancel()
	<-done

	p.mu.RLock()
	live := len(p.runners)
	p.mu.RUnlock()
	if live != 0 {
		t.Fatalf("%d runners still registered after Trace returned", live)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(targetIPs) != 1 || targetIPs[0] != testDest || len(up) == 0 || !up[0] {
		t.Fatalf("target IPs %v, up %v", targetIPs, up)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

type hopKey struct {
	ttl  int
	from string
	dest bool
}

type recorder struct {
	mu       sync.Mutex
	timeouts map[int]int
	replies  map[hopKey]int
	destTTL  []int
}

func (rec *recorder) callbacks() Callbacks {
	return Callbacks{
		OnTimeout: func(ttl int) { rec.mu.Lock(); rec.timeouts[ttl]++; rec.mu.Unlock() },
		OnDestTTL: func(n int) { rec.mu.Lock(); rec.destTTL = append(rec.destTTL, n); rec.mu.Unlock() },
		OnReply: func(rep Reply) {
			rec.mu.Lock()
			rec.replies[hopKey{rep.TTL, rep.From.String(), rep.Dest}]++
			rec.mu.Unlock()
		},
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// newTestRunner registers a runner for testDest whose replies the fake path
// delivers synchronously from Send, so each runRound completes its replies
// before returning.
func newTestRunner(t *testing.T, path *fakePath, opts Options) (*recorder, *runner, *fakeClock) {
	t.Helper()
	return newTestRunnerAt(t, path, opts, testDest)
}

func newTestRunnerAt(t *testing.T, path *fakePath, opts Options, dest netip.Addr) (*recorder, *runner, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	var p *Prober
	if dest.Is6() {
		p = newProber(slog.New(slog.DiscardHandler), nil, path)
	} else {
		p = newProber(slog.New(slog.DiscardHandler), path, nil)
	}
	p.now = clock.Now
	p.spacing = 0
	path.prober = p
	path.clock = clock
	rec := &recorder{timeouts: map[int]int{}, replies: map[hopKey]int{}}
	r := p.newRunner(dest.String(), opts, rec.callbacks())
	if !p.register(r) {
		t.Fatal("register failed")
	}
	r.setIP(dest)
	return rec, r, clock
}

func runRounds(r *runner, clock *fakeClock, n int) {
	for range n {
		r.runRound(context.Background())
		clock.Advance(r.opts.Interval)
	}
}

func assertReplies(t *testing.T, got, want map[hopKey]int) {
	t.Helper()
	for key, n := range want {
		if got[key] != n {
			t.Errorf("replies %+v = %d, want %d", key, got[key], n)
		}
	}
	for key, n := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("unexpected replies %+v = %d", key, n)
		}
	}
}

type packet struct {
	b    []byte
	from netip.Addr
	ttl  int
	at   time.Time
}

// fakePath simulates routers 10.0.0.1-3 at TTLs 1-3 in front of testDest at
// TTL 4. Replies go straight to the prober's dispatch at the fake clock plus
// rtt, or through Recv when async is set.
type fakePath struct {
	prober    *Prober
	clock     *fakeClock
	routers   []netip.Addr
	destDown  bool
	duplicate bool
	quoteDst  netip.Addr
	answers   func(ttl, round int) bool
	rtt       func(ttl int) time.Duration
	async     chan packet
	closeOnce sync.Once
	// deferred queues replies until deliver, to reorder them.
	deferred bool
	queue    []packet

	mu    sync.Mutex
	sends map[int]int
}

func newFakePath() *fakePath {
	return &fakePath{
		routers: []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.3")},
		answers: func(int, int) bool { return true },
		rtt:     func(ttl int) time.Duration { return time.Duration(ttl) * time.Millisecond },
		sends:   map[int]int{},
	}
}

// sent returns how many probes the path carried at ttl.
func (f *fakePath) sent(ttl int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sends[ttl]
}

// Send answers like the simulated path would, in ICMPv6 when dst is IPv6.
func (f *fakePath) Send(b []byte, ttl int, dst netip.Addr) error {
	v6 := dst.Is6()
	proto, echoReply, timeExceeded := 1, icmp.Type(ipv4.ICMPTypeEchoReply), icmp.Type(ipv4.ICMPTypeTimeExceeded)
	if v6 {
		proto, echoReply, timeExceeded = 58, ipv6.ICMPTypeEchoReply, ipv6.ICMPTypeTimeExceeded
	}
	msg, err := icmp.ParseMessage(proto, b)
	if err != nil {
		return err
	}
	echo := msg.Body.(*icmp.Echo)
	f.mu.Lock()
	f.sends[ttl]++
	f.mu.Unlock()
	if !f.answers(ttl, echo.Seq>>ttlBits) {
		return nil
	}
	var reply []byte
	var from netip.Addr
	switch {
	case ttl > len(f.routers):
		if f.destDown {
			return nil
		}
		reply, from = mustMarshal(&icmp.Message{Type: echoReply, Body: echo}), dst
	default:
		quoteDst := dst
		if f.quoteDst.IsValid() {
			quoteDst = f.quoteDst
		}
		var header []byte
		if v6 {
			header = ip6Header(quoteDst)
		} else {
			header = ipHeader(quoteDst)
		}
		quote := append(header, b[:8]...)
		reply, from = mustMarshal(&icmp.Message{Type: timeExceeded, Body: &icmp.TimeExceeded{Data: quote}}), f.routers[ttl-1]
	}
	copies := 1
	if f.duplicate {
		copies = 2
	}
	for range copies {
		switch {
		case f.async != nil:
			f.async <- packet{b: reply, from: from}
		case f.deferred:
			f.queue = append(f.queue, packet{b: reply, from: from, ttl: ttl, at: f.clock.Now().Add(f.rtt(ttl))})
		default:
			f.prober.dispatch(reply, from, f.clock.Now().Add(f.rtt(ttl)))
		}
	}
	return nil
}

// deliver dispatches the queued replies, highest TTL first when reversed.
func (f *fakePath) deliver(reversed bool) {
	queue := f.queue
	f.queue = nil
	if reversed {
		slices.Reverse(queue)
	}
	for _, pkt := range queue {
		f.prober.dispatch(pkt.b, pkt.from, pkt.at)
	}
}

func (f *fakePath) Recv(b []byte) (int, netip.Addr, error) {
	pkt, ok := <-f.async
	if !ok {
		return 0, netip.Addr{}, net.ErrClosed
	}
	return copy(b, pkt.b), pkt.from, nil
}

func (f *fakePath) Close() error {
	if f.async != nil {
		f.closeOnce.Do(func() { close(f.async) })
	}
	return nil
}

func ipHeader(dst netip.Addr) []byte {
	h := make([]byte, ipv4.HeaderLen)
	h[0] = 0x45
	h[8] = 1
	h[9] = 1
	src := testSource.As4()
	copy(h[12:16], src[:])
	d := dst.As4()
	copy(h[16:20], d[:])
	return h
}

func ip6Header(dst netip.Addr) []byte {
	h := make([]byte, ipv6.HeaderLen)
	h[0] = 0x60
	h[5] = 8 // payload length: the 8-byte echo header
	h[6] = 58
	h[7] = 1
	src := testSource6.As16()
	copy(h[8:24], src[:])
	d := dst.As16()
	copy(h[24:40], d[:])
	return h
}

func echoRequest(t *testing.T, id, seq uint16) []byte {
	t.Helper()
	return mustMarshal(&icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: int(id), Seq: int(seq), Data: payload}})
}

func echoReply(t *testing.T, id, seq uint16) []byte {
	t.Helper()
	return mustMarshal(&icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: &icmp.Echo{ID: int(id), Seq: int(seq), Data: payload}})
}

func timeExceeded(t *testing.T, probe []byte, dst netip.Addr) []byte {
	t.Helper()
	return timeExceededRaw(t, append(ipHeader(dst), probe...))
}

func timeExceededRaw(_ *testing.T, quote []byte) []byte {
	return mustMarshal(&icmp.Message{Type: ipv4.ICMPTypeTimeExceeded, Body: &icmp.TimeExceeded{Data: quote}})
}

func mustMarshal(msg *icmp.Message) []byte {
	b, err := msg.Marshal(nil)
	if err != nil {
		panic(err)
	}
	return b
}
