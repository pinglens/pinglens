{
  _config+:: {
    // With showMultiCluster the dashboards select one cluster through the
    // clusterLabel label, which the scraper adds to every series (for example
    // as a Prometheus external label). Without it the dashboards have no
    // cluster selector and read every PingLens series.
    showMultiCluster: false,
    clusterLabel: 'cluster',

    // Regex limiting the data source picker; empty lists every Prometheus
    // data source.
    datasourceRegex: '',

    dashboardTags: ['pinglens'],

    // With showLogs the PingLens dashboard adds agent and coordinator log
    // panels from a Loki data source, selecting each component's streams
    // with these label matchers. The defaults match Kubernetes pod labels
    // mapped to Loki labels with underscores.
    showLogs: false,
    logSelectors: {
      agent: 'app_kubernetes_io_name="pinglens", app_kubernetes_io_component="agent"',
      coordinator: 'app_kubernetes_io_name="pinglens", app_kubernetes_io_component="coordinator"',
    },
  },
}
