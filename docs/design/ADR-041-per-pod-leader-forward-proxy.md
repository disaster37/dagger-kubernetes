# ADR-041: Per-pod leader-forward proxy (drop label-based leader routing)

> **Revision (2026-09-23, unmerged branch):** §2 "Internal forward hop" changed
> before this ADR ever merged: the hop no longer targets the leader's *public*
> control port and no longer requires a cert-manager wildcard/DNS-01. It now
> targets a dedicated **internal-only control listener** (`server.control_addr`
> port + 2 = **8082**, pod-to-pod only) that always serves a leaf minted by the
> shared goca minting CA with each pod's **own exact FQDN SANs**, in **every**
> TLS-provider mode. The forward hop's verification pool is **unconditionally**
> the minting-CA pool; cert-manager stays public-facing only.

- **Status:** accepted
- **Date:** 2026-09-22
- **Deciders:** dagger-kubernetes maintainers
- **Supersedes:** ADR-026 decision 2 (leader-routed `-control`/`-data` Services)
- **Resolves:** ADR-016 OQ1 / risk R3 (leader-only Service)

## Context

ADR-026 kept session leases Raft-replicated and made the Helm chart's
`-control` and `-data` Services select the current Raft **leader** pod through
the runtime label `dagger-kubernetes.io/raft-leader` (each pod patched its own
label in `observeLeadership`). Everything — API reads and writes, data-plane
tunnels — terminated on the leader.

That design had three problems:

1. **Failover window / zero-endpoint outage.** A leader change requires a label
   patch, an endpoint-controller update, and kube-proxy propagation — two
   independent control loops. During the window the Services can have **zero
   ready endpoints**, so even read-only requests fail.
2. **Needless RBAC.** Each pod needed `pods patch` in the release namespace.
3. **No read fan-out.** Followers were idle hot standbys; ADR-016 D6 already
   allows followers to serve stale local reads, but the Service selector
   prevented it.

Issue #24 ("fix: raft routing") asked for a per-pod proxy that routes to the
leader instead.

## Decision

### 1. Control plane: a per-pod leader-forward middleware

`internal/handler/leader_forward.go` adds a global Hertz middleware
(`Server.leaderForward`) that forwards **leader-pinned** requests to the
current leader and serves everything else locally.

- **Classification (`leaderPinnedRoute`)** is endpoint-aware, not merely
  method-based. Every mutating method (`POST`/`PUT`/`DELETE`/`PATCH`) is
  leader-pinned, plus three GET route families that read leader-local state or
  write through Raft despite the read-only method:
  - `GET /api/v1/traces/:traceID/live` — SSE; the `liveHub` is per-pod and
    events are produced only on the leader (OTLP ingest is forwarded there).
  - `GET /api/v1/fleet/:version/purge-cache` — the purge-job status lives in
    the leader-local `EngineCachePurgeService`.
  - `GET /api/v1/auth/oauth/{github,oidc}/callback` — a GET that runs the OAuth
    exchange and then `completeOAuthLogin` → `EnsureOAuthUser` + membership
    reconciliation, all of which are Raft writes (a follower would return 503).
  All other `GET`/`HEAD`/`OPTIONS` routes are follower-safe and served locally
  (stale reads, ADR-016 D6): probes, SPA, traces list/detail/url/logs/search/
  metrics, fleet/history/status/image-cache/connect/cli/auth/tokens/users/
  groups/projects GETs.
- **Loop prevention:** the `X-Dagger-Kubernetes-Leader-Forwarded: 1` header. A
  request already carrying it is served locally, so a stale leader address
  mid-election can never bounce a request forever.
- **Per-request leader lookup:** `leaderInfo.LeaderAddress()` + `IsLeader()`
  (an injected interface implemented by `*repository.RaftStore`). `""` →
  503 `{"message":"no raft leader available"}`; a proxy transport/verification
  error → 502 `{"message":"leader unreachable"}` via the existing
  `ErrorResponse` shape.
- **Forward before auth:** the middleware is registered with `h.Use` and runs
  before per-handler `requireAuth`/`requireAdmin`. Auth is resolved from the
  local, replicated FSM/JWT secret, so forwarding first is correct and avoids
  double auth work; the leader re-verifies the credentials carried through.

### 2. Internal forward hop: an internal-only HTTPS listener

The hop targets a dedicated **internal-only control listener** on the leader's
**pod FQDN** (`LeaderAddress()` host) at `server.control_addr` port + 2
(**8082**). The listener is pod-to-pod only: no Service, no ingress, exactly
like the relay-in port 8444 (a Service's `ports` list does not participate in
pod DNS, so the existing headless-Service A-records already resolve the FQDN).

Trust is uniform across every TLS-provider mode (`embedded`, `cert-manager`,
`external`): the internal listener **always** serves a dedicated leaf minted by
the shared goca minting CA (`ca.minting_ca_secret`), cached as
`server-internal.crt`/`.key`, carrying this pod's **own exact, deterministic
pod-FQDN SANs** — `<hostname>.<headless>.<ns>.svc.<clusterDomain>` and the
`.svc`-short form `<hostname>.<headless>.<ns>.svc` (for the
`raft.cluster_domain=""` override), where `hostname` is the pod name from
`os.Hostname()` and the remaining values come from the existing Raft discovery
config (`repository.ServerCertFQDNSANs`, plus the engine data host). No
wildcard and no all-pod enumeration: the only host a follower ever dials is
the leader's own FQDN, so per-pod exact SANs are complete for verification and
strictly stronger than a wildcard (one key never covers other names), and new
pods mint their own leaves at boot with zero cross-pod coupling. Followers
verify with `RootCAs` = the shared minting-CA pool, **unconditionally** — the
same pool the Dagger CLI uses for the embedded data-plane cert. The public
server certificate (cert-manager Let's Encrypt, external PEM, or the embedded
`server.crt`) is never presented on the internal listener, so **cert-manager
remains public-only** and no wildcard/DNS-01 operator step exists.

Dual-listener mechanics: Hertz's `server.Hertz` owns exactly one listener per
engine, so the server builds **two** engines that share a single
route-registration function (`Server.registerRoutes`). The public engine is
unchanged; the internal engine is built with `server.WithListener(...)` +
`server.WithTLS(...)` and given the **identical** middleware stack + route
table (request log, security headers, CORS, `leaderForward`, per-handler auth).
The leader re-verifies forwarded credentials: the loop-prevention header
(`X-Dagger-Kubernetes-Leader-Forwarded: 1`) short-circuits `leaderForward` on
the leader and its handlers re-run auth against the replicated FSM/JWT.

`tls.Config.ServerName` is left empty so Go derives it from the dialed leader
FQDN; `MinVersion` is TLS 1.2. `InsecureSkipVerify` is never used. Cached
`server-internal.crt`/`server-internal.key` that predate a SAN-set change
(e.g. `cluster_domain`/`headless_service` override) are transparently
re-issued in place under the same CA (`EmbeddedProvider.InternalServerTLSCert`
re-checks SAN coverage via `coversRequiredServerSANs`), mirroring the raft-leaf
re-issue behavior (ADR-029) and `ServerTLSCert`'s own re-issue. Listener
failures (unparsable `control_addr`, bind failure) are log-only: the internal
listener is disabled and the public control plane keeps running; the forward
hop fails closed (`internalControlPort <= 0` → 503).

### 3. Streaming (SSE) through the forward proxy

The proxy is a single `github.com/hertz-contrib/reverseproxy.ReverseProxy`
built with `client.WithResponseBodyStream(true)`, `client.WithTLSConfig(full
verification)`, `client.WithDialer(standard.NewDialer())` (netpoll cannot do
client TLS) and a 2s dial timeout. One proxy handles both buffered JSON writes
and the streaming SSE route; the backend body streams and Hertz flushes it
incrementally. This is verified by
`TestLeaderForwardStreamsSSE` (the backend withholds the second event until the
test has read the first).

### 4. Data plane: a per-pod L4 relay

The `-data` Service selector is restored to all pods; every pod keeps serving
its mTLS listener on `server.data_addr` (8443).

- **Leader:** existing `serveTLSConn` (handshake + `handleDataConn` →
  `serveDataTunnel`), terminal.
- **Follower:** `serveDataRelay` dials the leader's dedicated **relay-in port**
  (`server.data_addr` port + 1 = 8444, derived — no new config key) and pumps
  raw bytes bidirectionally. TLS stays **end-to-end** (client ↔ leader); the
  follower relays ciphertext only, so no trust change.
- **Loop prevention:** the relay-in listener is served only by the leader and
  is terminal; a pod that has stepped down rejects relay-in connections at
  accept (`IsLeader()==false` → close). Relay connections can never re-relay.
- **Half-close:** each direction calls `CloseWrite` on its underlying
  `*net.TCPConn` when its copy EOFs and the relay waits for **both** directions
  before the full close, so a client `close_notify`/half-close does not
  truncate the leader's response stream.
- **Limits:** a 5s dial timeout and the existing `dataConnSem` (512) cap.
  The relay-in port is reachable only pod-to-pod (no Service, no ingress); if a
  NetworkPolicy is added it must allow pod→pod on 8444.

### 5. Removal of the label machinery

- `cmd/api/main.go`: `observeLeadership`, the `raftLeaderLabel` const, and the
  now-orphaned `metadata`/`types` imports are deleted.
- `internal/repository/raft_store.go`: `RaftStore.LeaderCh()` and its test are
  deleted; `LeaderAddress()` replaces it.
- `deploy/helm/.../service.yaml`: the `dagger-kubernetes.io/raft-leader: "true"`
  selector is removed from both Services (standard selector labels only).
- `deploy/helm/.../rbac.yaml`: `pods`/`services` no longer grant `patch`.
- `deploy/helm/.../statefulset.yaml`: the supervisor advertises the internal
  relay-in `containerPort: 8444` and the internal control listener
  `containerPort: 8082` (both pod-to-pod only, no Service).

## Consequences

- No label convergence and no zero-endpoint window: every pod always answers
  probes and serves reads; follower pods have endpoints during an election.
- Control-plane writes forward to the leader; reads fan out to any pod.
- The `pods patch` privilege is gone.
- One internal HTTPS hop (with full verification against the minting-CA pool)
  and one internal L4 relay are added. Both internal ports (8082 and 8444) must
  stay pod-to-pod only.
- cert-manager and external tooling remain **public-only** trust providers: no
  wildcard SAN and no DNS-01 solver are ever required for the mesh.
- A brief leader-discovery lag window during an election yields 503 + client
  retry for writes (bounded by the election window), and a stale-follower
  relay-in dial fails and the Dagger CLI's gRPC reconnect retries — the same
  tolerance ADR-026 already relies on.
- Single-node deployments never forward: `IsLeader()` is always true, so the
  middleware is a no-op and the accept loop always takes `serveTLSConn`.

## Alternatives considered

- **Keep the leader-routed labels (status quo).** Rejected: the label-lag
  window, the zero-endpoint outage, the `pods patch` RBAC, and the lack of read
  fan-out are exactly the problems issue #24 targets.
- **Hybrid (labels for `-data`, forward for `-control`).** Rejected: it keeps
  `observeLeadership` + `raftLeaderLabel` + `pods patch` alive for one Service
  and leaves the label-lag window for tunnels.
- **Wildcard SAN for the embedded forward hop.** Rejected: one key would cover
  other pods' names, and enumerating all pod SANs on every cert would force
  re-issuance cascades on scale-up. Per-pod exact own-FQDN SANs are complete
  for the only host a follower dials.
- **Disable verification on the internal hop (`InsecureSkipVerify`).**
  Rejected: it is an unnecessary trust downgrade for a hop that is fully
  verifiable per provider.
- **A dedicated `server.data_relay_port` config key.** Rejected in favor of
  deriving the port (`data_addr` port + 1), matching the fixed-port convention
  (8080/8443/8081/8444) and avoiding config-sample/values/README churn.

## Testing

- `internal/repository/raft_store_test.go`: `TestRaftStoreLeaderAddress`
  (in-mem non-empty + real TCP transport `host:port`).
- `internal/repository/raft_discovery_test.go`: `TestServerCertFQDNSANs`
  (both DNS forms, no wildcard, empty-config cases).
- `internal/repository/ca_providers_test.go`:
  `TestEmbeddedProviderServerCertSANs` (own-FQDN SANs present, no wildcard),
  `TestServerTLSCertReissuesOnSANGrowth` (cached cert re-issued when SANs grow;
  reissued cert verifies under the same CA; subsequent calls reuse it),
  `TestEmbeddedProviderInternalServerCert` (internal leaf minted with the
  own-FQDN SANs, verifies against the minting-CA pool, cached pair reused),
  `TestInternalServerCertReissuesOnSANGrowth` (stale internal leaf re-issued),
  and `TestFileCAProviderInternalCertDelegatesMinting` (cert-manager/external
  modes mint the internal leaf from the shared minting CA).
- `internal/handler/internal_control_test.go`: internal-port derivation
  (`control_addr` port + 2, error cases), TLS handshake against the minted
  internal leaf with correct/wrong `ServerName`, log-only disable without a
  certificate, and shutdown closing the internal listener.
- `internal/handler/leader_forward_test.go`: route classification (including
  both OAuth callback routes as leader-pinned and both OAuth login routes as
  follower-safe); local serving on the leader; follower read served locally;
  write/SSE/purge-cache/OAuth-callback forwarded (method/path/body + loop header
  observed) against a minted TLS backend; 503 without a leader; local serving
  when already forwarded; 502 on transport error; `controlForwardTarget`
  internal-port derivation (always https); TLS config unconditionally carrying
  the minting-CA pool + empty `ServerName`; exact-FQDN verification against a
  real minted leaf; incremental SSE streaming through the forward hop.
- `internal/handler/data_relay_test.go`: byte pump both ways; no-leader closes
  the client; dial failure closes the client; half-close does not truncate the
  opposite direction.
