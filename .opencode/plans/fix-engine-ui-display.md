# Plan: Fix Engine Fleet UI Display (`/fleet`)

## Goal
The Runners page (`/fleet`) renders blank cards instead of the deployed Dagger
engine fleet. Fix the root cause, harden the backend/frontend, rebuild, and
redeploy to the `dagger-cache-test` namespace.

## Root Cause (verified against code)
`internal/domain/fleet.go` defines `Replica` and `FleetInfo` with **no JSON tags**.
`encoding/json` therefore marshals PascalCase Go field names:

```json
[{"Version":"v0.21.4","STSName":"...","Replicas":2,"ReadyReplicas":2,
  "Ordinals":[{"Name":"...","Ordinal":0,"PodIP":"...","Ready":true,
  "StartedAt":"...","PinnedSessions":0}]}]
```

`ui/src/fleet/Runners.vue` reads camelCase keys (`version.version`,
`version.readyReplicas`, `version.replicas`, `version.ordinals`,
`ordinal.name`, `ordinal.pinnedSessions`, `ordinal.startedAt`). Every access
returns `undefined` → empty titles, `undefined/undefined ready`, empty tables.
The fleet "appears blank" even though the API returns 200 with data.

Secondary bugs found:
1. `Manager.AllFleetInfo()` returns a **nil slice** when there are no versions
   → marshals to JSON `null` (not `[]`). Frontend `fleet.length` then breaks and
   the "No engine fleets" message never shows.
2. `handleFleetInfo` swallows the `AllFleetInfo()` error (no logging) before
   returning 500.
3. `cmd/api/main.go createProvider()` silently falls back to `StubProvider` on
   k8s clientset failure (logs WARN). In a misconfigured deploy the supervisor
   starts "healthy" but `AllVersions()` is always empty → fleet always blank,
   with no visible signal.

## Scope
In scope: domain JSON tags, service nil-slice fix, handler error logging +
provider observability, UI TypeScript types + loading/error/auto-refresh,
rebuild + redeploy + verify.
Out of scope: per-version filtering UI, CORS/Cache-Control header policy
(separate ADR), renaming `Ordinals` field to `Pods` (cross-cutting refactor).

---

## Phase 1 — Diagnose & Fix Root Cause

### 1.1 Add JSON tags to domain fleet types
File: `internal/domain/fleet.go`

Add `json:"..."` tags matching the frontend's camelCase contract exactly:

```go
type Replica struct {
    Name           string    `json:"name"`
    Ordinal        int       `json:"ordinal"`
    Version        string    `json:"version"`
    PodIP          string    `json:"podIP"`
    Ready          bool      `json:"ready"`
    StartedAt      time.Time `json:"startedAt"`
    PinnedSessions int       `json:"pinnedSessions"`
}

type FleetInfo struct {
    Version       string    `json:"version"`
    STSName       string    `json:"stsName"`
    Replicas      int       `json:"replicas"`
    ReadyReplicas int       `json:"readyReplicas"`
    Ordinals      []Replica `json:"ordinals"`
}
```

Keep the Go field name `Ordinals` (do not rename) — only the JSON key changes.
`AcquireResult` is not consumed by the fleet UI; leave its tags as-is (or add
tags opportunistically, but not required).

Edge cases / validation:
- `Ordinals` nil → marshals to `null`. `GetVersionFleet` always appends into
  `info.Ordinals`, so it is nil only when there are zero pods. Acceptable for
  the per-version object; the UI `v-for` over `null` renders nothing (correct).
- `StartedAt` zero value (`time.Time{}`) marshals to `"0001-01-01T00:00:Z"`.
  `k8s_provider.GetReplicas` defaults `startedAt := time.Now()` when
  `pod.Status.StartTime` is nil, so this only happens for the stub or a pod with
  no start time. UI guard added in Phase 2.

### 1.2 Fix nil-slice → `null` in `AllFleetInfo`
File: `internal/service/fleet.go`, function `AllFleetInfo()` (line ~232)

Change:
```go
var infos []domain.FleetInfo
```
to:
```go
infos := make([]domain.FleetInfo, 0, len(versions))
```
so an empty fleet marshals to `[]` instead of `null`. The frontend's
`fleet.length === 0` check then works and the empty-state message renders.

### 1.3 Add TypeScript types and type the API client
File: `ui/src/api/types.ts` — append:

```ts
export interface FleetReplica {
  name: string
  ordinal: number
  version: string
  podIP: string
  ready: boolean
  startedAt: string
  pinnedSessions: number
}

export interface FleetInfo {
  version: string
  stsName: string
  replicas: number
  readyReplicas: number
  ordinals: FleetReplica[]
}
```

File: `ui/src/api/client.ts`
- Add `FleetInfo` to the type import list.
- Change `fetchFleetInfo`:
```ts
export async function fetchFleetInfo(): Promise<FleetInfo[]> {
  const { data } = await api.get('/api/v1/fleet')
  return data as FleetInfo[]
}
```
Guard against `null` from older backends: if `data == null` return `[]`. (After
Phase 1.2 the backend never returns `null`, but keep the guard for safety:
`return (data as FleetInfo[] | null) ?? []`.)

### 1.4 Fix `Runners.vue` to use typed data
File: `ui/src/fleet/Runners.vue`

Replace the script block to use `FleetInfo` typing (full rewrite in Phase 2
adds loading/error/auto-refresh). Minimal change here: type the ref:
```ts
import { ref, onMounted } from 'vue'
import { fetchFleetInfo } from '@/api/client'
import type { FleetInfo } from '@/api/types'

const fleet = ref<FleetInfo[]>([])
```
Template already uses the correct camelCase keys; no template change needed
once the backend emits camelCase JSON.

---

## Phase 2 — UI Improvements

File: `ui/src/fleet/Runners.vue` (full rewrite of `<script setup>` and template
additions).

### 2.1 State
```ts
import { ref, onMounted, onUnmounted } from 'vue'
import { fetchFleetInfo } from '@/api/client'
import type { FleetInfo } from '@/api/types'

const REFRESH_MS = 10_000

const fleet = ref<FleetInfo[]>([])
const loading = ref(true)
const error = ref<string | null>(null)
let timer: number | undefined
```

### 2.2 Load function (shared by mount + poll)
```ts
async function load(): Promise<void> {
  try {
    fleet.value = await fetchFleetInfo()
    error.value = null
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'Failed to load fleet'
  } finally {
    loading.value = false
  }
}
```

### 2.3 Lifecycle
```ts
onMounted(() => {
  void load()
  timer = window.setInterval(() => void load(), REFRESH_MS)
})
onUnmounted(() => {
  if (timer !== undefined) window.clearInterval(timer)
})
```

### 2.4 Template additions
- Loading state: `v-if="loading"` spinner/skeleton above the list.
- Error state: `v-if="error"` red banner with `{{ error }}` and a "Retry"
  button calling `load()`.
- Empty state: keep existing `v-if="fleet.length === 0"` (now reachable because
  backend returns `[]`).
- Uptime guard: `formatTime` should return `'-'` when input is empty or parses
  to a non-finite date:
  ```ts
  function formatTime(t: string): string {
    if (!t) return '-'
    const d = new Date(t)
    if (isNaN(d.getTime())) return '-'
    const diff = (Date.now() - d.getTime()) / 1000
    if (diff < 0) return '-'           // future/zero-time → unknown
    if (diff < 60) return `${Math.floor(diff)}s`
    if (diff < 3600) return `${Math.floor(diff / 60)}m`
    if (diff < 86400) return `${Math.floor(diff / 3600)}h`
    return `${Math.floor(diff / 86400)}d`
  }
  ```

Edge cases handled: API failure (error banner, stale `fleet` retained), null
response (client guard → `[]` → empty state), zero-time `startedAt` → `'-'`,
poll continuing after error (keeps retrying every 10s).

---

## Phase 3 — Backend Improvements

### 3.1 Log errors in `handleFleetInfo`
File: `internal/handler/server.go`, `handleFleetInfo` (line ~609)

```go
func (s *Server) handleFleetInfo(_ context.Context, c *app.RequestContext) {
    if !s.requireAuth(c) {
        return
    }
    infos, err := s.fleetManager.AllFleetInfo()
    if err != nil {
        s.logger.WithError(err).Error("fleet info unavailable")
        writeError(c, consts.StatusInternalServerError, "fleet unavailable")
        return
    }
    writeJSON(c, infos)
}
```
`AllFleetInfo` currently swallows per-version errors (logs them inside the
loop). That is intentional (one bad version shouldn't fail the whole list).
Keep that behavior; the top-level error path here is for `AllVersions()`
failures only. No change to `AllFleetInfo` error semantics beyond 1.2.

### 3.2 Make the stub-provider fallback observable
File: `cmd/api/main.go`, `createProvider()` (line ~413)

Change the fallback log from `Warn` to `Error` and add a structured field so
operators can grep it:
```go
clientset, err := newK8sClientset()
if err != nil {
    logger.WithError(err).WithField("fleet_provider", "stub").Error(
        "k8s clientset unavailable; falling back to in-memory stub provider — " +
            "engine fleet will be empty and provisioning will not persist")
    return repository.NewStubProvider(), nil
}
```
Rationale: keep the fallback (dev ergonomics, local runs without a cluster)
but make it loud. Do NOT fail-fast — that would break local development and
existing tests.

Optional (recommended) observability: add a Prometheus gauge so the active
provider is visible in `/metrics`. File: `internal/observ/metrics.go` (read it
first to match the existing pattern). Add:
```go
FleetProviderInfo *prometheus.GaugeVec // labels: type
```
Set it once in `createProvider` after choosing the provider:
`metrics.FleetProviderInfo.WithLabelValues("k8s"|"stub").Set(1)`. This requires
threading `metrics` into `createProvider` (currently it is created after the
provider in `run()`). Reorder: create `metrics` before `createProvider`, or set
the gauge in `run()` after both are constructed. Mark this task as
recommended-but-optional; if `observ.Metrics` edit is risky, ship 3.2 (log) only
and file a follow-up for the metric.

### 3.3 No other nil-pointer / empty-slice issues
Verified: `GetVersionFleet` initializes `info` with `len(replicas)` and
increments `ReadyReplicas` — no nil deref. `GetReplicas` returns `nil, nil` for
a missing version (stub) which `GetVersionFleet` handles (zero replicas). No
additional fixes needed beyond 1.2.

---

## Phase 4 — Build & Deploy

### 4.1 Local verification (before image build)
1. `cd ui && npm ci && npm run build` → produces `ui/dist/`.
2. Copy build output into the embed path:
   `rm -rf internal/handler/ui-dist && cp -r ui/dist internal/handler/ui-dist`
   (the Dockerfile does this in-image; for local `go build` it must exist
   because of `//go:embed all:ui-dist`).
3. `go build ./cmd/api` and `go test ./internal/... ./cmd/...`.
4. `go vet ./...` and `gofmt -l .` (must be clean).

### 4.2 Build the Docker image
From repo root:
```
docker build -t ghcr.io/disaster37/dagger-kubernetes:dev .
```
The Dockerfile (multi-stage) builds the UI, copies it to
`internal/handler/ui-dist/`, and builds `supervisor` + `dagger-cache-ci`.

### 4.3 Make the image available to the cluster
Depends on the local cluster type (the kubeconfig at `/home/user/.kube/home`
points at it). Load the local image so the cluster can pull it:
- kind: `kind load docker-image ghcr.io/disaster37/dagger-kubernetes:dev`
- minikube: `minikube image load ghcr.io/disaster37/dagger-kubernetes:dev`
- k3d: `k3d image import ghcr.io/disaster37/dagger-kubernetes:dev`
If unreachable, push to a registry the cluster can pull and use that tag.

### 4.4 Helm deploy
```
helm upgrade --install dagger-kubernetes \
  ./deploy/helm/dagger-kubernetes \
  --namespace dagger-cache-test --create-namespace \
  --kubeconfig /home/user/.kube/home \
  --set supervisor.image.tag=dev \
  --set supervisor.image.pullPolicy=Always \
  --set supervisor.replicaCount=1
```
Notes:
- `supervisor.image.pullPolicy=Always` ensures the cluster re-pulls the local
  tag each rollout (avoids stale image caching).
- `replicaCount=1` avoids SQLite PVC contention during verification.
- The chart's configmap sets `fleet.namespace` to the release namespace
  (`dagger-cache-test`), so engine StatefulSets/Pods are created and listed
  there. RBAC is namespaced (Role/RoleBinding) to the same namespace.

### 4.5 Verify
1. Pods ready:
   `kubectl --kubeconfig /home/user/.kube/home -n dagger-cache-test get pods -l app.kubernetes.io/name=dagger-kubernetes`
2. Get an auth token (the API requires `requireAuth`):
   - Read the bootstrap admin password from supervisor logs:
     `kubectl ... logs deploy/dagger-kubernetes-dagger-kubernetes | grep "bootstrap admin created"`
   - `TOKEN=$(curl -sk https://<svc>/api/v1/auth/login -d '{"username":"admin","password":"..."}' | jq -r .access_token)`
   - Or port-forward: `kubectl -n dagger-cache-test port-forward svc/dagger-kubernetes-dagger-kubernetes 8080:80`
     then `curl -sk https://localhost:8080/...` (note: control plane serves
     HTTPS; readiness probe uses HTTPS).
3. Hit the fleet API:
   `curl -sk -H "Authorization: Bearer $TOKEN" https://localhost:8080/api/v1/fleet`
   Expect `[]` (empty but valid array) when no engines exist.
4. Seed a visible fleet without running a full pipeline — create a dummy
   StatefulSet with the engine labels so `AllVersions()` returns it:
   ```
   kubectl --kubeconfig /home/user/.kube/home -n dagger-cache-test apply -f - <<'EOF'
   apiVersion: apps/v1
   kind: StatefulSet
   metadata:
     name: dagger-engine-v0-21-4
     namespace: dagger-cache-test
     labels: {app: dagger-engine, version: v0.21.4}
   spec:
     serviceName: dagger-engine-v0-21-4
     replicas: 0
     selector: {matchLabels: {app: dagger-engine, version: v0.21.4}}
     template:
       metadata: {labels: {app: dagger-engine, version: v0.21.4}}
       spec:
         containers: [{name: engine, image: registry.dagger.io/engine:v0.21.4}]
   EOF
   ```
   Then `GET /api/v1/fleet` should return one entry with `replicas:0`,
   `readyReplicas:0`, `ordinals:[]`. Scale it (`kubectl scale sts
   dagger-engine-v0-21-4 --replicas=1`) and re-query to see a populated
   `ordinals` array with camelCase keys.
5. UI check: open the SPA in a browser (via ingress or port-forward to the
   control-plane service), log in, navigate to `/fleet`. Confirm:
   - A card titled `v0.21.4` appears with `0/1 ready` (or `1/1` once ready).
   - The pod row shows name, ordinal, Ready/Down badge, sessions, uptime.
   - Auto-refresh: scale the STS and watch the table update within 10s without
     a manual reload.
   - Stop the supervisor (or seed a bad token) and confirm the error banner
     renders with a Retry button.
6. Confirm the stub-fallback log line is absent (we want the k8s provider). If
   present, debug RBAC: `kubectl auth can-i list statefulsets -n
   dagger-cache-test --as system:serviceaccount:dagger-cache-test:dagger-kubernetes-dagger-kubernetes --kubeconfig /home/user/.kube/home`.

### 4.6 Cleanup
Delete the dummy StatefulSet after verification:
`kubectl ... -n dagger-cache-test delete sts dagger-engine-v0-21-4`.

---

## Test Approach

### Backend unit tests (standard `testing`, table-driven, no testify)
File: `internal/service/fleet_test.go` — add:
- `TestAllFleetInfoEmptyReturnsEmptySlice`: stub provider, no versions; assert
  `len(infos) == 0` and `infos != nil` (so JSON marshals to `[]`). Marshal with
  `encoding/json` and assert the output is `[]`, not `null`.
- `TestAllFleetInfoJSONTags`: stub provider with one version + one replica;
  marshal `[]FleetInfo` and assert keys `version`, `stsName`, `replicas`,
  `readyReplicas`, `ordinals`, and nested `name`, `ordinal`, `podIP`, `ready`,
  `startedAt`, `pinnedSessions` exist (use `map[string]any` decode). Assert
  PascalCase keys (`Version`, `ReadyReplicas`) are absent.

File: `internal/handler/server_test.go` — add:
- `TestHandleFleetInfoEmpty`: assert 200 and body `[]`.
- `TestHandleFleetInfoJSONShape`: provision a stub replica, assert 200 and
  decoded body has camelCase keys (guard against regression of the root cause).
- `TestHandleFleetInfoError`: inject a provider that returns an error from
  `AllVersions` (use a tiny in-test stub implementing `domain.FleetProvider`),
  assert 500 and that the error is logged (capture logger output via
  `logrus.New()` + a `bytes.Buffer` and assert the message is present).

Use the existing `newTestServer`/`newTestEngine`/`ut.PerformRequest` harness.
Test logger: `logrus.New()` with `Out: io.Discard` per AGENTS.md, except the
error-logging test which writes to a buffer.

### Frontend
- `npm run typecheck` (vue-tsc) must pass with the new `FleetInfo` types.
- Manual verification per 4.5 (no frontend unit-test framework is configured;
  do not add one).

### Coverage target
100% for touched packages (`internal/domain`, `internal/service`,
`internal/handler`) per AGENTS.md.

---

## Risks & Notes
- **JSON tag change is a public API contract change.** Any external consumer
  of `/api/v1/fleet` expecting PascalCase will break. Searched the repo: only
  `Runners.vue` and tests consume it. If external Dagger-Cloud-compatible
  clients rely on this endpoint, document the change in `docs/README.md` and
  add an ADR in `docs/design/`. (Mark as a doc task if such consumers exist;
  none found in-repo.)
- **`Ordinals` naming**: the field semantically holds pods/replicas. Renaming
  to `Pods` is a larger refactor across service/handler/tests and is
  explicitly out of scope; only the JSON key is aligned here.
- **Auth required for `/api/v1/fleet`**: verification must log in first. The UI
  client already attaches the Bearer token via the axios interceptor.
- **HTTPS control plane**: the readiness/liveness probes and the served API
  use HTTPS when TLS certs are configured (embedded provider). `curl` needs
  `-k` unless the CA is trusted locally.
- **Stub fallback** is intentionally kept (dev ergonomics); only its logging is
  escalated. If a stricter policy is later desired, file a separate ADR.

## Validation Checklist
- [ ] `go test ./internal/... ./cmd/...` passes, 100% coverage on touched pkgs
- [ ] `go vet ./...` clean; `gofmt -l .` empty
- [ ] `npm run typecheck` passes
- [ ] `GET /api/v1/fleet` returns `[]` (not `null`) when empty
- [ ] `GET /api/v1/fleet` returns camelCase keys; PascalCase absent
- [ ] `/fleet` page shows a populated card after seeding a dummy STS
- [ ] Auto-refresh updates the table within 10s after scaling
- [ ] Error banner + Retry render when the API fails
- [ ] Stub-fallback log is absent in the deployed pod's logs
- [ ] `docs/README.md` + ADR updated if the JSON contract is considered public
