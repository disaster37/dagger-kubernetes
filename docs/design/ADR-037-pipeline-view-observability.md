# ADR-037: Pipeline-view observability — internal-span filtering, service detection, exec logs, and trace-scoped engine metrics

- **Status:** accepted
- **Date:** 2026-09-17
- **Deciders:** dagger-kubernetes maintainers

## Context

Five pipeline-view defects were reported (issues #9, #10, #11, #12, #15):

- **#9** — the step tree and expanded steps were flooded with internal engine
  transport spans (`POST /query`, `GET /blobs`, `connect`, …) and their empty
  logs. The CI step builder already classifies these by span **name**
  (`internalSpanPrefixes`/`internalSpanExact`, ADR-024), but the UI only
  filtered on `dagger.io/ui.*` boolean attributes.
- **#10** — the Services summary was empty even when services existed. Detection
  was log-signal-only (`body === "tunnel started"` or a flat
  `attributes.http_url`/`https_url`), which did not match what the installed
  engine emits.
- **#11** — `exec`/Dockerfile `RUN` stdout/stderr was missing. `logText`
  over-filtered stdio records, and hidden/passthrough spans' logs were not
  reliably attributed to a visible ancestor.
- **#12** — there was no engine resource metrics surface. The only PromQL
  access was the raw `/api/v1/metrics` proxy, which is not trace-scoped and not
  consumed by the UI; VictoriaMetrics received only aggregate OTLP metrics with
  no pod-level CPU/memory/disk/network.
- **#15** — large OTLP log batches were rejected with `413` by nginx's 1 MiB
  default, the supervisor's 4 MiB control-API cap, or the collector's default.

## Decision

### 1. Internal-span filtering lives in the UI, by name (aligned with ADR-024)

The pipeline UI ports the CI builder's name rules
(`internalSpanPrefixes`/`internalSpanExact`) into `isInternalSpanName`, and
folds internal/name-internal spans out of both the top-level step walk and the
sub-span walk. Filtering stays client-side because span visibility already
lives there (the `dagger.io/ui.*` tree); doing it server-side would require
correlating Loki logs with Tempo span names for no gain. The two rule sets are
kept in sync manually and documented as a single source of truth for
internal-span names.

### 2. Log ownership: transparent spans route to the nearest visible ancestor

A log's owner is resolved once per tree:

- a **visible** span owns its own logs;
- a **passthrough/encapsulated** span is transparent — its logs belong to the
  nearest visible ancestor (so `RUN`/exec output stays under the build step);
- an **internal/name-internal** span is noise — its logs are dropped.

Children of a hidden span inherit the same owner, so user-relevant children of
an internal span still surface. Logs whose span is absent from the tree remain
in the "unmatched" section. `logText` renders content-bearing stdio records
(stripping only the stream prefix/ANSI) and drops only empty/whitespace-only
records.

### 3. Service detection is span-identity + lenient log parsing

A span is a service when its name matches the tunable `SERVICE_SPAN_NAMES` set
(`up`, `host.tunnel`, `Service.Up`, `service.Up`) **or** it carries a
`tunnel started` log / non-empty `http_url`/`https_url` attribute. OTLP
attributes are normalized whether they arrive flat or as OTLP `AnyValue`
objects (`{stringValue}` / `{intValue}` / `{doubleValue}`). The constants are
isolated for one-line tuning after a live-trace inspection.

### 4. Trace-scoped engine metrics: cAdvisor → VictoriaMetrics → curated endpoint

**Data path.** kubelet **cAdvisor** `container_*` metrics are scraped into
VictoriaMetrics by the subchart's default `kubernetes-nodes-cadvisor` job
(enabled via `victoria.server.scrape.enabled`; the subchart's ClusterRole grants
`nodes/metrics`). No extra scrape job is added: the default job already covers
kubelet `/metrics/cadvisor`, and a duplicate job would double every summed
series. The Dagger engine's own OTLP metrics are BuildKit/engine aggregates and
do not include the engine pod's CPU/memory/disk/network, so cAdvisor is the
canonical source.

**Query path.** A new, auth-gated `GET /api/v1/traces/:traceID/metrics`
endpoint builds scoped PromQL server-side and queries VictoriaMetrics; the UI
never writes PromQL. The raw `/api/v1/metrics` proxy is unchanged for
operators.

**Scoping.** By time window + engine pod label selector derived from
`trace_meta`: window `[started_at, started_at+duration_ms]` (fallback last 24h,
bounded to 24h), selector
`{namespace="<fleet.namespace>",pod=~"<stsName>-.*",container="engine"}` where
`stsName = domain.StsName(version)`. Multiple replicas are summed (fleet
aggregate). A missing `trace_meta` or absent data yields an empty-but-valid
payload (HTTP 200); a disabled/unconfigured backend returns `501`.

**Charting.** A hand-rolled SVG component (`MetricChart.vue`) — no charting
dependency is added for four small time-series panels.

### 5. OTLP ingest body size is a dedicated, configurable cap

`otel.ingest_max_body_size` (default 64 MiB) is separate from the 4 MiB
control-API cap. The chart raises the nginx `proxy-body-size` annotation and the
collector's `max_request_body_size` to match. The control-API cap is unchanged.

## Alternatives considered

- **Server-side internal-span filtering.** Rejected: requires a second
  dependency between the log and trace stores, and the UI already owns span
  visibility.
- **Adding a charting library.** Rejected: four small panels do not justify a
  heavy dependency; the SVG component is ~100 lines.
- **Per-replica metric breakdown.** Rejected for now: the fleet aggregate is
  what the pipeline view needs; per-pod breakdown can be added later.
- **Auto-discovering metric names.** Rejected: PromQL expressions are constants,
  tunable in code after a live-cluster inspection.
- **Raising the control-API `maxControlBody`.** Rejected: it would widen the
  unauthenticated DoS surface on public endpoints; OTLP gets its own cap.

## Consequences

- The pipeline UI matches the CI step view (same internal-span name rules).
- `RUN`/exec output is preserved while genuine internal noise is removed.
- Engine resource charts work without a new dependency; the endpoint is
  tolerant of missing cAdvisor data.
- The cAdvisor metric names/labels and the VictoriaMetrics subchart scrape key
  shape are isolated constants/config, verified against the installed chart
  (`victoria.server.scrape.enabled`, whose default config already scrapes
  kubelet cAdvisor) and tunable after a live inspection.
- The 4 MiB control-API cap is untouched; OTLP ingest is bounded by the
  collector's own limit for chunked bodies.

## Open questions

- Exact engine signal shape for `tunnel started` and stdio attribute nesting
  (constants isolated for one-line tuning).
- cAdvisor metric availability/labels on the target cluster (endpoint tolerant
  of empty data).
