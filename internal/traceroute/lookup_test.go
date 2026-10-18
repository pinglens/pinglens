package traceroute

import (
	"net/netip"
	"testing"
)

func TestASNQuery(t *testing.T) {
	if got := asnQuery(netip.MustParseAddr("203.0.113.9")); got != "9.113.0.203.origin.asn.cymru.com" {
		t.Fatalf("unexpected IPv4 query: %s", got)
	}
	if got := asnQuery(netip.MustParseAddr("2001:db8::1")); got == "" || got[len(got)-21:] != "origin6.asn.cymru.com" {
		t.Fatalf("unexpected IPv6 query: %s", got)
	}
}
