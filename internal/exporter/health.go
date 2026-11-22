package exporter

import (
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/pinglens/pinglens/internal/traceroute"
)

// counters are the exporter's own event counts and timings, shared by every
// target and read at collect time.
type counters struct {
	reloadsApplied, reloadsInvalid, reloadsFailed atomic.Uint64
	resolveSuccess, resolveFailure                atomic.Uint64
	replies, timeouts                             atomic.Uint64
	// lastReload is the Unix time in nanoseconds of the latest successful
	// configuration load.
	lastReload atomic.Int64
	// warmup is the time from Run to warm in nanoseconds, stored before warm
	// latches.
	warmup atomic.Int64
	// collectDuration is the wall time of the latest finished Collect in
	// nanoseconds, negative until one finishes.
	collectDuration atomic.Int64
}

var (
	buildInfoDesc = prometheus.NewDesc("pinglens_exporter_build_info",
		"Version, VCS revision and Go version the exporter was built from; always 1.",
		[]string{"version", "revision", "goversion"}, nil)
	readyDesc = prometheus.NewDesc("pinglens_exporter_ready",
		"Whether the exporter has warmed up, which is what /readyz reports.", nil, nil)
	warmupDesc = prometheus.NewDesc("pinglens_exporter_warmup_seconds",
		"Seconds from start until the exporter warmed up.", nil, nil)
	reloadsDesc = prometheus.NewDesc("pinglens_exporter_config_reloads_total",
		"Loads of a changed configuration file: applied, invalid (kept the running configuration) or error (read failure).",
		[]string{"result"}, nil)
	lastReloadDesc = prometheus.NewDesc("pinglens_exporter_config_last_reload_success_timestamp_seconds",
		"Unix time of the latest successful configuration load, including the one at startup.", nil, nil)
	targetsDesc = prometheus.NewDesc("pinglens_exporter_targets",
		"Configured targets.", nil, nil)
	discoveringDesc = prometheus.NewDesc("pinglens_exporter_targets_discovering",
		"Targets whose trace has not discovered its path yet.", nil, nil)
	unresolvedDesc = prometheus.NewDesc("pinglens_exporter_targets_unresolved",
		"Targets whose latest resolution failed or that have not resolved yet.", nil, nil)
	resolutionsDesc = prometheus.NewDesc("pinglens_exporter_target_resolutions_total",
		"Target resolutions: one per DNS lookup of a hostname target, and one success per trace of an IP literal.",
		[]string{"result"}, nil)
	outcomesDesc = prometheus.NewDesc("pinglens_exporter_probe_outcomes_total",
		"Probe outcomes across all targets: a reply within the timeout, or a timeout.",
		[]string{"outcome"}, nil)
	receiveErrorsDesc = prometheus.NewDesc("pinglens_exporter_icmp_receive_errors_total",
		"Failed reads from the raw ICMP sockets.", nil, nil)
	unmatchedDesc = prometheus.NewDesc("pinglens_exporter_icmp_unmatched_total",
		"Received ICMP messages that answer no pending probe: other ICMP traffic, and late or duplicate replies.", nil, nil)
	tracesFailedDesc = prometheus.NewDesc("pinglens_exporter_traces_failed_total",
		"Traces that could not start: invalid_options, family_unavailable (an IP literal of a family without a socket) or no_echo_id.",
		[]string{"reason"}, nil)
	hopNamesDesc = prometheus.NewDesc("pinglens_exporter_hop_names",
		"Hop addresses in the name cache.", nil, nil)
	hopNamesPendingDesc = prometheus.NewDesc("pinglens_exporter_hop_name_lookups_pending",
		"Hop addresses whose name lookups have not all settled.", nil, nil)
	hopNameLookupsDesc = prometheus.NewDesc("pinglens_exporter_hop_name_lookups_total",
		"Completed hop name lookups by kind and result: success, not_found (NXDOMAIN, NODATA or an unusable answer) or error.",
		[]string{"kind", "result"}, nil)
	hopSeriesDesc = prometheus.NewDesc("pinglens_exporter_hop_series",
		"RTT histogram series in this scrape.", nil, nil)
	collectDurationDesc = prometheus.NewDesc("pinglens_exporter_last_collect_duration_seconds",
		"Wall time of the previous collection of the exporter's metrics.", nil, nil)
)

// buildVersion, buildRevision and buildGoVersion label build_info; each is
// empty when the binary lacks it.
var buildVersion, buildRevision, buildGoVersion = buildLabels()

func buildLabels() (version, revision, goVersion string) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", "", ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			revision = setting.Value
		}
	}
	return info.Main.Version, revision, info.GoVersion
}

// collectHealth sends the exporter health metrics. targets are the
// snapshots this Collect exported and hopSeries the RTT histograms it sent.
func (e *Exporter) collectHealth(send func(prometheus.Metric, error) bool, targets []targetSnapshot, hopSeries int) {
	gauge := func(desc *prometheus.Desc, v float64, labels ...string) {
		send(prometheus.NewConstMetric(desc, prometheus.GaugeValue, v, labels...))
	}
	counter := func(desc *prometheus.Desc, v uint64, labels ...string) {
		send(prometheus.NewConstMetric(desc, prometheus.CounterValue, float64(v), labels...))
	}
	seconds := func(nanos int64) float64 { return time.Duration(nanos).Seconds() }

	gauge(buildInfoDesc, 1, buildVersion, buildRevision, buildGoVersion)
	warm := e.warm.Load()
	gauge(readyDesc, boolValue(warm))
	if warm {
		gauge(warmupDesc, seconds(e.stats.warmup.Load()))
	}
	counter(reloadsDesc, e.stats.reloadsApplied.Load(), "applied")
	counter(reloadsDesc, e.stats.reloadsInvalid.Load(), "invalid")
	counter(reloadsDesc, e.stats.reloadsFailed.Load(), "error")
	gauge(lastReloadDesc, seconds(e.stats.lastReload.Load()))

	discovering, unresolved := 0, 0
	for _, t := range targets {
		if !pathFound(t.sent[1]) {
			discovering++
		}
		if !t.up {
			unresolved++
		}
	}
	gauge(targetsDesc, float64(len(targets)))
	gauge(discoveringDesc, float64(discovering))
	gauge(unresolvedDesc, float64(unresolved))
	counter(resolutionsDesc, e.stats.resolveSuccess.Load(), "success")
	counter(resolutionsDesc, e.stats.resolveFailure.Load(), "failure")
	counter(outcomesDesc, e.stats.replies.Load(), "reply")
	counter(outcomesDesc, e.stats.timeouts.Load(), "timeout")

	prober := e.tracer.Stats()
	counter(receiveErrorsDesc, prober.ReceiveErrors)
	counter(unmatchedDesc, prober.Unmatched)
	counter(tracesFailedDesc, prober.InvalidOptions, "invalid_options")
	counter(tracesFailedDesc, prober.FamilyUnavailable, "family_unavailable")
	counter(tracesFailedDesc, prober.NoEchoID, "no_echo_id")

	gauge(hopNamesDesc, float64(e.names.Len()))
	gauge(hopNamesPendingDesc, float64(e.names.Pending()))
	lookups := e.names.Lookups()
	for _, kind := range []struct {
		name    string
		results traceroute.LookupResults
	}{{"ptr", lookups.PTR}, {"asn", lookups.ASN}, {"as_name", lookups.ASName}} {
		counter(hopNameLookupsDesc, kind.results.Success, kind.name, "success")
		counter(hopNameLookupsDesc, kind.results.NotFound, kind.name, "not_found")
		counter(hopNameLookupsDesc, kind.results.Error, kind.name, "error")
	}

	gauge(hopSeriesDesc, float64(hopSeries))
	if d := e.stats.collectDuration.Load(); d >= 0 {
		gauge(collectDurationDesc, seconds(d))
	}
}
