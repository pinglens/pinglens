#!/bin/sh
# Builds the mixin's dashboards in both cluster modes and checks the queries.
# Usage: mixin/test.sh [OUTPUT_DIR]  (needs jsonnet, jsonnetfmt and jq)
set -eu

mixin_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
output=${1:-}
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

jsonnetfmt --test "$mixin_dir"/*.libsonnet "$mixin_dir"/*.jsonnet "$mixin_dir"/dashboards/*.libsonnet ||
  fail "jsonnet files are not formatted; run jsonnetfmt -i"

# build CONFIG: the trace dashboard with CONFIG merged into _config.
build() {
  jsonnet -J "$mixin_dir" -e "((import 'mixin.libsonnet') + { _config+:: $1 }).grafanaDashboards['pinglens-trace.json']"
}
# strings FILE: every string in the dashboard, one per line.
strings() {
  jq -r '.. | strings' "$1"
}

build '{}' > "$work_dir/single.json"
strings "$work_dir/single.json" | grep -q 'cluster' &&
  fail "single-cluster dashboard still selects a cluster"
jq -e '[.templating.list[].name] == ["datasource", "group", "target", "ttl"]' "$work_dir/single.json" > /dev/null ||
  fail "single-cluster dashboard variables are not datasource, group, target, ttl"
strings "$work_dir/single.json" | grep -Fq 'pinglens_trace_hop_info))' ||
  fail "single-cluster dashboard did not drop a selector that only held the cluster"

build '{ showMultiCluster: true, clusterLabel: "k8s_cluster", datasourceRegex: "/^prom$/" }' > "$work_dir/multi.json"
strings "$work_dir/multi.json" | grep -Eq '(^|[^_])cluster="' &&
  fail "multi-cluster dashboard still uses the cluster label"
# count PATTERN FILE: occurrences of the fixed string PATTERN in FILE's strings.
count() {
  strings "$2" | grep -oF "$1" | wc -l
}
[ "$(count 'k8s_cluster="$cluster"' "$work_dir/multi.json")" -eq "$(count 'cluster="$cluster"' "$mixin_dir/dashboards/pinglens-trace.json")" ] ||
  fail "multi-cluster queries do not all select k8s_cluster"
jq -e '.templating.list[] | select(.name == "cluster") | .query.query == "label_values(pinglens_trace_target_info, k8s_cluster)"' "$work_dir/multi.json" > /dev/null ||
  fail "cluster variable does not list k8s_cluster values"
jq -e '.templating.list[] | select(.name == "datasource") | .regex == "/^prom$/"' "$work_dir/multi.json" > /dev/null ||
  fail "datasource regex not applied"

if [ -n "$output" ]; then
  mkdir -p "$output"
  jsonnet -J "$mixin_dir" -m "$output" "$mixin_dir/dashboards.jsonnet" > /dev/null
fi
