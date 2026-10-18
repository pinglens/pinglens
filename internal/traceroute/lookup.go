// Package traceroute probes network paths hop by hop and annotates the
// routers it finds with reverse DNS and origin AS names.
package traceroute

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

// LookupTimeout bounds each hop's PTR and ASN lookup so a slow or broken DNS
// server only costs that hop its annotation, never the whole trace.
var LookupTimeout = 2 * time.Second

// LookupHost returns addr's first PTR name without the trailing dot, or ""
// when there is none.
func LookupHost(ctx context.Context, resolver *net.Resolver, addr netip.Addr) string {
	ctx, cancel := context.WithTimeout(ctx, LookupTimeout)
	defer cancel()
	names, err := resolver.LookupAddr(ctx, addr.String())
	if err != nil || len(names) == 0 {
		return ""
	}
	return strings.TrimSuffix(names[0], ".")
}

// LookupASN returns the origin ASN for addr ("AS15169") from Team Cymru's
// "ASN | prefix | CC | registry | allocated" TXT record.
func LookupASN(ctx context.Context, resolver *net.Resolver, addr netip.Addr) string {
	query := asnQuery(addr)
	if query == "" {
		return ""
	}
	fields := lookupCymruTXT(ctx, resolver, query)
	if len(fields) == 0 {
		return ""
	}
	// Multi-origin prefixes list several space-separated ASNs; keep the first.
	origins := strings.Fields(fields[0])
	if len(origins) == 0 {
		return ""
	}
	return "AS" + origins[0]
}

// LookupASName returns the AS description from Team Cymru's
// "ASN | CC | registry | allocated | name" TXT record for asn ("AS15169").
func LookupASName(ctx context.Context, resolver *net.Resolver, asn string) string {
	fields := lookupCymruTXT(ctx, resolver, asn+".asn.cymru.com")
	if len(fields) < 5 {
		return ""
	}
	return fields[4]
}

func lookupCymruTXT(ctx context.Context, resolver *net.Resolver, query string) []string {
	ctx, cancel := context.WithTimeout(ctx, LookupTimeout)
	defer cancel()
	// Query the rooted name so resolv.conf search domains (Kubernetes uses
	// ndots:5) are not tried first; each miss would eat into LookupTimeout.
	txts, err := resolver.LookupTXT(ctx, query+".")
	if err != nil {
		return nil
	}
	for _, txt := range txts {
		fields := strings.Split(txt, "|")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		if fields[0] != "" {
			return fields
		}
	}
	return nil
}

func asnQuery(addr netip.Addr) string {
	addr = addr.Unmap()
	if addr.Is4() {
		octets := addr.As4()
		return fmt.Sprintf("%d.%d.%d.%d.origin.asn.cymru.com", octets[3], octets[2], octets[1], octets[0])
	}
	if addr.Is6() {
		bytes := addr.As16()
		var b strings.Builder
		for i := len(bytes) - 1; i >= 0; i-- {
			lo := bytes[i] & 0x0f
			hi := bytes[i] >> 4
			_, _ = fmt.Fprintf(&b, "%x.%x.", lo, hi)
		}
		b.WriteString("origin6.asn.cymru.com")
		return b.String()
	}
	return ""
}
