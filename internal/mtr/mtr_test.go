package mtr

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"
)

func TestBuildArgsUsesFixedFlags(t *testing.T) {
	args := BuildArgs("8.8.8.8", Config{Cycles: 15, MaxTTL: 30})
	joined := ""
	for _, arg := range args {
		joined += arg + " "
	}
	if joined != "--report --json --show-ips --report-cycles 15 --max-ttl 30 8.8.8.8 " {
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

func mustAddr(value string) netip.Addr {
	addr, err := netip.ParseAddr(value)
	if err != nil {
		panic(err)
	}
	return addr
}
