# Querying

Timbala does not provide a user interface for querying; you are expected to
use the API or a third-party tool such as [Grafana][] to view your data.

## Grafana

You can use Timbala as a datasource in [Grafana][] by adding a new
'Prometheus' datasource that points to Timbala.

Follow the [Prometheus documentation for Grafana][].

[Grafana]: https://grafana.com/grafana
[Prometheus documentation for Grafana]: https://prometheus.io/docs/visualization/grafana/#creating-a-prometheus-data-source

## Query language

Timbala supports PromQL, as used by Prometheus. Please see the [Prometheus
query documentation][] for documentation on how PromQL works.

Timbala makes no changes or additions to PromQL, so all queries that work in
Prometheus will work in Timbala and vice-versa.

[Prometheus query documentation]: https://prometheus.io/docs/querying/basics/

## HTTP API

Timbala has a HTTP API that is compatible with the [Prometheus v1 API][]. See the
[Prometheus v1 API][] documentation to learn how to use it.

[Prometheus v1 API]: https://prometheus.io/docs/querying/api/

## 'Remote read' integration with Prometheus

Support is [planned][] for a '[remote read][]' endpoint, allowing Prometheus to
use Timbala as a storage backend for queries.

[planned]: https://github.com/mattbostock/timbala/issues/43
[remote read]: https://prometheus.io/docs/operating/configuration/#<remote_read>
