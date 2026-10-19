// Package traceroute probes network paths hop by hop and annotates the
// routers it finds with reverse DNS and origin AS names.
package traceroute

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/pinglens/pinglens/internal/target"
)

const (
	// maxHopLookups bounds concurrent hop name lookups so a burst of new
	// hops, such as every trace starting at once, does not flood the
	// resolver.
	maxHopLookups = 8
	// How long a lookup outcome is used before the lookup is repeated,
	// counted from when it finished. These are freshness limits for
	// annotations, not DNS TTLs: a changed record shows within about an
	// hour, a new one within about five minutes. Each is jittered by
	// lifetimeJitter so lookups that finished together, such as every hop
	// at startup, are not all repeated in the same minute.
	answerLifetime  = time.Hour
	missingLifetime = 5 * time.Minute
	failureRetry    = 30 * time.Second
	lifetimeJitter  = 0.2
	// settleAfter is how long a field may keep failing without ever
	// answering before it settles empty, so a hop whose PTR server never
	// answers is still exported, unnamed. It counts from the first failure
	// and needs no further lookups, so it also settles a hop that stopped
	// replying (and so stopped retrying) after one failed lookup.
	settleAfter = 90 * time.Second
)

// HopName annotates a hop address. Each field is empty when there is no
// such record or no answer yet.
type HopName struct {
	Host   string
	ASN    string
	ASName string
}

// HopNames annotates hop addresses with three independent lookups: the PTR
// name and origin ASN by address, and the AS description by ASN. Lookups run
// asynchronously, one at a time per field. A positive answer is used for
// about an hour after its lookup finished, a missing record (NXDOMAIN, NODATA
// or an unusable answer) for about five minutes, and a failed lookup
// (timeout, SERVFAIL) is retried after about 30 seconds while keeping the
// previous answer. Entries unseen for the retention period are pruned.
type HopNames struct {
	resolver *net.Resolver
	retain   time.Duration
	now      func() time.Time
	jitter   func(time.Duration) time.Duration
	slots    chan struct{}

	mu      sync.Mutex
	hops    map[netip.Addr]*hopEntry
	asNames map[string]*asNameEntry
}

type hopEntry struct {
	seen time.Time
	ptr  cachedLookup
	// noASN marks addresses without a public origin AS (private, reserved).
	noASN bool
	asn   cachedLookup
}

type asNameEntry struct {
	seen time.Time
	name cachedLookup
}

// cachedLookup is one field's latest answer and when to look it up again.
type cachedLookup struct {
	value string
	// answered is set once a lookup answered (a record or a definitive
	// miss); failingSince is when lookups started failing without an answer
	// yet. See settled.
	answered     bool
	failingSince time.Time
	busy         bool
	retryAt      time.Time
}

// settled reports whether the field's value is usable at now: it answered,
// or has failed for settleAfter.
func (r *cachedLookup) settled(now time.Time) bool {
	return r.answered || (!r.failingSince.IsZero() && !now.Before(r.failingSince.Add(settleAfter)))
}

// NewHopNames returns an empty cache that prunes entries unseen for retain.
// A nil resolver uses net.DefaultResolver.
func NewHopNames(resolver *net.Resolver, retain time.Duration) *HopNames {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return &HopNames{
		resolver: resolver,
		retain:   retain,
		now:      time.Now,
		jitter:   jitterLifetime,
		slots:    make(chan struct{}, maxHopLookups),
		hops:     map[netip.Addr]*hopEntry{},
		asNames:  map[string]*asNameEntry{},
	}
}

// jitterLifetime scales d by a random factor in [1-lifetimeJitter,
// 1+lifetimeJitter).
func jitterLifetime(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (1 - lifetimeJitter + 2*lifetimeJitter*rand.Float64()))
}

// Get marks addr as seen, starts any lookup that is due, and returns the
// current fields. ok is false until every lookup that applies to addr has
// settled.
func (c *HopNames) Get(addr netip.Addr) (HopName, bool) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.hops[addr]
	if entry == nil {
		entry = &hopEntry{noASN: !target.AddressAllowed(addr)}
		c.hops[addr] = entry
	}
	entry.seen = now
	c.refreshLocked(&entry.ptr, now, func(ctx context.Context) (string, error) {
		return lookupHost(ctx, c.resolver, addr)
	}, nil)
	if !entry.noASN {
		c.refreshLocked(&entry.asn, now, func(ctx context.Context) (string, error) {
			return lookupASN(ctx, c.resolver, addr)
		}, func(now time.Time) { c.touchASNameLocked(entry, now) })
		c.touchASNameLocked(entry, now)
	}
	return c.nameLocked(entry, now)
}

// Range calls fn for every entry seen within the retention period whose
// lookups have all settled. fn runs without the cache locked.
func (c *HopNames) Range(fn func(netip.Addr, HopName)) {
	type hop struct {
		addr netip.Addr
		name HopName
	}
	now := c.now()
	cutoff := now.Add(-c.retain)
	c.mu.Lock()
	hops := make([]hop, 0, len(c.hops))
	for addr, entry := range c.hops {
		if !entry.seen.After(cutoff) {
			continue
		}
		if name, ok := c.nameLocked(entry, now); ok {
			hops = append(hops, hop{addr, name})
		}
	}
	c.mu.Unlock()
	for _, h := range hops {
		fn(h.addr, h.name)
	}
}

// Pending counts the entries seen within the retention period whose lookups
// have not all settled yet.
func (c *HopNames) Pending() int {
	now := c.now()
	cutoff := now.Add(-c.retain)
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, entry := range c.hops {
		if _, ok := c.nameLocked(entry, now); entry.seen.After(cutoff) && !ok {
			n++
		}
	}
	return n
}

// Prune drops hop and AS-name entries unseen for the retention period.
// Entries with a lookup in flight are kept.
func (c *HopNames) Prune() {
	cutoff := c.now().Add(-c.retain)
	c.mu.Lock()
	defer c.mu.Unlock()
	for addr, entry := range c.hops {
		if !entry.seen.After(cutoff) && !entry.ptr.busy && !entry.asn.busy {
			delete(c.hops, addr)
		}
	}
	for asn, entry := range c.asNames {
		if !entry.seen.After(cutoff) && !entry.name.busy {
			delete(c.asNames, asn)
		}
	}
}

// nameLocked assembles entry's fields and whether they are all settled at
// now. The AS description belongs to the current ASN, so an ASN change never
// pairs the new ASN with the old description.
func (c *HopNames) nameLocked(entry *hopEntry, now time.Time) (HopName, bool) {
	name := HopName{Host: entry.ptr.value}
	done := entry.ptr.settled(now)
	if entry.noASN {
		return name, done
	}
	done = done && entry.asn.settled(now)
	if entry.asn.value == "" {
		return name, done
	}
	name.ASN = entry.asn.value
	if as := c.asNames[name.ASN]; as != nil && as.name.settled(now) {
		name.ASName = as.name.value
	} else {
		done = false
	}
	return name, done
}

// touchASNameLocked marks the description of entry's current ASN as in use
// and refreshes it when due. One entry per ASN serves every hop in the AS.
func (c *HopNames) touchASNameLocked(entry *hopEntry, now time.Time) {
	asn := entry.asn.value
	if asn == "" {
		return
	}
	as := c.asNames[asn]
	if as == nil {
		as = &asNameEntry{}
		c.asNames[asn] = as
	}
	as.seen = now
	c.refreshLocked(&as.name, now, func(ctx context.Context) (string, error) {
		return lookupASName(ctx, c.resolver, asn)
	}, nil)
}

// refreshLocked starts lookup for rec when it is due and none is in flight.
// after runs with the cache locked once the outcome is stored.
func (c *HopNames) refreshLocked(rec *cachedLookup, now time.Time, lookup func(context.Context) (string, error), after func(time.Time)) {
	if rec.busy || now.Before(rec.retryAt) {
		return
	}
	rec.busy = true
	go func() {
		c.slots <- struct{}{}
		value, err := lookup(context.Background())
		<-c.slots
		c.mu.Lock()
		defer c.mu.Unlock()
		done := c.now()
		rec.store(value, err, done, c.jitter)
		if after != nil {
			after(done)
		}
	}()
}

// store records a lookup outcome. Lifetimes count from now, the moment the
// lookup finished, so a hop that keeps replying cannot keep an answer alive.
// A failure keeps the previous answer.
func (r *cachedLookup) store(value string, err error, now time.Time, jitter func(time.Duration) time.Duration) {
	r.busy = false
	if err != nil {
		if !r.answered && r.failingSince.IsZero() {
			r.failingSince = now
		}
		r.retryAt = now.Add(jitter(failureRetry))
		return
	}
	r.answered = true
	r.failingSince = time.Time{}
	r.value = value
	if value == "" {
		r.retryAt = now.Add(jitter(missingLifetime))
	} else {
		r.retryAt = now.Add(jitter(answerLifetime))
	}
}

// lookupTimeout bounds each hop's PTR and ASN lookup so a slow or broken DNS
// server only costs that hop its annotation, never the whole trace.
var lookupTimeout = 2 * time.Second

// The lookups return an error only for a failed lookup; a missing record is
// an empty answer.

func lookupHost(ctx context.Context, resolver *net.Resolver, addr netip.Addr) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	names, err := resolver.LookupAddr(ctx, addr.String())
	if err != nil || len(names) == 0 {
		return "", failure(err)
	}
	// DNS names are bytes, and label values must be UTF-8.
	return strings.ToValidUTF8(strings.TrimSuffix(names[0], "."), "\uFFFD"), nil
}

func lookupASN(ctx context.Context, resolver *net.Resolver, addr netip.Addr) (string, error) {
	query := asnQuery(addr)
	if query == "" {
		return "", nil
	}
	fields, err := lookupCymruTXT(ctx, resolver, query)
	if err != nil || len(fields) == 0 {
		return "", err
	}
	// Multi-origin prefixes list several space-separated ASNs; keep the first.
	origins := strings.Fields(fields[0])
	if len(origins) == 0 {
		return "", nil
	}
	return "AS" + origins[0], nil
}

func lookupASName(ctx context.Context, resolver *net.Resolver, asn string) (string, error) {
	fields, err := lookupCymruTXT(ctx, resolver, asn+".asn.cymru.com")
	if err != nil || len(fields) < 5 {
		return "", err
	}
	return fields[4], nil
}

// lookupCymruTXT returns the first non-empty Team Cymru record split into its
// "|"-separated fields. TXT data is arbitrary bytes, so invalid UTF-8 is
// replaced to keep the fields usable as Prometheus label values.
func lookupCymruTXT(ctx context.Context, resolver *net.Resolver, query string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	// Query the rooted name so resolv.conf search domains (Kubernetes uses
	// ndots:5) are not tried first; each miss would eat into lookupTimeout.
	txts, err := resolver.LookupTXT(ctx, query+".")
	if err != nil {
		return nil, failure(err)
	}
	for _, txt := range txts {
		fields := strings.Split(strings.ToValidUTF8(txt, "\uFFFD"), "|")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		if fields[0] != "" {
			return fields, nil
		}
	}
	return nil, nil
}

// failure returns err unless it reports a missing record (NXDOMAIN or
// NODATA), which is an answer.
func failure(err error) error {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return nil
	}
	return err
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
