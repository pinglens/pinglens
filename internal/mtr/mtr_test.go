package mtr

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/pinglens/pinglens/internal/protocol"
	"github.com/pinglens/pinglens/internal/traceroute"
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

// TestEnrichHopsBoundsSlowReverseDNS covers the reason mtr runs with
// --no-dns: one hop whose PTR server never answers must not hold back the
// trace or the names of the other hops.
func TestEnrichHopsBoundsSlowReverseDNS(t *testing.T) {
	previous := traceroute.LookupTimeout
	traceroute.LookupTimeout = 300 * time.Millisecond
	t.Cleanup(func() { traceroute.LookupTimeout = previous })

	resolver, _ := fakeResolver(t, map[string]dnsmessage.ResourceBody{
		"1.2.0.192.in-addr.arpa.": &dnsmessage.PTRResource{PTR: dnsmessage.MustNewName("gw.example.net.")},
	})
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

func TestEnrichHopsNamesEachASOnce(t *testing.T) {
	previous := traceroute.LookupTimeout
	traceroute.LookupTimeout = 300 * time.Millisecond
	t.Cleanup(func() { traceroute.LookupTimeout = previous })

	resolver, queries := fakeResolver(t, map[string]dnsmessage.ResourceBody{
		"8.8.8.8.origin.asn.cymru.com.": txt("15169 | 8.8.8.0/24 | US | arin | 2023-12-28"),
		"4.4.8.8.origin.asn.cymru.com.": txt("15169 | 8.8.4.0/24 | US | arin | 2023-12-28"),
		"AS15169.asn.cymru.com.":        txt("15169 | US | arin | 2000-03-30 | GOOGLE - Google LLC, US"),
	})
	report := protocol.TraceReport{Rows: []protocol.TraceRow{
		{Hop: 1, Address: "8.8.8.8"},
		{Hop: 2, Address: "8.8.4.4"},
	}}

	enrichHops(context.Background(), resolver, &report)
	for _, row := range report.Rows {
		if row.ASN != "AS15169" || row.ASName != "GOOGLE - Google LLC, US" {
			t.Fatalf("hop %d ASN = %q name = %q, want AS15169 GOOGLE - Google LLC, US", row.Hop, row.ASN, row.ASName)
		}
	}
	if got := queries("AS15169.asn.cymru.com."); got != 1 {
		t.Fatalf("AS name queried %d times, want once per trace", got)
	}
}

func txt(value string) *dnsmessage.TXTResource {
	return &dnsmessage.TXTResource{TXT: []string{value}}
}

// fakeResolver answers queries for the given names and silently drops every
// other query, like an unresponsive authoritative server. The returned
// function reports how many queries a name received.
func fakeResolver(t *testing.T, records map[string]dnsmessage.ResourceBody) (*net.Resolver, func(string) int) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var mu sync.Mutex
	counts := map[string]int{}
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
			mu.Lock()
			counts[question.Name.String()]++
			mu.Unlock()
			body, ok := records[question.Name.String()]
			if !ok || question.Type != recordType(body) {
				continue
			}
			msg.Header.Response = true
			msg.Answers = []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassINET, TTL: 60},
				Body:   body,
			}}
			packed, err := msg.Pack()
			if err != nil {
				continue
			}
			_, _ = conn.WriteTo(packed, from)
		}
	}()
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "udp", conn.LocalAddr().String())
		},
	}
	return resolver, func(name string) int {
		mu.Lock()
		defer mu.Unlock()
		return counts[name]
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

func mustAddr(value string) netip.Addr {
	addr, err := netip.ParseAddr(value)
	if err != nil {
		panic(err)
	}
	return addr
}
