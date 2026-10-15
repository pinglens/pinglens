package mtr

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/pinglens/pinglens/internal/protocol"
)

func TestBuildArgsUsesFixedFlags(t *testing.T) {
	args := BuildArgs("8.8.8.8", Config{Cycles: 15, MaxTTL: 30})
	joined := ""
	for _, arg := range args {
		joined += arg + " "
	}
	if joined != "--report --json --no-dns --report-cycles 15 --max-ttl 30 8.8.8.8 " {
		t.Fatalf("unexpected args: %q", joined)
	}
}

func TestTimeoutErrorReplacesRawContextDeadline(t *testing.T) {
	parent := context.Background()
	runCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
	defer cancel()
	<-runCtx.Done()

	err := mtrContextError(parent, runCtx, 30*time.Second)
	var timeoutErr TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("expected TimeoutError, got %T %v", err, err)
	}
	if err.Error() != "MTR timed out after 30s" {
		t.Fatalf("unexpected timeout message: %q", err.Error())
	}
}

func TestParseJSON(t *testing.T) {
	report, err := ParseJSON([]byte(`{"report":{"hubs":[{"count":"1","host":"gw (192.0.2.1)","Loss%":0.0,"Snt":15,"Last":1.2,"Avg":1.3,"Best":1.0,"Wrst":2.0,"StDev":0.2},{"count":"2","host":"???","Loss%":"100.0","Snt":"15"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(report.Rows))
	}
	if report.Rows[0].Hop != 1 || report.Rows[0].Host != "gw" || report.Rows[0].Address != "192.0.2.1" || report.Rows[0].Sent != 15 || report.Rows[0].AvgMillis != 1.3 {
		t.Fatalf("bad first row: %+v", report.Rows[0])
	}
	if report.Rows[1].Host != "" || report.Rows[1].LossPct != 100 {
		t.Fatalf("bad second row: %+v", report.Rows[1])
	}
}

func TestASNQuery(t *testing.T) {
	if got := asnQuery(mustAddr("203.0.113.9")); got != "9.113.0.203.origin.asn.cymru.com" {
		t.Fatalf("unexpected IPv4 query: %s", got)
	}
	if got := asnQuery(mustAddr("2001:db8::1")); got == "" || got[len(got)-21:] != "origin6.asn.cymru.com" {
		t.Fatalf("unexpected IPv6 query: %s", got)
	}
}

// TestEnrichHopsBoundsSlowReverseDNS covers the reason mtr runs with
// --no-dns: one hop whose PTR server never answers must not hold back the
// trace or the names of the other hops.
func TestEnrichHopsBoundsSlowReverseDNS(t *testing.T) {
	previous := lookupTimeout
	lookupTimeout = 300 * time.Millisecond
	t.Cleanup(func() { lookupTimeout = previous })

	resolver := fakePTRResolver(t, map[string]string{"1.2.0.192.in-addr.arpa.": "gw.example.net."})
	report := protocol.TraceReport{Rows: []protocol.TraceRow{
		{Hop: 1, Address: "192.0.2.1"},
		{Hop: 2, Address: "192.0.2.2"},
		{Hop: 3},
	}}

	started := time.Now()
	enrichHops(context.Background(), resolver, &report)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("enrichHops took %s; slow PTR lookup was not bounded", elapsed)
	}
	if got := report.Rows[0].Host; got != "gw.example.net" {
		t.Fatalf("resolved hop host = %q, want gw.example.net", got)
	}
	if got := report.Rows[1].Host; got != "" {
		t.Fatalf("unanswered hop host = %q, want empty", got)
	}
}

// fakePTRResolver answers PTR queries from names and silently drops every
// other query, like an unresponsive authoritative server.
func fakePTRResolver(t *testing.T, names map[string]string) *net.Resolver {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
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
			name, ok := names[question.Name.String()]
			if question.Type != dnsmessage.TypePTR || !ok {
				continue
			}
			msg.Header.Response = true
			msg.Answers = []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET, TTL: 60},
				Body:   &dnsmessage.PTRResource{PTR: dnsmessage.MustNewName(name)},
			}}
			packed, err := msg.Pack()
			if err != nil {
				continue
			}
			_, _ = conn.WriteTo(packed, from)
		}
	}()
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "udp", conn.LocalAddr().String())
		},
	}
}

func mustAddr(value string) netip.Addr {
	addr, err := netip.ParseAddr(value)
	if err != nil {
		panic(err)
	}
	return addr
}
