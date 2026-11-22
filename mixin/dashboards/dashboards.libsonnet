// The dashboard sources are plain Grafana JSON, written for a multi-cluster
// setup with the cluster label "cluster"; the config adapts their queries.
// pinglens.json selects log streams with the AGENT_SELECTOR and
// COORDINATOR_SELECTOR placeholders, which the config's logSelectors fill.
local trace = import 'pinglens-trace.json';
local app = import 'pinglens.json';

local mapStrings(f, x) =
  if std.isString(x) then f(x)
  else if std.isArray(x) then std.map(function(e) mapStrings(f, e), x)
  else if std.isObject(x) then std.mapWithKey(function(_, v) mapStrings(f, v), x)
  else x;

local replaceAll(s, pairs) = std.foldl(function(acc, p) std.strReplace(acc, p[0], p[1]), pairs, s);

{
  local config = $._config,

  local queries =
    [
      ['AGENT_SELECTOR', config.logSelectors.agent],
      ['COORDINATOR_SELECTOR', config.logSelectors.coordinator],
    ] +
    if config.showMultiCluster then [
      ['cluster="$cluster"', '%s="$cluster"' % config.clusterLabel],
      ['cluster=~"$log_cluster"', '%s=~"$log_cluster"' % config.clusterLabel],
      [', cluster)', ', %s)' % config.clusterLabel],
    ] else [
      ['{cluster="$cluster", ', '{'],
      ['{cluster="$cluster"}', ''],
      ['{cluster=~"$log_cluster", ', '{'],
    ],

  local clusterVariables = ['cluster', 'log_cluster'],
  local logVariables = ['log_datasource', 'log_cluster'],

  local adapt(dashboard) =
    mapStrings(function(s) replaceAll(s, queries), dashboard) + {
      tags: config.dashboardTags,
      panels: [
        panel
        for panel in super.panels
        if config.showLogs || panel.type != 'logs'
      ],
      templating+: {
        list: [
          if variable.name == 'datasource' then variable { regex: config.datasourceRegex } else variable
          for variable in super.list
          if (config.showMultiCluster || !std.member(clusterVariables, variable.name)) &&
             (config.showLogs || !std.member(logVariables, variable.name))
        ],
      },
    },

  grafanaDashboards+:: {
    'pinglens-trace.json': adapt(trace),
    'pinglens.json': adapt(app),
  },
}
