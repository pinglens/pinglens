package exporter

import (
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/pinglens/pinglens/internal/traceroute"
)

// rttBuckets are the RTT histogram's upper bounds in seconds.
var rttBuckets = [...]float64{.001, .0025, .005, .01, .025, .05, .1, .2, .4, 1}

const (
	metricSent    = "pinglens_trace_probes_sent_total"
	metricRTT     = "pinglens_trace_reply_rtt_seconds"
	metricDestTTL = "pinglens_trace_destination_ttl"
	metricUp      = "pinglens_trace_target_up"
	metricTarget  = "pinglens_trace_target_info"
	metricHop     = "pinglens_trace_hop_info"
)

var hopInfoDesc = prometheus.NewDesc(metricHop,
	"Reverse DNS name and origin AS of a hop address; join on hop_ip.",
	[]string{"hop_ip", "hop_host", "hop_asn", "hop_as_name"}, nil)

// Describe sends nothing, making the exporter an unchecked collector: target
// label names follow the configuration file and may change at any reload.
func (e *Exporter) Describe(chan<- *prometheus.Desc) {}

// Collect builds every series from the current target states. All targets
// share one label set, target plus the union of their configured label
// names, so each metric family has consistent label names; a target without
// a label exports it empty.
//
// Target state is copied under its lock and the metrics are built and sent
// after releasing it: the receive goroutine needs the same locks to record
// replies, and a stall there would add to the RTT of queued replies.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	states := e.states()
	targets := make([]targetSnapshot, len(states))
	keySet := map[string]bool{}
	for i, state := range states {
		targets[i] = state.snapshot()
		for key := range targets[i].labels {
			keySet[key] = true
		}
	}
	keys := slices.Sorted(maps.Keys(keySet))
	base := append([]string{"target"}, keys...)
	with := func(extra ...string) []string { return append(slices.Clone(base), extra...) }

	sentDesc := prometheus.NewDesc(metricSent, "Probes per TTL, counted when answered or timed out.", with("ttl"), nil)
	rttDesc := prometheus.NewDesc(metricRTT,
		"RTT of replies within the timeout, per TTL and responding address; dest is true when the target itself answered.",
		with("ttl", "hop_ip", "dest"), nil)
	destDesc := prometheus.NewDesc(metricDestTTL, "TTL at which the target currently answers, 0 while it is unreached.", base, nil)
	upDesc := prometheus.NewDesc(metricUp, "Whether the target's latest resolution succeeded.", base, nil)
	infoDesc := prometheus.NewDesc(metricTarget, "Address the target currently resolves to.", with("target_ip"), nil)

	// An invalid series (such as a label value that is not UTF-8) is logged
	// and dropped rather than failing the scrape or panicking in Gather.
	send := func(m prometheus.Metric, err error) {
		if err != nil {
			e.logger.Error("drop invalid metric", "err", err)
			return
		}
		ch <- m
	}
	for _, t := range targets {
		values := make([]string, 0, len(base))
		values = append(values, t.host)
		for _, key := range keys {
			values = append(values, t.labels[key])
		}
		withValues := func(extra ...string) []string { return append(slices.Clone(values), extra...) }

		for ttl, n := range t.sent {
			if n > 0 {
				send(prometheus.NewConstMetric(sentDesc, prometheus.CounterValue, float64(n), withValues(ttlLabel(ttl))...))
			}
		}
		for _, hop := range t.hops {
			buckets := make(map[float64]uint64, len(rttBuckets))
			var cumulative uint64
			for i, bound := range rttBuckets {
				cumulative += hop.series.buckets[i]
				buckets[bound] = cumulative
			}
			send(prometheus.NewConstHistogram(rttDesc, hop.series.count, hop.series.sum, buckets,
				withValues(ttlLabel(hop.key.ttl), hop.key.ip.String(), strconv.FormatBool(hop.key.dest))...))
		}
		send(prometheus.NewConstMetric(destDesc, prometheus.GaugeValue, float64(t.destTTL), values...))
		send(prometheus.NewConstMetric(upDesc, prometheus.GaugeValue, boolValue(t.up), values...))
		targetIP := ""
		if t.ip.IsValid() {
			targetIP = t.ip.String()
		}
		send(prometheus.NewConstMetric(infoDesc, prometheus.GaugeValue, 1, withValues(targetIP)...))
	}

	e.names.Range(func(addr netip.Addr, name traceroute.HopName) {
		send(prometheus.NewConstMetric(hopInfoDesc, prometheus.GaugeValue, 1, addr.String(), name.Host, name.ASN, name.ASName))
	})
}

// targetSnapshot is a copy of one target's state for a scrape.
type targetSnapshot struct {
	host    string
	labels  map[string]string
	up      bool
	ip      netip.Addr
	destTTL int
	sent    [traceroute.MaxTTLLimit + 1]uint64
	hops    []hopSnapshot
}

type hopSnapshot struct {
	key    hopKey
	series hopSeries
}

// snapshot copies the state under its lock. labels is shared: setLabels
// replaces the map rather than modifying it.
func (s *targetState) snapshot() targetSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := targetSnapshot{
		host:    s.host,
		labels:  s.labels,
		up:      s.up,
		ip:      s.ip,
		destTTL: s.destTTL,
		sent:    s.sent,
		hops:    make([]hopSnapshot, 0, len(s.hops)),
	}
	for key, series := range s.hops {
		snap.hops = append(snap.hops, hopSnapshot{key: key, series: *series})
	}
	return snap
}

// ttlLabel zero-pads the TTL so lexical order is hop order.
func ttlLabel(ttl int) string {
	return fmt.Sprintf("%02d", ttl)
}

func boolValue(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
