package traceroute

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestASNQuery(t *testing.T) {
	if got := asnQuery(netip.MustParseAddr("203.0.113.9")); got != "9.113.0.203.origin.asn.cymru.com" {
		t.Fatalf("unexpected IPv4 query: %s", got)
	}
	if got := asnQuery(netip.MustParseAddr("2001:db8::1")); got == "" || got[len(got)-21:] != "origin6.asn.cymru.com" {
		t.Fatalf("unexpected IPv6 query: %s", got)
	}
}

// privateHop has no public origin AS, so only its PTR is looked up.
var privateHop = netip.MustParseAddr("192.0.2.9")

const privatePTR = "9.2.0.192.in-addr.arpa."

// TestHopNamesAnswerExpiresAfterAnHour covers the original bug: a hop that
// keeps replying must not keep its answer cached beyond its lifetime.
func TestHopNamesAnswerExpiresAfterAnHour(t *testing.T) {
	dns, names, clock := newTestHopNames(t)
	dns.set(privatePTR, ptr("gw1.example.net."))
	if got := lookup(t, names, privateHop); got.Host != "gw1.example.net" {
		t.Fatalf("host = %q", got.Host)
	}

	dns.set(privatePTR, ptr("gw2.example.net."))
	for range 59 {
		clock.Advance(time.Minute)
		lookup(t, names, privateHop)
	}
	if n := dns.queries(privatePTR); n != 1 {
		t.Fatalf("queried %d times within the hour, want 1", n)
	}
	clock.Advance(time.Minute)
	if got := lookup(t, names, privateHop); got.Host != "gw2.example.net" {
		t.Fatalf("after an hour: host = %q, want the new answer", got.Host)
	}
}

func TestHopNamesMissingRecordRetriedAfterFiveMinutes(t *testing.T) {
	dns, names, clock := newTestHopNames(t)
	dns.nxdomain(privatePTR)
	lookup(t, names, privateHop)

	dns.set(privatePTR, ptr("new.example.net."))
	clock.Advance(missingLifetime - time.Second)
	if got := lookup(t, names, privateHop); got.Host != "" || dns.queries(privatePTR) != 1 {
		t.Fatalf("before 5m: host %q after %d queries, want the cached miss", got.Host, dns.queries(privatePTR))
	}
	clock.Advance(time.Second)
	if got := lookup(t, names, privateHop); got.Host != "new.example.net" {
		t.Fatalf("after 5m: host = %q, want the new record", got.Host)
	}
}

// TestHopNamesFailureKeepsAnswer covers a resolver outage at refresh time:
// the last answer stays and the lookup is retried every 30 seconds.
func TestHopNamesFailureKeepsAnswer(t *testing.T) {
	dns, names, clock := newTestHopNames(t)
	dns.set(privatePTR, ptr("gw1.example.net."))
	lookup(t, names, privateHop)

	dns.drop(privatePTR)
	clock.Advance(answerLifetime)
	if got := lookup(t, names, privateHop); got.Host != "gw1.example.net" || dns.queries(privatePTR) != 2 {
		t.Fatalf("failed refresh: host %q after %d queries, want the old answer and 2", got.Host, dns.queries(privatePTR))
	}
	clock.Advance(failureRetry - time.Second)
	lookup(t, names, privateHop)
	if n := dns.queries(privatePTR); n != 2 {
		t.Fatalf("retried before 30s (%d queries)", n)
	}

	dns.set(privatePTR, ptr("gw2.example.net."))
	clock.Advance(time.Second)
	if got := lookup(t, names, privateHop); got.Host != "gw2.example.net" {
		t.Fatalf("after 30s: host = %q, want the retried answer", got.Host)
	}
}

// TestHopNamesASNChangeDropsOldDescription covers an address that moves to
// another AS: the old AS description must never be reported with the new
// ASN, even while the new description cannot be looked up.
func TestHopNamesASNChangeDropsOldDescription(t *testing.T) {
	dns, names, clock := newTestHopNames(t)
	addr := netip.MustParseAddr("8.8.4.4")
	const origin = "4.4.8.8.origin.asn.cymru.com."
	dns.nxdomain("4.4.8.8.in-addr.arpa.")
	dns.set(origin, txt("64500 | 8.8.4.0/24 | US | arin | 2023-12-28"))
	dns.set("AS64500.asn.cymru.com.", txt("64500 | US | arin | 2000-03-30 | OLD"))
	if got := lookup(t, names, addr); got.ASN != "AS64500" || got.ASName != "OLD" {
		t.Fatalf("first lookup = %+v", got)
	}

	dns.set(origin, txt("64501 | 8.8.4.0/24 | US | arin | 2023-12-28"))
	clock.Advance(answerLifetime)
	if got := lookup(t, names, addr); got.ASN != "AS64501" || got.ASName != "" {
		t.Fatalf("after the ASN changed = %+v, want AS64501 without a description", got)
	}
}

// TestHopNamesPrunesSharedASName covers an AS description whose hops all
// went idle: once pruned, a returning hop looks the description up again.
func TestHopNamesPrunesSharedASName(t *testing.T) {
	dns, names, clock := newTestHopNames(t)
	addr := netip.MustParseAddr("8.8.4.4")
	const asName = "AS15169.asn.cymru.com."
	dns.nxdomain("4.4.8.8.in-addr.arpa.")
	dns.set("4.4.8.8.origin.asn.cymru.com.", txt("15169 | 8.8.4.0/24 | US | arin | 2023-12-28"))
	dns.set(asName, txt("15169 | US | arin | 2000-03-30 | OLD NAME"))
	lookup(t, names, addr)

	clock.Advance(time.Hour + time.Minute)
	names.Prune()
	dns.set(asName, txt("15169 | US | arin | 2000-03-30 | NEW NAME"))
	if got := lookup(t, names, addr); got.ASName != "NEW NAME" || dns.queries(asName) != 2 {
		t.Fatalf("returning hop = %+v after %d AS-name queries, want NEW NAME and 2", got, dns.queries(asName))
	}
}

func TestHopNamesQueryEachASNameOnce(t *testing.T) {
	dns, names, _ := newTestHopNames(t)
	for _, addr := range []string{"8.8.8.8", "8.8.4.4"} {
		a := netip.MustParseAddr(addr).As4()
		dns.nxdomain(reverse4(a) + ".in-addr.arpa.")
		dns.set(reverse4(a)+".origin.asn.cymru.com.", txt("15169 | 8.8.0.0/16 | US | arin | 2023-12-28"))
	}
	// TXT data is arbitrary bytes; label values must be UTF-8.
	dns.set("AS15169.asn.cymru.com.", txt("15169 | US | arin | 2000-03-30 | GOOGLE \xff LLC"))

	for _, addr := range []string{"8.8.8.8", "8.8.4.4"} {
		if got := lookup(t, names, netip.MustParseAddr(addr)); got.ASN != "AS15169" || got.ASName != "GOOGLE \uFFFD LLC" {
			t.Fatalf("%s = %+v", addr, got)
		}
	}
	if n := dns.queries("AS15169.asn.cymru.com."); n != 1 {
		t.Fatalf("AS name queried %d times, want once per ASN", n)
	}
}

func newTestHopNames(t *testing.T) (*fakeDNS, *HopNames, *fakeClock) {
	t.Helper()
	previous := lookupTimeout
	lookupTimeout = 200 * time.Millisecond
	t.Cleanup(func() { lookupTimeout = previous })
	dns := newFakeDNS(t)
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	names := NewHopNames(dns.resolver, time.Hour)
	names.now = clock.Now
	return dns, names, clock
}

// lookup calls Get, waits for every lookup it started, including an AS
// name lookup started by an ASN answer, and returns the fields then.
func lookup(t *testing.T, names *HopNames, addr netip.Addr) HopName {
	t.Helper()
	names.Get(addr)
	deadline := time.Now().Add(5 * time.Second)
	for busy(names) {
		if time.Now().After(deadline) {
			t.Fatalf("lookups for %s never finished", addr)
		}
		time.Sleep(5 * time.Millisecond)
	}
	names.mu.Lock()
	defer names.mu.Unlock()
	name, _ := names.nameLocked(names.hops[addr])
	return name
}

func busy(names *HopNames) bool {
	names.mu.Lock()
	defer names.mu.Unlock()
	for _, entry := range names.hops {
		if entry.ptr.busy || entry.asn.busy {
			return true
		}
	}
	for _, entry := range names.asNames {
		if entry.name.busy {
			return true
		}
	}
	return false
}

func reverse4(a [4]byte) string {
	return netip.AddrFrom4([4]byte{a[3], a[2], a[1], a[0]}).String()
}

func ptr(name string) *dnsmessage.PTRResource {
	return &dnsmessage.PTRResource{PTR: dnsmessage.MustNewName(name)}
}

func txt(value string) *dnsmessage.TXTResource {
	return &dnsmessage.TXTResource{TXT: []string{value}}
}

// fakeDNS answers the records it holds, returns NXDOMAIN for names marked
// missing and silently drops every other query, like an unresponsive server.
type fakeDNS struct {
	resolver *net.Resolver

	mu      sync.Mutex
	records map[string]dnsmessage.ResourceBody
	missing map[string]bool
	counts  map[string]int
}

func newFakeDNS(t *testing.T) *fakeDNS {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	f := &fakeDNS{records: map[string]dnsmessage.ResourceBody{}, missing: map[string]bool{}, counts: map[string]int{}}
	go f.serve(conn)
	f.resolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "udp", conn.LocalAddr().String())
		},
	}
	return f
}

func (f *fakeDNS) set(name string, body dnsmessage.ResourceBody) {
	f.mu.Lock()
	f.records[name] = body
	delete(f.missing, name)
	f.mu.Unlock()
}

func (f *fakeDNS) nxdomain(name string) {
	f.mu.Lock()
	delete(f.records, name)
	f.missing[name] = true
	f.mu.Unlock()
}

func (f *fakeDNS) drop(name string) {
	f.mu.Lock()
	delete(f.records, name)
	delete(f.missing, name)
	f.mu.Unlock()
}

func (f *fakeDNS) queries(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[name]
}

func (f *fakeDNS) serve(conn net.PacketConn) {
	buf := make([]byte, 512)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		var msg dnsmessage.Message
		if err := msg.Unpack(buf[:n]); err != nil || len(msg.Questions) != 1 {
			continue
		}
		question := msg.Questions[0]
		name := question.Name.String()
		f.mu.Lock()
		f.counts[name]++
		body, ok := f.records[name]
		missing := f.missing[name]
		f.mu.Unlock()
		msg.Header.Response = true
		switch {
		case missing:
			msg.Header.RCode = dnsmessage.RCodeNameError
		case ok && question.Type == recordType(body):
			msg.Answers = []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassINET, TTL: 60},
				Body:   body,
			}}
		default:
			continue
		}
		packed, err := msg.Pack()
		if err != nil {
			continue
		}
		_, _ = conn.WriteTo(packed, from)
	}
}

func recordType(body dnsmessage.ResourceBody) dnsmessage.Type {
	switch body.(type) {
	case *dnsmessage.PTRResource:
		return dnsmessage.TypePTR
	case *dnsmessage.TXTResource:
		return dnsmessage.TypeTXT
	}
	panic("unsupported fake DNS record")
}
