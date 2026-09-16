# ADR-033: Local image cache (Zot on-demand mirror)

**Status:** Accepted  
**Date:** 2026-09-15  
**Supersedes:** the CNCF Distribution (`registry:3`) design originally recorded in this ADR.

## Context

Engine pods pull container images from public registries (Docker Hub, GHCR,
Quay, GCR, public ECR, `registry.dagger.io`) on every uncached layer. In a
self-hosted fleet those pulls:

- consume upstream rate limits (Docker Hub anonymous pulls especially),
- fail or degrade when the upstream registry is temporarily unreachable, and
- travel over the internet even when the same image was just pulled by a
  sibling node.

The platform already ships an in-cluster **MinIO/S3** store (ADR-006, ADR-012)
and a generated `engine.toml` (ADR-011) that the Dagger engine reads for
registry mirrors. What was missing was a registry **inside the cluster** that
caches upstream blobs once and serves them to every engine pod.

The operator decision was explicit: add a **local image cache / registry
mirror** (enable/disable via Helm), backed by the shared MinIO/S3, wired into
the engine's `engine.toml` registry mirrors, with presets for the major
registries plus custom/private registries.

This ADR records the mechanism decision (D3) and the `http = true` marking
decision (D4) from the plan. It is unrelated to the removed worker-cache sync:
the image cache caches **container image layers**, not BuildKit's local worker
directory.

## Decision

### D3 — One Zot on-demand mirror per upstream, S3-backed

Run **Zot** (`ghcr.io/project-zot/zot:v2.1.21`) as an OCI registry with
`extensions.sync` **on-demand pull-through** (`onDemand: true`): one
`Deployment` + one `<name>-zot-config` ConfigMap + one `Service` (port `5000`)
per enabled upstream registry, storage on the shared MinIO/S3 (or a per-mirror
PVC).

- **One instance per upstream.** Each Zot mirror syncs a single upstream with
  no `destination`/`stripPrefix` rewriting, so the local repo path maps 1:1 to
  the upstream path (transparent BuildKit pull-through). A shared Service would
  need a host-routing L7 component.
- **Addressing is deterministic:** mirror address =
  `<release>-<slug>-mirror.<namespace>.svc:5000`, where `slug` is the upstream
  host lowercased with every run of non-`[a-z0-9]` characters collapsed to a
  single `-` (e.g. `docker.io` → `docker-io`). The address maps 1:1 to one
  `engine.toml` mirror entry.
- **Configuration is a JSON file** (`config.json`, chart-rendered into the
  `<name>-zot-config` ConfigMap): `distSpecVersion`, `storage`
  (`rootDirectory`, `dedupe`, `gc`/`gcDelay`/`gcInterval`/optional
  `gcTimeWindow`, and a `storageDriver` for s3), `http` (`compat:
  ["docker2s2"]`), `log`, and `extensions.sync` (`enable`, `downloadDir`,
  `registries`). Zot reads a file, not env vars.
- **`dedupe` is off by default.** Zot rejects `storage.dedupe: true` with the
  S3 driver ("no remote database configured") unless a remote cache/DB (e.g.
  Redis) is configured; the chart sets `imageCache.dedupe: false` and does not
  render a remote cache. A dedupe-capable backend is a documented follow-up.
- **`extensions.sync.downloadDir` is always rendered** (`imageCache.sync.
  downloadDir`, default `/var/lib/registry/sync`) whenever sync is enabled.
  Zot requires a local staging directory with S3 storage; the path sits under
  the mirror's data volume — the S3 `storage.rootDirectory` emptyDir **or** the
  per-mirror PVC — so it is writable for both backends.
- **S3 storage** uses `storage.storageDriver` (`name: s3`) with
  `regionendpoint` = `<release>-minio.<namespace>.svc:9000`,
  `forcepathstyle=true`, `secure=false`, `rootdirectory=/<slug>`. Credentials
  are **not** embedded in the ConfigMap: Zot uses the AWS SDK default
  credential chain, fed by `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` env vars
  injected from the existing `engine-s3-auth` Secret. Cached blobs live under
  the `image-cache` bucket, namespaced per upstream by the root directory.
- **Digest fidelity:** `http.compat: ["docker2s2"]` + `preserveDigest: true`
  keep upstream Docker manifest digests (no OCI conversion / digest drift), so
  `@digest` pulls and the supervisor's prune-by-digest keep working.
- **Auth from the engine's perspective:** the in-cluster mirror is
  unauthenticated plaintext HTTP, so the engine needs no credential for it.
  Each mirror Service is ClusterIP-only (never exposed via ingress), so the
  trust boundary for those anonymous reads is the cluster network itself.
  `engine-image-auth` (`.dockerconfigjson`) stays for the kubelet's engine-image
  pull and any non-mirrored registry.
- **Private upstreams (follow-up):** Zot supports them via
  `extensions.sync.credentialsFile` (a mounted JSON Secret) or Docker
  credentials auto-discovery / `credentialHelper` (e.g. `ecr`). v1 renders no
  credentials file (the presets are public); wiring
  `imageCache.registries[].username/passwordSecretRef` is a documented
  follow-up.
- **Storage backends:** `s3` (default; shares the MinIO capacity, no new
  storage class) or `pvc` (a per-mirror `PersistentVolumeClaim` — a Deployment
  cannot use `volumeClaimTemplates`, so the claim is standalone and the
  Deployment switches to `strategy: Recreate` to avoid an RWO-remount deadlock
  on rollout; escape hatch for clusters without the MinIO subchart). Under
  `s3`, `storage.rootDirectory` is the local dedupe/metadata cache (an
  `emptyDir`, rebuilt by scanning S3 at startup); under `pvc` it is the blob
  store.
- **Invalidation is explicit — there is no TTL.** On-demand sync has no
  pull-through TTL: the first request for an uncached repo/tag fetches it from
  upstream and serves it; subsequent requests are served from cache. Pruning
  (ADR-034) unlinks a manifest, and the next request re-fetches.
  `pollInterval` is **not** set by default (and must not be used for Docker Hub:
  rate limits / no upstream catalog).
- **Online GC (the reason for the Zot swap):** Zot runs garbage collection
  **inline** — the registry stays up; there is no stop-the-world/read-only
  step. `storage.gc: true` + `gcDelay` (minimum age of an unreferenced blob,
  chart default `2h`) + `gcInterval` (scheduler frequency, chart default `1h`)
  reclaim blob bytes automatically after a manifest `DELETE`. `gcTimeWindow`
  optionally restricts GC to a daily off-peak window. `storage.dedupe` stays
  `false` because the S3 driver requires a remote DB; there is **no** offline
  `registry garbage-collect` procedure and **no** Helm CronJob.

#### Registry presets

| Helm preset key | `engine.toml` host | Zot `urls` | Default |
|---|---|---|---|
| `docker.io` | `docker.io` | `https://registry-1.docker.io` | enabled |
| `ghcr.io` | `ghcr.io` | `https://ghcr.io` | enabled |
| `public.ecr.aws` | `public.ecr.aws` | `https://public.ecr.aws` | enabled |
| `quay.io` | `quay.io` | `https://quay.io` | enabled |
| `gcr.io` | `gcr.io` | `https://gcr.io` | disabled |
| `registry.dagger.io` | `registry.dagger.io` | `https://registry.dagger.io` | disabled |

Private AWS ECR, GAR, Harbor, and any other upstream are handled by the
`imageCache.registries` list (explicit `host` + `remoteUrl` + optional
credentials), which also covers the "internal registry" case. The chart
derives the slug from `host`; a slug collision between a custom registry and a
preset (or another custom entry) fails at `helm template` time.

### D4 — `http = true` marking via `fleet.engine_registry_mirrors_http`

`engine.toml` uses BuildKit-style registry configuration. For a plaintext
in-cluster mirror, BuildKit requires the **mirror host itself** to be declared
with `http = true`, separately from the mirrored registry:

```toml
[registry."docker.io"]
  mirrors = ["my-release-docker-io-mirror.dagger-kubernetes.svc:5000"]

[registry."my-release-docker-io-mirror.dagger-kubernetes.svc:5000"]
  http = true
```

Decision: add a supervisor config key **`fleet.engine_registry_mirrors_http`**
(`[]string`, mirror `host[:port]` values dialed over plaintext HTTP).
`engineTOML.render()` emits a `[registry."<mirror>"]\n  http = true` section per
entry (sorted, deduped) after the existing mirror sections. The Helm chart
computes the list from the generated mirror addresses and merges the generated
mirrors into `fleet.engine_registry_mirrors`; user-provided values pass through
unchanged when the image cache is disabled. Marking is explicit — no
hostname-pattern magic — so a mirror that terminates TLS can omit the entry.

## Alternatives considered

- **CNCF Distribution v2/v3 (`registry:3`) pull-through proxy** — rejected:
  Distribution has **no online GC**. Reclaiming blob bytes after a manifest
  delete requires taking the registry offline and running `registry
  garbage-collect` (a stop-the-world maintenance window and a Helm CronJob/sidecar
  procedure), which is operationally heavy and not something the admin
  prune-all endpoint could trigger safely. The Zot swap keeps the same OCI
  Distribution v2 API surface (so the supervisor client, config, API, UI, and
  tests carry over) while making reclamation automatic and online.
- **Zot multi-upstream sync** — rejected: one Zot instance can sync several
  upstreams, but with no `destination` rewriting each upstream's `urls` would
  have to be served under an explicit path list; one Zot per upstream keeps the
  local repo path mapping 1:1 to the upstream path (transparent pull-through)
  and keeps the deterministic 1:1 mirror-address model.
- **Harbor proxy-cache projects** — rejected: requires the full Harbor stack
  (Postgres, Redis, registry, core, portal, jobservice) for a single caching
  concern; operationally heavy relative to Zot + the existing MinIO.
- **Single registry with multiple proxy instances behind one Service** —
  rejected: a shared Service would need a custom router keyed on the upstream
  host, i.e. a new component to build and operate for no gain over one Service
  per mirror.
- **Per-mirror PVC (only)** — not chosen as the default: it fragments cache
  capacity per upstream and adds one PVC per mirror. Kept as
  `imageCache.storage.backend: pvc`.

## Consequences

- New Helm section `imageCache.*` (disabled by default) and a rewritten
  `templates/image-cache.yaml` rendering one Deployment + `<name>-zot-config`
  ConfigMap + Service (+ optional PVC) per enabled upstream.
- `minio.buckets` gains `image-cache`; the chart's `_helpers.tpl` keeps
  `imageCacheRegistries`, `imageCacheMirrorAddress`, `imageCacheMirrorHosts`,
  `imageCacheS3Endpoint`, and `engineRegistryMirrors` (merged with the user
  map).
- The supervisor gains `fleet.engine_registry_mirrors_http` and the read-only,
  chart-rendered `image_cache.mirrors` block (ADR-034) used by the admin image
  cache page.
- **Degraded mode:** if a mirror is down, the engine fails that pull; BuildKit
  does not silently fall back to the upstream for a configured mirror.
- **Scope:** `engine.toml` mirrors cover the images pipelines pull
  (`container from`, `with-exec`, etc.). The **engine image** itself is pulled
  by the kubelet before the pod starts and is not routed through the mirror —
  that remains `fleet.engine_image_registry` + `engine-image-auth`.
- Enabling the cache does not touch existing engine caches or PVCs; disabling
  it removes the mirror Deployments/Services and restores the raw upstream
  behavior (cached blobs remain in the bucket until deleted).
- **Migration:** the Distribution mirror was never enabled on the live cluster
  (`imageCache.enabled` defaults to `false`), so the swap is a pure
  chart/template change with no cache data to migrate.
