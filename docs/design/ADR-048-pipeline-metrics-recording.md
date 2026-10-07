# ADR-048: Pipeline metrics recorded during the run (leader-only, trace-tagged)

- **Status:** accepted
- **Date:** 2026-10-05
- **Deciders:** dagger-kubernetes maintainers

## Context

The pipeline view's "Engine metrics" card (`GET /api/v1/traces/:id/metrics`,
ADR-037) computed its series **at page-display time** by re-querying cAdvisor
`container_*` metrics over the trace window. That fails for real runs:

- `rate(...[5m])` needs at least two cAdvisor samples in the lookback; a short
  pipeline has 0–1 scrapes, so cpu/disk/net stay empty.
- After the run the engine StatefulSet scales down (~310s idle TTL), so a later
  page load has no live pod to query.
- Nothing sampled or stored metrics **during** the run.

The Runners page (`GET /api/v1/fleet/:version/metrics`, ADR-042) had a related
promptness problem: the same `[5m]` rate window meant charts stayed empty until
the lookback filled, and `MetricChart.vue` rendered `no data` for a single
sample even though the header already showed its value.

`MetricsClient.DeleteTraceSeries` already deletes `{trace_id="..."}` on history
purge, but no code wrote metrics carrying `trace_id`: the OTel collector's
`prometheusremotewrite` promotes `trace_id` only for **logs**, and cAdvisor is
scraped by VictoriaMetrics directly.

## Decision

1. **Record during the run.** A leader-only `TraceMetricsRecorder` samples every
   running trace every `pipeline.metrics.record_interval` (default `15s`) and
   writes the six cAdvisor-derived series into VictoriaMetrics under distinct
   `dagger_engine_*` metric names tagged
   `{trace_id="<id>",version="<version>"}`. Distinct names keep the fleet
   queries (which select `container_*`) from double-counting recorded samples.
2. **Read recorded-first.** `EngineMetricsService.TraceMetrics` reads
   `dagger_engine_*{trace_id="..."}` over the trace window first and only falls
   back to the live cAdvisor query when nothing was recorded. The response shape
   and status codes are unchanged.
3. **Durable + shared.** Writing to VictoriaMetrics (not the Raft FSM, not
   in-memory) means samples survive engine scale-down and supervisor
   restart/failover, and any of the three replicas can serve the endpoint.
   Cleanup reuses the existing `DeleteTraceSeries` `{trace_id="..."}` matcher.
4. **Bounded.** Recording is "only while running", capped per trace by
   `pipeline.metrics.max_record_window` (default `24h`), and bounded by
   VictoriaMetrics retention. Points per trace ≈
   `max_record_window / record_interval` (≈5760/metric at 24h @ 15s).
5. **Shorter rate window.** `defaultMetricQueries` is parameterized with a
   `{rate}` placeholder; `pipeline.metrics.rate_window` (default `1m`, down from
   `5m`) makes charts appear promptly after an engine starts. `MetricChart.vue`
   renders a single sample as a centered dot instead of `no data`.
6. **Safety.** The recorder validates the trace id (`ValidTraceID`, hex — so it
   can always be cleaned), the version (`safeVersionRe`), and escapes the
   namespace/pod (`escapePromQLLabelValue`); recorded metric names come from the
   fixed `defaultMetricQueries` allowlist. `/api/v1/import` is cluster-internal
   VictoriaMetrics only.

## Alternatives considered

- **FSM recorder** — rejected: Raft log churn plus snapshot/memory bloat for
  transient telemetry.
- **In-memory recorder** — rejected: invisible to follower-served reads, lost on
  restart/failover.
- **Query-time fixes alone (Option D)** — rejected: cannot reconstruct metrics
  after scale-down or for short runs.
- **Dagger-native engine metrics as the pipeline-view source** — rejected for
  now: Dagger's observability is trace-first; the engine's own Prometheus
  metrics are process-level BuildKit/dagql gauges behind an experimental env
  var, are not per-trace, and carry no correlation label. OTLP metrics are
  exported but the collector promotes `trace_id` only for logs, and Dagger's
  metrics carry no trace/pipeline id to promote. They are also a different
  signal than the card's six cAdvisor resource series. Revisit only if a
  supported engine emits per-run-scoped OTLP metrics with a stable correlation
  label; then add a collector `transform/metrics` promotion (mirroring the logs
  `trace_id` promotion) and point the recorded-read query at the new names —
  `DeleteTraceSeries` already covers cleanup.

## Consequences

- New config keys `pipeline.metrics.rate_window`, `record_interval`,
  `max_record_window` (all required `> 0` when `pipeline.metrics.enabled`).
- New `domain.TraceMetricsRecorderStore` (implemented by `MetricsClient` via
  `QueryInstant` + `WriteSamples`) and `TraceMetaRepository.ListRunning`.
- The recorder is leader-only, so a leader failover leaves a brief sampling gap;
  no duplicate samples across replicas.
- A disabled backend still returns `501`; a missing `trace_meta` or no data
  still returns an empty-but-valid `200`.

## Cross-references

- ADR-037 — the trace-scoped metrics endpoint this extends.
- ADR-042 — the fleet metrics endpoint that shares the `{rate}` window.
- ADR-018 — history purge, which deletes recorded series via `DeleteTraceSeries`.
