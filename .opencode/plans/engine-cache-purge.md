# Plan: Per-engine-version BuildKit local-cache purge via the raw dagql prune API (in-process session client) + immediate S3 sync

## 1. Overview / problem statement

Dagger engine pods keep a BuildKit **local cache** on their per-pod PVC at
`/var/lib/dagger/worker` (`metadata_v2.db` + `containerdmeta.db` + `workerid` +
a content-addressed `content/blobs/sha256/...` store). Today there is no way to
reclaim it short of deleting PVCs by hand. The operator wants the equivalent of
`dagger core engine local-cache prune` (with `useDefaultPolicy=false`, i.e. a
**full purge of releasable cache**) run **per engine StatefulSet** (per engine
version, e.g. `dagger-engine-v0-19-0`), across **every** pod in that
StatefulSet, **without taking the fleet down**.

The operator explicitly rejected the previous `scale-to-zero → delete PVCs →
scale back up` design (during scale-to-zero the version has no engine
available). The revised design must:

1. Invoke the **real engine prune API** — the dagql call
   `{ engine { localCache { prune(useDefaultPolicy: false) } } }` — directly
   against each **running** engine pod, **from the supervisor's own Go
   process** (no shelling out to the `dagger` CLI). No scale-to-zero, no PVC
   deletion, engines stay available.
2. Run the prune on **all pods of the version's StatefulSet concurrently**.
3. Because the pods remain alive with the pruned (now-empty) local cache, the
   shared S3 snapshot follows naturally through the existing `cache-sync`
   sidecar — but the feature must still guarantee an **immediate** sync "just
   after" the prune (the original ask: "purge cache … and sync cache juste
   after that"), i.e. an immediate sidecar push right after its pod's prune
   completes.

> **Scope note (unchanged).** This is NOT `POST /api/v1/cache/purge` (which
> deletes the remote BuildKit `cache/` prefix via `S3CacheGC.Purge`). This
> feature operates on the **worker-snapshot / local-cache** side. The two
> purges are orthogonal and must not be confused in code, docs, or UI.

---

## 2. Design decisions (answers to every design question)

### Q1 — How to invoke `engine.localCache.prune()` on a running engine pod

**Decision (REVISED, route B): the supervisor speaks the engine's session
protocol directly over plaintext TCP in-process — a minimal HTTP client that
POSTs the raw dagql query to the engine's `/query` endpoint carrying a
synthetic `X-Dagger-Client-Metadata` header. No `dagger` CLI subprocess, no
CLI download, no gRPC/connect-rpc dependency.**

This is "make the same call as the dagger CLI, directly over TCP" (route B).
Route A ("import `dagger.io/dagger`") was investigated and **rejected with hard
evidence** — see "Why route A is rejected" below.

#### Verified facts from the Dagger source (`dagger/dagger` @ v0.19.0)

The engine's session protocol is **not** gRPC/connect for the main client. It
is **plain HTTP (HTTP/1.1 or h2c-cleartext) with client metadata carried in one
HTTP header**. Evidence:

1. **The engine serves a single h2c handler on `:9999`** that routes gRPC and
   session-HTTP by content-type (`cmd/engine/main.go`):

   ```go
   http2Server := &http2.Server{}
   httpServer := &http.Server{
       ReadHeaderTimeout: 30 * time.Second,
       Handler: h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
           if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("content-type"), "application/grpc") {
               grpcServer.ServeHTTP(w, r) // buildkit control plane only
               return
           }
           srv.ServeHTTP(w, r) // the dagger session HTTP surface
       }), http2Server),
   }
   ```

   `h2c.NewHandler` accepts **both** h2c (HTTP/2 prior-knowledge) and plain
   HTTP/1.1 requests, so a stdlib `net/http` client (HTTP/1.1) is sufficient.

2. **Session/client metadata is one header** (`engine/opts.go`):

   ```go
   ClientMetadataMetaKey = "X-Dagger-Client-Metadata"

   func ClientMetadataFromHTTPHeaders(h http.Header) (*ClientMetadata, error) {
       bs, err := base64.StdEncoding.DecodeString(h.Get(ClientMetadataMetaKey))
       ...
       json.Unmarshal(bs, m)
   }
   ```

   `ClientMetadata` JSON fields: `client_id`, `client_secret_token`,
   `session_id`, `client_hostname`, `client_stable_id`, `client_version`,
   `labels`, …

3. **The session is created lazily and is NOT pre-registered/authorized**
   (`engine/server/session.go`): `srv.ServeHTTP` decodes the metadata header,
   checks `engine.CheckVersionCompatibility(NormalizeVersion(ClientVersion),
   MinimumClientVersion)`, then `getOrInitClient` requires only that
   `SessionID`, `ClientID`, `ClientSecretToken` are **non-empty**; the first
   client of a session becomes the *main* client. There is no lookup against
   any external registry. **The engine never consults the supervisor's
   `domain.SessionStore`/`SessionRegistry`** (those are a separate concept keyed
   by client-cert fingerprint, used only for L4 data-plane routing — see Q3).

4. **Query endpoint is `POST /query`** (`engine/consts.go`):
   `QueryEndpoint = "/query"` (also `/init`, `/shutdown`,
   `/sessionAttachables`). A single `POST /query` self-initialises the session;
   no `/init` is required for the main client (the CLI only calls `/init` in the
   *nested* session path; buildkit attachables + telemetry SSE subscriptions are
   for filesync/secret/log streaming and are **not** needed for a pure query).

5. **The prune dagql field exists and requires the main client**
   (`core/schema/engine.go`):

   ```go
   dagql.Fields[*core.EngineCache]{
       dagql.Func("prune", s.cachePrune).
           DoNotCache("Mutates mutable state").
           Doc("Prune the cache of releaseable entries").
           Args(dagql.Arg("useDefaultPolicy").Doc("... if false, prune the whole cache of any releasable entries.")),
   }

   func (s *engineSchema) cachePrune(... args struct { UseDefaultPolicy bool `default:"false"` }) {
       if err := query.RequireMainClient(ctx); err != nil { return void, err }
       _, err = query.PruneEngineLocalCacheEntries(ctx, args.UseDefaultPolicy)
   }
   ```

   ⇒ `useDefaultPolicy:false` = "prune the whole cache of any releasable
   entries"; the caller **must be the main client** — guaranteed by creating a
   fresh synthetic session per prune (first client = main client).

6. **Version floor / compatibility** (`engine/version.go` @ v0.19.0):
   `MinimumClientVersion = "v0.19.0"`; each engine only enforces a *minimum*
   client version. Setting the request's `client_version` to the pod's exact
   StatefulSet version (e.g. `v0.19.0`) satisfies the check and selects the
   engine's own schema view — exactly how the version-matched CLI behaves.

7. **The engine port is plaintext in this repo** (re-verified):
   - `internal/repository/k8s_provider.go` builds
     `--addr=tcp://0.0.0.0:9999` (`enginePort = 9999`), **no** `--tlscert`/
     `--tlskey` (TLS is only enabled when those flags are set, and they are
     not).
   - `internal/handler/server.go` `serveDataTunnel` already dials the pod IP
     raw: `net.DialTimeout("tcp", net.JoinHostPort(targetIP, "9999"),
     5*time.Second)` and `io.Copy`s bytes both ways. The supervisor's mTLS
     boundary is the data plane; **the engine hop is plaintext** — the pruner
     needs no client certificate.

   The supervisor can reach engine pod IPs (same data path; no NetworkPolicy
   blocks supervisor→engine).

#### Why route A (`dagger.io/dagger`) is rejected (decisive evidence)

- At v0.19.0 the SDK was already split into submodules; the public API is
  `dagger.Connect(ctx, dagger.WithConn(conn engineconn.EngineConn))` — there is
  **no** `WithEngineConn`, and `EngineConn` is **not** a raw `net.Conn`. It is
  an HTTP transport:

  ```go
  // dagger.io/dagger/engineconn (v0.19.0)
  type EngineConn interface {
      graphql.Doer            // Do(req *http.Request) (*http.Response, error)
      Host() string
      Close() error
  }
  ```

- `engineconn.Get(ctx, cfg)` returns `cfg.Conn` verbatim when set (so it does
  "skip engine provisioning"), but the caller must implement the full
  HTTP+session transport itself — i.e. **route A requires exactly the same
  HTTP/session work as route B**, plus `dagger.io/dagger` and its transitive
  tree (genqlient, `vektah/gqlparser`, OTel telemetry, `querybuilder`,
  `engineconn`) for a single static query. `Client.Do(ctx, *Request, *Response)`
  exists (`Request{Query, Variables, OpName}`, `Response{Data, Extensions,
  Errors}`) but it just wraps a genqlient HTTP client over *our* `EngineConn`.
  The SDK's curated query builder does **not** expose `Engine.localCache`
  (`EngineCache` is dagql-internal), so route A would still need `Client.Do`
  with the raw query string. Net effect: route A = route B + a large dependency
  for no protocol benefit. **Rejected.**

- The CLI itself has no raw-TCP runner driver either: its connectors are
  `bin`/`docker-container`/`docker-image`/`embedded`/`cloud`; the `cloud` driver
  speaks `dagger-cloud://` to the supervisor's control plane (the path the old
  plan was trying to bypass). There is no public `tcp://pod-ip:9999` connector.

**Exact per-pod call (supervisor process, concurrent):**

```text
POST http://<podIP>:9999/query            (HTTP/1.1, plaintext TCP)
Content-Type: application/json
X-Dagger-Client-Metadata: base64({
    "client_id":            "<16-byte random hex>",
    "client_secret_token":  "<16-byte random hex>",
    "session_id":           "<16-byte random hex>",
    "client_hostname":      "supervisor",
    "client_stable_id":     "<16-byte random hex>",
    "client_version":       "<pod's engine version, e.g. v0.19.0>",
    "labels": {}
})

{"query": "{ engine { localCache { prune(useDefaultPolicy: false) } } }"}
```

Success ⇒ HTTP 200 with a GraphQL body whose `errors` array is empty; any
non-200 status or non-empty `errors` is a per-pod failure. Fresh random IDs are
generated with `crypto/rand` (stdlib) — no `google/uuid` dependency needed.

### Q2 — How to trigger the immediate post-prune push

**Decision (unchanged): after a pod's prune succeeds, the supervisor immediately
execs `supervisor cache-sync push-once` inside that pod's `cache-sync` sidecar
container (K8s `pods/exec`).** The new `push-once` subcommand loads the same
`CACHE_SYNC_*` env as `serve`, builds the `S3SnapshotStore`, and calls
`pushWorkerSnapshotS3` once, then exits.

- **Why the sidecar, not the supervisor deleting a prefix:** the shared snapshot
  must reflect the *pruned (empty)* state. The faithful way is to push the
  pod's now-empty worker dir — the exact mechanism the sidecar already uses.
  A prefix delete is a different (stronger, cold-restart) semantic and can no
  longer be reached "just after a prune" on a live pod. Pushing an empty
  snapshot via the existing store keeps the store's own invariants intact.
- **Why `push-once`, not a SIGUSR1 handler:** a one-shot subcommand is simpler,
  idempotent, and testable; a signal handler adds a new signal path + PID
  plumbing to the long-running `serve` loop for no benefit. (`runCacheSyncServe`
  already serializes pushes with a mutex, but `push-once` runs as a **separate
  process**, so it must tolerate a concurrent periodic push — the store already
  documents last-writer-wins `meta.tar.gz` + idempotent blob uploads as safe.)
- **RBAC:** `deploy/helm/dagger-kubernetes/templates/rbac.yaml` **already
  grants** `pods/exec` verbs `["get", "create"]`. **No RBAC change is
  required.** (The `create` verb is what `pods/exec` actually uses.)

### Q3 — Session / pinned-session semantics

Two distinct "session" concepts are involved; both are resolved:

**Q3a — Which Dagger session does the pruner use?** The pruner **creates a
fresh synthetic Dagger session per pod** (new `session_id` + `client_id` +
`client_secret_token` every call), and never touches the supervisor's
`domain.SessionStore`/`SessionRegistry`. Evidence: the engine creates Dagger
sessions lazily from `X-Dagger-Client-Metadata` (Q1 facts 2–3) and makes the
first client the *main* client, which `engine.localCache.prune` requires via
`query.RequireMainClient` (Q1 fact 5). A fresh synthetic session guarantees the
pruner is the main client. The supervisor's `SessionStore`/`SessionRegistry` are
keyed by client-cert fingerprint and exist only to route data-plane tunnels to a
pod (ADR-026); they are unrelated to the engine's `SessionID` and are never
read by the engine.

**Q3b — Allow the purge while the version has pinned (running) sessions.**
Remove the old `ErrPurgeActiveSessions` 409 refusal. Rationale + evidence:
`engine.localCache.prune(useDefaultPolicy:false)` prunes only **releasable**
entries ("Prune the cache of releaseable entries"; `useDefaultPolicy=false` =
"prune the whole cache of any releasable entries"). Cache held by an in-flight
build/session is not releasable, so a live prune does not corrupt running
pipelines — it is the same operation Dagger itself performs under load. The
operator explicitly wants availability, so refusing on pinned sessions would
defeat the purpose. Live validation (step §9.3) must confirm a prune during an
active pipeline does not fail the pipeline. (Our synthetic session is a separate
`session_id`/`client_id` from any pinned CLI session, so there is no
token/session collision.)

### Q4 — API surface

| Item | Value |
|---|---|
| Purge route | `POST /api/v1/fleet/:version/purge-cache` |
| Status route | `GET /api/v1/fleet/:version/purge-cache` (last recorded status) |
| Auth | **admin-only** on both (`s.adminOnly(...)`) |
| Body | none (version in path) |
| Success | `200` with `EngineCachePurgeResult` JSON |

`EngineCachePurgeResult` JSON shape (see §3 for the Go structs):

```json
{
  "version": "v0.19.0",
  "state": "completed",
  "started_at": "2026-09-14T10:00:00Z",
  "finished_at": "2026-09-14T10:01:20Z",
  "replicas": 3,
  "pods": [
    { "pod_name": "dagger-engine-v0-19-0-0", "ordinal": 0, "pruned": true,  "synced": true  },
    { "pod_name": "dagger-engine-v0-19-0-1", "ordinal": 1, "pruned": true,  "synced": true  },
    { "pod_name": "dagger-engine-v0-19-0-2", "ordinal": 2, "pruned": false, "synced": false,
      "error": "prune 10.0.0.12: unexpected status 500: incompatible client version ..." }
  ],
  "message": "2 of 3 pods pruned; 1 pod failed"
}
```

**Notes on the shape change vs. the previous plan:**

- `freed_bytes` is **dropped**: the dagql `prune` mutation reports no byte
  count. Space accounting is deliberately out of scope.
- `pvcs_deleted`, `replicas_before/after`, `snapshot_purged`,
  `s3_objects_deleted` are **dropped** (no PVCs are deleted, no prefix is
  deleted).
- Per-pod `pruned`/`synced`/`error` carry the partial-failure detail the
  operator asked for.

**Validation:** `:version` must satisfy `domain.IsFullVersion(version)` → else
`400 "invalid version"`. Then the service verifies the StatefulSet exists →
`404 "engine fleet not found"`.

**Error mapping** (`writeFleetPurgeError`):

| Sentinel (domain) | HTTP | message |
|---|---|---|
| `ErrEngineFleetNotFound` | 404 | `engine fleet not found` |
| `ErrPurgeInProgress` | 409 | `purge already in progress` |
| other | 500 | `purge failed` (logged) |

(`ErrPurgeActiveSessions` is removed — see Q3.)

**Idempotency & concurrency:** serialized **per version** via an in-memory
`active` set + `status` map. A concurrent purge of the same version returns 409.
Re-running after a completed/failed purge is allowed (pruning an already-pruned
cache and pushing an already-empty snapshot are both no-ops). Different versions
can purge in parallel.

### Q5 — UI surface

**Placement: Runners page (`ui/src/fleet/Runners.vue`)** — unchanged. The
MagicCache "Purge cache" button still purges the **global remote** cache and is
unrelated.

**Per-version card** gains an admin-gated "Purge cache" button:

- gated by `auth.isAdmin` (`useAuthStore`);
- `window.confirm(...)` before firing;
- `:disabled` + "Purging…" while in flight (per-version boolean map);
- success → inline per-pod summary ("Pruned 3/3 pods, synced 3/3") plus the
  per-pod failure list when any `pod.error` is set, then `load()` refresh;
- failure (409/404/500) → inline `e.response?.data?.message || 'Purge failed'`.

### Q6 — State / status

**In-memory, not persisted** (unchanged): `EngineCachePurgeService` keeps a
`map[string]*EngineCachePurgeResult` (last result) + `map[string]bool`
(in-flight) under one mutex. A supervisor restart loses them (acceptable: the
fleet is untouched; re-running is idempotent). POST blocks until completion (or
per-pod timeouts fire); GET exists for out-of-band inspection.

---

## 3. Data structures & signatures

### `internal/domain/cache_purge.go` (new)

```go
package domain

import (
	"context"
	"errors"
)

// EngineCachePurgeResult is the response/status of a per-version local-cache
// purge. State is "running" | "completed" | "failed".
type EngineCachePurgeResult struct {
	Version    string                 `json:"version"`
	State      string                 `json:"state"`
	StartedAt  string                 `json:"started_at"`            // RFC3339 UTC
	FinishedAt string                 `json:"finished_at,omitempty"` // RFC3339 UTC
	Replicas   int                    `json:"replicas"` // pods targeted
	Pods       []EnginePodPurgeResult `json:"pods"`
	Message    string                 `json:"message,omitempty"`
}

// EnginePodPurgeResult is the per-pod outcome of the prune + immediate sync.
type EnginePodPurgeResult struct {
	PodName string `json:"pod_name"`
	Ordinal int    `json:"ordinal"`
	Pruned  bool   `json:"pruned"` // engine.localCache.prune succeeded on this pod
	Synced  bool   `json:"synced"` // immediate cache-sync push-once succeeded
	Error   string `json:"error,omitempty"`
}

// EnginePruner prunes one running engine pod's BuildKit local cache via the
// engine dagql API (engine.localCache.prune with useDefaultPolicy=false),
// spoken in-process over the engine's plaintext session HTTP port.
type EnginePruner interface {
	// PruneLocalCache prunes podIP's local cache (engine listens on 9999).
	// version is the pod's engine version, reported as the client_version so
	// the engine's version-compatibility check passes and its own schema view
	// is selected. It must leave the pod running and only remove releasable
	// cache.
	PruneLocalCache(ctx context.Context, podIP, version string) error
}

// SidecarSyncPusher triggers one immediate worker-snapshot push in an engine
// pod's cache-sync sidecar.
type SidecarSyncPusher interface {
	// PushOnce runs `supervisor cache-sync push-once` in the pod's cache-sync
	// sidecar container.
	PushOnce(ctx context.Context, podName, version string) error
}

// EngineCachePurger purges the local cache of every engine pod in a version's
// StatefulSet (concurrently) and syncs the pruned snapshot to S3 immediately.
type EngineCachePurger interface {
	Purge(ctx context.Context, version string) (*EngineCachePurgeResult, error)
	Status(version string) (*EngineCachePurgeResult, bool)
}

// Sentinel errors mapped by the handler to HTTP responses.
var (
	// ErrEngineFleetNotFound: no StatefulSet exists for the version.
	ErrEngineFleetNotFound = errors.New("engine fleet not found")
	// ErrPurgeInProgress: a purge for the version is already running.
	ErrPurgeInProgress = errors.New("purge already in progress")
)
```

(The `EnginePruner` interface is unchanged from the previous revision — only
its implementation changes, from CLI-exec to an in-process HTTP session client.)

### `internal/repository/engine_pruner.go` (new — the in-process `EnginePruner`)

```go
package repository

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const (
	enginePrunePort    = 9999            // engine session HTTP port (plaintext); mirrors repository.enginePort
	enginePruneTimeout = 10 * time.Minute // hard backstop; the caller's ctx is the primary bound
)

// engineClientMetadata is the minimal Dagger engine.ClientMetadata the engine
// reads from the X-Dagger-Client-Metadata header (engine/opts.go). It is
// JSON-marshalled then base64-encoded, exactly as the Dagger CLI does.
type engineClientMetadata struct {
	ClientID          string            `json:"client_id"`
	ClientSecretToken string            `json:"client_secret_token"`
	SessionID         string            `json:"session_id"`
	ClientHostname    string            `json:"client_hostname"`
	ClientStableID    string            `json:"client_stable_id"`
	ClientVersion     string            `json:"client_version"`
	Labels            map[string]string `json:"labels"`
}

// SessionEnginePruner prunes a running engine pod's BuildKit local cache by
// speaking the engine's session HTTP protocol directly over plaintext TCP —
// the same wire call the Dagger CLI makes — instead of shelling out to the CLI.
type SessionEnginePruner struct {
	client *http.Client
	port   int // engine session port (9999 in production; tests override it)
}

var _ domain.EnginePruner = (*SessionEnginePruner)(nil)

func NewSessionEnginePruner() *SessionEnginePruner {
	return &SessionEnginePruner{client: newPruneHTTPClient(), port: enginePrunePort}
}

func newPruneHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:             nil, // bypass HTTP_PROXY: the engine is a pod IP
			DisableKeepAlives: true, // one-shot admin op; no conn reuse, so a deleted pod's socket is never held
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
			},
		},
		Timeout: enginePruneTimeout,
	}
}

// PruneLocalCache implements domain.EnginePruner. It POSTs the raw dagql
// `{ engine { localCache { prune(useDefaultPolicy: false) } } }` to
// http://podIP:port/query with a fresh synthetic session, and treats any
// non-200 status or non-empty GraphQL errors array as a failure.
func (p *SessionEnginePruner) PruneLocalCache(ctx context.Context, podIP, version string) error {
	// 1. fresh synthetic session identity (crypto/rand 16-byte hex per field)
	// 2. metaBytes := json.Marshal(engineClientMetadata{... ClientVersion: version, ...})
	// 3. body := json.Marshal(map[string]any{"query": "{ engine { localCache { prune(useDefaultPolicy: false) } } }"})
	// 4. POST fmt.Sprintf("http://%s:%d/query", podIP, p.port) with
	//    headers Content-Type: application/json and
	//    X-Dagger-Client-Metadata: base64.StdEncoding.EncodeToString(metaBytes)
	// 5. on non-200: return fmt.Errorf("prune %s: unexpected status %d: %s", podIP, code, body)
	//    on GraphQL errors: return fmt.Errorf("prune %s: %s", podIP, errors[0].Message)
	//    on success: return nil
}
```

Key points the implementer must honor:

- **IDs via `crypto/rand`** (`newID()` returns `hex.EncodeToString(16 random
  bytes)`); three distinct values for `client_id`, `session_id`,
  `client_secret_token`, plus one for `client_stable_id`. No new dependency.
- **`client_version` = the pod's `version` argument** (the exact StatefulSet
  version, e.g. `v0.19.0`). This satisfies the engine's `MinimumClientVersion`
  check (equal semver) and selects the engine's own schema view. Do **not** hard
  code a single version, and do **not** send a normalized/latest.
- **Error wrapping** uses `%w` and includes `podIP` + server status/body (truncated
  to ~1KB) for diagnosability. No secret material is in the error (the token is
  never logged).
- **Close:** the response body is always closed (`defer resp.Body.Close()`);
  `DisableKeepAlives` means the transport closes the socket after the response,
  which is exactly when the engine tears the synthetic session down (its
  `activeCount` returns to 0 and it removes the session — harmless; prune already
  completed).

### `internal/repository/exec_sync_pusher.go` (new — the `SidecarSyncPusher`)

Unchanged from the previous revision:

```go
package repository

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// ExecSyncPusher triggers `supervisor cache-sync push-once` in an engine pod's
// cache-sync sidecar via the K8s exec subresource.
type ExecSyncPusher struct {
	clientset kubernetes.Interface
	restCfg   *rest.Config
	namespace string
	container string // "cache-sync"
}

var _ domain.SidecarSyncPusher = (*ExecSyncPusher)(nil)

func NewExecSyncPusher(clientset kubernetes.Interface, restCfg *rest.Config, namespace string) *ExecSyncPusher {
	return &ExecSyncPusher{clientset: clientset, restCfg: restCfg, namespace: namespace, container: "cache-sync"}
}

func (p *ExecSyncPusher) PushOnce(ctx context.Context, podName, version string) error {
	req := p.clientset.CoreV1().RESTClient().Post().
		Resource("pods").Name(podName).Namespace(p.namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command:   []string{"supervisor", "cache-sync", "push-once"},
			Container: p.container,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(p.restCfg, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("build exec request for %s: %w", podName, err)
	}
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: io.Discard, Stderr: io.Discard,
	}); err != nil {
		return fmt.Errorf("exec push-once in %s: %w", podName, err)
	}
	return nil
}
```

(`k8s.io/client-go/tools/remotecommand` and `kubernetes/scheme` are already
transitive deps of `client-go`; no new module is pulled.)

### `cmd/api/cache_sync.go` (modify — add `push-once`)

Unchanged from the previous revision. Add to the `cache-sync` command's
`Subcommands`:

```go
{
	Name:   "push-once",
	Usage:  "push the worker-dir snapshot once and exit (immediate sync after a local-cache purge)",
	Action: runCacheSyncPushOnce,
},
```

```go
// runCacheSyncPushOnce pushes the worker-dir snapshot once and exits. It is the
// target of the supervisor's pods/exec immediate-sync trigger after a
// local-cache prune. Failures return a non-zero exit so the exec caller can
// surface them; they never block the engine pod (best-effort, like serve).
func runCacheSyncPushOnce(c *cli.Context) error {
	env, err := loadCacheSyncEnv()
	if err != nil {
		return err
	}
	logger := observ.NewLogger("info", "text")

	ctx, cancel := context.WithTimeout(c.Context, cacheSyncOpTimeout)
	defer cancel()

	store, err := newS3SnapshotStore(env, logger)
	if err != nil {
		return err
	}
	return pushWorkerSnapshotS3(ctx, env, store, logger)
}
```

### `internal/service/engine_cache_purge.go` (new — orchestration)

```go
package service

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const (
	pruneConcurrency = 8             // bounded concurrent per-pod prune jobs
	prunePodTimeout  = 10 * time.Minute // dagql prune on a large cache can be slow
	pushOnceTimeout  = 5 * time.Minute  // == cacheSyncOpTimeout
)

// EngineCachePurgeService orchestrates a per-version local-cache purge:
// concurrently prune every pod via the engine dagql API (in-process), then
// immediately trigger each pod's cache-sync push-once. Status is in-memory only.
type EngineCachePurgeService struct {
	provider domain.FleetProvider
	pruner   domain.EnginePruner
	pusher   domain.SidecarSyncPusher
	logger   *logrus.Logger

	mu     sync.Mutex
	active map[string]bool
	status map[string]*domain.EngineCachePurgeResult
}

func NewEngineCachePurgeService(provider domain.FleetProvider, pruner domain.EnginePruner, pusher domain.SidecarSyncPusher, logger *logrus.Logger) *EngineCachePurgeService {
	return &EngineCachePurgeService{
		provider: provider,
		pruner:   pruner,
		pusher:   pusher,
		logger:   logger,
		active:   make(map[string]bool),
		status:   make(map[string]*domain.EngineCachePurgeResult),
	}
}

func (s *EngineCachePurgeService) Status(version string) (*domain.EngineCachePurgeResult, bool) { /* copy under mu */ }

func (s *EngineCachePurgeService) Purge(ctx context.Context, version string) (*domain.EngineCachePurgeResult, error) {
	// guard + running stamp (mu): ErrPurgeInProgress on double-start
	// validate: AllVersions() contains version, else ErrEngineFleetNotFound (failed)
	// replicas := provider.GetReplicas(version)
	// if len(replicas)==0 -> completed, replicas=0, message "no running engine pods; nothing to prune"
	// fan out: sem := make(chan struct{}, pruneConcurrency); one goroutine per replica:
	//   pruneCtx, cancel := context.WithTimeout(ctx, prunePodTimeout)
	//   pruner.PruneLocalCache(pruneCtx, r.PodIP, version)
	//   if prune ok -> pushCtx: pusher.PushOnce(pushCtx, r.Name, version)
	//   record EnginePodPurgeResult{Pruned, Synced, Error} under a mutex
	// sort pods by ordinal; compute aggregate state + message
	// finish("completed", nil) — per-pod failures are reported in Pods[] + Message
}
```

Aggregate semantics: the result is `state:"completed"` whenever the
orchestration ran to completion over all targeted pods (even with per-pod
failures — those live in `pods[].error` and are summarised in `message`).
`state:"failed"` is reserved for precondition failures (fleet not found) and
unexpected orchestration errors, surfaced via the sentinel mapping above. See
§6 "Partial failure".

### `internal/handler/server.go` (modify)

Add to `Deps` + `Server` + `NewServer`:

```go
	EngineCachePurger domain.EngineCachePurger
```

Register routes (next to the other fleet/cache routes in `configure()`):

```go
	h.POST("/api/v1/fleet/:version/purge-cache", s.adminOnly(s.handleFleetPurgeCache))
	h.GET("/api/v1/fleet/:version/purge-cache", s.adminOnly(s.handleFleetPurgeStatus))
```

### `internal/handler/fleet_purge.go` (new)

```go
package handler

import (
	"context"
	"errors"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func (s *Server) handleFleetPurgeCache(ctx context.Context, c *app.RequestContext) {
	version := c.Param("version")
	if !domain.IsFullVersion(version) {
		writeError(c, consts.StatusBadRequest, "invalid version")
		return
	}
	if s.engineCachePurger == nil {
		writeError(c, consts.StatusInternalServerError, "engine cache purge unavailable")
		return
	}
	result, err := s.engineCachePurger.Purge(ctx, version)
	if err != nil {
		s.writeFleetPurgeError(c, err)
		return
	}
	writeJSON(c, result)
}

func (s *Server) handleFleetPurgeStatus(ctx context.Context, c *app.RequestContext) {
	version := c.Param("version")
	if !domain.IsFullVersion(version) {
		writeError(c, consts.StatusBadRequest, "invalid version")
		return
	}
	if s.engineCachePurger == nil {
		writeError(c, consts.StatusInternalServerError, "engine cache purge unavailable")
		return
	}
	result, ok := s.engineCachePurger.Status(version)
	if !ok {
		writeError(c, consts.StatusNotFound, "no purge recorded")
		return
	}
	writeJSON(c, result)
}

func (s *Server) writeFleetPurgeError(c *app.RequestContext, err error) {
	switch {
	case errors.Is(err, domain.ErrEngineFleetNotFound):
		writeError(c, consts.StatusNotFound, "engine fleet not found")
	case errors.Is(err, domain.ErrPurgeInProgress):
		writeError(c, consts.StatusConflict, "purge already in progress")
	default:
		s.logger.WithError(err).Error("fleet purge failed")
		writeError(c, consts.StatusInternalServerError, "purge failed")
	}
}
```

### `cmd/api/main.go` (modify — wiring)

- Change `newK8sClientset()` to also return the `*rest.Config` (needed by
  `ExecSyncPusher`): widen it to `(kubernetes.Interface, *rest.Config, error)`
  and update its one call site (line 120).
- After the fleet/provider wiring, wire the pruner + pusher + service. The
  pruner is now **unconditional** (it only dials pod IPs — no CLI subsystem, no
  S3 config):

```go
	var engineCachePurger domain.EngineCachePurger
	if clientset != nil && provider != nil {
		pruner := repository.NewSessionEnginePruner()
		pusher := repository.NewExecSyncPusher(clientset, restCfg, cfg.Fleet.Namespace)
		engineCachePurger = service.NewEngineCachePurgeService(provider, pruner, pusher, logger)
	}
```

- Add `EngineCachePurger: engineCachePurger,` to the `handler.Deps{...}` literal.

> **Prerequisite change vs. the previous revision.** The purge endpoint no longer
> requires `cli.enabled=true` (the old `CLIService` materialization path is
> gone). It requires only that a Kubernetes clientset + FleetProvider are
> wired (always true in the live deployment) and that the target pods are
> reachable. The post-prune `push-once` still depends on the sidecar's
> `cache.sync` env (which exists whenever the chart's S3 cache is configured).

### UI changes

`ui/src/api/types.ts` (modify — add):

```ts
export interface EnginePodPurgeResult {
  pod_name: string
  ordinal: number
  pruned: boolean
  synced: boolean
  error?: string
}
export interface EngineCachePurgeResult {
  version: string
  state: 'running' | 'completed' | 'failed'
  started_at: string
  finished_at?: string
  replicas: number
  pods: EnginePodPurgeResult[]
  message?: string
}
```

`ui/src/api/client.ts` (modify — add):

```ts
export async function purgeEngineCache(version: string): Promise<EngineCachePurgeResult> {
  const { data } = await api.post(`/api/v1/fleet/${encodeURIComponent(version)}/purge-cache`)
  return data as EngineCachePurgeResult
}
```

`ui/src/fleet/Runners.vue` (modify): per-version admin "Purge cache" button,
per-pod result summary, per §2 Q5.

---

## 4. File-by-file change list

**Create**
1. `internal/domain/cache_purge.go` — result structs, `EnginePruner`,
   `SidecarSyncPusher`, `EngineCachePurger`, sentinel errors.
2. `internal/repository/engine_pruner.go` — `SessionEnginePruner` (in-process
   HTTP session client, route B).
3. `internal/repository/exec_sync_pusher.go` — pods/exec `SidecarSyncPusher`.
4. `internal/service/engine_cache_purge.go` — orchestration + status.
5. `internal/service/engine_cache_purge_test.go` — orchestration unit tests.
6. `internal/repository/engine_pruner_test.go` — unit tests (real pruner against
   a fake session HTTP server; see §7).
7. `internal/handler/fleet_purge.go` — handlers + error mapper.
8. `internal/handler/fleet_purge_test.go` — handler tests.
9. `tests/integration/engine_cache_purge_test.go` — integration test (fake
   pruner + fake pusher).
10. `docs/design/ADR-032-engine-cache-purge.md` — ADR.

**Modify**
11. `cmd/api/cache_sync.go` — add `cache-sync push-once`.
12. `cmd/api/main.go` — `newK8sClientset` returns `*rest.Config`; wire pruner,
    pusher, purge service into `Deps`.
13. `internal/handler/server.go` — `Deps.EngineCachePurger`, `Server` field,
    `NewServer` assignment, two routes.
14. `ui/src/api/types.ts`, `ui/src/api/client.ts`, `ui/src/fleet/Runners.vue`.
15. `docs/README.md` — feature docs (§8).
16. `docs/design/index.md` — add ADR-032 row.

**No change (and no `go.mod` change — route B is stdlib-only):**
`go.mod`/`go.sum`, `internal/service/cli_service.go` (the `MaterializeBinary`
addition from the previous revision is **dropped**), `internal/repository/
stub_provider.go` (no new `FleetProvider` method), `internal/domain/fleet.go`,
`internal/repository/s3_mock_test.go`, `config/` (timeouts are package
constants), and `deploy/helm/dagger-kubernetes/templates/rbac.yaml`
(`pods/exec` get/create already granted).

**Removed vs. the previous two revisions (do NOT reintroduce):**
- `internal/service/engine_local_cache_pruner.go` (`EngineLocalCachePruner`) +
  `cliBinaryMaterializer` + the CLI exec path + `MaterializeBinary` in
  `cli_service.go` + the stdlib gzip/tar helper + their tests.
- `internal/repository/s3_snapshot_purge.go` (`S3SnapshotPurger`).
- `internal/domain/fleet.go` `DeletePVCs` method + `internal/repository/
  k8s_provider.go` `DeletePVCs` + `stub_provider.go` stub.
- `internal/repository/s3_mock_test.go` `RemoveObject` extension.
- `domain.ErrPurgeActiveSessions` / `domain.WorkerSnapshotPurger`.

---

## 5. Implementation order

1. `internal/domain/cache_purge.go` (types + interfaces + errors).
2. `internal/repository/engine_pruner.go` (route B HTTP session client).
3. `internal/repository/exec_sync_pusher.go`; change `cmd/api/main.go`
   `newK8sClientset` to return `*rest.Config`.
4. `cmd/api/cache_sync.go` `push-once`.
5. `internal/service/engine_cache_purge.go`.
6. `internal/handler/server.go` (Deps/field/wiring/routes) +
   `internal/handler/fleet_purge.go`.
7. `cmd/api/main.go` wiring (pruner + pusher + service).
8. Unit tests (§7).
9. `go build ./... && go vet ./... && go test ./...` (fast local gate).
10. UI: `types.ts`, `client.ts`, `Runners.vue`.
11. Integration test `tests/integration/engine_cache_purge_test.go`.
12. Docs + ADR (§8).
13. Full CI gate + live redeploy (§9).

---

## 6. Edge cases, error handling & validation

- **StatefulSet missing** → `AllVersions()` does not contain the version →
  `ErrEngineFleetNotFound` → 404 (`GetReplicas` alone cannot distinguish
  "missing" from "present but 0 replicas", hence `AllVersions`).
- **Version malformed** → handler rejects non-`domain.IsFullVersion` with 400.
- **Version scaled to ZERO (0 pods)** → no-op: `replicas=0`, result
  `state:"completed"` with `message:"no running engine pods; nothing to prune"`.
  **The S3 snapshot prefix is NOT deleted** (deliberate): with no pod there is
  no local cache to prune and no sidecar to push, and the shared snapshot
  correctly still reflects the last-known cache. A future pod restores it
  (correct — nothing was pruned). A cold restart for a 0-replica version is the
  existing `cache/purge` + delete-StatefulSet path, out of scope here.
- **Raw TCP dial failure (pod unreachable / NotReady / IP changed)** → the
  pruner's `DialContext`/`client.Do` returns an error → `pods[i].error` set,
  `pruned:false`. The other pods proceed. Aggregate is `completed` with per-pod
  detail.
- **Session attach rejected (version mismatch)** → if the engine rejects the
  synthetic attach (e.g. because `client_version` is below the engine's
  `MinimumClientVersion`), the engine returns 500 with "incompatible client
  version …"; the pruner surfaces it in `pods[i].error`. This should not happen
  in practice because `client_version` is set to the pod's own version; it is
  the primary live-validation check (§9.3 step 2).
- **Protocol mismatch across engine versions (0.19 / 0.20 / 0.21)** → the wire
  protocol (HTTP POST `/query` + `X-Dagger-Client-Metadata` base64 JSON) is
  version-stable across these releases (the CLI upgrades independently of the
  engine; each engine enforces only a *minimum* client version). No per-version
  client selection is needed: one HTTP client, `client_version` parameterised
  per pod. If a future engine (≥ v0.22, out of fleet scope) changes the header
  name/encoding, that is a new-version migration, not a runtime branch.
- **dagql field missing on a version** → `engine.localCache.prune` is confirmed
  present at v0.19.0 (our floor) and has existed since ~v0.9; a GraphQL "field
  not found" error would still be captured per-pod (`pods[i].error`). Spot-check
  the field on each fleet version during §9 live validation.
- **Connection reuse vs. per-pod conns** → `DisableKeepAlives: true`: each prune
  is a fresh TCP connection, so a socket is never held open to a pod that is
  later scaled/deleted, and the engine tears the synthetic session down when the
  connection closes. Pruning is a rare admin op, so pooling buys nothing.
- **TLS at the engine hop** → none. The engine listens plaintext (`--addr=tcp://
  0.0.0.0:9999`, no `--tlscert`); the supervisor terminates mTLS only at the
  data plane. The pruner therefore sends no client cert and must **not** enable
  TLS (plain `http://` URL). No `HTTP_PROXY` interference: the transport sets
  `Proxy: nil`.
- **Engine restarting / not ready mid-prune** → the HTTP POST fails or times out
  → per-pod error. Re-run after the pod is Ready (idempotent).
- **Sidecar absent (sync disabled / no `cache-sync` container)** → the prune
  still succeeds (`pruned:true`); the `PushOnce` exec fails (container not
  found) → `synced:false` with the exec error. Documented as a partial result.
- **Timeouts on large caches** → per-pod `prunePodTimeout` (10m) bounds the
  dagql call via ctx; the pruner's `http.Client.Timeout` (10m) is a hard
  backstop. `pushOnceTimeout` (5m) bounds the push. A timeout is a per-pod
  error, not a fleet-wide failure.
- **RBAC missing for `pods/exec`** → `PushOnce` returns an API-server 403 →
  `synced:false` per pod. (Not expected: the chart already grants it.)
- **Concurrent purge requests (same version)** → in-memory `active` set → 409.
  Different versions run in parallel (bounded by `pruneConcurrency` per purge;
  two concurrent purges of different versions could each spawn up to
  `pruneConcurrency` goroutines; acceptable, admin-gated).
- **Partial failure — snapshot consistency story.** Prune and push are
  per-pod atomic in sequence (push fires only after that pod's prune returns
  success). Pods that failed to prune keep their cache and do NOT push, so the
  shared `meta.tar.gz` is last-written by a pod that either (a) pruned+synced
  (empty) or (b) did a normal periodic push. The final shared snapshot
  converges to "empty" only when every pod pruned+synced; on partial failure it
  reflects a mix. The operator re-runs the purge (idempotent) to finish the
  failed pods. This is the honest, documented behaviour of a live,
  availability-preserving purge.

---

## 7. Testing plan

**Conventions:** stdlib `testing` only, table-driven, `logrus` with
`io.Discard`/`observ.NewTestLogger()`, `%w` wrapping, no testify/ginkgo.
Handler tests drive routes via `ut.PerformRequest` on `s.configure()`.

### `internal/repository/engine_pruner_test.go` (the real pruner vs. a fake session server)

The pruner is now a real network client, so it is tested against an
`httptest.Server` whose handler emulates the engine's `/query` surface. Because
the unit test lives in package `repository` (white-box, like
`k8s_provider_integration_test.go` uses unexported `enginePort`), it can set the
unexported `port` field to the `httptest.Server`'s port and pass `podIP =
"127.0.0.1"`.

| Test | Asserts |
|---|---|
| `TestPruneSendsCorrectQueryAndMetadata` | request is `POST /query`; body contains `engine`/`localCache`/`prune(useDefaultPolicy: false)`; `X-Dagger-Client-Metadata` header base64-decodes to a metadata whose `client_id`, `session_id`, `client_secret_token`, `client_stable_id` are all non-empty and distinct, `client_version` equals the `version` arg, `client_hostname` == "supervisor". |
| `TestPruneHappyPath` | server returns `200` with `{"data":{"engine":{"localCache":{"prune":null}}}}` → `PruneLocalCache` returns nil. |
| `TestPruneGraphQLErrorSurfaced` | server returns `200` with non-empty `errors` → error contains the GraphQL message + pod IP. |
| `TestPruneNon200Surfaced` | server returns `500` with a body → error contains the status + truncated body. |
| `TestPruneDialErrorSurfaced` | pruner pointed at a closed port → error wraps the dial/connection error with pod IP. |
| `TestPruneClientVersionParameterized` | two calls with different `version` → the recorded `client_version` changes accordingly (no hard-coded version). |

### `internal/service/engine_cache_purge_test.go`

Fakes: a `stubFleetProvider` (reuse/extend `cache_stats_test.go`'s, scriptable
`GetReplicas`/`AllVersions`), a fake `domain.EnginePruner` (records
`podIP`/`version` calls, scriptable per-pod errors), a fake
`domain.SidecarSyncPusher` (records `podName`, scriptable errors).

| Test | Asserts |
|---|---|
| `TestPurgeFleetNotFound` | `ErrEngineFleetNotFound`, state `failed`, no pruner/pusher calls. |
| `TestPurgeConcurrentRejected` | start one (blocked via channel-gated fake), second → `ErrPurgeInProgress`. |
| `TestPurgeHappyPathNRunning` | N pods → N pruner calls (one per pod IP, with the version) and N pusher calls; each pod `pruned:true, synced:true`; state `completed`; pods ordered by ordinal. |
| `TestPurgePartialPruneFailure` | pod 2's pruner errors → `pods[2].error` set, `pruned:false`, `synced:false`; pod 2's pusher NOT called; state `completed`; message summarises. |
| `TestPurgePushFailureAfterPrune` | pruner ok, pusher errors → `pruned:true, synced:false`, error set. |
| `TestPurgeZeroReplicas` | `GetReplicas` returns `[]` → no pruner/pusher calls, `replicas:0`, state `completed`. |
| `TestPurgePrunerNilDisabled` | (if nil pruner guarded) → error path. |
| `TestStatusReflectsLastResult` | after purge, `Status` returns stored result; unknown → `false`. |

### `internal/handler/fleet_purge_test.go`

Build a `Server` via `NewServer` + `configure()` with a stub
`domain.EngineCachePurger` and the existing auth test helpers.

| Test | Asserts |
|---|---|
| `TestFleetPurgeRequiresAdmin` | non-admin → 403; unauthenticated → 401. |
| `TestFleetPurgeInvalidVersion` | `v` / `../../etc` / `1.2` → 400. |
| `TestFleetPurgeSuccess` | stub returns result → 200, JSON echoes pods. |
| `TestFleetPurgeErrorMapping` | `ErrEngineFleetNotFound`→404, `ErrPurgeInProgress`→409, generic→500. |
| `TestFleetPurgeStatusNotFound` | `Status`→`false` → 404. |
| `TestFleetPurgeNilPurger` | nil → 503. |

### `internal/domain` + `config`

No new logic/config; nothing beyond what compiles.

### `tests/integration/engine_cache_purge_test.go`

Follow `worker_snapshot_s3_test.go` / `net_helpers_test.go`: start a real Hertz
supervisor with a fake `domain.FleetProvider` (2 replicas), a **fake
`domain.EnginePruner`** and **fake `domain.SidecarSyncPusher`** (the real
HTTP-session transport is validated on the live cluster, not in-process), using
`freeListener(t)` + `ServerConfig.ControlListener/DataListener` + `t.Cleanup`
shutdown.

| Test | Asserts |
|---|---|
| `TestEngineCachePurgeEndToEnd` | `POST /api/v1/fleet/v0.19.0/purge-cache` as admin → `completed`, 2 pods each `pruned:true, synced:true`; fake pruner saw both pod IPs; fake pusher saw both pod names; `GET .../purge-cache` returns the recorded status. |
| `TestEngineCachePurgeFleetNotFound` | unknown version → 404, no pruner/pusher calls. |

### Manual live-cluster validation (§9.3)

1. Provision a throwaway engine (`POST /v1/engines` for `v0.19.0`), run a
   pipeline to populate the cache, confirm `worker-snapshots/v0-19-0/` in MinIO.
2. **Prove the in-process prune works against a real engine:** from a supervisor
   pod (or a scratch pod on the cluster network), confirm that a single
   `POST http://<engine-pod-ip>:9999/query` with the metadata header + prune
   query returns 200 (this is exactly what `SessionEnginePruner` does). Then
   `POST /api/v1/fleet/v0.19.0/purge-cache` (admin token) → `completed`, all
   pods `pruned:true, synced:true`.
3. Confirm the engine pod is **still Running/Ready** (not scaled), its worker
   dir is now effectively empty (or only in-use entries remain), and
   `worker-snapshots/v0-19-0/meta.tar.gz` was overwritten by the post-prune
   push (fresh engine cold-starts with an empty cache).
4. Repeat while an active pipeline is pinned to the version → expect the purge
   to succeed and the pipeline to keep running (Q3 evidence).
5. If the fleet also runs `v0.20.x` / `v0.21.x`, repeat steps 1–2 for each to
   confirm `client_version` = pod version and the `prune` field on those exact
   engine images.
6. Clean up the test StatefulSet/Service/PVC.

---

## 8. Documentation changes

- **`docs/design/ADR-032-engine-cache-purge.md`** — rewrite: per-version purge =
  live dagql `engine.localCache.prune(useDefaultPolicy:false)` against every pod
  **in-process** (the supervisor speaks the engine's session HTTP protocol
  directly over plaintext `tcp://<pod-ip>:9999`, one `POST /query` with a
  synthetic `X-Dagger-Client-Metadata` header — no `dagger` CLI subprocess) +
  immediate `cache-sync push-once` via `pods/exec`; engines stay up; pinned
  sessions allowed; status in-memory. Alternatives evaluated: scale-to-zero/PVC
  delete, Dagger Go SDK (route A, rejected — `engineconn.EngineConn` is an HTTP
  `Doer`, no raw-TCP connector), version-matched CLI exec (rejected by the
  operator), exec+buildctl, SIGUSR1 — with the evidence from §2 Q1.
- **`docs/design/index.md`** — add `032 | Engine local-cache purge (per version, live dagql prune)`.
- **`docs/README.md`**:
  - In "Worker-cache sync (warm start)" add "Purging the local cache (admin)":
    Runners-page per-version button → `POST /api/v1/fleet/:version/purge-cache`;
    what it does (in-process live prune on every pod + immediate push);
    admin-only; allowed with running sessions; idempotent; no longer requires
    `cli.enabled`; the `cache-sync push-once` subcommand; per-pod failure
    reporting.
  - In "Purging cache (admin)" cross-link the new per-version **local**-cache
    purge (distinct from the remote `cache/` purge).

---

## 9. CI gate + redeploy validation

**Local fast gate (minimum when no Docker):**
```bash
go build ./... && go vet ./... && go test ./...
dagger call -m ./dagger --src . lint
```

**Full CI gate (merge gate, must pass):**
```bash
dagger call -m ./dagger --src . ci export --path out
```
Watch the `unused` linter: no dead symbols (e.g. drop `ErrPurgeActiveSessions`,
`EngineLocalCachePruner`, `cliBinaryMaterializer`, and any orphaned helper after
the rewrite — `MaterializeBinary` must never land). No change to `dagger/`, CI
scripts, or `.github/workflows/`, so `DAGGER.md` is unaffected.

**Live redeploy (mandatory, per AGENTS.local.md §4):**
```bash
docker build -t docker.io/disaster/dagger-kubernetes:dev .
docker push docker.io/disaster/dagger-kubernetes:dev
helm --kubeconfig /home/user/.kube/home get values dagger-kubernetes-test \
  -n dagger-kubernetes-test -o yaml > /tmp/dagger-kubernetes-test.values.yaml
helm --kubeconfig /home/user/.kube/home upgrade --install dagger-kubernetes-test \
  ./deploy/helm/dagger-kubernetes --namespace dagger-kubernetes-test \
  -f /tmp/dagger-kubernetes-test.values.yaml \
  --set supervisor.image.tag=dev \
  --set supervisor.image.pullPolicy=Always \
  --set supervisor.image.repository=docker.io/disaster/dagger-kubernetes
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  rollout restart statefulset/dagger-kubernetes-test-dagger-kubernetes
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  rollout status statefulset/dagger-kubernetes-test-dagger-kubernetes --timeout=300s
```

Then §9.3 manual live validation (above) + agent verification (§5.1) + human
verification of the Runners page (§5.2). Do not mark complete until both pass.

---

## 10. Open questions / risks

1. **HTTP/1.1 acceptance + `client_version` = pod version, confirmed end-to-end.**
   The source proves `h2c.NewHandler` accepts plain HTTP/1.1 and the engine only
   enforces a *minimum* client version, but the plan should still confirm the
   exact `POST /query` round-trip against a **running** `v0.19.0` engine in §9.3
   step 2 (the single highest-value live check). Low risk (all evidence from
   source), but it is the one fact that cannot be proven without the live pod.
2. **`engine.localCache.prune` field presence on the exact `v0.20.x` / `v0.21.x`
   images the fleet runs.** Confirmed present at v0.19.0 (floor) and stable
   since ~v0.9; spot-check on each fleet version during §9.3 step 5. If any
   image lacks it, the per-pod error is captured (no fleet-wide failure).
3. **Future engine (≥ v0.22) protocol drift.** If a newer engine changes the
   `X-Dagger-Client-Metadata` header name/encoding or the `/query` path, the
   pruner's minimal protocol must be re-verified. Out of scope for this fleet
   (floor v0.19.0, allowlist 0.19–0.21) but the ADR should note the single
   header + endpoint as the compatibility surface to re-check.
4. **Prune return value has no byte accounting.** `engine.localCache.prune`
   returns no freed-bytes count; the result JSON intentionally omits
   `freed_bytes`. If operators want space accounting, it would require a
   separate `engine.localCache.entrySet`/stat call (out of scope for v1).
5. **Concurrent `pods/exec` `push-once` vs. the sidecar's periodic/stop push.**
   A separate `push-once` process can overlap the sidecar's own push. The store
   documents last-writer-wins `meta.tar.gz` + idempotent blobs as safe, but the
   plan should be validated for a torn-read race (two processes tarring the same
   worker dir) — the periodic push already accepts a "slightly dirty" snapshot,
   so this is expected to be harmless; confirm on the live cluster.

(End of file)
