# PingLens

PingLens measures network paths from many locations at once.

- **PingLens** is a web service for on-demand tests. Enter a host, and an
  agent in every region pings it and traces the path to it, with a live
  mtr-style hop table per region.
- **pinglens-exporter** is a Prometheus exporter that traces a list of
  targets continuously, one ICMP probe per hop every 2.5 seconds, and
  exports per-hop probe counts and RTT histograms. With the bundled Grafana
  dashboard it gives a history of every path: loss and latency per hop,
  and which router answered at each hop over time.

Both trace with the same in-process ICMP engine and support IPv4 and IPv6
(the exporter traces IPv4).

| Artifact | Location |
|---|---|
| PingLens image | `ghcr.io/pinglens/pinglens` |
| Exporter image | `ghcr.io/pinglens/pinglens-exporter` |
| Helm charts | `oci://ghcr.io/pinglens/charts/pinglens`, `oci://ghcr.io/pinglens/charts/pinglens-exporter` |
| Grafana dashboard | [`mixin/`](mixin/), and `pinglens-trace.json` on each [release](https://github.com/pinglens/pinglens/releases) |

Images are published for `linux/amd64` and `linux/arm64`, tagged with the
release (`v0.1.0`), its minor line (`v0.1`) and `latest`; `main` tracks the
main branch.

## PingLens

PingLens runs as a coordinator and one agent per region:

- The **coordinator** serves the web UI and API, queues each test for every
  region's agent and streams results to the browser. It keeps state in
  Valkey.
- An **agent** long-polls the coordinator for tests, pings and traces the
  target, and posts the results back. It needs the `NET_RAW` capability for
  raw ICMP sockets.

Targets that resolve to private or reserved addresses are rejected.

### Quick start

```sh
docker compose up --build
```

starts Valkey, a coordinator and one local agent; open
<http://localhost:18080>.

### Kubernetes

```sh
helm install pinglens oci://ghcr.io/pinglens/charts/pinglens --version 0.1.0 \
  --set 'coordinator.regions={dfw}' \
  --set coordinator.agentTokensSecret.name=pinglens-agent-tokens
```

Run each region's agent with `coordinator.enabled=false`, `agent.enabled=true`,
`agent.id`, `agent.coordinatorURL` and `agent.tokenSecret`; see the chart's
[`values.yaml`](charts/pinglens/values.yaml).

### Configuration

The binary runs as a coordinator or an agent, chosen by `PINGLENS_MODE`
(`coordinator` or `agent`; it defaults to `agent` when `PINGLENS_AGENT_ID` and
`PINGLENS_COORDINATOR_URL` are set).

Coordinator:

| Variable | Default | Meaning |
|---|---|---|
| `PINGLENS_LISTEN` | `:8080` | UI and API listen address |
| `PINGLENS_METRICS_LISTEN` | `:9090` | Prometheus metrics listen address |
| `PINGLENS_AGENT_TOKENS` | | Agent credentials, `id=token,id=token` |
| `PINGLENS_REGIONS` | | Agent IDs shown first in the UI, in this order; other agents follow |
| `PINGLENS_VALKEY_URL` | | Valkey URL, e.g. `redis://valkey:6379/0` |
| `PINGLENS_VALKEY_SENTINEL_ADDRS` | | Sentinel addresses, comma-separated, used instead of the URL |
| `PINGLENS_VALKEY_SENTINEL_MASTER` | `myprimary` | Sentinel primary name |
| `PINGLENS_VALKEY_PASSWORD` | | Valkey password |
| `PINGLENS_VALKEY_PREFIX` | `pinglens` | Key prefix |
| `PINGLENS_JOB_DURATION` | `10m` | How long a test runs |
| `PINGLENS_JOB_RETENTION` | `15m` | How long finished results stay viewable |
| `PINGLENS_PING_COUNT` | `0` | Pings per test; `0` pings for the whole test |
| `PINGLENS_PING_INTERVAL` | `1s` | Time between pings |
| `PINGLENS_PING_TIMEOUT` | `3s` | How long a ping waits for its reply |
| `PINGLENS_TRACE_INTERVAL` | `1s` | Time between trace rounds; each round probes every hop once |
| `PINGLENS_TRACE_TIMEOUT` | `900ms` | How long a trace probe waits; shorter than the interval |
| `PINGLENS_TRACE_MAX_TTL` | `30` | Farthest hop traced (1-31) |
| `PINGLENS_TRACE_REPORT_INTERVAL` | `5s` | How often agents send the hop table |
| `PINGLENS_AGENT_POLL_TIMEOUT` | `30s` | Long-poll duration |
| `PINGLENS_AGENT_ONLINE_TIMEOUT` | `45s` | Time without a poll before an agent shows offline |
| `PINGLENS_MAX_QUEUED_JOBS_PER_AGENT` | `16` | Tests queued per agent |
| `PINGLENS_GLOBAL_CREATE_RATE_LIMIT` | `120` | Tests created per window, in total |
| `PINGLENS_TARGET_CREATE_RATE_LIMIT` | `30` | Tests created per window, per target |
| `PINGLENS_CLIENT_CREATE_RATE_LIMIT` | `20` | Tests created per window, per client |
| `PINGLENS_CREATE_RATE_WINDOW` | `1m` | Rate limit window |
| `PINGLENS_GLOBAL_CONCURRENCY_LIMIT` | `100` | Running tests, in total |
| `PINGLENS_TARGET_CONCURRENCY_LIMIT` | `25` | Running tests per target |
| `PINGLENS_CLIENT_CONCURRENCY_LIMIT` | `10` | Running tests per client |
| `PINGLENS_APP_NAME` | `PingLens` | Name shown in the UI |
| `PINGLENS_BRAND_NAME`, `PINGLENS_BRAND_URL`, `PINGLENS_BRAND_LOGO_URL` | | Operator name, link and icon in the UI |
| `PINGLENS_THEME_ACCENT`, `PINGLENS_THEME_ACCENT_SECONDARY`, `PINGLENS_THEME_BACKGROUND` | | UI colors (CSS colors) |

Agent:

| Variable | Default | Meaning |
|---|---|---|
| `PINGLENS_COORDINATOR_URL` | | Coordinator base URL |
| `PINGLENS_AGENT_ID` | | Region ID, a key of the coordinator's `PINGLENS_AGENT_TOKENS` |
| `PINGLENS_AGENT_TOKEN`, `PINGLENS_AGENT_TOKEN_FILE` | | The agent's token, or a file holding it |
| `PINGLENS_DISPLAY_NAME` | | Region name shown in the UI |
| `PINGLENS_POLL_BACKOFF` | `2s` | Wait after a failed poll |
| `PINGLENS_USER_AGENT` | `pinglens-agent` | HTTP user agent |

Both read `PINGLENS_LOG_LEVEL` (`debug`, `info`, `warn`, `error`).

## pinglens-exporter

```yaml
# config.yml
interval: 2.5s   # time between rounds; each round probes every hop once
timeout: 2s      # how long a probe waits; shorter than interval
max_ttl: 30      # farthest hop probed while the target is unreached (1-31)
targets:
  - host: 1.1.1.1
    group_title: Internet Targets
    endpoint_name: Cloudflare DNS
  - host: example.com
```

Each target has a `host` (an IPv4 address or a name) and any other keys as
labels on its series. The exporter re-reads the file every 30 seconds and
applies target changes in place without resetting other targets' counters;
`interval`, `timeout` and `max_ttl` apply at startup.

```sh
docker run --cap-drop ALL --cap-add NET_RAW --user 0 -p 9430:9430 \
  -v "$PWD/config.yml:/config/config.yml:ro" ghcr.io/pinglens/pinglens-exporter
```

| Variable | Default | Meaning |
|---|---|---|
| `PINGLENS_EXPORTER_CONFIG` | `/config/config.yml` | Config file |
| `PINGLENS_EXPORTER_LISTEN` | `:9430` | Listen address |
| `PINGLENS_EXPORTER_RELOAD_INTERVAL` | `30s` | How often the config file is re-read |
| `PINGLENS_LOG_LEVEL` | `info` | Log level |

`/metrics` serves the metrics, `/healthz` passes once traces run, and
`/readyz` passes once every target's path is discovered and its hop names
are looked up (at most five minutes after start).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `pinglens_trace_probes_sent_total` | counter | `ttl` | Probes per hop, counted when answered or timed out |
| `pinglens_trace_reply_rtt_seconds` | native histogram | `ttl`, `hop_ip`, `dest` | Reply RTT per hop and responding address; `dest="true"` for the target's own replies |
| `pinglens_trace_destination_ttl` | gauge | | Hop at which the target answers, `0` while unreached |
| `pinglens_trace_target_up` | gauge | | Whether the target's latest resolution succeeded |
| `pinglens_trace_target_info` | gauge | `target_ip` | Address the target resolves to |
| `pinglens_trace_hop_info` | gauge | `hop_ip`, `hop_host`, `hop_asn`, `hop_as_name` | Reverse DNS and origin AS of each hop |

Every series except `pinglens_trace_hop_info` also carries `target` and the
target's labels. `ttl` is zero-padded (`01`), so it sorts in hop order. Loss
per hop is `1 - replies / probes sent` over any window.

The RTT histogram is a [native histogram](https://prometheus.io/docs/specs/native_histograms/)
(8 buckets per doubling, each about 9% wide). Scraped natively it is one
series per hop and responding address that carries the whole latency
distribution; enable it with `scrape_native_histograms: true` in the scrape
job (Prometheus 3.8 or later), or `serviceMonitor.scrapeNativeHistograms` in
the chart (Prometheus Operator 0.88 or later). Scraped in the text format it
is `_count`, `_sum` and a single `+Inf` bucket: averages and loss, without
the distribution.

The [chart](charts/pinglens-exporter/values.yaml) renders `config.yml` from
groups of targets, adding `ping_group` and `endpoint_fullname` labels:

```yaml
targets:
  internet:
    labels:
      group_title: Internet Targets
    targets:
      - 1.1.1.1:
          endpoint_name: Cloudflare DNS
      - 8.8.8.8
# Exporters in several sites can share one target list and each skip its
# own site:
excludeTargets:
  - target_region: dfw
```

## Grafana dashboard

The [mixin](mixin/) builds the **PingLens Trace** dashboard: a hop table for
the selected range, the responding address per hop over time, destination
latency and loss, latency and loss per hop, and the destination RTT
distribution. It selects targets by the chart's `group_title` and
`endpoint_fullname` labels. The percentile columns and the RTT distribution
come from native histograms; every other panel works with either scrape
format.

```jsonnet
// With jsonnet-bundler: jb install github.com/pinglens/pinglens/mixin@main
local pinglens = (import 'github.com/pinglens/pinglens/mixin/mixin.libsonnet') + {
  _config+:: {
    showMultiCluster: true,  // select one cluster by clusterLabel
    clusterLabel: 'cluster',
  },
};
pinglens.grafanaDashboards
```

Each release also attaches the dashboard built with the default config
(single cluster) as `pinglens-trace.json`.

## Development

```sh
go vet ./... && go test -race ./...
node --test web/*.test.js
sh charts/pinglens-exporter/tests/test.sh
sh mixin/test.sh   # needs jsonnet, jsonnetfmt and jq
```

## Releases

`main` is always releasable. Pushing a tag `vX.Y.Z` on `main` publishes the
release: both images, both charts at version `X.Y.Z` with `appVersion`
`vX.Y.Z` (so each chart runs its own release's images), and a GitHub release
with generated notes. Tags with a pre-release suffix (`v0.2.0-rc.1`) publish
without moving `latest`.

To fix an older minor line after `main` has moved on, branch `release-X.Y`
from its latest tag, cherry-pick the fix from `main`, and tag `vX.Y.Z+1` on
the branch.

## License

[Apache License 2.0](LICENSE)
