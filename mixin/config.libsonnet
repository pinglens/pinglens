{
  _config+:: {
    // With showMultiCluster the dashboard selects one cluster through the
    // clusterLabel label, which the scraper adds to every series (for example
    // as a Prometheus external label). Without it the dashboard has no
    // cluster selector and reads every pinglens-exporter series.
    showMultiCluster: false,
    clusterLabel: 'cluster',

    // Regex limiting the data source picker; empty lists every Prometheus
    // data source.
    datasourceRegex: '',

    dashboardTags: ['pinglens'],
  },
}
