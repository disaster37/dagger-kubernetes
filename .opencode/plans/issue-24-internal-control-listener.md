# Plan: Internal-only control listener for the follower→leader forward hop (delta on PR #28 / issue #24)

Amends: `.opencode/plans/issue-24-leader-forward-proxy.md`. Implemented as **additional commits on branch `feat/issue-24-leader-forward-proxy`** (verified: HEAD is that branch; commits `232f99c` + `b9cf698` are present). This delta changes the *where* and *what-cert* of the internal forward hop; it does not touch leader-pinning classification (`leaderPinnedRoute`), OAuth-callback pinning, loop-prevention, streaming, or timeouts.

## 1. Decision

### 1.1 The security boundary (why this is right)

The prior design confused **two trust domains**:

- **Public/edge trust** (ingress → cert-manager Let's Encrypt) — user's stated intent: cert-manager is for *public access only*.
- **Intra-mesh trust** (follower pod → leader pod) — should be the *private* goca minting CA only.

By forwarding to the leader's **public control port (8080)** and verifying with the same cert the public listener serves, the prior design forced cert-manager/external operators to put a **wildcard SAN** on their public `Certificate` (ACME DNS-01) just so pod-to-pod hops could verify. That is (a) a wider-scope credential than needed on the public edge, and (b) an operator failure mode (502s on every login/write until the wildcard exists).

**This delta dedicates an internal-only HTTPS listener** (port `control_addr` + 2 = **8082**, pod-to-pod only — no Service, no ingress, exactly like relay-in 8444) that **always** serves a minting-CA-signed per-pod leaf with the pod's **own exact FQDN SANs** (no wildcard). The forward hop's verification pool becomes **unconditionally** the minting-CA pool. Consequences:

- cert-manager stays public-only (user's intent). No operator wildcard step, no DNS-01 requirement.
- No wildcard anywhere in the mesh: each pod holds a leaf covering only its own deterministic FQDNs. No all-pod enumeration, so **no re-issuance cascade on scale events**.
- The minting CA (goca, `github.com/disaster37/goca`) is **always available in every provider mode** — verified: `NewCertManagerProvider`/`NewExternalProvider` wrap an `*EmbeddedProvider` for the minting CA (ca_providers.go ~341–376), and `selectTLSProvider` always returns a provider whose `MintingCA()` is the embedded goca CA; a self-contained internal leaf can therefore be minted in **all** modes.

### 1.2 Internal leaf: dedicated, minted in ALL modes (chosen over "reuse server.crt in embedded")

Uniform trust story: the internal listener **always** serves a leaf minted by the embedded goca CA — `embedded`, `cert-manager`, and `external` all use the identical issuance/caching/re-issue path. The public listener keeps its own cert (`server.crt` in embedded; cert-manager PEM otherwise). Justification: (1) no code path ever presents the public cert-manager cert on the mesh listener; (2) rotating the public cert never disturbs the internal listener; (3) one small, single re-issue routine governs the internal leaf's lifetime/SANs. The alternative (reuse `server.crt` in embedded, mint a new leaf only in file modes) leaves two divergent "what cert serves the internal listener" paths for no benefit.

### 1.3 Dual-listener mechanics (hertz v0.10.5)

Verified against `pkg/app/server/hertz.go` + `pkg/common/config/option.go`: `server.Hertz` is `struct { *route.Engine; signalWaiter }`, and each `route.Engine` owns **one** `Options` (one `Listener`/`Addr`/`TLS`). There is **no** `server.WithEngine` and no way for one engine to bind two listeners. `Options.AltTransporterNewer` is a second *transporter* (monitoring), not a second HTTP listener.

**Chosen mechanism:** build **two** `*server.Hertz` instances that share **one route-registration function** (`registerRoutes`). The public engine is unchanged; a second engine is built with `server.WithListener(internalListener)` + `server.WithTLS(goca internal leaf)` and given the identical middleware stack + route table. This is the safest concrete mechanism given the one-engine/one-listener constraint. *Fallback if routing duplication proves objectionable:* register a small forward-only handler on a raw `http.Server`-style wrapper is **not** acceptable (loses auth middleware) — instead the fallback is a second full route registration (i.e. exactly this design). The repo already binds listeners the same way for the public control plane (`server.WithListener(s.cfg.ControlListener)`, server.go ~545).

### 1.4 Internal listener runs the SAME routes + auth

**Yes — the leader re-verifies authentication.** The internal listener serves the identical full stack (request log, security headers, CORS, `leaderForward`, and every per-handler `requireAuth`/`adminOnly` route). Correctness: a follower forwards the original request with credentials (Authorization/Cookie) and the `X-Dagger-Kubernetes-Leader-Forwarded: 1` header; the leader's internal listener serves it locally (loop header short-circuits `leaderForward`) and its handlers re-run auth against the same replicated FSM/JWT (forward-before-auth is unchanged). Unmarked leader-pinned requests that ever reached a follower's internal listener would still forward correctly (idempotent), but the only caller — the follower forward proxy — always marks the header, so no loop is possible.

### 1.5 Port derivation

`internalControlPortFromControlAddr(controlAddr) = control port + 2 = 8082`. Verified collision table: control 8080 (`config.app.yaml`/configmap `control_addr: ":8080"`, statefulset `containerPort: 8080`), **raft 8081 = control+1** (`raft.bind_addr: ":8081"`, statefulset `name: raft`/`containerPort: 8081`, headless Service `port: 8081`), data 8443, relay-in 8444 = data+1. So +1 is raft and +2 is free. **No new config key** — derived like relay-in (which is also absent from the ConfigMap/values and only added as a `containerPort`).

---

## 2. Exact file changes (ordered)

### Phase 1 — CA provider: internal leaf issuance

1. **`internal/domain/ca.go`** — add to the `CAProvider` interface:
   ```go
   type CAProvider interface {
       MintingCA() (MintingCA, error)
       ServerTLSCert() (tls.Certificate, error)
       InternalServerTLSCert() (tls.Certificate, error) // NEW
   }
   ```

2. **`internal/repository/ca_providers.go`** — implement:
   - `func (p *EmbeddedProvider) InternalServerTLSCert() (tls.Certificate, error)` — mint+cache `server-internal.crt`/`server-internal.key` under `p.caPath`, SAN-coverage re-issue on mismatch (mirrors `ServerTLSCert`, lines 69–94): load cached pair → `x509.ParseCertificate` → `coversRequiredServerSANs(leaf, p.internalServerSANs(), nil)` → reuse else `issueInternalServerCert`.
   - `func (p *EmbeddedProvider) internalServerSANs() []string` — returns `dedupeStrings(p.extraSANs)` (the pod's own FQDN SANs + engine data host, already computed by `mintingProvider`; DataHost is harmless extra). No new constructor params.
   - `func (p *EmbeddedProvider) issueInternalServerCert(ca *MintingCA, certPath, keyPath string) (tls.Certificate, error)` — `ca.IssueServerCertificate("supervisor-internal", "dagger-kubernetes", p.internalServerSANs(), 5*365*24*time.Hour)` + write 0600 + `tls.X509KeyPair`.
   - `func (p *fileCAProvider) InternalServerTLSCert() (tls.Certificate, error) { return p.minting.InternalServerTLSCert() }`.

### Phase 2 — Wire it in main.go

3. **`cmd/api/main.go`**:
   - **Delete** the provider-dependent pool block (lines 117–125): `var leaderForwardRootCAs *x509.CertPool; if embedded { leaderForwardRootCAs = serverMintingCA.CertPool() }` and its comment.
   - **Delete** the `crypto/x509` import (line 8) — verified it is used **only** by that block (grep: only lines 8 + 122).
   - After `serverTLS, err := tlsProvider.ServerTLSCert()` (line ~127), add:
     ```go
     internalTLS, err := tlsProvider.InternalServerTLSCert()
     if err != nil {
         return fmt.Errorf("get internal server TLS cert: %w", err)
     }
     ```
   - In `handler.ServerConfig{...}` add `InternalTLSCert: &internalTLS,`.
   - **Delete** `LeaderForwardRootCAs: leaderForwardRootCAs` from the `Deps` literal (line 401). Keep `LeaderInfo: raftStore`.

### Phase 3 — Handler: dual listener + unconditional pool

4. **`internal/handler/server.go`**:
   - **Deps:** delete `LeaderForwardRootCAs *x509.CertPool` (lines 170–173) + its comment.
   - **ServerConfig:** add `InternalListener net.Listener` (tests) and `InternalTLSCert *tls.Certificate` (minted leaf), with comments.
   - **Server struct:** delete `leaderForwardRootCAs *x509.CertPool` (line 221); add `internalHertz *server.Hertz`, `internalListener net.Listener`, `internalControlPort int`.
   - **NewServer:** delete the `leaderForwardRootCAs: deps.LeaderForwardRootCAs` copy (line 274).
   - **`configure()`** — extract the middleware+route body (lines 577–664) into `func (s *Server) registerRoutes(h *server.Hertz)`; `configure()` keeps `buildProxies()` + public-engine opts + `buildLeaderForward()` and ends with `s.registerRoutes(h)`.
   - **`Start()`** — after `s.hertz = h` (line 324), call `s.startInternalControlListener()` (before or after data-plane boot; do it immediately after control-plane boot).
   - **`Shutdown()`** — before the existing listener closes, add `if s.internalListener != nil { _ = s.internalListener.Close() }` and `if s.internalHertz != nil { _ = s.internalHertz.Shutdown(ctx) }` (best-effort; the public `s.hertz.Shutdown(ctx)` error remains the returned signal).

5. **`internal/handler/leader_forward.go`**:
   - **`leaderForwardTLSConfig()`** — replace body with unconditional minting-CA pool:
     ```go
     func (s *Server) leaderForwardTLSConfig() *tls.Config {
         return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: s.mintingCA.CertPool()}
     }
     ```
     Update its doc comment (no more "per-provider pool / nil = system pool").
   - **`controlForwardTarget()`** — new scheme/port logic:
     ```go
     func (s *Server) controlForwardTarget(leaderAddress string) (scheme, hostPort string, ok bool) {
         host, _, err := net.SplitHostPort(leaderAddress)
         if err != nil || host == "" { return "", "", false }
         if s.internalControlPort <= 0 { return "", "", false }
         return "https", net.JoinHostPort(host, strconv.Itoa(s.internalControlPort)), true
     }
     ```
     Scheme is now unconditionally `https` (the internal listener always carries TLS). Update the doc comment. Add `strconv` import; drop the now-unused `CertPath`/`KeyPath` reads from this function (they still gate the *public* listener TLS in `configure()`, unchanged).
   - `buildLeaderForward()` is otherwise unchanged (still `WithTLSConfig(s.leaderForwardTLSConfig())`, `WithResponseBodyStream`, `standard.NewDialer`, 2s dial timeout, loop header in `leaderForward()`).

6. **`internal/handler/internal_control.go`** (NEW) — mirrors `data_relay.go:112 startRelayListener`:
   ```go
   func internalControlPortFromControlAddr(controlAddr string) (int, error)
   func (s *Server) startInternalControlListener()
   ```
   `startInternalControlListener`:
   1. if `s.cfg.InternalTLSCert == nil` → log `"internal control listener disabled: no internal TLS certificate"`, return.
   2. `port, err := internalControlPortFromControlAddr(s.cfg.ControlAddr)`; on error log `.WithField("control_addr", ...).Error("internal control listener disabled: invalid server.control_addr")`, return (fail-closed).
   3. `s.internalControlPort = port`; `host,_,_ := net.SplitHostPort(s.cfg.ControlAddr)`; `addr := net.JoinHostPort(host, strconv.Itoa(port))`.
   4. `tlsCfg := &tls.Config{Certificates: []tls.Certificate{*s.cfg.InternalTLSCert}, MinVersion: tls.VersionTLS12}` (no `ClientAuth` — application-layer auth re-verifies the forwarded credentials).
   5. `ln := s.cfg.InternalListener`; if nil `net.Listen("tcp", addr)`; on bind error log `.WithField("addr", addr).Error("internal control listener disabled: bind failed")`, return.
   6. `s.internalListener = ln`; `internal := server.Default(server.WithListener(ln), server.WithReadTimeout(0), server.WithStreamBody(true), server.WithTLS(tlsCfg))`; `s.registerRoutes(internal)`; `s.internalHertz = internal`.
   7. `go` log `.WithField("addr", ln.Addr().String()).Info("internal control plane listening")` then `internal.Run()` with `.WithError(err).Error("internal control plane error")` on non-nil return.

### Phase 4 — Chart

7. **`deploy/helm/dagger-kubernetes/templates/statefulset.yaml`** — add to the `ports` list (after `raft`, keeping the existing control/data/relay/raft entries):
   ```yaml
             - name: internal-control
               containerPort: 8082
   ```
   No Service/ingress/NetworkPolicy template change (pod-to-pod only; DNS resolution of `leader-FQDN:8082` reuses the existing headless Service A-records — a Service's `ports` list does not participate in pod DNS). No `configmap.yaml`/`values.yaml` change (port is derived, not config).

### Phase 5 — Docs

8. **`docs/design/ADR-041-per-pod-leader-forward-proxy.md`** — revise in place (same unmerged PR; a second ADR-042 would record a supersession of a decision that was never merged — revise instead). Changes: §"2. Internal forward hop" rewritten from "public control port + per-provider pool + cert-manager wildcard" → "internal-only listener (control+2=8082, pod-to-pod), dedicated minting-CA leaf with own exact FQDN SANs in **all** modes, unconditional minting-CA verification pool; cert-manager stays public-only"; delete the cert-manager wildcard/DNS-01 requirement; add the dual-listener `registerRoutes` mechanism note; update §5 (statefulset `containerPort: 8082` alongside 8444) and Testing (internal listener leaf/lifecycle tests, removed leaderForwardRootCAs tests). Add a short revision note at the top.
9. **`deploy/helm/dagger-kubernetes/README.md`**:
   - Ports line (~78–79): add `internal control :8082` to the fixed-ports sentence.
   - "Leader-forward hop trust (ADR-041)" (~187–207): delete the cert-manager/external wildcard + DNS-01 + system-pool paragraph; state that the hop targets an **internal-only** HTTPS listener (control+2 = 8082, pod-to-pod) serving a minting-CA leaf with own exact FQDN SANs in **every** provider; no operator action; verification pool is always the minting CA.
   - Raft/leader-forwarding paragraphs (~336–348 and ~542–550): mention the internal control listener port 8082 (pod-to-pod only, no Service) alongside relay-in 8444, and note cert-manager remains public-only.
   - TLS table row "Server certificate" (~162): note the minting CA additionally issues the internal-listener leaf.
10. **`docs/README.md`** — update the two ADR-041 sentences: ~294–298 ("forwards … to the current Raft leader **[over the internal-only control listener `8082`]**") and ~1332–1340 (replace the "cert-manager/external must add a wildcard SAN" clause with "the forward hop targets the pod-to-pod internal control listener (control+2=8082) serving a minting-CA leaf with each pod's own exact FQDN SANs; cert-manager is public-facing only"). Add `8082` to the ports table (~75) if it lists internal ports.
11. **`AGENTS.local.md`** (gitignored, machine-specific) — §5.1 step 5: replace the cert-manager wildcard paragraph with: no operator wildcard step anymore; follower→leader forwards use the internal control listener 8082 (pod-to-pod) with the minting-CA leaf; §7 ADR-041 revision note: remove "cert-manager wildcard SAN must be added" and state cert-manager is public-only. §7 entry title unchanged. Also update §7's `provider: cert-manager` note to say the internal hop no longer depends on it.

### Phase 6 — Tests (also §3 of this plan)

12. **`internal/repository/ca_providers_test.go`** — add `TestEmbeddedProviderInternalServerCert` (leaf SANs contain an injected own-FQDN SAN, no `"*."`; leaf verifies against `ca.CertPool()` via a `tls` handshake; second call reuses the cached `server-internal.crt`/`.key`), `TestInternalServerCertReissuesOnSANGrowth` (pre-write a `server-internal` pair lacking the FQDN SAN → re-issued), `TestFileCAProviderInternalCertDelegatesMinting` (`NewCertManagerProvider(...).InternalServerTLSCert()` returns a minting-CA-signed leaf carrying the FQDN SAN — proves cert-manager mode mints the internal leaf).
13. **`internal/handler/leader_forward_test.go`** — update `TestControlForwardTarget` (scheme always https; port from `s.internalControlPort`; `internalControlPort==0`→`!ok`; drop CertPath/KeyPath cases), `TestLeaderForwardTLSConfig` (assert `RootCAs == s.mintingCA.CertPool()` always; drop the nil-pool branch), `TestLeaderForwardRouting` (switch the fake leader to a minted TLS backend: `repository.NewMintingCA` → issue leaf with SAN `127.0.0.1`/`localhost` → `httptest.NewUnstartedServer`+`StartTLS`; set `s.mintingCA = ca` and `s.internalControlPort = backendPort`; keep all routing/loop/503/502 assertions). `TestLeaderForwardTLSVerification` and `TestLeaderForwardStreamsSSE` remain (streaming test: convert the plain backend+server to TLS via `httptest.NewTLSServer`, set `s.mintingCA`/`s.internalControlPort` similarly).
14. **`internal/handler/internal_control_test.go`** (NEW) — `TestInternalControlPortFromControlAddr` (table: `:8080`→8082, `0.0.0.0:8080`→8082, `:8443`→8445, `not-a-hostport`→error, `:65533`→error); `TestInternalControlListenerServesMintingCALeaf` (bind a real minted leaf, dial `ln.Addr()` with `RootCAs=ca.CertPool()` + `ServerName` = a SAN in the leaf → handshake succeeds; wrong name fails); `TestInternalControlListenerDisabledWithoutCert` (nil `InternalTLSCert` → no bind, log-only); `TestInternalControlListenerShutdown` (start → `Shutdown` → dial fails).
15. **`cmd/api/main_test.go`** — grep-remove any reference to `leaderForwardRootCAs`/`LeaderForwardRootCAs`; add `TestMainMintsInternalCertForCertManagerMode` (if not covered by the repository test) asserting `selectTLSProvider` (cert-manager) + `InternalServerTLSCert` yields a minting-CA leaf.

---

## 3. Data structures & exact signatures (Go)

```go
// internal/domain/ca.go
type CAProvider interface {
    MintingCA() (MintingCA, error)
    ServerTLSCert() (tls.Certificate, error)
    InternalServerTLSCert() (tls.Certificate, error) // NEW
}
```

```go
// internal/repository/ca_providers.go
func (p *EmbeddedProvider) InternalServerTLSCert() (tls.Certificate, error)
func (p *EmbeddedProvider) internalServerSANs() []string          // dedupeStrings(p.extraSANs)
func (p *EmbeddedProvider) issueInternalServerCert(ca *MintingCA, certPath, keyPath string) (tls.Certificate, error)
func (p *fileCAProvider) InternalServerTLSCert() (tls.Certificate, error) // => p.minting.InternalServerTLSCert()
```

```go
// internal/handler/server.go
type ServerConfig struct {
    // ... existing ...
    InternalListener net.Listener     // NEW: pre-bound internal listener (tests)
    InternalTLSCert  *tls.Certificate // NEW: minting-CA-signed internal leaf (nil = listener disabled)
}
type Server struct {
    // ...
    hertz               *server.Hertz   // public control listener (existing)
    internalHertz       *server.Hertz   // NEW
    internalListener    net.Listener    // NEW
    internalControlPort int             // NEW (control port + 2)
    // leaderForwardRootCAs *x509.CertPool  // DELETED
}
func (s *Server) registerRoutes(h *server.Hertz) // NEW: middleware + full route table, shared by both listeners
```

```go
// internal/handler/internal_control.go
func internalControlPortFromControlAddr(controlAddr string) (int, error)
func (s *Server) startInternalControlListener()
```

```go
// internal/handler/leader_forward.go (changed)
func (s *Server) leaderForwardTLSConfig() *tls.Config                    // RootCAs: s.mintingCA.CertPool() unconditional
func (s *Server) controlForwardTarget(leaderAddress string) (scheme, hostPort string, ok bool) // scheme="https", port=s.internalControlPort
```

---

## 4. Edge cases

- **cert-manager/external mode minting the internal leaf:** the leaf is brand-new (`server-internal.crt` is a new filename), so there is no "pre-existing leaf missing SANs" to migrate — it is minted fresh on first boot and re-used thereafter. PVC caching means the leaf persists; `InternalServerTLSCert` re-issues in place only if the FQDN SAN *definition* changes (cluster_domain/headless_service change), mirroring `ServerTLSCert`. In file modes the `minting` provider's `extraSANs` (FQDN SANs + DataHost, set by `mintingProvider`) are what the leaf is minted from — verified `mintingProvider` already prepends `ServerCertFQDNSANs` for every provider.
- **FQDN SANs empty (single-node / bare dev, no headless_service):** `ServerCertFQDNSANs` returns nil, so `extraSANs` may be only `[DataHost]` or empty; the internal leaf is still minted (possibly SAN-less) and the listener still binds. Harmless: single-node never forwards (`IsLeader` always true), no peer dials it. (Open question OQ-1 notes the alternative of gating; recommendation: no gating.)
- **Port collision / derivation failure:** `internalControlPortFromControlAddr` returns an error for unparsable or out-of-range ports → listener is disabled with an error log (fail-closed); `controlForwardTarget` also returns `!ok` when `internalControlPort <= 0`, so a failed listener cannot cause the forward hop to dial a bogus port. Bind failure (port occupied) → listener disabled, log-only — identical to `startRelayListener`.
- **Internal listener bind failure must not crash Start:** dispatches log-only, matches relay-in precedent; public control plane is unaffected.
- **DNS resolution:** unchanged — `controlForwardTarget` still takes the host from `LeaderAddress()` (the Raft-advertised FQDN `DeriveAdvertiseAddr` produces); only the port is swapped from the Raft port to `internalControlPort`. The `dnsPolicy: None` / FQDN machinery (ADR-029) applies to these dials too.
- **Rolling update of the unmerged PR:** since nothing is merged, this lands as more commits on the same branch. A pod still running `b9cf698` (public-port forwarding + operator wildcard path) will, after this commit, mint `server-internal.crt`, serve the internal listener on 8082, and forward to 8082 + minting-CA pool. A mixed-revision window (one old pod forwarding to `:8080` public cert, one new pod forwarding to `:8082`) is not externally safe mid-rollout for the cert-manager path — but this branch has not been shipped/merged, so there is no live mixed-revision population to worry about; the §5.1/§7 live-cluster notes are updated in the same changeset and the standard forced-rollout (§4.5) applies.
- **Leader change mid-request / no leader / loops / half-close / streaming:** unchanged from the base plan (§4 there) — only the target port and pool changed.
- **`s.mintingCA` nil safety:** `s.mintingCA` is always injected (production `serverMintingCA`, tests inject a real CA); it is the same field `Start` already uses for `ClientCAs`, so no new nil-guard is needed beyond the existing contract.

---

## 5. Error handling & validation

- **Fail-closed behaviors:** port-derivation failure and bind failure both disable the internal listener with `Error`-level logs and leave the public control plane running; they never demote to `InsecureSkipVerify`. `controlForwardTarget` fails closed (`!ok` → 503 "no raft leader available") when the port was not derived. `InternalServerTLSCert` failure aborts startup in `main.go` (`fmt.Errorf("get internal server TLS cert: %w", err)`) — a failed CA is fatal, consistent with `ServerTLSCert`.
- **Error wrapping (%w):** new wraps only in `internalControlPortFromControlAddr` (`split control_addr %q: %w`, `parse control_addr port %q: %w`) and `issueInternalServerCert` (`issue internal server cert: %w`, `write internal server cert/key: %w`).
- **Log fields:** `"internal control listener disabled"` variants carry `"control_addr"`/`"addr"` + `WithError`; run loop logs `"internal control plane listening"` with `"addr"` and `"internal control plane error"` with `WithError` — matching the `data relay listener`/`relay-in` naming.
- **No new config keys:** the port is derived (`control_addr` + 2), matching the relay-in decision. **No `config/loader.go`, `config/config.app.yaml(.sample)`, `values.yaml`, or `configmap.yaml` change.** Verified the base plan already added no config keys for this feature.
- **Dead-symbol lint (AGENTS.md) — every deletion in one changeset:**
  - `cmd/api/main.go`: `leaderForwardRootCAs` var, the `LeaderForwardRootCAs:` Deps literal entry, and `crypto/x509` import (grep-confirmed only-usage).
  - `internal/handler/server.go`: `Deps.LeaderForwardRootCAs`, `Server.leaderForwardRootCAs`, the `NewServer` copy, and the old `leaderForwardTLSConfig` nil-pool branch.
  - `internal/handler/leader_forward.go`: the `Certificate`/`CertPath`/`KeyPath` reads removed from `controlForwardTarget` must not orphan the `crypto/tls` import (it is still used by `leaderForwardTLSConfig`) — verify `strconv` added and no import orphaned via `goimports`.

---

## 6. Tests (exact)

- **New:** `internal/handler/internal_control_test.go` — `TestInternalControlPortFromControlAddr` (table: `:8080`→8082, `0.0.0.0:8080`→8082, `127.0.0.1:8443`→8445, `not-a-hostport`→error, `:65533`→error), `TestInternalControlListenerServesMintingCALeaf`, `TestInternalControlListenerDisabledWithoutCert`, `TestInternalControlListenerShutdown`.
- **New (repository):** `TestEmbeddedProviderInternalServerCert`, `TestInternalServerCertReissuesOnSANGrowth`, `TestFileCAProviderInternalCertDelegatesMinting` — in `ca_providers_test.go`.
- **Modified:** `internal/handler/leader_forward_test.go` — `TestControlForwardTarget` (internal-port + unconditional https), `TestLeaderForwardTLSConfig` (minting-CA pool unconditionally; delete the nil-pool/system-pool branch), `TestLeaderForwardRouting` + `TestLeaderForwardStreamsSSE` (minted TLS backend, `s.mintingCA`/`s.internalControlPort` injection).
- **Modified:** `cmd/api/main_test.go` — remove `leaderForwardRootCAs` references (grep); add `TestMainMintsInternalCertForCertManagerMode` (optional if repository test already proves the cert-manager path).
- **Freed by removing `LeaderForwardRootCAs`:** delete the old `TestLeaderForwardTLSConfig` "nil pool = system pool" case and any `main_test.go` assertions on provider-dependent pool selection; replace with the unconditional-pool assertion.
- Rules honored: stdlib `testing` only, table-driven, `logrus`→`io.Discard`, no hardcoded ports in new black-box work (reuse `freeListener` when adding integration coverage; existing fixed-port integration tests already derive the internal port non-fatally via bind-failure log-only, so they stay green).

---

## 7. Docs

- **ADR-041 revised in place** (recommendation, and stated rationale in §2.8): remove cert-manager wildcard/DNS-01 requirement; document the internal listener + port 8082 + network isolation + dedicated minting-CA leaf in all modes + dual-listener `registerRoutes` mechanism; update Consequences/Testing.
- **Helm README:** fixed-ports sentence; "Leader-forward hop trust" section (remove wildcard/system-pool text); Raft/leader-forwarding paragraphs (add 8082); TLS table row note.
- **docs/README.md:** ~294–298 and ~1332–1340 ADR-041 sentences; ports table (~75).
- **AGENTS.local.md:** §5.1 step 5 + §7 revision note (remove cert-manager wildcard operator step, state cert-manager is public-only, add internal listener 8082 + re-derive-on-rollout). Note: file is gitignored.

---

## 8. Verification

**CI gate (mandatory):** `dagger call -m ./dagger --src . ci export --path out`; minimum `go build ./... && go vet ./... && go test ./...` + `dagger call -m ./dagger --src . lint`. `helm lint`/template matrix must render the new `containerPort: 8082`.

**Grep-checks:**
- `grep -rn "InsecureSkipVerify" internal/` → zero occurrences in the forward path.
- `grep -rn "LeaderForwardRootCAs" .` → zero (all removed).
- Forward target: assert `controlForwardTarget`/director build `:8082` (grep for `internalControlPort` + `8082` in statefulset), **not** `:8080`.
- No provider-dependent pool: `leaderForwardTLSConfig` body references only `s.mintingCA.CertPool()`.

**Live-cluster redeploy (AGENTS.local.md §4–5, mandatory):** build/push `docker.io/disaster/dagger-kubernetes:dev`, capture values, `helm upgrade --install`, force-rollout. Agent checks: 3/3 Ready; `-control`/`-data` endpoints list **all 3 pods** (unchanged from base); follower→leader writes succeed with **no** operator wildcard on the cert-manager Certificate (this is the whole point — verify `provider: cert-manager` no longer requires DNS-01); leader log shows `internal control plane listening` on `:8082`; `openssl s_client -connect <leader-fqdn>:8082` shows the minting-CA leaf with the pod's exact FQDN SAN and **no** wildcard; a follower forwarding a write shows no `x509` errors (minting-CA pool). Human verification §5.2 unchanged.

---

## 9. Open questions / risks (short)

- **OQ-1 — RESOLVED (recommendation: no gating).** Start the internal listener unconditionally when `InternalTLSCert != nil` (which is always in production), mirroring relay-in. It is harmless in single-node/bare dev. *Fallback if reviewers object:* gate on multi-node via a passed-in flag — not recommended (adds a handler-layer dependency on Raft topology).
- **R1 — dual-listener route duplication.** Two engines each register the identical route table; the handler methods are all `*Server` methods sharing state (liveHub, fleetManager, stores), so behavior is identical. The only residual risk is an engine-option drift between the two listeners — mitigated by construction (`registerRoutes` + the same `server.WithReadTimeout(0)`/`WithStreamBody(true)` used for both; the internal one adds only `WithListener` + `WithTLS`).
- **R2 — `registerRoutes` extraction is the largest mechanical edit** (moving ~90 lines), not a semantic change; review it as a pure move.

---

## Recommended answer (summary)

Option (a) is the correct security boundary: public trust lives only at the ingress/edge (cert-manager stays public-facing), and intra-pod trust is uniformly the private goca minting CA with per-pod exact-FQDN certs — no wildcard, no scale-event churn, and no operator DNS-01 step. Implement via a dedicated internal-only control listener (control+2 = **8082**, pod-to-pod) that always serves a minting-CA leaf, with the forward pool unconditionally `serverMintingCA.CertPool()`.