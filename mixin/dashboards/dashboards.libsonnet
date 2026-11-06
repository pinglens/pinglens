// The dashboard source is plain Grafana JSON, written for a multi-cluster
// setup with the cluster label "cluster"; the config adapts its queries.
local source = import 'pinglens-trace.json';

local mapStrings(f, x) =
  if std.isString(x) then f(x)
  else if std.isArray(x) then std.map(function(e) mapStrings(f, e), x)
  else if std.isObject(x) then std.mapWithKey(function(_, v) mapStrings(f, v), x)
  else x;

local replaceAll(s, pairs) = std.foldl(function(acc, p) std.strReplace(acc, p[0], p[1]), pairs, s);

{
  local config = $._config,

  local queries =
    if config.showMultiCluster then [
      ['cluster="$cluster"', '%s="$cluster"' % config.clusterLabel],
      [', cluster)', ', %s)' % config.clusterLabel],
    ] else [
      ['{cluster="$cluster", ', '{'],
      ['{cluster="$cluster"}', ''],
    ],

  local adapt(dashboard) =
    mapStrings(function(s) replaceAll(s, queries), dashboard) + {
      tags: config.dashboardTags,
      templating+: {
        list: [
          if variable.name == 'datasource' then variable { regex: config.datasourceRegex } else variable
          for variable in super.list
          if config.showMultiCluster || variable.name != 'cluster'
        ],
      },
    },

  grafanaDashboards+:: {
    'pinglens-trace.json': adapt(source),
  },
}
