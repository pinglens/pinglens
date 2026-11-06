#!/bin/sh
set -eu

tests_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
chart_dir=$(dirname "$tests_dir")
output=${1:-}
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM

targets="$work_dir/targets.yaml"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

helm lint --strict "$chart_dir" -f "$tests_dir/values-targets.yaml"
helm template pinglens-exporter "$chart_dir" -f "$tests_dir/values-targets.yaml" > "$targets"

# The exporter reads targets as "host" plus string labels.
grep -Eq '^ +- host: "1\.1\.1\.1"$' "$targets" || fail "target not rendered with host key"
grep -Eq '^ +- host: "8\.8\.8\.8"$' "$targets" || fail "plain target not rendered"
grep -Eq '^ +ping_group: "customer"$' "$targets" || fail "ping_group missing"
grep -Eq '^ +endpoint_fullname: "SITE-D1 192\.0\.2\.10"$' "$targets" || fail "endpoint_fullname missing"
grep -Eq '^ +asn: "64496"$' "$targets" || fail "numeric label not rendered as a string"

# The exporter reloads targets in place: target changes must leave the pod
# template alone, global config changes must roll the pod.
deployment() {
  helm template pinglens-exporter "$chart_dir" -f "$tests_dir/values-targets.yaml" \
    --show-only templates/deployment.yaml "$@"
}
deployment > "$work_dir/deployment.yaml"
deployment --set 'targets.extra.targets[0]=192.0.2.20' > "$work_dir/deployment-targets.yaml"
deployment --set config.interval=5s > "$work_dir/deployment-config.yaml"
cmp -s "$work_dir/deployment.yaml" "$work_dir/deployment-targets.yaml" ||
  fail "target change alters the pod template"
cmp -s "$work_dir/deployment.yaml" "$work_dir/deployment-config.yaml" &&
  fail "global config change does not alter the pod template"

# render_fails MESSAGE ARGS...: rendering must fail with MESSAGE.
render_fails() {
  message=$1
  shift
  if helm template pinglens-exporter "$chart_dir" "$@" > /dev/null 2> "$work_dir/error"; then
    return 1
  fi
  grep -Fq "$message" "$work_dir/error"
}
render_fails "pinglens-exporter: no targets configured" ||
  fail "rendered without any targets"
render_fails '"host" is reserved' -f "$tests_dir/values-targets.yaml" \
  --set 'targets.default.targets[1].8\.8\.8\.8.host=x' ||
  fail "rendered a target with a host label"

helm template pinglens-exporter "$chart_dir" -f "$tests_dir/values-targets.yaml" \
  --set serviceMonitor.enabled=true,serviceMonitor.labels.release=x \
  --show-only templates/servicemonitor.yaml > "$work_dir/servicemonitor.yaml"
grep -Eq '^kind: ServiceMonitor$' "$work_dir/servicemonitor.yaml" || fail "ServiceMonitor not rendered"
grep -Eq '^    release: x$' "$work_dir/servicemonitor.yaml" || fail "ServiceMonitor labels not merged"
grep -q 'scrapeNativeHistograms' "$work_dir/servicemonitor.yaml" &&
  fail "ServiceMonitor sets scrapeNativeHistograms by default"
helm template pinglens-exporter "$chart_dir" -f "$tests_dir/values-targets.yaml" \
  --set serviceMonitor.enabled=true,serviceMonitor.scrapeNativeHistograms=true \
  --show-only templates/servicemonitor.yaml | grep -Eq '^  scrapeNativeHistograms: true$' ||
  fail "ServiceMonitor does not set scrapeNativeHistograms when enabled"

# hosts ARGS...: the target hosts rendered with the test values plus ARGS.
hosts() {
  helm template pinglens-exporter "$chart_dir" -f "$tests_dir/values-targets.yaml" \
    --show-only templates/configmap.yaml "$@" |
    sed -nE 's/^ +- host: "(.*)"$/\1/p' | sort | tr '\n' ' '
}
[ "$(hosts --set-json 'excludeTargets=[{"ping_group":"customer"}]')" = "1.1.1.1 8.8.8.8 " ] ||
  fail "excludeTargets did not skip a group by label"
[ "$(hosts --set-json 'excludeTargets=[{"ping_group":"customer","asn":"1"}]')" = "1.1.1.1 192.0.2.10 8.8.8.8 " ] ||
  fail "excludeTargets skipped a target matching only some labels"
[ "$(hosts --set-json 'excludeTargets=[{"host":"8.8.8.8"},{"endpoint_name":"Cloudflare DNS"}]')" = "192.0.2.10 " ] ||
  fail "excludeTargets did not skip by host or by any entry"
render_fails "pinglens-exporter: no targets configured" -f "$tests_dir/values-targets.yaml" \
  --set-json 'excludeTargets=[{"group_title":"Customer"},{"public":"true"}]' ||
  fail "rendered with every target excluded"
render_fails "excludeTargets entries need at least one label" -f "$tests_dir/values-targets.yaml" \
  --set-json 'excludeTargets=[{}]' ||
  fail "rendered with an empty excludeTargets entry"

if [ -n "$output" ]; then
  cat "$targets" > "$output"
fi
