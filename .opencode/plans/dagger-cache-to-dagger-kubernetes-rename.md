# Plan: Rename `dagger-cache` → `dagger-kubernetes` (project identity)

## Objective

Rename EVERY remaining project-identity reference to `dagger-cache` (all casing
variants: `dagger-cache`, `dagger_cache`, `DaggerCache`, `DAGGER_CACHE`,
`Dagger Cache`, `dagger.cache`) to the equivalent `dagger-kubernetes` form, across
source, Go identifiers, strings, configs, Helm chart, docs, scripts, Makefiles,
Dockerfiles, CI configs, k8s manifests, image names, binary names, CLI text, error
messages, log fields, test fixtures, and generated files.

Cache-FEATURE references (the remote shared build cache: OCI cache repo path,
BuildKit cache config, cache proxy, cache stats, cache GC, etc.) KEEP `cache`.

The Go module is already `github.com/disaster/dagger-kubernetes` (`go.mod`); the
Helm chart is already `dagger-kubernetes`; the container image is already
`docker.io/disaster/dagger-kubernetes` / `ghcr.io/disaster37/dagger-kubernetes`.
This plan covers the residual `dagger-cache` strings.

## Classification rule (A = rename, B = keep)

- **A — project identity → rename to `dagger-kubernetes`:** the platform/app
  itself: module path, binary name, image name, Helm chart/release name, k8s
  namespace + resource labels/selectors, the viper env prefix `DAGGER_CACHE_`
  and every env var built on it (incl. `DAGGER_CACHE_TOKEN`), filesystem paths
  owned by the app (`/etc/dagger-cache`, `/var/lib/dagger-cache`), app config
  YAML keys whose value is a project-identity default (namespace, raft CA org,
  minting CA subject), app metric namespace prefix `dagger_cache_`, UI
  localStorage keys `dagger_cache_*`, the `~/.dagger-cache.env` user file, the
  local Dagger CI module name + `DaggerCache` type, the GHA action name, the
  Drone plugin name/image, the Jenkins shared-library name + `daggerCache`
  function, the GitHub OAuth `User-Agent`, ADR author/team, docs titles, the
  internal cache-proxy headers `X-Dagger-Cache-*`, the `dagger_cache_backend_id`
  context key, the `volumeDaggerCache` k8s volume name, and the docker-compose
  network name.
- **B — cache feature → KEEP `cache`:** the OCI repository path segment
  `dagger-cache` inside cache refs (e.g. `cache.reg/dagger-cache`,
  `<cachePublicHost>/dagger-cache`, `localhost:5000/dagger-cache`,
  `dagger-cache-test-registry:5000/dagger-cache`) — this is the BuildKit cache
  blob repo path, fixed by the Helm `_helpers.tpl` and stored in existing
  registries; the Dagger CLI env var `_EXPERIMENTAL_DAGGER_CACHE_CONFIG` (a
  third-party Dagger CLI contract, NOT ours); the `Cache` Go type and
  `cache.*` config keys / `Cache` struct fields (cache backend config); all
  `cache.reg/...` example values that are cache-registry refs; the
  `cache_proxy` / `cache_routes` / `cache_stats` / `registry_router` /
  `registry_client` test fixtures that use `dagger-cache` as the OCI repo name
  (the cache blob repo); the `CACHE_REGISTRY` shell var; and Dagger CLI env
  vars `DAGGER_CLOUD_URL`, `DAGGER_CLOUD_TOKEN`, `DAGGER_TAG`,
  `_EXPERIMENTAL_DAGGER_TAG`, `_EXPERIMENTAL_DAGGER_RUNNER_HOST`.

### Concrete examples found in the repo

- **A:** `v.SetEnvPrefix("DAGGER_CACHE")` → `DAGGER_KUBERNETES`; binary
  `dagger-cache-ci` → `dagger-kubernetes-ci`; namespace default `dagger-cache`
  → `dagger-kubernetes`; `/etc/dagger-cache/tokens` →
  `/etc/dagger-kubernetes/tokens`; `dagger_cache_engine_acquire_total` →
  `dagger_kubernetes_engine_acquire_total`; `DaggerCache` struct →
  `DaggerKubernetes`; `X-Dagger-Cache-Target` → `X-Dagger-Kubernetes-Target`;
  `~/.dagger-cache.env` → `~/.dagger-kubernetes.env`.
- **B:** `cache.reg/dagger-cache` (OCI repo path) KEEP; `_EXPERIMENTAL_DAGGER_CACHE_CONFIG`
  KEEP; `Cache{Registry: "cache.reg/dagger-cache"}` — the `Cache` type and the
  repo path KEEP; test paths `/v2/dagger-cache/manifests/v0-21-4` (OCI repo
  `dagger-cache`) KEEP.

## Decisions (resolved with user)

1. **Full breaking rename** of all project-identity deployed/env contracts
   (env prefix, filesystem paths, k8s namespace, binary name, helm release name,
   Prometheus metric prefix, UI localStorage keys). Migration/rollout note
   included below.
2. `~/.dagger-cache.env` → `~/.dagger-kubernetes.env` (UI + rebuild ui-dist).
3. Dagger CI module `dagger-cache` → `dagger-kubernetes`; `DaggerCache` →
   `DaggerKubernetes`; **regenerate** `dagger/dagger.gen.go` via `dagger develop`.
4. `X-Dagger-Cache-Target/User/Pass` → `X-Dagger-Kubernetes-Target/User/Pass`
   (project-identity, rename).
5. Jenkins `daggerCache(...)` → `daggerKubernetes(...)`; `@Library('dagger-cache')`
   → `@Library('dagger-kubernetes')`.

## Inventory table (file → occurrences → replacement)

> Paths are repo-relative. "A" = rename to dagger-kubernetes form; "B" = KEEP
> (cache feature). For A items the replacement is the dagger-kubernetes form
> (same casing/separator as the original). Generated/build artifacts
> (`ui/dist/`, `internal/handler/ui-dist/`, `coverage.out`, `out/`, root
> `supervisor` + `dagger-cache-ci` binaries, `dagger/dagger.gen.go`) are
> regenerated, not hand-edited.

### Go source — config & entry points

| File | Line(s) | Current | Class | Replacement |
|---|---|---|---|---|
| `config/loader.go` | 21 | `v.SetEnvPrefix("DAGGER_CACHE")` | A | `DAGGER_KUBERNETES` |
| `config/loader.go` | 31 | `"/etc/dagger-cache/tokens"` | A | `/etc/dagger-kubernetes/tokens` |
| `config/loader.go` | 54 | `"/var/lib/dagger-cache"` | A | `/var/lib/dagger-kubernetes` |
| `config/loader.go` | 73 | `"dagger-cache-raft"` | A | `dagger-kubernetes-raft` |
| `config/loader.go` | 87 | `"cache.reg/dagger-cache"` | B | KEEP (OCI cache repo path) |
| `config/loader.go` | 111 | `"dagger-cache"` (fleet.namespace) | A | `dagger-kubernetes` |
| `config/loader.go` | 145 | `"/var/lib/dagger-cache/ca"` | A | `/var/lib/dagger-kubernetes/ca` |
| `config/loader.go` | 146 | `"/etc/dagger-cache/tls/tls.crt"` | A | `/etc/dagger-kubernetes/tls/tls.crt` |
| `config/loader.go` | 147 | `"/etc/dagger-cache/tls/tls.key"` | A | `/etc/dagger-kubernetes/tls/tls.key` |
| `cmd/api/main.go` | 42 | `Usage: "dagger-cache control plane"` | A | `dagger-kubernetes control plane` |
| `cmd/api/main.go` | 94 | `"dagger-cache supervisor starting"` | A | `dagger-kubernetes supervisor starting` |
| `cmd/api/main.go` | 960 | comment `/var/lib/dagger-cache/ca under /var/lib/dagger-cache` | A | `/var/lib/dagger-kubernetes/...` |
| `cmd/api/main.go` | 1005,1007 | `DAGGER_CACHE_TOKEN` (reserved env) | A | `DAGGER_KUBERNETES_TOKEN` |
| `cmd/api/main.go` | 1069 | comment `"cache.reg/dagger-cache" -> "cache.reg"` | B | KEEP (cache repo path example) |
| `cmd/api/main.go` | 1161 | `namespace = "dagger-cache"` | A | `dagger-kubernetes` |
| `cmd/ci/main.go` | 24 | `Name: "dagger-cache-ci"` | A | `dagger-kubernetes-ci` |
| `cmd/ci/main.go` | 25 | `Usage: "Dagger Cache CI helper ..."` | A | `Dagger Kubernetes CI helper ...` |
| `cmd/ci/main.go` | 27 | `Usage: "Dagger Cache server URL ..."` | A | `Dagger Kubernetes server URL ...` |
| `cmd/ci/main.go` | 31 | `Value: "cache.reg/dagger-cache"` | B | KEEP (cache registry ref) |
| `cmd/ci/main.go` | 88 | `_EXPERIMENTAL_DAGGER_CACHE_CONFIG` | B | KEEP (Dagger CLI env var) |
| `cmd/ci/main.go` | 171,177 | `"[dagger-cache] Pipeline View:"` | A | `[dagger-kubernetes] Pipeline View:` |

### Go source — internal/repository

| File | Line(s) | Current | Class | Replacement |
|---|---|---|---|---|
| `internal/repository/k8s_provider.go` | 36 | `volumeDaggerCache = "dagger-cache"` | A | `volumeDaggerKubernetes = "dagger-kubernetes"` (const + usages L253,309) |
| `internal/repository/k8s_provider.go` | 80 | `cfg.Namespace = "dagger-cache"` | A | `dagger-kubernetes` |
| `internal/repository/k8s_provider.go` | 272 | `secretEnvVar("DAGGER_CACHE_TOKEN", ...)` | A | `DAGGER_KUBERNETES_TOKEN` |
| `internal/repository/raft_store.go` | 233 | `cfg.Dir = "/var/lib/dagger-cache"` | A | `/var/lib/dagger-kubernetes` |
| `internal/repository/ca.go` | 39 | `CommonName: "dagger-cache-minting-ca"` | A | `dagger-kubernetes-minting-ca` |
| `internal/repository/ca.go` | 40 | `Organization: []string{"dagger-cache"}` | A | `dagger-kubernetes` |
| `internal/repository/ca_providers.go` | 227 | `Organization: "dagger-cache"` | A | `dagger-kubernetes` |
| `internal/repository/ca_providers.go` | 234 | `goca.New("dagger-cache-minting-ca", ...)` | A | `dagger-kubernetes-minting-ca` |
| `internal/repository/ca_providers.go` | 271 | `"supervisor-control.dagger-cache.svc"` | A | `supervisor-control.dagger-kubernetes.svc` |
| `internal/repository/ca_providers.go` | 284 | `"supervisor-server", "dagger-cache"` | A | `dagger-kubernetes` |
| `internal/repository/ca_providers.go` | 301 | comment `outside of dagger-cache` | A | `outside of dagger-kubernetes` |
| `internal/repository/raft_tls.go` | 32 | `// default "dagger-cache-raft"` | A | `dagger-kubernetes-raft` |
| `internal/repository/raft_tls.go` | 75 | `cfg.Organization = "dagger-cache-raft"` | A | `dagger-kubernetes-raft` |
| `internal/repository/raft_tls.go` | 153,245 | `createRaftCAWithGoca("dagger-cache-raft-ca", ...)` | A | `dagger-kubernetes-raft-ca` |

### Go source — internal/handler, service, observ, domain

| File | Line(s) | Current | Class | Replacement |
|---|---|---|---|---|
| `internal/handler/server.go` | 156 | `cacheProxyBackendIDKey = "dagger_cache_backend_id"` | A | `dagger_kubernetes_backend_id` |
| `internal/handler/server.go` | 1061-1063 | `X-Dagger-Cache-Target/User/Pass` (Set) | A | `X-Dagger-Kubernetes-Target/User/Pass` |
| `internal/handler/server.go` | 1303-1308 | `X-Dagger-Cache-Target/User/Pass` (Peek/Del) | A | `X-Dagger-Kubernetes-...` |
| `internal/service/oauth_github.go` | 170 | `User-Agent", "dagger-cache"` | A | `dagger-kubernetes` |
| `internal/service/connect_service.go` | 80 | `Name: "_EXPERIMENTAL_DAGGER_CACHE_CONFIG"` | B | KEEP (Dagger CLI env var) |
| `internal/service/cache_stats.go` | 92-93 | comment `("dagger-cache" for "cache.reg/dagger-cache")` | B | KEEP (cache repo path) |
| `internal/domain/cache.go` | 68 | comment `full ref "cache.reg/dagger-cache:v0-21-4"` | B | KEEP (cache ref example) |
| `internal/observ/metrics.go` | 31 | `dagger_cache_engine_acquire_total` | A | `dagger_kubernetes_engine_acquire_total` |
| `internal/observ/metrics.go` | 36 | `dagger_cache_engine_acquire_duration_seconds` | A | `dagger_kubernetes_...` |
| `internal/observ/metrics.go` | 42 | `dagger_cache_active_leases` | A | `dagger_kubernetes_active_leases` |
| `internal/observ/metrics.go` | 47 | `dagger_cache_active_replicas` | A | `dagger_kubernetes_active_replicas` |
| `internal/observ/metrics.go` | 52 | `dagger_cache_otel_ingest_total` | A | `dagger_kubernetes_otel_ingest_total` |
| `internal/observ/metrics.go` | 57 | `dagger_cache_cache_size_bytes` | A | `dagger_kubernetes_cache_size_bytes` (inner `cache` = feature, KEEP) |
| `internal/observ/metrics.go` | 62 | `dagger_cache_cache_object_count` | A | `dagger_kubernetes_cache_object_count` |
| `internal/observ/metrics.go` | 67 | `dagger_cache_cache_purge_total` | A | `dagger_kubernetes_cache_purge_total` |
| `internal/observ/metrics.go` | 72 | `dagger_cache_gc_run_total` | A | `dagger_kubernetes_gc_run_total` |
| `internal/observ/metrics.go` | 77 | `dagger_cache_history_purge_total` | A | `dagger_kubernetes_history_purge_total` |
| `internal/observ/metrics.go` | 82 | `dagger_cache_history_gc_run_total` | A | `dagger_kubernetes_history_gc_run_total` |
| `internal/observ/metrics.go` | 87 | `dagger_cache_pipeline_disconnect_failed_total` | A | `dagger_kubernetes_pipeline_disconnect_failed_total` |

### Go tests — RENAME (project-identity: namespace, hostname, CA org, env var, headers, metrics)

| File | Line(s) | Current | Class | Replacement |
|---|---|---|---|---|
| `config/loader_test.go` | 28,37,40,55 | `/etc/dagger-cache/...`, `/var/lib/dagger-cache`, `dagger-cache` ns | A | dagger-kubernetes forms |
| `config/loader_test.go` | 248-304,482 | `DAGGER_CACHE_*` env vars | A | `DAGGER_KUBERNETES_*` |
| `cmd/api/main_test.go` | 227,230,284 | `DAGGER_CACHE_TOKEN` | A | `DAGGER_KUBERNETES_TOKEN` |
| `cmd/api/main_test.go` | 367,375,384,454,464,494 | `cache.reg/dagger-cache` | B | KEEP (cache registry ref) |
| `cmd/api/main_test.go` | 648,655,662,669,714,720,726 | `dagger-cache-supervisor-N` hostnames | A | `dagger-kubernetes-supervisor-N` |
| `cmd/api/main_test.go` | 981-986 | `/var/lib/dagger-cache/...`, `/etc/dagger-cache/...` | A | dagger-kubernetes paths |
| `internal/repository/raft_discovery_test.go` | 52-62 | `dagger-cache-supervisor`, `dagger-cache-headless`, ns `dagger-cache`, DNS `...dagger-cache-headless.dagger-cache.svc...` | A | dagger-kubernetes forms |
| `internal/repository/ca_providers_test.go` | 358 | `supervisor-control.dagger-cache.svc` | A | `supervisor-control.dagger-kubernetes.svc` |
| `internal/repository/ca_test.go` | 123 | `"dagger-cache-raft"` | A | `dagger-kubernetes-raft` |
| `internal/repository/raft_test_helpers_test.go` | 190,206 | `"dagger-cache-raft"` | A | `dagger-kubernetes-raft` |
| `internal/repository/raft_tls_test.go` | 23 | `Organization: "dagger-cache-raft"` | A | `dagger-kubernetes-raft` |
| `internal/repository/k8s_provider_test.go` | 19,50,60,123,179,180,215,... | namespace `dagger-cache`, PVC name `dagger-cache`, `DAGGER_CACHE_TOKEN` | A | `dagger-kubernetes`, `DAGGER_KUBERNETES_TOKEN` (all ~40 namespace + 7 token + 2 PVC-name hits) |
| `internal/repository/k8s_provider_integration_test.go` | 42,58,71,88,123,142,150,... | ns `dagger-cache-test`, PVC `dagger-cache` | A | `dagger-kubernetes-test`, `dagger-kubernetes` |
| `internal/observ/observ_test.go` | 100 | `dagger_cache_pipeline_disconnect_failed_total` | A | `dagger_kubernetes_pipeline_disconnect_failed_total` |
| `internal/service/pipeline_lifecycle_test.go` | 110,269 | `dagger_cache_pipeline_disconnect_failed_total` | A | `dagger_kubernetes_...` |
| `internal/handler/cache_proxy_test.go` | 272-305 | `X-Dagger-Cache-Target/User/Pass` | A | `X-Dagger-Kubernetes-...` |
| `internal/handler/connect_test.go` | 106,154,155 | `_EXPERIMENTAL_DAGGER_CACHE_CONFIG` | B | KEEP (Dagger CLI env var) |
| `internal/service/connect_service_test.go` | 96,118,145,167,187,190,312,331,346 | `_EXPERIMENTAL_DAGGER_CACHE_CONFIG` | B | KEEP |

### Go tests — KEEP (cache feature: OCI repo name `dagger-cache`)

These use `dagger-cache` as the OCI repository name (the BuildKit cache blob
repo) in registry client / cache routing / cache stats / cache proxy fixtures.
**KEEP every occurrence.**

- `internal/repository/registry_client_test.go` (all ~17 hits: `/v2/dagger-cache/...`, `Tags(...,"dagger-cache")`, `ManifestSize(...,"dagger-cache",...)`, etc.)
- `internal/repository/cache_routes_repo_test.go` (L20,23,27,92,99,197,204: repo `dagger-cache`)
- `internal/service/cache_stats_test.go` (all ~50 hits: `reg.tags["dagger-cache"]`, `manifestBody["dagger-cache:v0-21-4"]`, `cache.reg/dagger-cache`, etc.)
- `internal/service/cache_test.go` (L20,22,28,30,37: `cache.reg/dagger-cache` cache ref)
- `internal/service/registry_router_test.go` (L105,109,116,119,124,134,145,162,171,181,188,204: repo `dagger-cache`)
- `internal/handler/cache_proxy_test.go` (L182-195,272,339,344,351,355,382-401,427,432,440,445,453,467,478,490,527,553,569: `/v2/dagger-cache/...` paths + `dagger-cache:v0-21-4` refs)
- `internal/handler/cache_test.go` (L44: `cache.reg/dagger-cache`)
- `internal/handler/test_helper_test.go` (L94,131: `cache.reg/dagger-cache`)

### Helm chart — `deploy/helm/dagger-kubernetes/`

| File | Line(s) | Current | Class | Replacement |
|---|---|---|---|---|
| `values.yaml` | 105 | `dir: "/var/lib/dagger-cache"` | A | `/var/lib/dagger-kubernetes` |
| `values.yaml` | 153 | `organization: "dagger-cache-raft"` | A | `dagger-kubernetes-raft` |
| `values.yaml` | 177 | `tokensFile: "/etc/dagger-cache/tokens"` | A | `/etc/dagger-kubernetes/tokens` |
| `values.yaml` | 203 | comment `<cachePublicHost>/dagger-cache` | B | KEEP (cache repo path) |
| `templates/_helpers.tpl` | 60-61,63 | comment + `printf "%s/dagger-cache"` | B | KEEP (cache repo path fixed to `dagger-cache`) |
| `templates/statefulset.yaml` | 39-72 | `DAGGER_CACHE_*` env names | A | `DAGGER_KUBERNETES_*` |
| `templates/statefulset.yaml` | 93 | `mountPath: /etc/dagger-cache/tokens` | A | `/etc/dagger-kubernetes/tokens` |
| `templates/statefulset.yaml` | 105 | `mountPath: /var/lib/dagger-cache` | A | `/var/lib/dagger-kubernetes` |
| `templates/configmap.yaml` | (none) | — | — | (renders from values; no literal dagger-cache) |
| `README.md` | 94 | `my-registry:5000/dagger-cache` | B | KEEP (cache repo path) |
| `README.md` | 389 | `<cachePublicHost>/dagger-cache` | B | KEEP |
| `README.md` | 402 | `/var/lib/dagger-cache` | A | `/var/lib/dagger-kubernetes` |
| `README.md` | 421 | `dagger-cache-raft` | A | `dagger-kubernetes-raft` |
| `README.md` | 458 | `<cachePublicHost>/dagger-cache` | B | KEEP |

> Chart name (`Chart.yaml` `name: dagger-kubernetes`), image repo
> (`ghcr.io/disaster37/dagger-kubernetes`), and `_helpers.tpl` define names are
> already `dagger-kubernetes` — no change. The cache repo path segment
> `dagger-cache` in `_helpers.tpl` `cacheRegistry` is **B (KEEP)** — it is the
> OCI cache blob repo path stored in existing registries; renaming would
> orphan existing cache blobs.

### K8s manifests — `deploy/k8s/`

| File | Line(s) | Current | Class | Replacement |
|---|---|---|---|---|
| `namespace-rbac.yaml` | 4,10,38,44,54,69,79,90,98,104 | namespace `dagger-cache`, `/etc/dagger-cache/tokens`, `cache-registry:5000/dagger-cache` | A for ns+path; B for cache repo | ns→`dagger-kubernetes`; tokens path→`/etc/dagger-kubernetes/tokens`; **KEEP** `cache-registry:5000/dagger-cache` (L98, cache repo) |
| `supervisor.yaml` | 5,21,31,33,36,37,41,44,47,81,94,108 | ns `dagger-cache`, image `dagger-cache/supervisor:latest`, `DAGGER_CACHE_*`, paths `/etc/dagger-cache`, `/var/lib/dagger-cache` | A | ns→`dagger-kubernetes`; image→`dagger-kubernetes/supervisor:latest`; env→`DAGGER_KUBERNETES_*`; paths→`/etc/dagger-kubernetes`, `/var/lib/dagger-kubernetes` |
| `cache-registry.yaml` | 5,38,50 | namespace `dagger-cache` | A | `dagger-kubernetes` |
| `telemetry.yaml` | 5,37,51,89,123,135 | namespace `dagger-cache` | A | `dagger-kubernetes` |

### Docker — `Dockerfile`, `deploy/docker/`, `.dockerignore`

| File | Line(s) | Current | Class | Replacement |
|---|---|---|---|---|
| `Dockerfile` | 26 | `# dagger-cache-ci builder` | A | `# dagger-kubernetes-ci builder` |
| `Dockerfile` | 30 | `go build ... -o /out/dagger-cache-ci ./cmd/ci/` | A | `/out/dagger-kubernetes-ci` |
| `Dockerfile` | 38 | `COPY --from=ci-builder /out/dagger-cache-ci /usr/local/bin/dagger-cache-ci` | A | `dagger-kubernetes-ci` (both) |
| `Dockerfile` | 39 | `/etc/dagger-cache/config.app.yaml.sample` | A | `/etc/dagger-kubernetes/config.app.yaml.sample` |
| `Dockerfile` | 44 | `CMD ["--config=/etc/dagger-cache/config.app.yaml"]` | A | `/etc/dagger-kubernetes/config.app.yaml` |
| `deploy/docker/docker-compose.yaml` | 12-28 | `DAGGER_CACHE_*` env | A | `DAGGER_KUBERNETES_*` |
| `deploy/docker/docker-compose.yaml` | 21 | `localhost:5000/dagger-cache` | B | KEEP (cache repo path) |
| `deploy/docker/docker-compose.yaml` | 22 | `/tmp/dagger-cache/ca` | A | `/tmp/dagger-kubernetes/ca` |
| `deploy/docker/docker-compose.yaml` | 24 | `/var/lib/dagger-cache` | A | `/var/lib/dagger-kubernetes` |
| `deploy/docker/docker-compose.yaml` | 35,37 | `/etc/dagger-cache/tokens`, `/var/lib/dagger-cache` | A | dagger-kubernetes paths |
| `deploy/docker/docker-compose.yaml` | 41,50,60,67,79,88,99,102 | network `dagger-cache` | A | `dagger-kubernetes` |
| `.dockerignore` | 11 | `dagger-cache-ci` | A | `dagger-kubernetes-ci` |

### Scripts & CI integrations

| File | Line(s) | Current | Class | Replacement |
|---|---|---|---|---|
| `scripts/dagger-cache.sh` | 4,5 | `DAGGER_CACHE_SERVER`, `DAGGER_CACHE_UI` | A | `DAGGER_KUBERNETES_SERVER`, `DAGGER_KUBERNETES_UI` |
| `scripts/dagger-cache.sh` | 6 | `cache.reg/dagger-cache` | B | KEEP (cache repo) |
| `scripts/dagger-cache.sh` | 13 | `DAGGER_CLOUD_URL="$DAGGER_CACHE_SERVER"` | A (var ref) | `$DAGGER_KUBERNETES_SERVER` |
| `scripts/dagger-cache.sh` | 27 | `_EXPERIMENTAL_DAGGER_CACHE_CONFIG` | B | KEEP (Dagger CLI) |
| `scripts/dagger-cache.sh` | 30 | `echo "Dagger Cache: ..."` | A | `Dagger Kubernetes: ...` |
| `scripts/dagger-cache.sh` | 53,55 | `$DAGGER_CACHE_UI/...` | A | `$DAGGER_KUBERNETES_UI/...` |
| `ci-integrations/gha/dagger-cache.sh` | (same as scripts/) | identical | A/B | same as above |
| `ci-integrations/gha/action.yml` | 1 | `name: 'dagger-cache'` | A | `dagger-kubernetes` |
| `ci-integrations/gha/action.yml` | 5 | `description: 'Dagger Cache server URL'` | A | `Dagger Kubernetes server URL` |
| `ci-integrations/gha/action.yml` | 29 | `"${GITHUB_ACTION_PATH}/dagger-cache.sh"` | A | `dagger-kubernetes.sh` (file renamed) |
| `ci-integrations/gha/action.yml` | 33,34 | `DAGGER_CACHE_SERVER`, `DAGGER_CACHE_UI` | A | `DAGGER_KUBERNETES_*` |
| `ci-integrations/drone/config-extension.sh` | 4,5,6,9,14,15,33,35 | `DAGGER_CACHE_SERVER/TOKEN/UI` | A | `DAGGER_KUBERNETES_*` |
| `ci-integrations/drone/config-extension.sh` | 30 | `name: dagger-cache-summary` | A | `dagger-kubernetes-summary` |
| `ci-integrations/drone/config-extension.sh` | 36 | `from_secret: dagger_cache_ui` | A | `dagger_kubernetes_ui` |
| `ci-integrations/jenkins/daggerCache.groovy` | 4,5,6 | `env.DAGGER_CACHE_SERVER/TOKEN/UI` | A | `DAGGER_KUBERNETES_*` |
| `ci-integrations/jenkins/daggerCache.groovy` | 10 | `error "daggerCache: ..."` | A | `daggerKubernetes: ...` |
| `ci-integrations/jenkins/daggerCache.groovy` | 28,36,42 | `echo "[dagger-cache] ..."` | A | `[dagger-kubernetes] ...` |
| `ci-integrations/jenkins/daggerCache.groovy` | (function) | `def call(...)`, `def withStages(...)` | A | rename file to `daggerKubernetes.groovy`; function name `daggerKubernetes` (public API; users update Jenkinsfiles) |

**File renames (A):**
- `scripts/dagger-cache.sh` → `scripts/dagger-kubernetes.sh`
- `ci-integrations/gha/dagger-cache.sh` → `ci-integrations/gha/dagger-kubernetes.sh`
- `ci-integrations/jenkins/daggerCache.groovy` → `ci-integrations/jenkins/daggerKubernetes.groovy`
- Keep the two wrapper scripts byte-identical (CONTRIBUTING.md §"Wrapper script
  sync"); update the `cmp` command in CONTRIBUTING.md to the new names.

### Dagger CI module — `dagger/`

| File | Line(s) | Current | Class | Replacement |
|---|---|---|---|---|
| `dagger/dagger.json` | 2 | `"name": "dagger-cache"` | A | `dagger-kubernetes` |
| `dagger/go.mod` | 1 | `module dagger/dagger-cache` | A | `module dagger/dagger-kubernetes` |
| `dagger/main.go` | 1 | `// dagger-cache is the local Dagger CI module ...` | A | `dagger-kubernetes ...` |
| `dagger/main.go` | 15 | `"dagger/dagger-cache/internal/dagger"` | A | `dagger/dagger-kubernetes/internal/dagger` |
| `dagger/main.go` | 35 | `out: "bin/dagger-cache-ci"` | A | `bin/dagger-kubernetes-ci` |
| `dagger/main.go` | 52,53,58,62,63,70,91,116,137,154,167,196,242 | `DaggerCache` type + methods + `New() *DaggerCache` | A | `DaggerKubernetes` (all) |
| `dagger/main.go` | 132,240 | comments `supervisor and dagger-cache-ci` | A | `dagger-kubernetes-ci` |
| `dagger/dagger.gen.go` | (generated) | `DaggerCache`, `dagger/dagger-cache/internal/dagger`, `case "DaggerCache"` | A | **REGENERATE** via `dagger develop` after editing dagger.json/main.go (DO NOT hand-edit) |

### Config YAML — `config/`

| File | Line(s) | Current | Class | Replacement |
|---|---|---|---|---|
| `config/config.app.yaml.sample` | 8-12 | `DAGGER_CACHE_` prefix examples | A | `DAGGER_KUBERNETES_` |
| `config/config.app.yaml.sample` | 29 | comment `dagger-cache-ci wrapper` | A | `dagger-kubernetes-ci wrapper` |
| `config/config.app.yaml.sample` | 38 | `/etc/dagger-cache/tokens` | A | `/etc/dagger-kubernetes/tokens` |
| `config/config.app.yaml.sample` | 54 | `DAGGER_CACHE_AUTH_TOKEN_ENCRYPTION_KEY` | A | `DAGGER_KUBERNETES_AUTH_TOKEN_ENCRYPTION_KEY` |
| `config/config.app.yaml.sample` | 82 | `/var/lib/dagger-cache` | A | `/var/lib/dagger-kubernetes` |
| `config/config.app.yaml.sample` | 107,108 | `dagger-cache-supervisor`, `dagger-cache-headless` | A | `dagger-kubernetes-supervisor`, `dagger-kubernetes-headless` |
| `config/config.app.yaml.sample` | 120 | `dagger-cache-raft` | A | `dagger-kubernetes-raft` |
| `config/config.app.yaml.sample` | 130 | `DAGGER_CACHE_OTEL_OTLP_ENDPOINT` | A | `DAGGER_KUBERNETES_OTEL_OTLP_ENDPOINT` |
| `config/config.app.yaml.sample` | 140 | `cache.reg/dagger-cache` | B | KEEP (cache repo) |
| `config/config.app.yaml.sample` | 181 | `namespace: "dagger-cache"` | A | `dagger-kubernetes` |
| `config/config.app.yaml.sample` | 235 | `/var/lib/dagger-cache/ca` | A | `/var/lib/dagger-kubernetes/ca` |
| `config/config.app.yaml.sample` | 236,237 | `/etc/dagger-cache/tls/...` | A | `/etc/dagger-kubernetes/tls/...` |
| `config/config.app.yaml` | 9,10 | `DAGGER_CACHE_` prefix | A | `DAGGER_KUBERNETES_` |
| `config/config.app.yaml` | 19 | `https://dagger-cache.example.com` | A | `https://dagger-kubernetes.example.com` |
| `config/config.app.yaml` | 24 | `/etc/dagger-cache/tokens` | A | `/etc/dagger-kubernetes/tokens` |
| `config/config.app.yaml` | 49 | `/var/lib/dagger-cache` | A | `/var/lib/dagger-kubernetes` |
| `config/config.app.yaml` | 58,59 | `dagger-cache-supervisor`, `dagger-cache-headless` | A | dagger-kubernetes forms |
| `config/config.app.yaml` | 71 | `dagger-cache-raft` | A | `dagger-kubernetes-raft` |
| `config/config.app.yaml` | 82 | `cache.reg/dagger-cache` | B | KEEP (cache repo) |

### Docs

| File | Line(s) | Current | Class | Replacement |
|---|---|---|---|---|
| `README.md` | 1 | `# dagger-cache` | A | `# dagger-kubernetes` |
| `README.md` | 21,25 | `scripts/dagger-cache.sh` | A | `scripts/dagger-kubernetes.sh` |
| `AGENTS.md` | 1 | `# AGENTS.md — dagger-cache ...` | A | `dagger-kubernetes` |
| `AGENTS.md` | 13 | release `dagger-cache-test` | A | `dagger-kubernetes-test` |
| `AGENTS.md` | 106 | `Usage: "dagger-cache control plane"` | A | `dagger-kubernetes control plane` |
| `AGENTS.md` | 125 | `Env prefix: DAGGER_CACHE_` | A | `DAGGER_KUBERNETES_` |
| `AGENTS.md` | 139 | binary `dagger-cache-ci` | A | `dagger-kubernetes-ci` |
| `AGENTS.md` | 148 | `scripts/dagger-cache.sh` | A | `scripts/dagger-kubernetes.sh` |
| `AGENTS.local.md` | 9,36-39,54-55,81-96,115-119,132,147,171,259-263 | release/namespace `dagger-cache-test`, image, paths, `DAGGER_CACHE_*` (none — uses `dagger-cache-test` release + `dagger-kubernetes` image) | A | release/namespace→`dagger-kubernetes-test`; legacy SQLite `dagger-cache.db` recovery path `/tmp/dagger-cache-recovery/` is a historical artifact — KEEP (it documents a one-time recovery dir that no longer exists) |
| `CONTRIBUTING.md` | 1,94,151,174,182,241-245 | `dagger-cache`, `DAGGER_CACHE_`, `dagger-cache-ci`, `dagger-cache.sh` | A | dagger-kubernetes forms; update `cmp` line to new script names |
| `DAGGER.md` | 5 | module name `dagger-cache` | A | `dagger-kubernetes` |
| `DAGGER.md` | 43 | `out/bin/dagger-cache-ci` | A | `out/bin/dagger-kubernetes-ci` |
| `docs/README.md` | 79,160 | `DAGGER_CACHE_*`, `DAGGER_CACHE_TOKEN` | A | `DAGGER_KUBERNETES_*`, `DAGGER_KUBERNETES_TOKEN` |
| `docs/README.md` | 185,202,235,543,546,1143,1262,1319 | `_EXPERIMENTAL_DAGGER_CACHE_CONFIG` | B | KEEP (Dagger CLI) |
| `docs/README.md` | 193,1249,1258 | `scripts/dagger-cache.sh` | A | `scripts/dagger-kubernetes.sh` |
| `docs/README.md` | 220 | `source ~/.dagger-cache.env` | A | `~/.dagger-kubernetes.env` |
| `docs/README.md` | 312,318-323,331 | `DAGGER_CACHE_` prefix table | A | `DAGGER_KUBERNETES_` |
| `docs/README.md` | 348 | `/etc/dagger-cache/tokens` | A | `/etc/dagger-kubernetes/tokens` |
| `docs/README.md` | 358 | `/var/lib/dagger-cache` | A | `/var/lib/dagger-kubernetes` |
| `docs/README.md` | 376 | `dagger-cache-raft` | A | `dagger-kubernetes-raft` |
| `docs/README.md` | 384 | `cache.reg/dagger-cache` | B | KEEP (cache repo) |
| `docs/README.md` | 403 | namespace `dagger-cache` | A | `dagger-kubernetes` |
| `docs/README.md` | 440 | `go build -o dagger-cache-ci ./cmd/ci` | A | `dagger-kubernetes-ci` |
| `docs/README.md` | 449,450,459,460 | `/etc/dagger-cache/...` | A | `/etc/dagger-kubernetes/...` |
| `docs/README.md` | 457,461 | `dagger-cache/supervisor:latest` | A | `dagger-kubernetes/supervisor:latest` |
| `docs/README.md` | 506,597 | `DAGGER_CACHE_TOKEN`, `DAGGER_CACHE_CACHE_AUTH_TOKEN` | A | `DAGGER_KUBERNETES_TOKEN`, `DAGGER_KUBERNETES_CACHE_AUTH_TOKEN` |
| `docs/README.md` | 546,1261 | `cache.<public_host>/dagger-cache:V0-21-4` | B | KEEP (cache repo path) |
| `docs/README.md` | 572 | `DAGGER_CACHE_CACHE_AUTH_TOKEN` | A | `DAGGER_KUBERNETES_CACHE_AUTH_TOKEN` |
| `docs/README.md` | 613 | `bucket: "my-dagger-cache"` | A | `my-dagger-kubernetes` (example S3 bucket; rename for consistency) |
| `docs/README.md` | 784 | `client_id: "dagger-cache"` | A | `dagger-kubernetes` (OAuth client registration) |
| `docs/README.md` | 837,844-846 | `DAGGER_CACHE_*` env overrides | A | `DAGGER_KUBERNETES_*` |
| `docs/README.md` | 1162 | `dagger-cache-ci wrapper` | A | `dagger-kubernetes-ci wrapper` |
| `docs/README.md` | 1212,1215,1216 | `@Library('dagger-cache')`, `daggerCache(...)` | A | `@Library('dagger-kubernetes')`, `daggerKubernetes(...)` |
| `docs/README.md` | 1229,1233,1234 | `dagger-cache/drone-config-extension`, `name: dagger-cache`, `image: dagger-cache/drone-config-extension` | A | `dagger-kubernetes/...`, `dagger-kubernetes` |
| `docs/README.md` | 1238 | `from_secret: dagger_cache_token` | A | `dagger_kubernetes_token` |
| `docs/README.md` | 1253,1254,1264 | `DAGGER_CACHE_SERVER`, `DAGGER_CACHE_UI` | A | `DAGGER_KUBERNETES_*` |
| `docs/design/index.md` | 1,4 | `dagger-cache` (title + body) | A | `dagger-kubernetes` |
| `docs/design/ADR-001,004,005,006,012,014,017,018,019,020,021` | author/deciders | `dagger-cache team` / `dagger-cache maintainers` | A | `dagger-kubernetes team` / `dagger-kubernetes maintainers` |
| `docs/design/ADR-004` | 3 | `Author: dagger-cache team` | A | `dagger-kubernetes team` |
| `docs/design/ADR-005` | 48 | `/var/lib/dagger-cache/ca` | A | `/var/lib/dagger-kubernetes/ca` |
| `docs/design/ADR-006` | 18 | `cache.reg/dagger-cache:V<slug>` | B | KEEP (cache repo) |
| `docs/design/ADR-006` | 37 | `_EXPERIMENTAL_DAGGER_CACHE_CONFIG` | B | KEEP |
| `docs/design/ADR-009` | 22,43,64 | `dagger-cache-ci` binary, `cmd/dagger-cache-ci` | A | `dagger-kubernetes-ci`, `cmd/dagger-kubernetes-ci` (note: ADR-009 D4 says `cmd/dagger-cache-ci → cmd/ci`; update to `cmd/dagger-kubernetes-ci → cmd/ci` for historical accuracy, or rephrase) |
| `docs/design/ADR-010` | 125 | `/var/lib/dagger-cache` | A | `/var/lib/dagger-kubernetes` |
| `docs/design/ADR-011` | 13 | `DAGGER_CACHE_TOKEN` | A | `DAGGER_KUBERNETES_TOKEN` |
| `docs/design/ADR-012` | 5 | `dagger-cache maintainers` | A | `dagger-kubernetes maintainers` |
| `docs/design/ADR-013` | 12,32,82,85 | `_EXPERIMENTAL_DAGGER_CACHE_CONFIG`, `DAGGER_CACHE_AUTH_TOKEN_ENCRYPTION_KEY` | B for former, A for latter | KEEP `_EXPERIMENTAL_...`; rename `DAGGER_KUBERNETES_AUTH_TOKEN_ENCRYPTION_KEY` |
| `docs/design/ADR-014` | 3,11,28 | `dagger-cache team`, `dagger-cache-test-registry:5000/dagger-cache:v0-19-0`, `DAGGER_CACHE_TOKEN` | A for team+token; B for cache ref | team→`dagger-kubernetes team`; **KEEP** `dagger-cache-test-registry:5000/dagger-cache` (cache repo path; the `dagger-cache-test-registry` host is the release-name-derived registry Service — see note); `DAGGER_CACHE_TOKEN`→`DAGGER_KUBERNETES_TOKEN` |
| `docs/design/ADR-018` | 5,53,54 | `dagger-cache maintainers`, `DAGGER_CACHE_HISTORY_GC_*` | A | `dagger-kubernetes maintainers`, `DAGGER_KUBERNETES_HISTORY_GC_*` |
| `docs/design/ADR-019` | 5,93-96,102 | `dagger-cache maintainers`, `DAGGER_CACHE_PIPELINE_*`, `dagger_cache_pipeline_disconnect_failed_total` | A | `dagger-kubernetes maintainers`, `DAGGER_KUBERNETES_PIPELINE_*`, `dagger_kubernetes_pipeline_disconnect_failed_total` |

> **ADR-014 cache ref note:** `dagger-cache-test-registry:5000/dagger-cache` —
> the host `dagger-cache-test-registry` is the in-cluster registry Service
> derived from the Helm release name `dagger-cache-test`. Once the release is
> renamed to `dagger-kubernetes-test`, this becomes
> `dagger-kubernetes-test-registry:5000/dagger-cache`. The trailing
> `/dagger-cache` (OCI repo path) is **B (KEEP)**. Update the ADR example to
> `dagger-kubernetes-test-registry:5000/dagger-cache` for consistency with the
> renamed release.

### UI

| File | Line(s) | Current | Class | Replacement |
|---|---|---|---|---|
| `ui/src/views/Connect.vue` | 142,195 | `~/.dagger-cache.env` | A | `~/.dagger-kubernetes.env` |
| `ui/src/stores/auth.ts` | 7,8,20,21,63,64 | `dagger_cache_token`, `dagger_cache_refresh_token` | A | `dagger_kubernetes_token`, `dagger_kubernetes_refresh_token` |
| `ui/package.json` | 2 | `"name": "dagger-cache-ui"` | A | `dagger-kubernetes-ui` |
| `ui/package-lock.json` | 2,8 | `"name": "dagger-cache-ui"` | A | `dagger-kubernetes-ui` |
| `ui/dist/**`, `internal/handler/ui-dist/**` | (generated) | baked brand + `~/.dagger-cache.env` + localStorage keys | A | **REBUILD** via `cd ui && npm ci && npm run build && cp -r dist ../internal/handler/ui-dist` |

### Other

| File | Line(s) | Current | Class | Replacement |
|---|---|---|---|---|
| `.gitignore` | 49 | `/dagger-cache-ci` | A | `/dagger-kubernetes-ci` |
| `kilo.json` | — | (no dagger-cache) | — | none |
| `.github/workflows/*.yml` | — | (no dagger-cache; use `github.repository` for image) | — | none |
| `go.mod` | 1 | `module github.com/disaster/dagger-kubernetes` | — | already correct |
| root `supervisor`, `dagger-cache-ci`, `out/`, `coverage.out` | (build artifacts) | — | — | gitignored; rebuilt, not edited |

## Data structures: renamed Go identifiers

| Old | New | Files |
|---|---|---|
| `DaggerCache` (struct + all methods + `New() *DaggerCache`) | `DaggerKubernetes` | `dagger/main.go`, `dagger/dagger.gen.go` (regenerated) |
| `volumeDaggerCache = "dagger-cache"` (const) | `volumeDaggerKubernetes = "dagger-kubernetes"` | `internal/repository/k8s_provider.go` (L36 + refs L253,309) |
| `cacheProxyBackendIDKey = "dagger_cache_backend_id"` | `dagger_kubernetes_backend_id` | `internal/handler/server.go` L156 |
| Prometheus metric names `dagger_cache_*` (12 metrics) | `dagger_kubernetes_*` | `internal/observ/metrics.go` (+ test refs) |
| dagger module import path `dagger/dagger-cache/internal/dagger` | `dagger/dagger-kubernetes/internal/dagger` | `dagger/main.go`, `dagger/dagger.gen.go` |
| dagger module path `dagger/dagger-cache` | `dagger/dagger-kubernetes` | `dagger/go.mod` |

> No domain/service struct renames: `Cache`, `CacheConfig`, `CacheStats`,
`CacheBackend`, `CacheStatsService`, `RegistryBackend`, `S3Ref` are
cache-FEATURE types → KEEP. No function signatures change except the
`DaggerCache` → `DaggerKubernetes` receiver renames (signatures identical
modulo receiver name).

## Function signatures affected

Only the dagger module:

```go
// dagger/main.go — old
func New(src *dagger.Directory) *DaggerCache
func (m *DaggerCache) Lint(...) (string, error)
func (m *DaggerCache) Test(...) (*dagger.File, error)
func (m *DaggerCache) Ui(...) (*dagger.Directory, error)
func (m *DaggerCache) Build(...) (*dagger.Directory, error)
func (m *DaggerCache) Docker(...) (*dagger.Container, error)
func (m *DaggerCache) Helm(...) error
func (m *DaggerCache) Publish(...) (string, error)
func (m *DaggerCache) Ci(...) (*dagger.Directory, error)
// new: receiver/type DaggerKubernetes; signatures otherwise identical
```

No other Go function signatures change (the env-prefix, paths, namespace,
metric names, header names, and context-key values are string literals, not
identifiers).

## Config key changes

- **Viper env prefix:** `DAGGER_CACHE` → `DAGGER_KUBERNETES` (`config/loader.go`
  L21). Every env override changes prefix, e.g. `DAGGER_CACHE_LOG_LEVEL` →
  `DAGGER_KUBERNETES_LOG_LEVEL`, `DAGGER_CACHE_CACHE_REGISTRY` →
  `DAGGER_KUBERNETES_CACHE_REGISTRY`, `DAGGER_CACHE_RAFT_*` →
  `DAGGER_KUBERNETES_RAFT_*`, `DAGGER_CACHE_AUTH_*` → `DAGGER_KUBERNETES_AUTH_*`.
  Note the double-prefix for cache keys: `cache.registry` →
  `DAGGER_KUBERNETES_CACHE_REGISTRY` (the inner `CACHE` is the config section
  name, kept; the outer prefix is the app prefix, renamed).
- **Standalone engine env var:** `DAGGER_CACHE_TOKEN` (injected into engine
  pods, read by engines to auth to the cache proxy) → `DAGGER_KUBERNETES_TOKEN`.
  This follows the app prefix convention; update `k8s_provider.go` L272,
  `cmd/api/main.go` L1005-1007, all `k8s_provider_test.go` refs, Helm
  `statefulset.yaml` (if present — it is not; the engine env is injected by the
  supervisor, not the chart), docs.
- **YAML keys:** unchanged (keys like `cache.registry`, `fleet.namespace`,
  `database.dir`, `raft.tls.organization`, `tls.ca_path` keep their names; only
  their DEFAULT VALUES change where the default is a project-identity string).
- **Default value changes:** `fleet.namespace` `dagger-cache`→`dagger-kubernetes`;
  `database.dir` `/var/lib/dagger-cache`→`/var/lib/dagger-kubernetes`;
  `raft.tls.organization` `dagger-cache-raft`→`dagger-kubernetes-raft`;
  `auth.internal.tokens_file` `/etc/dagger-cache/tokens`→`/etc/dagger-kubernetes/tokens`;
  `tls.ca_path` `/var/lib/dagger-cache/ca`→`/var/lib/dagger-kubernetes/ca`;
  `tls.cert_path`/`tls.key_path` `/etc/dagger-cache/tls/...`→`/etc/dagger-kubernetes/tls/...`.
- **Cache-feature defaults KEPT:** `cache.registry` default `cache.reg/dagger-cache`
  (B — OCI repo path); the `cache.*` config section name (B).

## Helm chart changes

- Chart name `dagger-kubernetes` — already correct, no change.
- `values.yaml`: `supervisor.config.database.dir`, `raft.tls.organization`,
  `auth.internal.tokensFile` defaults → dagger-kubernetes forms (A).
- `templates/statefulset.yaml`: env var names `DAGGER_CACHE_*` →
  `DAGGER_KUBERNETES_*`; mount paths `/etc/dagger-cache/tokens`,
  `/var/lib/dagger-cache` → dagger-kubernetes paths.
- `templates/_helpers.tpl`: `cacheRegistry` helper keeps `/dagger-cache` repo
  path (B). `controlTLSCertPath`/`controlTLSKeyPath` already use
  `/etc/dagger-kubernetes/data-tls/...` — no change.
- `templates/configmap.yaml`: renders from values; no literal dagger-cache.
- k8s resource names/labels/selectors: derived from `dagger-kubernetes.name` /
  `fullname` helpers — already `dagger-kubernetes`, no change.
- README.md: update A items (paths, org, env names); KEEP B cache-repo refs.

## Binary names & images

- `cmd/api` binary `supervisor` — unchanged (not a dagger-cache name).
- `cmd/ci` binary `dagger-cache-ci` → `dagger-kubernetes-ci` (urfave/cli
  `Name`, Dockerfile build/copy, .gitignore/.dockerignore, dagger/main.go
  `binaries` out path, DAGGER.md/README/CONTRIBUTING/AGENTS docs).
- Image `docker.io/disaster/dagger-kubernetes` / `ghcr.io/disaster37/dagger-kubernetes`
  — already correct. Stale ref `dagger-cache/supervisor:latest` in
  `deploy/k8s/supervisor.yaml` → `dagger-kubernetes/supervisor:latest` (A).
- Helm release name `dagger-cache-test` (AGENTS.local.md) →
  `dagger-kubernetes-test` (A; requires `helm uninstall` + reinstall on the
  local cluster — see rollout note).

## Edge cases

- **String concatenation:** AGENTS.md mandates `fmt.Sprintf` (no `+`). The only
  `+` concatenation with `dagger-cache` is in tests using `"/v2/dagger-cache/..."+var`
  (`registry_client_test.go`, `cache_proxy_test.go`) — these are B (cache repo
  path), KEEP. No A item uses `+` concatenation.
- **`fmt.Sprintf` sites:** `cmd/ci/main.go` L88
  `fmt.Sprintf("type=registry,ref=%s,mode=max", cacheRef)` — the `cacheRef`
  embeds the cache registry (B); the format string has no dagger-cache literal.
  No change.
- **Maps keyed by name strings:** test fixtures `reg.tags["dagger-cache"]`,
  `reg.manifestBody["dagger-cache:v0-21-4"]`, `reg.manifestDigest["dagger-cache:..."]`
  (cache_stats_test.go) — B (OCI repo name), KEEP. No A map keys.
- **Test fixtures/golden files:** none beyond the inline test data above. No
  golden files.
- **Docs cross-references:** README.md, DAGGER.md, CONTRIBUTING.md, ADRs all
  cross-reference `scripts/dagger-cache.sh`, `dagger-cache-ci`, `@Library('dagger-cache')`
  — update all to dagger-kubernetes forms (A).
- **Scripts referencing release names:** `AGENTS.local.md` §4.3-4.5 commands use
  release `dagger-cache-test` and StatefulSet
  `dagger-cache-test-dagger-kubernetes` → after release rename:
  `dagger-kubernetes-test` and `dagger-kubernetes-test-dagger-kubernetes`.
  Update all kubectl/helm commands in AGENTS.local.md §3-§8.
- **Git/CI workflows:** `.github/workflows/*.yml` use `github.repository` for
  the image tag — no dagger-cache literal. No change.
- **Shell completions:** none.
- **Generated files:**
  - `dagger/dagger.gen.go` — REGENERATE via `dagger develop` (or
    `dagger init --sdk go` re-run) after editing `dagger.json` + `dagger/main.go`.
    DO NOT hand-edit.
  - `ui/dist/**` and `internal/handler/ui-dist/**` — REBUILD via
    `cd ui && npm ci && npm run typecheck && npm run build` then
    `rm -rf internal/handler/ui-dist && cp -r ui/dist internal/handler/ui-dist`.
    Hashed asset filenames change; replace the whole `assets/` dir.
  - `deploy/helm/dagger-kubernetes/README.md` — has `<!-- version-marker -->`
    comments; no dagger-cache in markers. helm-docs (`scripts/update-helm-docs.sh`)
    may regenerate the params table — re-run it after values.yaml edits.
- **`dagger-cache.db` / `/tmp/dagger-cache-recovery/`** in AGENTS.local.md §3:
  historical legacy SQLite recovery dir, documents a one-time migration. KEEP
  (renaming would falsify the historical record; the dir no longer exists).

## DO NOT RENAME (cache-feature whitelist, with justification)

| Item | Where | Why KEEP |
|---|---|---|
| OCI repo path segment `dagger-cache` in cache refs | `config/loader.go` L87, `config/config.app.yaml(.sample)`, Helm `_helpers.tpl` L60-63, `values.yaml` L203, `deploy/k8s/namespace-rbac.yaml` L98, `deploy/docker/docker-compose.yaml` L21, `cmd/ci/main.go` L31, `scripts/*.sh` L6, `docs/README.md` L384,546,1261, ADR-006 L18, ADR-014 L11 | This is the BuildKit cache blob repository path stored in existing OCI registries. Renaming orphans all existing cache blobs. The Helm helper fixes it to `dagger-cache` deliberately. |
| `_EXPERIMENTAL_DAGGER_CACHE_CONFIG` env var | `cmd/ci/main.go` L88, `scripts/*.sh` L27, `connect_service.go` L80, `connect_service_test.go`, `connect_test.go`, `docs/README.md` (many), ADR-006/013 | Third-party Dagger CLI env var (not ours); controls the BuildKit cache backend. Renaming breaks the Dagger CLI contract. |
| `Cache` type, `CacheConfig`, `CacheStats`, `CacheBackend`, `CacheStatsService`, `RegistryBackend`, `S3Ref`, `cache.*` config keys | `internal/service/cache.go`, `internal/domain/cache.go`, `config/loader.go`, `internal/service/cache_stats.go` | Cache-feature types and config section. |
| Test OCI repo `dagger-cache` (paths `/v2/dagger-cache/...`, repo arg `"dagger-cache"`, refs `dagger-cache:v0-21-4`) | `registry_client_test.go`, `cache_routes_repo_test.go`, `cache_stats_test.go`, `cache_test.go`, `registry_router_test.go`, `cache_proxy_test.go`, `cache_test.go`, `test_helper_test.go` | These test the OCI cache proxy / cache routing / cache stats with the real cache repo name. |
| `cache.reg/dagger-cache` and `cache.<host>/dagger-cache` example values | `cmd/api/main.go` L1069 comment, `cmd/api/main_test.go` (6 hits), `cache_stats.go` L92-93 comment, `domain/cache.go` L68 comment | Cache-registry ref examples (host + repo path). |
| `CACHE_REGISTRY` shell var | `scripts/*.sh` L6 | Generic shell var for the cache registry ref; value is cache repo (B). |
| `DAGGER_CLOUD_URL`, `DAGGER_CLOUD_TOKEN`, `DAGGER_TAG`, `_EXPERIMENTAL_DAGGER_TAG`, `_EXPERIMENTAL_DAGGER_RUNNER_HOST` | CI scripts, `cmd/ci/main.go` | Dagger CLI env vars (third-party). |
| `dagger-cache.db`, `/tmp/dagger-cache-recovery/` | `AGENTS.local.md` §3 | Historical legacy SQLite recovery artifact; dir no longer exists. |

## Validation (must pass after implementation)

### Grep gates (zero hits expected, excluding B whitelist)

```sh
# Project-identity variants — must return ZERO hits after implementation.
# (B-whitelisted cache-repo occurrences are filtered out below.)
grep -rni "dagger-cache" --include=*.go --include=*.yaml --include=*.yml \
  --include=*.sh --include=*.groovy --include=*.md --include=*.json --include=*.tpl \
  --include=Dockerfile --include=.gitignore --include=.dockerignore \
  --include=action.yml . \
  | grep -vE "_EXPERIMENTAL_DAGGER_CACHE_CONFIG|cache\.reg/dagger-cache|cache-registry:5000/dagger-cache|localhost:5000/dagger-cache|<cachePublicHost>/dagger-cache|/v2/dagger-cache/|\"dagger-cache\"|dagger-cache:v0-|dagger-cache\.db|dagger-cache-recovery|cache\.<public_host>/dagger-cache|dagger-cache-test-registry:5000/dagger-cache|my-registry:5000/dagger-cache"

# PascalCase / snake_case project-identity — must be zero.
grep -rn "DaggerCache\|dagger_cache_token\|dagger_cache_refresh_token\|dagger_cache_ui" .
# (dagger_cache_* metric names and dagger_cache_backend_id must also be gone:)
grep -rn "dagger_cache_" --include=*.go .
# (X-Dagger-Cache-* headers must be gone:)
grep -rn "X-Dagger-Cache" .
# (DAGGER_CACHE_ env prefix must be gone — only _EXPERIMENTAL_DAGGER_CACHE_CONFIG may remain:)
grep -rn "DAGGER_CACHE" . | grep -v "_EXPERIMENTAL_DAGGER_CACHE_CONFIG"
```

> The first grep's exclusion list is the B whitelist; any remaining hits it
> surfaces are missed A items. Adjust the exclusion list only if a new B case
> is discovered and documented in the plan.

### Build / test / lint

```sh
go build ./...                              # compiles with new identifiers/paths
go vet ./...
gofmt -l . | grep . && echo "UNFORMATTED" || echo "ok"
goimports -w -local github.com/disaster/dagger-kubernetes .
go test -race -coverprofile=coverage.out -covermode=atomic ./...
golangci-lint run ./...
```

### Dagger module

```sh
cd dagger && dagger develop                 # regenerates dagger.gen.go
cd .. && dagger call -m ./dagger --src . ci export --path out   # full CI
```

### UI

```sh
cd ui && npm ci && npm run typecheck && npm run build
cd .. && rm -rf internal/handler/ui-dist && cp -r ui/dist internal/handler/ui-dist
go build ./...                              # embed still valid
```

### Helm

```sh
helm dependency update deploy/helm/dagger-kubernetes
helm lint deploy/helm/dagger-kubernetes
helm template dagger-kubernetes deploy/helm/dagger-kubernetes --debug
helm template dagger-kubernetes deploy/helm/dagger-kubernetes \
  --set tools.otelCollector.enabled=false --set tools.registry.enabled=false --debug
bash scripts/update-helm-docs.sh           # regenerate helm README params if present
```

### Docs consistency

- `config/config.app.yaml.sample` env-prefix examples match `config/loader.go`
  `SetEnvPrefix` + `SetDefault` values.
- `deploy/helm/dagger-kubernetes/values.yaml` defaults match `config.app.yaml.sample`.
- `docs/README.md` env-override table uses `DAGGER_KUBERNETES_*`.
- `CONTRIBUTING.md` wrapper-sync `cmp` line uses new script names.

### If a renamed cache-feature item would break tests

The B whitelist is cache-feature and is intentionally KEPT, so no test should
break from B items. If an A rename breaks a test that hard-coded an old A
default (e.g. a test asserting `fleet.namespace == "dagger-cache"`), update the
test expectation to the new dagger-kubernetes value — do NOT add a backward-
compat alias. The mandate is a clean break.

## Ordered implementation steps

1. **Go source — config & identifiers (mechanical, do first):**
   `config/loader.go` (env prefix, path defaults, namespace, raft org),
   `cmd/api/main.go`, `cmd/ci/main.go`, `internal/repository/*` (k8s_provider
   const + namespace + token env, raft_store path, ca/ca_providers/raft_tls
   subjects + SANs), `internal/handler/server.go` (context key + headers),
   `internal/service/oauth_github.go` (User-Agent), `internal/observ/metrics.go`
   (12 metric names).
2. **Go tests — A items:** update namespace/hostname/CA-org/path/env/header/metric
   expectations to dagger-kubernetes forms. LEAVE B test fixtures (OCI repo
   `dagger-cache`) untouched.
3. **Helm chart:** `values.yaml` defaults, `templates/statefulset.yaml` env +
   mount paths. LEAVE `_helpers.tpl` cache repo path. Re-run helm-docs.
4. **K8s manifests:** `deploy/k8s/*.yaml` namespaces, image, env, paths. LEAVE
   cache-registry repo path in `namespace-rbac.yaml` L98.
5. **Docker:** `Dockerfile` (binary name, paths), `deploy/docker/docker-compose.yaml`
   (env, paths, network), `.dockerignore`, `.gitignore`.
6. **Scripts & CI integrations:** rename files
   (`scripts/dagger-cache.sh`→`dagger-kubernetes.sh`,
   `ci-integrations/gha/dagger-cache.sh`→`dagger-kubernetes.sh`,
   `ci-integrations/jenkins/daggerCache.groovy`→`daggerKubernetes.groovy`);
   update env var names, echo strings, action.yml, drone config-extension.
   Keep the two wrapper scripts byte-identical; verify with `cmp`.
7. **Dagger module:** edit `dagger/dagger.json`, `dagger/go.mod`,
   `dagger/main.go` (DaggerCache→DaggerKubernetes, import path, binary out
   name). Run `cd dagger && dagger develop` to regenerate `dagger.gen.go`.
8. **Config YAML:** `config/config.app.yaml` + `.sample` (env prefix comments,
   path defaults, namespace, raft org, binary name in comments). LEAVE
   `cache.reg/dagger-cache`.
9. **UI:** `ui/src/views/Connect.vue` (`~/.dagger-kubernetes.env`),
   `ui/src/stores/auth.ts` (localStorage keys), `ui/package.json` +
   `package-lock.json` (npm name). Rebuild `ui/dist` → `internal/handler/ui-dist`.
10. **Docs:** `README.md`, `AGENTS.md`, `AGENTS.local.md` (release name +
    commands), `CONTRIBUTING.md`, `DAGGER.md`, `docs/README.md`, `docs/design/index.md`,
    all ADRs (author/team, paths, env names, metric names; LEAVE
    `_EXPERIMENTAL_DAGGER_CACHE_CONFIG` and cache-repo paths).
11. **Verify:** run all validation gates above. Fix any A hits the grep
    surfaces; confirm B whitelist is intact.
12. **Local cluster redeploy (per AGENTS.local.md §6):** rebuild image → push →
    `helm uninstall dagger-cache-test -n dagger-cache-test` → `helm install
    dagger-kubernetes-test ./deploy/helm/dagger-kubernetes -n dagger-kubernetes-test ...`
    → rollout → agent + human verification. Update AGENTS.local.md §3/§7 with
    the new release name + endpoints.

## Rollback note

This is a wide breaking rename. To roll back, `git revert` the changeset. The
irreversible parts are: (a) the local Helm release rename (must `helm uninstall
dagger-kubernetes-test` and `helm install dagger-cache-test` to restore), and
(b) the OCI cache repo path is KEPT (B) precisely so existing cache blobs
survive — no cache-data migration is needed. The minting CA / raft CA Secrets
are loaded by content, not by name, so the renamed CA subject fields do not
invalidate existing certs (only newly-issued certs get the new subject). UI
localStorage key rename logs out existing UI users once (they must re-login);
no server-side migration is possible.

## Open questions

None — all design decisions resolved with the user (full breaking rename;
`~/.dagger-cache.env` renamed; DaggerCache module renamed + regenerated;
X-Dagger-Cache headers renamed; daggerCache Jenkins fn renamed).
