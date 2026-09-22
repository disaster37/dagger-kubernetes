# Plan: Per-pod leader-forward proxy (drop label-based leader routing) — issue #24 "fix - raft routing"

## 1. Decision

### 1.1 Is the gohookbridge-style per-pod forward proxy better than the current label-based routing?

**Yes — for the control plane, clearly; for the data plane, marginally (the win is correctness/operational, not distribution).** See the honest comparison below.

| Axis | (a) Label-based routing (current) | (b) Per-pod forward proxy (proposed) |
|---|---|---|
| **Failover latency / flakiness** | Leader change → `observeLeadership` patches the pod label → kube-proxy/endpoint-controller propagate → Service re-selects. Two independent control loops; a **window with zero ready endpoints** exists (fresh cluster, or during an election). Measured in seconds (label patch + endpoint propagation). | Leader change is read **per-request** from the local Raft node's view of the leader (`raft.Leader()`), which is updated on every AppendEntries heartbeat (sub-second). No endpoints ever drop: every pod always serves. |
| **RBAC privilege** | Requires `pods` `patch` (each pod patches its own label). | No K8s API calls for routing. `pods patch` is removed. |
| **Control-plane load distribution** | ALL reads AND writes terminate on the leader. Followers are idle hot-standbys. | Writes forward to the leader; **reads are served locally by followers** (stale reads, ADR-016 D6). Real read fan-out. |
| **Operational simplicity** | Two moving parts (label patcher + two label-selector Services); "which pod is leader" is a K8s label you must reconcile. | One middleware + one L4 relay. "Which pod is leader" stays inside Raft, where it already lives. |
| **Risks** | Label-lag window; zero-endpoint outage; a mispatched label silently routes traffic to a follower (503 storm). | Internal hop trust; streaming/flush semantics through the reverse proxy; loop prevention; a brief leader-discovery lag window (503 + client retry) during elections. |

**The honest caveat:** for the **data plane**, the tunnels are long-lived and *must* reach the leader (session-lease touches are Raft writes), so the per-pod relay does **not** change load distribution there — it only removes the label-lag window and the RBAC/patch machinery. The real wins are (1) **correctness/operational** (no label convergence, no zero-endpoint window, simpler ops) and (2) **follower-served reads on the control plane**.

### 1.2 Control-plane design (chosen)

A Hertz-native leader-forward middleware (new file `internal/handler/leader_forward.go`) using the already-vendored `github.com/hertz-contrib/reverseproxy`:

- **Endpoint-aware classification, NOT merely method-based** (the #1 trap). Default: all mutating methods (`POST/PUT/DELETE/PATCH`) are leader-pinned. Two GET routes are *also* leader-pinned because they read leader-local state:
  1. `GET /api/v1/traces/:traceID/live` — SSE; the `liveHub` is per-pod and events are produced **only on the leader** (`broadcastOTelUpdate` runs in `handleOTel`, which is forwarded). Confirmed: followers have no live events.
  2. `GET /api/v1/fleet/:version/purge-cache` — reads the in-memory purge-job status of `EngineCachePurgeService`, which is leader-local (the `POST .../purge-cache` is forwarded, so its status lives on the leader).
  Everything else that is read-only (`GET/HEAD/OPTIONS`) is **follower-safe** and served locally (stale reads): probes, SPA, traces list/detail/url/logs/search/metrics, fleet/history/status/image-cache/connect/cli/auth/tokens/users/groups/projects GETs.
- **Loop-prevention header** `X-Dagger-Kubernetes-Leader-Forwarded: 1` (project-specific; does not clash with gohookbridge). A request already carrying it is served locally.
- **Per-request leader lookup** via `leaderInfo.LeaderAddress()` + `IsLeader()` (an injected interface; `*repository.RaftStore` implements it). 503 `{"message":"no raft leader available"}` when no leader; 502 `{"message":"leader unreachable"}` on proxy transport error (matches `writeError`/`ErrorResponse` shape).
- **Forward BEFORE auth.** Auth does not use leader-local state (identity is resolved from the local, replicated FSM; JWT uses the replicated `jwt_secret`), so the middleware — a global `h.Use()` — naturally runs before per-handler `requireAuth`/`requireAdmin`. The leader re-verifies auth on the forwarded request (credentials are carried through). This is correct and avoids double auth work on the follower.
- **Internal forward hop — full TLS verification (no `InsecureSkipVerify`).** The hop is HTTPS to the leader's control port (8080) with **full certificate verification**. **Embedded provider: NO wildcard** — each pod's control-plane server cert gets its **own exact, deterministic pod-FQDN SANs**: `<hostname>.<headless>.<ns>.svc.<clusterDomain>` and the `.svc`-short form `<hostname>.<headless>.<ns>.svc` (for the `raft.cluster_domain=""` override), where `hostname` is the pod name (`<fullname>-<ordinal>`, from `os.Hostname()`) and `headless_service`/`namespace`/`cluster_domain` come from the existing Raft discovery config — **no new config keys**. *Rationale:* the only host a follower ever dials is the leader's own pod FQDN (from `LeaderAddress()`), so per-pod own-FQDN SANs are complete for verification; no wildcard (one key never covers other names) and no all-pod enumeration (which would force re-issuance cascades on every scale-up). New pods issue their own certs with their own FQDN at boot — zero cross-pod coupling. The forward client leaves `tls.Config.ServerName` empty so Go derives it from the dialed host (the leader FQDN) and verifies against a **per-provider root pool**: **embedded** → the shared minting-CA pool (`serverMintingCA.CertPool()`, shared across pods via the `ca.minting_ca_secret` Secret); **cert-manager/external** → the system pool (nil `RootCAs`; operators add a wildcard SAN to their Certificate — see §7). The pool is injected prebuilt via `Deps.LeaderForwardRootCAs *x509.CertPool` (nil = system pool).
- **Streaming/flush:** `hertz-contrib/reverseproxy` v1.0.6 buffers responses by default (`client.Do` reads the full body). For SSE to work, the forward proxy is constructed with `config.WithResponseBodyStream(true)` (verified: `ClientOptions.ResponseBodyStream` exists and is threaded into the HTTP/1 host client via `newHttp1OptionFromClient`), so the backend body streams and Hertz flushes it incrementally. **One** forward proxy handles both buffered JSON writes and the streaming SSE route, carrying the full-verification `tls.Config` above. (§6 adds an explicit flush-behaviour test. If the reverseproxy's TLS + streaming combination is found incompatible at implementation time, fall back to a bespoke forwarder using `tls.DialWithDialer` with a 2s `net.Dialer`, `ServerName` = leader FQDN, `MinVersion` TLS1.2 — the plan keeps this as the documented fallback.)

### 1.3 Data-plane design (chosen: full per-pod L4 relay, option ii)

A transparent L4 byte-relay of mTLS tunnels from any pod to the leader:

- The `-data` Service selector is restored to **all** pods (standard selector labels). Every pod keeps serving its mTLS listener on `server.data_addr` (8443).
- On accept, the pod checks `IsLeader()`:
  - **Leader** → existing `serveTLSConn` (TLS handshake + `handleDataConn` → `serveDataTunnel`), terminal.
  - **Follower** → new `serveDataRelay`: dial `leader-host:relay-in-port` and pump bytes bidirectionally.
- **Loop prevention:** the relay target is a **dedicated relay-in port served only by the leader** (derived from `server.data_addr` port+1 = **8444**, no new config key). The leader's relay-in listener is terminal (always does `serveTLSConn`); followers never relay a connection received on it. A follower that has stepped down **rejects** relay-in connections at accept (`IsLeader()==false` → close), so a stale-follower window causes a dial failure / connection reset — which the Dagger CLI's gRPC reconnect already tolerates today (ADR-026). Relays can never re-relay.
- **TLS stays end-to-end** (client ↔ leader): the follower relays raw ciphertext bytes; certs/SANs/fingerprints are unchanged. `serveTLSConn` still runs on the leader (its `remote_addr` log will show the follower pod IP — acceptable, noted in §8). The full-verification decision in §1.2 applies to the **control-plane forward hop only** — the data relay performs no TLS termination on the follower (it is a raw byte pump), so there is no trust change there.
- **Half-close:** the pump uses `CloseWrite` on the underlying `*net.TCPConn` when one direction EOFs, so a client `close_notify`/half-close is not truncated (matches the semantics the tunnel already relies on).
- **Dial timeout** 5s; **max-connection semaphore:** the follower relay acquires `dataConnSem` (512) exactly like the leader's `serveTLSConn`, so relay connections count against the same cap.

**Why not hybrid (keep the label for `-data` only)?** It would leave `observeLeadership` + `raftLeaderLabel` + `pods patch` alive for one Service, keep the label-lag window for tunnels, and split the routing story across two mechanisms. The L4 relay is small (a byte-pump, ~50 lines) and fully removes the label machinery — matching the issue's intent ("add proxy on each pod that route to the leader").

---

## 2. Exact file changes (ordered task list)

### Phase A — Raft store: leader address

1. **`internal/repository/raft_store.go`** — add the method (after `IsLeader`, ~line 738):

   ```go
   // LeaderAddress returns the current leader's Raft transport address
   // (host:port, e.g. <pod>.<headless>.<ns>.svc.cluster.local:8081), or "" when
   // no leader is known. The host is a stable pod FQDN (ADR-029); the port is
   // the Raft transport port, NOT the control/data-plane port — callers
   // substitute the port they need.
   func (s *RaftStore) LeaderAddress() string {
       return string(s.raft.Leader())
   }
   ```

   `hashicorp/raft` v1.7.3 exposes `Leader() ServerAddress` (and `LeaderWithID() (ServerAddress, ServerID)`); `Leader()` returns the leader's advertised address directly, which is exactly the FQDN:raftPort we need. (`LeaderWithID` is unnecessary here — no ID is consumed.)
2. **Delete `RaftStore.LeaderCh()`** (line 792) — it becomes orphaned once `observeLeadership` is removed (the store itself uses `s.raft.LeaderCh()` directly inside `waitForLeaderCondition`). `golangci-lint` `unused` hazard: delete method + its test together (Phase F).

### Phase B — Control-plane middleware

3. **Create `internal/handler/leader_forward.go`** — see §3 for exact signatures. Contents: `leaderInfo` interface, the loop-prevention header const, `leaderPinnedRoute(method, path) bool`, `Server.leaderForward()` middleware, and proxy construction (built once in `configure`, using `reverseproxy.NewSingleHostReverseProxy` with `config.WithResponseBodyStream(true)` + `config.WithTLSConfig` (full verification: `RootCAs` from the injected pool, empty `ServerName`, `MinVersion` TLS1.2) + `client.WithDialer(standard.NewDialer())`, a custom `SetDirector` that rewrites to the current leader host, and `SetErrorHandler` → 502).
4. **`internal/handler/server.go`**:
   - Add `LeaderInfo leaderInfo` and `LeaderForwardRootCAs *x509.CertPool` (nil = system pool) to `Deps`; add matching `leaderInfo leaderInfo` + `leaderForwardRootCAs *x509.CertPool` fields to `Server`.
   - In `NewServer`, copy `deps.LeaderInfo` → `s.leaderInfo` and `deps.LeaderForwardRootCAs` → `s.leaderForwardRootCAs`.
   - In `configure()`, after `h.Use(s.corsMiddleware())` (before route registration), add `h.Use(s.leaderForward())`.
   - `buildLeaderForward()` builds the `*tls.Config` from `s.leaderForwardRootCAs` (nil → system pool) — see §3.
   - (Data-plane changes are Phase C, same file.)
4a. **`internal/repository/raft_discovery.go`** — add `ServerCertFQDNSANs(cfg *RaftDiscoveryConfig, hostname string) []string` next to `PodSANs`, returning the pod's **own exact** DNS forms (no wildcard): `<hostname>.<headless>.<ns>.svc.<clusterDomain>` and the `.svc`-short form `<hostname>.<headless>.<ns>.svc` (the latter covers the `cluster_domain=""` override; mirrors `PodSANs`' dual-form coverage). `hostname` is the pod name from `os.Hostname()`.
4b. **`internal/repository/ca_providers.go`** — make `EmbeddedProvider.ServerTLSCert` re-issue the cached server cert when its SAN set no longer covers the required set (so the newly-added own-FQDN SANs are picked up on this rollout). After `loadTLSKeyPair` succeeds, parse the cert and check coverage against the required set (base `[localhost, supervisor, supervisor-control, supervisor-control.dagger-kubernetes.svc]` + `p.extraSANs` (now including the pod's own FQDN SANs) + `os.Hostname()` + IP `127.0.0.1`); on a gap, fall through to `issueServerCert`. Add helper `coversRequiredServerSANs(cert *x509.Certificate, requiredDNS []string, requiredIPs []net.IP) bool` (set containment of `cert.DNSNames`/`cert.IPAddresses` — exact strings, no wildcard matching). The required set changes only when the SAN *definition* changes (this rollout, or a `cluster_domain`/`headless_service` change), so re-issuance is a rare one-time event, not a per-pod cascade.

### Phase C — Data-plane relay

5. **`internal/handler/server.go`**:
   - Add `relayListener net.Listener` and `relayInPort int` fields to `Server`.
   - In `Start()`: compute `relayInPort` from `s.cfg.DataAddr` (parse port, `+1`); add a second listener goroutine for the relay-in port whose accept loop closes the conn when `!IsLeader()` and otherwise calls `s.serveTLSConn(raw, tlsConfig)` (terminal).
   - Modify the existing data accept loop: on accept, if `!s.leaderInfo.IsLeader()` → `s.serveDataRelay(raw)` instead of `serveTLSConn`.
   - Update `Shutdown` to close `s.relayListener`.
6. **Create `internal/handler/data_relay.go`** — `func (s *Server) serveDataRelay(client net.Conn)` (byte-pump with `dataConnSem` acquisition, 5s dial timeout, half-close via `CloseWrite`). See §3.

### Phase D — Remove label machinery

7. **`cmd/api/main.go`**:
   - Delete `const raftLeaderLabel = "dagger-kubernetes.io/raft-leader"` (line 859).
   - Delete `func observeLeadership(...)` (lines 865–899) and the `go observeLeadership(...)` call (line 530).
   - Remove now-orphaned imports `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"` and `"k8s.io/apimachinery/pkg/types"` — they are used **only** in `observeLeadership` (grep-confirmed: `.Patch(`/`metav1.`/`types.` appear only at line 876).
   - In `handler.NewServer(...)`'s `Deps`, add `LeaderInfo: raftStore` and `LeaderForwardRootCAs: leaderForwardRootCAs` (see below).
   - In `mintingProvider(cfg, clientset)`: build the discovery config once (`discovery := raftDiscoveryConfig(cfg)`) and `hostname, _ := os.Hostname()`; prepend `repository.ServerCertFQDNSANs(&discovery, hostname)` to the `extraSANs` passed to **both** `NewEmbeddedProviderWithSecret(...)` and `NewEmbeddedProvider(...)` (currently only `cfg.Server.DataHost` is passed). Keep `cfg.Server.DataHost` (the engine-facing data host) in the SAN list — the own-FQDN SANs are additive and do not disturb the data-host SAN. These `extraSANs` only affect the server cert, which is minting-CA-signed in embedded mode; in cert-manager/external mode the server cert is operator-managed (the same `extraSANs` do not apply to the external cert, and the operator adds the wildcard instead — see item 15).
   - Compute the verification pool once in `run()`: `var leaderForwardRootCAs *x509.CertPool`; when `cfg.Supervisor.Dataplane.TLS.Provider == "embedded"` set `leaderForwardRootCAs = serverMintingCA.CertPool()` (the shared minting CA — `domain.MintingCA.CertPool()`); otherwise leave nil (system pool for cert-manager/external, whose server cert is not minting-CA-signed). Pass it into `Deps`.
   - (`clientset`, `corev1`, `kubernetes`, `clientcmd`, `rest` stay — still used by TLS CA Secret, minting CA, fleet provider, `newK8sClientset`.)
8. **`deploy/helm/dagger-kubernetes/templates/service.yaml`** — remove the `dagger-kubernetes.io/raft-leader: "true"` selector lines (and their comments) from **both** the `-control` and `-data` Services. Both revert to the standard `dagger-kubernetes.selectorLabels` selector (all pods).
9. **`deploy/helm/dagger-kubernetes/templates/rbac.yaml`** — remove the `patch` verb from the `["services", "pods"]` rule. Verified: the **only** `pods patch` consumer is `observeLeadership`; the fleet provider (`k8s_provider.go`) uses pods `List`/`Get` only (engine scale is via StatefulSet `replicas`, not pod patch); the CA providers (`raft_tls.go` `ensureRaftCAFromSecret`, `ca_providers.go`) use `Secrets` only. `services` never needs `patch` either (`Update` is used). Resulting verbs: `["get", "list", "watch", "create", "update", "delete"]`.
10. **`deploy/helm/dagger-kubernetes/templates/statefulset.yaml`** — add a `relay` containerPort (`containerPort: 8444`) to the supervisor `ports` list (documentation + NetworkPolicy matching; the pod DNS is already provided by the headless Service, no new Service needed).

### Phase E — Docs

11. **Create `docs/design/ADR-041-per-pod-leader-forward-proxy.md`** (status Accepted; next number after ADR-040). Records: drop label-based leader routing; control-plane leader-forward middleware (classification, loop header, forward-before-auth); the **full-verification HTTPS internal hop** (embedded → each pod's own exact FQDN SANs, no wildcard; cert-manager/external → operator-supplied wildcard SAN, DNS-01 note) with per-provider root pool (embedded → minting CA pool; cert-manager/external → system pool); data-plane L4 relay (relay-in port 8444, loop prevention, half-close); removal of `observeLeadership`/`raftLeaderLabel`/`RaftStore.LeaderCh()`/`pods patch`.
12. **`docs/design/ADR-026-replicated-session-leases.md`** — add a revision note: the leader-routed Services (decision 2) are superseded by ADR-041's per-pod forward proxy + data relay.
13. **`docs/design/ADR-016-raft-multinode-tls.md`** — revision note: the "leader-only Service" open question (OQ1 / risk R3) is resolved by ADR-041 (per-pod forward).
14. **`docs/README.md`** (~line 295 + leader-routing mentions) — replace the "leader-routed Service / `raft-leader` label" description with "every pod forwards writes/tunnels to the leader; reads served by any pod".
15. **`deploy/helm/dagger-kubernetes/README.md`** (~line 318 + Raft section + TLS section) — update the leader-routing paragraph + Parameters tables (remove any `raft-leader` label references; document the relay-in port 8444 as an internal port). Document the TLS SANs: **embedded** auto-adds each pod's own exact FQDN SANs (no operator action); **cert-manager/external** require the operator to add the wildcard SAN `*.<release>-headless.<namespace>.svc.<clusterDomain>` (+ the `.svc` form) to their data-plane Certificate/Secret so follower→leader forward hops verify; ACME wildcard issuance requires a DNS-01 solver; private-CA operators must mount their CA bundle into the pod trust store.
16. **`AGENTS.local.md`** — §7 add a revision note: the `-control`/`-data` endpoints now select **all** pods (no longer one leader endpoint); update §5.1 verification accordingly.
17. **`DAGGER.md`** — no change (no `dagger/`, CI-script, or workflow change).
18. **Config samples** — no change (no new Go config keys; the relay-in port is derived from `server.data_addr`).

### Phase F — Tests (also see §6)

19. `internal/repository/raft_store_test.go` — delete `TestRaftStoreLeaderCh`; add `TestRaftStoreLeaderAddress`.
20. Create `internal/handler/leader_forward_test.go` and `internal/handler/data_relay_test.go`; update `internal/handler/test_helper_test.go` (`newTestEnv` sets `LeaderInfo: store`).
21. `cmd/api/main_test.go` — remove/adjust any test touching `observeLeadership`/`raftLeaderLabel` (grep first; none expected beyond what Phase D removes).

---

## 3. Data structures & function signatures

```go
// internal/repository/raft_store.go
func (s *RaftStore) LeaderAddress() string   // "" when no leader; FQDN:raftPort

// DELETED:
//   func (s *RaftStore) LeaderCh() <-chan bool
```

```go
// internal/repository/raft_discovery.go
// ServerCertFQDNSANs returns this pod's OWN exact DNS SANs (no wildcard) so any
// follower can verify the leader's control-plane server cert when dialing the
// leader's pod FQDN. The only host a follower ever dials is the leader's own
// pod FQDN (from LeaderAddress()), so per-pod exact FQDNs are complete for
// verification and strictly stronger than a wildcard (one key never covers
// other names). Both the FQDN form and the ".svc"-short form (cluster_domain="")
// are emitted, mirroring PodSANs. hostname is the pod name (os.Hostname()).
func ServerCertFQDNSANs(cfg *RaftDiscoveryConfig, hostname string) []string

// internal/repository/ca_providers.go
func (p *EmbeddedProvider) requiredServerSANs() (dns []string, ips []net.IP) // base + p.extraSANs + os.Hostname() + 127.0.0.1
func coversRequiredServerSANs(cert *x509.Certificate, dns []string, ips []net.IP) bool
```

```go
// internal/handler/leader_forward.go
const leaderForwardedHeader = "X-Dagger-Kubernetes-Leader-Forwarded"

// leaderInfo is the subset of the Raft store the forward middleware needs.
// *repository.RaftStore implements it.
type leaderInfo interface {
    IsLeader() bool
    LeaderAddress() string
}

// leaderPinnedRoute reports whether (method, path) must be served by the leader.
func leaderPinnedRoute(method, path string) bool {
    if method != "GET" && method != "HEAD" && method != "OPTIONS" {
        return true // every write is a Raft write or touches leader-local state
    }
    if strings.HasPrefix(path, "/api/v1/traces/") && strings.HasSuffix(path, "/live") {
        return true // SSE: leader-local liveHub
    }
    if strings.HasPrefix(path, "/api/v1/fleet/") && strings.HasSuffix(path, "/purge-cache") {
        return true // purge-job status: leader-local
    }
    return false
}

func isReadOnlyMethod(method string) bool { /* GET/HEAD/OPTIONS */ }

// leaderForward is the global middleware. Returns a no-op (c.Next) on the
// leader, for nil leaderInfo, for follower-safe routes, or when already
// forwarded; otherwise rewrites + proxies to the current leader.
func (s *Server) leaderForward() app.HandlerFunc

// controlForwardTarget derives scheme + host:port for the leader's control
// plane from leaderAddress + ServerConfig (https when CertPath/KeyPath set,
// else http; control port from ControlAddr).
func (s *Server) controlForwardTarget(leaderAddress string) (scheme, hostPort string, ok bool)
```

```go
// internal/handler/data_relay.go
// serveDataRelay (follower) pumps raw bytes between client and the leader's
// relay-in port. TLS terminates end-to-end on the leader.
func (s *Server) serveDataRelay(client net.Conn)

// halfCloseWrite closes the write side when the conn supports it (TCPConn).
func halfCloseWrite(c net.Conn)
```

Middleware shape (Hertz):

```go
func (s *Server) leaderForward() app.HandlerFunc {
    return func(ctx context.Context, c *app.RequestContext) {
        if s.leaderInfo == nil || s.leaderInfo.IsLeader() ||
            !leaderPinnedRoute(string(c.Method()), string(c.Path())) ||
            c.Request.Header.Get(leaderForwardedHeader) != "" {
            c.Next(ctx)
            return
        }
        scheme, hostPort, ok := s.controlForwardTarget(s.leaderInfo.LeaderAddress())
        if !ok {
            writeError(c, consts.StatusServiceUnavailable, "no raft leader available")
            return
        }
        c.Request.Header.Set(leaderForwardedHeader, "1")
        // director rewrites req to scheme://hostPort + original path/query;
        // errorHandler → writeError(c, consts.StatusBadGateway, "leader unreachable").
        s.leaderProxy.ServeHTTP(ctx, c)
    }
}
```

Forward proxy construction (in `configure()` / a `buildLeaderForward()` helper):

```go
// Full verification (no InsecureSkipVerify): RootCAs from the injected pool
// (nil = system pool); ServerName left empty so Go's tls.Client derives it from
// the dialed leader FQDN (it uses the dial address when ServerName is unset).
// Min version TLS1.2. Standard dialer required — netpoll does not do client TLS.
tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
if s.leaderForwardRootCAs != nil {
    tlsCfg.RootCAs = s.leaderForwardRootCAs
}
p, err := reverseproxy.NewSingleHostReverseProxy("https://leader.invalid",
    config.WithResponseBodyStream(true),           // SSE streaming
    config.WithDialTimeout(2*time.Second),          // dial timeout
    client.WithDialer(standard.NewDialer()),        // TLS-capable dialer
    config.WithTLSConfig(tlsCfg),                   // full verification
)
p.SetDirector(func(req *protocol.Request) {
    // rewrite req to scheme://<leader-fqdn>:<controlPort> + original path/query
    // (host = host portion of LeaderAddress(); port from ControlAddr).
})
p.SetErrorHandler(func(c *app.RequestContext, err error) {
    s.logger.WithError(err).Error("leader forward failed")
    writeError(c, consts.StatusBadGateway, "leader unreachable")
})
```

`controlForwardTarget` returns the **scheme** (https when `CertPath`/`KeyPath` are set, http for bare dev) and the leader `host:port`; the director sets `req.URI().SetScheme(scheme)` / `SetHost(hostPort)`. `tls.Config.ServerName` stays empty so verification runs against the leader FQDN derived from the dial. Embedded verification needs no wildcard: the leader cert carries the leader pod's own exact FQDN SANs (see `ServerCertFQDNSANs` above); cert-manager/external certs carry the operator-supplied wildcard. If a bespoke fallback is needed (reverseproxy TLS+streaming incompatibility), use `tls.DialWithDialer(&net.Dialer{Timeout: 2*time.Second}, "tcp", hostPort, tlsCfg.Clone())` after setting `tlsCfg.ServerName = leaderFQDN` explicitly.

Data relay shape:

```go
func (s *Server) serveDataRelay(client net.Conn) {
    select {
    case s.dataConnSem <- struct{}{}:
    default:
        _ = client.Close()
        s.logger.Warn("data relay connection limit reached, dropping connection")
        return
    }
    defer func() { <-s.dataConnSem; _ = client.Close() }()

    if s.leaderInfo == nil {
        return
    }
    addr := s.leaderInfo.LeaderAddress()
    if addr == "" {
        return
    }
    host, _, err := net.SplitHostPort(addr)
    if err != nil {
        return
    }
    backend, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(s.relayInPort)), 5*time.Second)
    if err != nil {
        s.logger.WithError(err).WithField("leader", host).Warn("data relay dial leader failed")
        return
    }
    defer backend.Close()

    done := make(chan struct{}, 2)
    go func() { _, _ = io.Copy(backend, client); halfCloseWrite(backend); done <- struct{}{} }()
    go func() { _, _ = io.Copy(client, backend); halfCloseWrite(client); done <- struct{}{} }()
    <-done
    _ = client.Close()
    _ = backend.Close()
}
```

---

## 4. Edge cases

- **Leader change mid-request:** forwarded control request already in flight to the old leader completes there; the old leader's write may return `ErrNotLeader` (→ 503), the client retries against the (now multi-endpoint) Service. Already-forwarded requests landing on a stale follower are served locally (loop header) and 503 — no infinite loop, bounded by the election window.
- **No leader known:** `LeaderAddress()==""` → control 503 "no raft leader available"; data relay closes the client conn (client reconnects). Followers always have endpoints (probes still answer), so no zero-endpoint window.
- **Forwarding loop:** prevented by the `X-Dagger-Kubernetes-Leader-Forwarded` header (control) and the terminal relay-in port + `IsLeader()` accept-gate (data). Relays never re-relay.
- **Follower SSE pinning:** `GET .../live` is leader-pinned (forwarded with streaming). A client subscribed via a follower transparently receives the leader's `liveHub` events. Streams flush per event via `ResponseBodyStream`.
- **Streaming flush:** covered by `config.WithResponseBodyStream(true)`; §6 test asserts an SSE-style stream is forwarded incrementally (not buffered until EOF).
- **Relay half-close:** `CloseWrite` on one direction after its `io.Copy` EOF keeps the opposite direction alive until the TLS `close_notify` completes.
- **Dial timeout to unreachable leader:** 5s → close client conn (data) / 502 (control).
- **Leader FQDN resolution:** reuses the raft discovery FQDN (`LeaderAddress()` returns the same FQDN form `DeriveAdvertiseAddr` advertises); the pod's `dnsPolicy: None` bypass (ADR-029) applies to these dials too.
- **Single-node deployment:** `IsLeader()` is always true → the middleware never forwards; the data accept loop always takes the `serveTLSConn` branch. The relay-in listener is started but never receives (or immediately rejects) — harmless.
- **Bare-binary / non-K8s multi-node:** `LeaderAddress()` still returns the leader's advertised `host:port` (static `raft.peers` or DNS); the forward hop uses `http` when no control-plane cert is configured (bare dev). Relay-in port derives from `server.data_addr` as usual.
- **HTTP/1.1 vs h2c:** the control-plane server does not enable H2C (`Options.H2C` default false); the forward hop is plain HTTP/1.1 over TLS or plaintext. `reverseproxy` speaks HTTP/1.1 (its HTTP/1 host client). No h2c path to special-case.
- **Request body replay (large OTLP bodies):** the middleware forwards **before** per-handler body caps (`maxControlBody`, `defaultOTLPMaxBodyBytes`). Because `server.WithStreamBody(true)` is enabled, the body is streamed, not double-buffered; the leader's handlers still apply their own caps (`readBoundedBody` / OTel `Content-Length` check), so caps remain enforced at the authority that actually reads the body. OTLP ingest is a POST → forwarded to the leader, where the existing 64 MiB cap applies.

---

## 5. Error handling & validation

- **Error wrapping (`%w`)**: only new wrapping is in `serveDataRelay`/`controlForwardTarget` parse errors (`fmt.Errorf("parse leader addr %s: %w", ...)`). `LeaderAddress()` has no error path (returns `""`).
- **Status codes**: 503 `writeError(c, consts.StatusServiceUnavailable, "no raft leader available")`; 502 `writeError(c, consts.StatusBadGateway, "leader unreachable")`. Both use the existing `ErrorResponse{Message: ...}` shape (no new JSON schema).
- **Log fields**: `s.logger.WithError(err).WithField("leader", host).Warn("data relay dial leader failed")`; `WithError(err).Error("leader forward failed")`.
- **Config validation**: no new config keys → no new `validateXxx`. The relay-in port derivation (`DataAddr` port + 1) is fail-closed by construction (parse failure → refuse to start the relay listener; log an error). The own-FQDN SANs are derived from `os.Hostname()` + `headless_service`/`namespace`/`cluster_domain` (already validated for Raft); if `headless_service` is empty in a multi-node deployment the FQDN SANs are skipped and followers fail verification (fail-closed, logged at startup). Nothing else to validate.
- **TLS verification failure (fail-closed):** a forward-hop verification error surfaces via the proxy `ErrorHandler` → 502 `writeError(..., "leader unreachable")` + `s.logger.WithError(err).Error("leader forward failed")`. Never downgrade to `InsecureSkipVerify`.
- **Embedded server-cert re-issuance on rollout:** `ServerTLSCert` now re-checks SAN coverage on each boot. Pods with a cached `server.crt`/`server.key` that predate the own-FQDN SANs are transparently re-issued under the same shared minting CA (no trust split, no manual PVC deletion) — mirroring the raft leaf re-issue behavior in ADR-029. Re-issuance triggers only when the SAN *definition* changes (this rollout, or a `cluster_domain`/`headless_service` change); new pods issue their own certs with their own FQDN at boot with zero cross-pod coupling. cert-manager/external certs are operator-managed: the operator must rotate their Certificate with the wildcard SAN added (one-time).
- **Dead-symbol lint hazard (AGENTS.md) — every deleted symbol, explicitly:**
  - `cmd/api/main.go`: `raftLeaderLabel` (const), `observeLeadership` (func), imports `metav1`, `types`. Delete all four in one changeset.
  - `internal/repository/raft_store.go`: `LeaderCh()` method. Delete in the same changeset as its only caller (`observeLeadership`) and its test `TestRaftStoreLeaderCh`.
  - No Helm/CI `unused`-equivalent beyond `helm lint`/template matrix (selector removal is a pure template edit).

---

## 6. Tests

Project rules: stdlib `testing` only (no testify/ginkgo); table-driven; `logrus` with `io.Discard`; no hardcoded ports in `tests/integration` (use `freeListener` from `net_helpers_test.go` + timed `Shutdown` in `t.Cleanup`).

- **`internal/repository/raft_store_test.go`**:
  - Delete `TestRaftStoreLeaderCh`.
  - Add `TestRaftStoreLeaderAddress`: using `NewInmemRaftStore` (always leader), assert `LeaderAddress()` is non-empty and `net.SplitHostPort` parses. Add a case where leadership is unknown (construct a store and assert `""` before election, or use a stub — see below). Cover the "no leader → empty string" branch via a `fakeLeaderInfo`-style unit test in the handler package instead if inmem always elects.
- **`internal/repository/raft_discovery_test.go`** — add table-driven `TestServerCertFQDNSANs`: for `clusterDomain="cluster.local"` assert `[<hostname>.<headless>.<ns>.svc.cluster.local, <hostname>.<headless>.<ns>.svc]`; for `clusterDomain=""` assert the `.svc` form only (no FQDN form); assert the strings embed the configured `hostname`/`HeadlessService`/`Namespace` exactly, and that **no `"*."` wildcard entry is produced**.
- **`internal/repository/ca_providers_test.go`** — extend `TestEmbeddedProviderServerCertSANs` to assert the returned cert's `DNSNames` contains the injected own-FQDN SAN(s) **and contains no `"*."` wildcard entry**; add `TestServerTLSCertReissuesOnSANGrowth`: pre-write a `server.crt`/`server.key` lacking the own-FQDN SANs, call `ServerTLSCert`, assert a fresh cert is issued whose `DNSNames` now include them (proves the re-issue path).
- **`internal/handler/leader_forward_test.go`** (new): drive a `route.NewEngine(config.NewOptions(nil))` with the middleware + a fake leader backend (an `httptest`-style Hertz test server; the project already uses `ut.PerformRequest` against `route.NewEngine` — see `middleware_test.go`). Table-driven cases using a `fakeLeaderInfo{leader bool; addr string}`:
  1. leader serves locally (handler runs, status echoed);
  2. follower serves a `GET /api/v1/traces/xyz` read locally (handler runs);
  3. follower forwards a `POST /api/v1/users` to the fake leader (method/path/body/`X-Dagger-Kubernetes-Leader-Forwarded: 1` observed on the backend);
  4. follower forwards `GET /api/v1/traces/xyz/live` (SSE is leader-pinned despite GET);
  5. follower forwards `GET /api/v1/fleet/v0.19.0/purge-cache` (leader-local status);
  6. follower returns 503 when `LeaderAddress()` is `""`;
  7. already-forwarded request served locally (no re-forward);
  8. proxy transport error → 502.
  The fake leader backend is a real `httptest.NewServer`/Hertz engine bound to `127.0.0.1:0` (no hardcoded port); `LeaderAddress()` is faked to `"127.0.0.1:<leader-port>"` so the middleware substitutes the control port from `ControlAddr`. TLS-verification cases (feasible with stdlib only): (9) **pool selection** — embedded mode injects `LeaderForwardRootCAs` and the proxy's `tls.Config.RootCAs` equals it; cert-manager/external mode (nil) yields nil `RootCAs` (system pool); (10) **ServerName derivation** — `controlForwardTarget`/director produce the leader FQDN host, and `tls.Config.ServerName` is left empty. A full end-to-end handshake is **optional**: if added, mint a CA + a leaf with exact SAN `leader.localhost` via the existing `repository.NewMintingCA`/`IssuePeerCertificate` (stdlib `crypto/x509`), stand up an `httptest.NewTLSServer`, and dial with `RootCAs` = that CA and `ServerName` set to `leader.localhost` — assert success and that a non-matching name fails. This proves exact-FQDN verification without a real cluster.
- **`internal/handler/data_relay_test.go`** (new): use `net.Pipe()`/`freeListener` + `serveDataRelay` directly (as `data_conn_test.go` does for `serveDataTunnel`). Cases: (a) relay pumps bytes both ways to a local relay-in listener (echo/discard), (b) `LeaderAddress()==""` closes client without dialing, (c) dial failure closes client, (d) half-close propagates EOF in one direction without killing the other. No hardcoded ports.
- **`internal/handler/test_helper_test.go`**: set `LeaderInfo: store` in `newTestEnv` so existing handler tests (single-node, always leader) keep serving locally.
- **`cmd/api/main_test.go`**: remove/adjust any `observeLeadership`/`raftLeaderLabel` references (grep; none expected). Add a table-driven `leaderPinnedRoute` classification unit test (in the handler package test) locking in the exact leader-pinned set (all mutating methods; the two GET exceptions; all other GETs follower-safe).
- **Existing tests that change**: `internal/repository/raft_store_test.go` (delete `TestRaftStoreLeaderCh`); `cmd/api/main_test.go` (if it references `observeLeadership`). Integration tests: no new integration test required; if one is added, use `freeListener` + timed `Shutdown`.

---

## 7. Docs

- New ADR `docs/design/ADR-041-per-pod-leader-forward-proxy.md` (Accepted). Records the full-verification decision: **embedded** uses each pod's **own exact FQDN SANs** (`<hostname>.<headless>.<ns>.svc.<clusterDomain>` + `.svc` form — no wildcard: the only dialed host is the leader's own FQDN, so exact per-pod SANs are complete and stronger than a wildcard, with zero cross-pod coupling); **cert-manager/external** uses the operator-supplied wildcard SAN (`*.<headless>.<ns>.svc.<clusterDomain>` + `.svc` form; ACME DNS-01 for wildcards; private-CA bundle mounting); per-provider root pool (embedded → minting CA; cert-manager/external → system pool).
- Update `ADR-026` (revision note), `ADR-016` (resolve OQ1/R3), `docs/README.md` (~line 295), `deploy/helm/dagger-kubernetes/README.md` (~line 318 + Parameters + TLS SAN docs: embedded exact FQDN SANs auto-added, cert-manager/external wildcard operator requirement), `AGENTS.local.md` (§5.1 + §7 revision note).
- `config/config.app.yaml` / `.sample`: no change. `DAGGER.md`: no change.

---

## 8. Verification

**CI gate (mandatory, AGENTS.md):**

```bash
dagger call -m ./dagger --src . ci export --path out
```

Minimum without a Docker daemon: `go build ./... && go vet ./... && go test ./...` plus `dagger call -m ./dagger --src . lint`.

**Local-cluster redeploy (AGENTS.local.md §4–5, mandatory):** release `dagger-kubernetes-test`, image `docker.io/disaster/dagger-kubernetes:dev`, kubeconfig `/home/user/.kube/home`.

1. `docker build -t docker.io/disaster/dagger-kubernetes:dev .` && `docker push ...`.
2. `helm --kubeconfig /home/user/.kube/home get values dagger-kubernetes-test -n dagger-kubernetes-test -o yaml > /tmp/dagger-kubernetes-test.values.yaml`.
3. `helm --kubeconfig /home/user/.kube/home upgrade --install dagger-kubernetes-test ./deploy/helm/dagger-kubernetes --namespace dagger-kubernetes-test -f /tmp/dagger-kubernetes-test.values.yaml --set supervisor.image.tag=dev --set supervisor.image.pullPolicy=Always --set supervisor.image.repository=docker.io/disaster/dagger-kubernetes`.
4. `kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test rollout restart statefulset/dagger-kubernetes-test-dagger-kubernetes` then `rollout status ... --timeout=300s`.

**Change-specific checks:**
- `kubectl ... get endpoints dagger-kubernetes-test-dagger-kubernetes-control dagger-kubernetes-test-dagger-kubernetes-data` → **both list all 3 pods** (no longer one leader pod).
- Writes reach the leader: `POST /api/v1/groups` via the ingress succeeds regardless of which pod answered; leader logs show the applied write.
- Tunnels work: run a real pipeline (`dagger core container from --address=alpine:3.20 ...`); confirm session lease touches land on the leader (`touchSession` no `ErrNotLeader`).
- SSE works: open `/pipelines/<id>` in the UI; live updates stream while the pipeline runs.
- Follower reads: `GET /api/v1/status`, `/api/v1/fleet`, `/api/v1/traces` return 200 from any pod.
- Failover: `kubectl delete pod <leader-pod>`; during the election the control plane answers (no zero-endpoint gap); writes 503 briefly then succeed on retry; a long-lived tunnel is not dropped.
- Relay logging: leader `serveTLSConn` success log shows `remote_addr` = the follower pod IP (expected).
- **Own-FQDN SAN present (embedded, no wildcard):** `kubectl exec` the leader pod and dump the served control-plane cert (`openssl s_client -connect <leader-fqdn>:8080 2>/dev/null | openssl x509 -noout -text | grep -A1 'Subject Alternative Name'`) — assert `DNS:<leader-pod>.<release>-headless.<namespace>.svc.cluster.local` (exact, no `*.*` wildcard).
- **Forward hop fully verified (no InsecureSkipVerify):** force a follower to forward a write (delete the leader pod, or `port-forward` a follower's control port) and confirm the follower log shows a successful forward with **no** `x509`/`certificate`/`unknown authority` errors and the leader applies the write. Grep the forward path for `InsecureSkipVerify` — assert zero occurrences.
- **cert-manager/external path (documentation + operator check):** the live home cluster runs `provider: cert-manager`, so the forward-hop SAN is **not** auto-added (only embedded auto-adds exact FQDN SANs); the operator must add `*.<release>-headless.<namespace>.svc.cluster.local` to the cert-manager Certificate (DNS-01) before the forward hop verifies end-to-end on the home cluster. Flag this explicitly in the §5.2 human-verification step.

---

## 9. Open questions / risks (human decision required)

- **OQ-1 — RESOLVED: full TLS verification (no `InsecureSkipVerify`).** The forward hop is HTTPS with full certificate verification. **Embedded** auto-adds each pod's **own exact FQDN SANs** (`<hostname>.<headless>.<ns>.svc.<clusterDomain>` + `.svc` form — no wildcard: the only dialed host is the leader's own FQDN, so exact per-pod SANs are complete and stronger than a wildcard); followers verify with `RootCAs = minting CA pool`. **cert-manager/external** operators add the wildcard SAN `*.<headless>.<ns>.svc.<clusterDomain>` (+ `.svc` form) to their Certificate resource (ACME wildcard → DNS-01; private CA → trivial); followers use the system pool (or a mounted private-CA bundle). See §1.2 / §7.
- **OQ-2 — RESOLVED: both planes in scope.** The plan implements the control-plane middleware **and** the data-plane L4 relay, removing all label machinery.
- **OQ-3 — relay-in port selection.** Derived `server.data_addr` port+1 (=8444), no new config key (matches the fixed-port convention: 8080/8443/8081). *Alternative:* `server.data_relay_port` config key (explicit, but adds `v.SetDefault` + sample + values + README churn). *Recommendation:* derived.
- **OQ-4 — transition window.** No dual-mode transition is planned: the change is atomic (code + chart in one changeset + one rollout). A brief mixed-revision window (one old pod still labeling, new pods not) is harmless: the old label selector simply stops matching the new pods and the new forward handles traffic. *Recommendation:* no transition flag.
- **R1 — data relay port is network-isolated by convention, not enforcement.** It carries end-to-end mTLS ciphertext (no plaintext), so no extra auth is required; it MUST remain reachable only pod-to-pod (no Service, no ingress). If a NetworkPolicy is added later, it must allow pod→pod on 8444.
- **R2 — reverseproxy flush behavior.** The plan relies on `config.WithResponseBodyStream(true)` for SSE flush. If the §6 SSE test shows buffering, switch the `/live` route to a bespoke `client.WithResponseBodyStream(true)` + `sse.Writer` pump (fallback documented in §1.2), still carrying the full-verification TLS config.
- **R3 — dead-symbol lint hazard** is the main CI risk (Phase A/D/F must land in one changeset).
- **R4 — cert-manager/external wildcard is operator-managed.** On the live home cluster (`provider: cert-manager`), full verification is NOT automatic: the operator must add the wildcard SAN to the cert-manager Certificate before the forward hop verifies. ACME wildcards require a DNS-01 solver (HTTP-01 cannot issue wildcards); a private-CA issuer adds the SAN trivially. Until the SAN is added, follower→leader forwards fail verification and 502 — the operator step is mandatory, not optional, for cert-manager/external deployments.
- **R5 — embedded server-cert re-issuance on rollout.** Cached `server.crt`/`server.key` from before this change lack the own-FQDN SANs; `ServerTLSCert` re-issues them in place on first boot (same shared CA, no PVC deletion). This is an unavoidable one-time event caused by the SAN-set definition change; new pods issue their own certs with their own FQDN at boot with zero cross-pod coupling. Confirmed safe; verify once on the home cluster if it were embedded via `openssl` (see §8).

---

## Recommended answer to the user's question

The per-pod leader-forward proxy is **better** than the label-based routing, and it is worth doing. On the control plane it is strictly superior — it removes the label-patch/endpoint-propagation failover window and the zero-endpoint outage, drops the `pods patch` RBAC privilege, and enables follower-served reads instead of pinning all traffic to one pod. On the data plane the win is narrower (long-lived tunnels already had to reach the leader), but the L4 relay removes the same label-lag window and RBAC surface for a small amount of code, so it is still the right call. The one honest caveat is that the control-plane internal forward hop must stay HTTPS with **full certificate verification** — auto-added as each pod's own exact FQDN SANs for the embedded provider (no wildcard), but a documented one-time operator step (a wildcard SAN + DNS-01) for cert-manager/external — a trust-configuration cost, not a correctness regression.
