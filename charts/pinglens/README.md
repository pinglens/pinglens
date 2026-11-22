# pinglens

PingLens regional ping and MTR-style path testing service

## Installing

```sh
helm install pinglens oci://ghcr.io/pinglens/charts/pinglens --version 0.1.0 \
  --set 'coordinator.regions={dfw}' \
  --set coordinator.agentTokensSecret.name=pinglens-agent-tokens
```

Run each region's agent as its own release with `coordinator.enabled=false`,
`agent.enabled=true`, `agent.id`, `agent.coordinatorURL` and
`agent.tokenSecret`.

The coordinator stores its state in the bundled [Valkey
chart](https://github.com/valkey-io/valkey-helm), configured under `valkey`.
To use an existing Valkey, set `valkey.enabled=false` and `stateStore.url`
or `stateStore.sentinelAddrs`.

## Requirements

| Repository | Name | Version |
|------------|------|---------|
| https://valkey.io/valkey-helm/ | valkey | 0.9.* |

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity for the coordinator and agents, unless `coordinator.affinity` or `agent.affinity` is set |
| agent.affinity | object | `affinity` | Agent affinity |
| agent.coordinatorURL | string | `""` | Coordinator base URL |
| agent.displayName | string | `""` | Region name shown in the UI |
| agent.dnsConfig | object | `{}` | Agent pod DNS configuration |
| agent.dnsPolicy | string | `""` | Agent pod DNS policy |
| agent.enabled | bool | `false` | Deploy an agent, which runs tests for one region |
| agent.id | string | `""` | Region ID, a key in the coordinator's agent tokens |
| agent.pollBackoff | string | `"2s"` | Wait after a failed poll |
| agent.replicaCount | int | `1` | Agent replicas |
| agent.tokenSecret.key | string | `"token"` | Key in the Secret |
| agent.tokenSecret.name | string | `""` | Secret holding the agent's token |
| agentSecurityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"add":["NET_RAW"],"drop":["ALL"]},"runAsGroup":0,"runAsNonRoot":false,"runAsUser":0}` | Agent container security context. Agents run as root with `NET_RAW` to open raw ICMP sockets. |
| agentService.annotations | object | `{}` | Service annotations |
| agentService.enabled | bool | `false` | Create an extra coordinator Service for agents, e.g. a load balancer that agents in other clusters connect to |
| agentService.externalTrafficPolicy | string | `""` | External traffic policy, applied when `type` is `LoadBalancer` |
| agentService.loadBalancerIP | string | `""` | Load balancer IP |
| agentService.loadBalancerSourceRanges | list | `[]` | CIDRs allowed to reach the load balancer |
| agentService.name | string | `"agent"` | Suffix appended to the coordinator name for the Service name |
| agentService.port | int | `8080` | Service port |
| agentService.type | string | `"ClusterIP"` | Service type |
| annotations | object | `{}` | Annotations on the coordinator and agent Deployments, for tools that read their settings from Deployment metadata |
| branding.appName | string | `"PingLens"` | Name shown in the UI |
| branding.providerLogoURL | string | `""` | Operator icon URL |
| branding.providerName | string | `""` | Operator name shown in the UI |
| branding.providerURL | string | `""` | Link for the operator name |
| branding.theme.accent | string | `""` | Primary accent color |
| branding.theme.accentSecondary | string | `""` | Secondary accent color |
| branding.theme.background | string | `""` | Page background color |
| coordinator.affinity | object | `affinity` | Coordinator affinity |
| coordinator.agentOnlineTimeout | string | `"45s"` | Time without a poll before an agent shows offline |
| coordinator.agentPollTimeout | string | `"30s"` | Agent long-poll duration |
| coordinator.agentTokensSecret.key | string | `"agent-tokens"` | Key in the Secret |
| coordinator.agentTokensSecret.name | string | `""` | Secret holding agent credentials as `id=token,id=token` |
| coordinator.clientConcurrencyLimit | int | `10` | Running tests per client |
| coordinator.clientCreateRateLimit | int | `20` | Tests created per window, per client |
| coordinator.createRateWindow | string | `"1m"` | Rate limit window |
| coordinator.enabled | bool | `true` | Deploy the coordinator, which serves the UI and API and queues tests for agents |
| coordinator.globalConcurrencyLimit | int | `100` | Running tests, in total |
| coordinator.globalCreateRateLimit | int | `120` | Tests created per window, in total |
| coordinator.jobDuration | string | `"10m"` | How long a test runs |
| coordinator.jobRetention | string | `"15m"` | How long finished results stay viewable |
| coordinator.listen | string | `":8080"` | UI and API listen address |
| coordinator.maxQueuedJobsPerAgent | int | `16` | Tests queued per agent |
| coordinator.metricsListen | string | `":9090"` | Metrics listen address |
| coordinator.pingCount | int | `0` | Pings per test; `0` pings for the whole test |
| coordinator.pingInterval | string | `"1s"` | Time between pings |
| coordinator.pingTimeout | string | `"3s"` | How long a ping waits for its reply |
| coordinator.regions | list | `[]` | Agent IDs shown first in the UI, in this order; other agents follow |
| coordinator.replicaCount | int | `1` | Coordinator replicas |
| coordinator.targetConcurrencyLimit | int | `25` | Running tests per target |
| coordinator.targetCreateRateLimit | int | `30` | Tests created per window, per target |
| coordinator.traceInterval | string | `"1s"` | Time between trace rounds; each round probes every hop once |
| coordinator.traceMaxTTL | int | `30` | Farthest hop traced (1-31) |
| coordinator.traceReportInterval | string | `"5s"` | How often agents send the hop table, which is kept with the test's events |
| coordinator.traceTimeout | string | `"900ms"` | How long a trace probe waits; shorter than `traceInterval` |
| extraEnv | list | `[]` | Extra environment variables for the coordinator and agent containers |
| fullnameOverride | string | `""` | Overrides the full resource name prefix |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy |
| image.repository | string | `"ghcr.io/pinglens/pinglens"` | Image repository for the coordinator and agents |
| image.tag | string | the chart's `appVersion` | Image tag |
| imagePullSecrets | list | `[]` | Image pull secrets for the coordinator and agent pods |
| ingress.annotations | object | `{}` | Ingress annotations |
| ingress.className | string | `""` | Ingress class name |
| ingress.enabled | bool | `false` | Create an Ingress for the coordinator UI and API |
| ingress.hosts | list | `[]` | Ingress hosts, each a `host` with `paths` of `path` and `pathType`; required when the Ingress is enabled |
| ingress.labels | object | `{}` | Ingress labels |
| ingress.tls | list | `[]` | Ingress TLS configuration |
| metricsService.annotations | object | `{}` | Service annotations |
| metricsService.enabled | bool | `true` | Create the coordinator metrics Service |
| metricsService.port | int | `9090` | Service port, also the coordinator's metrics container port |
| metricsService.type | string | `"ClusterIP"` | Service type |
| nameOverride | string | `""` | Overrides the chart name in resource names and labels |
| nodeSelector | object | `{}` | Node selector for the coordinator and agents |
| podAnnotations | object | `{}` | Annotations on the coordinator and agent pods |
| podLabels | object | `{}` | Labels on the coordinator and agent pods |
| podSecurityContext | object | `{}` | Pod security context for the coordinator and agents |
| resources | object | `{}` | Container resources for the coordinator and agents |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}` | Coordinator container security context |
| service.annotations | object | `{}` | Service annotations |
| service.enabled | bool | `true` | Create the coordinator UI and API Service |
| service.externalTrafficPolicy | string | `""` | External traffic policy, applied when `type` is `LoadBalancer` |
| service.loadBalancerIP | string | `""` | Load balancer IP |
| service.loadBalancerSourceRanges | list | `[]` | CIDRs allowed to reach the load balancer |
| service.port | int | `8080` | Service port, also the coordinator's container port |
| service.type | string | `"ClusterIP"` | Service type |
| serviceAccount.annotations | object | `{}` | ServiceAccount annotations |
| serviceAccount.create | bool | `true` | Create a ServiceAccount |
| serviceAccount.name | string | the full name when `create` is true, otherwise `default` | ServiceAccount name |
| serviceMonitor.enabled | bool | `false` | Create a Prometheus Operator ServiceMonitor for the coordinator metrics |
| serviceMonitor.interval | string | `"30s"` | Scrape interval |
| serviceMonitor.labels | object | `{}` | ServiceMonitor labels |
| serviceMonitor.scrapeTimeout | string | `"10s"` | Scrape timeout |
| stateStore.passwordSecret.key | string | `"password"` | Key in the Secret |
| stateStore.passwordSecret.name | string | `""` | Secret holding the Valkey password |
| stateStore.prefix | string | `"pinglens"` | Key prefix |
| stateStore.sentinelAddrs | list | `[]` | Valkey Sentinel addresses, used instead of `url` |
| stateStore.sentinelMaster | string | `"myprimary"` | Valkey Sentinel primary name |
| stateStore.url | string | the bundled Valkey when `valkey.enabled` is not `false` | Valkey URL, e.g. `redis://valkey:6379/0` |
| tolerations | list | `[]` | Tolerations for the coordinator and agents |
| valkey.auth.enabled | bool | `false` | Require a Valkey password |
| valkey.dataStorage.enabled | bool | `false` | Persist Valkey data to a volume |
| valkey.replica.enabled | bool | `false` | Run Valkey replicas |
| valkey.replica.persistence.size | string | `""` | Valkey replica volume size |
| valkey.replica.replicas | int | `1` | Valkey replica count |
| valkey.replica.service.enabled | bool | `false` | Create a Service for the Valkey replicas |

Generated by [helm-docs](https://github.com/norwoodj/helm-docs) from
`values.yaml` and `README.md.gotmpl`.
