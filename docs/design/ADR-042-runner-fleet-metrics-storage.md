# ADR-042: Runner-fleet metrics + storage usage (cAdvisor, 15m rolling window)

- **Status:** accepted
- **Date:** 2026-09-24
- **Deciders:** dagger-kubernetes maintainers

## Context

Issue #33 asks the Runners page to display per-version engine metrics "like the
pipeline view", plus the current storage used percent and used size in human
units. The pipeline view already has a trace-scoped metrics endpoint
(ADR-037): cAdvisor `container_*` series scraped into VictoriaMetrics by the
subchart's `kubernetes-nodes-cadvisor` job, queried server-side through
`MetricsClient.QueryRange`. The fleet page has no trace (and therefore no
trace time window), and the Kubernetes PVC API exposes `capacity` but never
**used** bytes.

## Decision

1. **Reuse the cAdvisor data path.** `GET /api/v1/fleet/:version/metrics`
   (auth-gated, any authenticated user) serves the same six curated engine
   series as the trace endpoint (CPU, memory, disk read/write, network rx/tx),
   selected by
   `{namespace="<fleet.namespace>",pod=~"<StsName(version)>-.*",container="engine"}`
   and summed across replicas.
2. **Rolling window.** With no trace window on the fleet page, the window is
   `[now - 15m, now]`, `step` = `pipeline.metrics.step`. The endpoint shares
   `pipeline.metrics.enabled` as its feature flag — no new config key.
3. **Storage from `container_fs_*`.** Used = last point of
   `container_fs_usage_bytes`; capacity = last point of
   `container_fs_limit_bytes`. When capacity is absent/0 it falls back to
   `fleet.engine_storage_size × replicas` (parsed once at startup with
   `resource.ParseQuantity`); when still unknown, `percent = -1` (UI renders
   `-`). `storage` is omitted entirely when neither value is known.
4. **Tolerant semantics.** Malformed `:version` → `400`; disabled service →
   `501`; unknown-but-well-formed version or a backend failure → empty-but-
   valid payload (HTTP 200), mirroring the trace-metrics endpoint. Version
   strings are validated with `safeVersionRe` before PromQL interpolation
   (CWE-89/CWE-943).

## Alternatives considered

- **kubelet `kubelet_volume_stats_used_bytes`/`_capacity_bytes`** — rejected:
  it is the canonical source for PVC used bytes, but it needs a **second**
  kubelet `/metrics` scrape job plus its own RBAC beyond the existing
  cAdvisor-only job, doubling the chart surface for one card. The cAdvisor
  `container_fs_*` series already reflect the mounted engine PVC device.
- **Kubernetes PVC API** — rejected: `PVC.Status.capacity` exists, used bytes
  do not.
- **Per-pod storage/metric breakdown** — deferred; fleet aggregate first,
  consistent with ADR-037's "fleet aggregate, per-pod later".

## Consequences

- One new service (`FleetMetricsService`), one new auth-gated endpoint, and a
  metrics grid + storage card on the Runners page; no charting dependency, no
  new scrape job, no new config key, no Helm values change.
- The `pipeline.metrics.*` config now gates both metrics endpoints (documented
  in `docs/README.md` and `config/config.app.yaml.sample`).
- If cAdvisor does not emit `container_fs_limit_bytes` for the engine PVC on a
  given cluster, the storage card still shows used bytes and the
  `engine_storage_size × replicas` fallback percent (verify once live via
  `GET /api/v1/fleet/<version>/metrics`).

## Cross-references

- ADR-037 — the trace-scoped metrics path this endpoint mirrors.
