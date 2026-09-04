# Plan: FQDN + Headless Service for HashiCorp Raft (drop pod-IP anti-pattern, simplify lifecycle, NodeLocal DNSCache bypass)

## 1. Decision

### 1.1 Is "FQDN + headless Service" the correct best practice here?

**Yes — and the project already implements ~90% of it.** The correct blueprint is already in place:

- The Helm chart ships a **StatefulSet** (`podManagementPolicy: Parallel`, per-pod PVCs via `volumeClaimTemplates`) and a **headless Service** (`clusterIP: None`, `publishNotReadyAddresses: true`) — `deploy/helm/dagger-kubernetes/templates/statefulset.yaml` + `service.yaml`.
- `dnsPeerResolver` (`internal/repository/raft_discovery.go`) already builds FQDN peer addresses `<sts>-<i>.<headless>.<ns>.svc.<clusterDomain>:<port>`.
- `PodSANs` already covers every DNS SAN form (`<host>`, `<host>.<headless>`, `...ns`, `...ns.svc`, `...ns.svc.<clusterDomain>`) plus `127.0.0.1`.
- `raft.cluster_domain` already defaults to `"cluster.local"` (FQDN) in `config/loader.go`, `config/config.app.yaml`, `config/config.app.yaml.sample`, and `deploy/helm/.../values.yaml`.
- `hostAddr`/`tlsStreamLayer.Dial` already keep the DNS name in the Raft `ServerAddress` and re-resolve on each connection, so pod recreation does not strand peers on an old IP.

**The anti-pattern that remains:** `cmd/api/main.go` *prefers* pod-IP discovery. `initRaftStore` calls `repository.NewPeerResolverWithClientset`, which returns a `k8sPeerResolver` (pod IPs from the K8s API) whenever a clientset is present (always true in-cluster). It then (a) overrides the advertise address with the pod IP, and (b) injects the pod IP into the TLS SAN set. This is exactly the "pod IP" anti-pattern (ephemeral identity, per-IP cert concerns, stale-address-after-restart). The fix is to **remove the pod-IP path entirely and make DNS FQDN the only discovery mode**.

### 1.2 DNS-cache fix: implement the bypass (fix #3) IN the chart, keep #1/#2 complementary

The stale-positive-DNS-cache fix is now implemented **in the chart's StatefulSet**, not merely documented:

- **Fix #3 (adopted, defaults ON):** render `dnsPolicy: None` + `dnsConfig` on the supervisor StatefulSet so Raft pods bypass NodeLocal DNSCache and query the cluster's main CoreDNS (`kube-dns`) directly, with `ndots: 1`. Configurable via a new `supervisor.dns` block (see §2 Phase C); the escape hatch is `supervisor.dns.enabled: false`, which restores the cluster default (NodeLocal DNSCache when installed). Details, exact template, and the kube-dns ClusterIP default are in §2 Phase C and §5.
- **Fix #1 (still recommended, complementary operator change):** reduce NodeLocal DNSCache `cache 30` → `cache 5` for the `cluster.local` zone. This remains the **only** way to actually shorten the TTL on a cluster that keeps NodeLocal DNSCache for *other* workloads (the supervisor's bypass only affects the supervisor's own lookups). It is a cluster add-on ConfigMap (`nodelocaldns`) change, not a chart change — documented, not automated.
- **Fix #2 (already satisfied — simplify, do not build):** the stale-IP window is also self-healing via (a) startup advertise resolution retrying in-process for up to 2 minutes (`resolveAdvertiseAddr`), (b) `joinLoop` reconciling membership every 5s, and (c) the Raft `NetworkTransport` re-dialing the FQDN (re-resolving) after a connection breaks. Hashicorp `raft.NetworkTransport` exposes no reconnect-backoff knob, so there is nothing to build. The `retryDNSResolver`/`BackoffConfig` machinery in `raft_store.go` was a half-built attempt at exactly this and is **dead code** — delete it (§2 Phase B).

### 1.3 Corrections to the user's assumptions (must-read)

- **"the chart defaults `raft.clusterDomain` to empty" is wrong.** `values.yaml`, `loader.go`, and both sample configs default to `"cluster.local"` (FQDN). The `.svc`-only form is a **live-cluster override** (`AGENTS.local.md` §7) plus one stale sentence in `docs/README.md` (~lines 1038–1047). This changeset **flips the live override back to `cluster.local`** (Decision 2, §8).
- **Cert-superset nuance (Decision 2):** the superset-reuse check (`coversSANs`/`reusableNodeCert`) does **not** keep every legacy `.svc`-form cert "valid" verbatim. A leaf that carries *only* the `.svc`-form SANs (minted while the live override was `""`) does **not** cover the new FQDN SAN, so it is **re-issued** at boot — transparently, in place, under the same shared CA (from the `<release>-raft-ca` Secret), so there is **no trust split and no manual PVC deletion**. Certs that already carry both forms (the original `cluster.local`-era leaves) are reused as-is. This is the correct safe behavior; the rollout just performs a leaf re-issue on the `.svc`-only pods.

---

## 2. Exact file changes (ordered task list)

### Phase A — Remove the pod-IP discovery path (core)

1. **`internal/repository/raft_discovery_k8s.go`** — **DELETE the entire file.** It contains `k8sPeerResolver`, `NewK8sPeerResolver`, and `PodIP()` (pod-IP discovery + pod-IP self). Nothing else references it after step 2.

2. **`internal/repository/raft_discovery.go`**:
   - Delete `NewPeerResolverWithClientset` (the function that prefers `k8sPeerResolver` and returns `podIP`).
   - Remove the now-unused `k8s.io/client-go/kubernetes` import (it was only used by `NewPeerResolverWithClientset`).
   - `NewPeerResolver` remains the **single** entry point. Selection order unchanged: explicit `raft.peers` → `staticPeerResolver`; else `statefulset_name`+`headless_service` → `dnsPeerResolver` (FQDN); else `singleNodeResolver`.

3. **`cmd/api/main.go`** — in `initRaftStore`:
   - Replace `resolver, podIP := repository.NewPeerResolverWithClientset(&discovery, clientset, hostname)` with `resolver := repository.NewPeerResolver(&discovery)`.
   - Delete the `detectPodIP()` fallback block (`if podIP == "" && clientset != nil ...`).
   - Delete the advertise override `if podIP != "" { advertise = net.JoinHostPort(podIP, ...) }`. The advertise address now always comes from `repository.DeriveAdvertiseAddr(&discovery, hostname)` (FQDN for StatefulSet pods).
   - Delete the pod-IP SAN injection block (`if podIP != "" { ipAddrs = append(ipAddrs, ...) }`). SANs come solely from `repository.PodSANs(&discovery, hostname)`.
   - Delete the `detectPodIP` function (now orphaned). Keep `clientset` — still required for the TLS CA Secret (`ensureRaftCAFromSecret`), `observeLeadership` pod-label patching, and the minting CA (`mintingProvider`).
   - `raftNodeCommonName` is unchanged (resolver.Self() still returns the DNS peer ID = pod name).

### Phase B — Delete dead Raft code + unwired exported helpers (simplify; Decision 3)

4. **`internal/repository/raft_store.go`** — delete the following (grep-confirm zero references outside their own definitions before each; AGENTS.md mandates removing orphaned helpers):
   - Dead DNS retry machinery: `DNSResolver` interface, `retryDNSResolver` struct, `BackoffConfig`, `DefaultBackoffConfig`, `NewRetryDNSResolver`, and the methods `Start`, `resolveLoop`, `Resolve`, `Resolved`, `resolveAttempt`.
   - `RaftStoreConfig.Backoff BackoffConfig` field (declared but never read).
   - `UpdateAdvertiseAddr` (no-op stub; the transport's advertise address is fixed at creation and is already the FQDN).
   - **Unwired exported helpers (Decision 3):** `WritePeersJSON`, `RecoverFromPeersJSON`, `StartPeersJSONWriter`, `StartRemovedChecker`, and `RetryJoin`/`JoinConfig`/`joinAttempt`. Each is exported (so not a lint risk alone) but zero-callers; deleting them also orphans the unexported `joinAttempt` and `peersJSONPath`, which **must** be deleted in the same changeset.
   - **Unexported auto-clear logic that FQDN makes a no-op:** `isStalePeersJSON` (reads `peers.json`; with FQDN peers the stored addresses never change, so staleness never triggers). Delete it and simplify `clearStaleRaftState` to handle **only** `RecoveryMode` (manual `clearRaftState`), dropping the `shouldBootstrap`/`Resolver`/`isStalePeersJSON` branch. New call shape: `clearStaleRaftState(cfg, dir, logger)`.
   - **Keep:** `RaftStoreConfig.AdvertiseResolveTimeout` (wired into `newStreamTransport`/`resolveAdvertiseAddr`; delete its `// Deprecated: use the DNSResolver...` comment and re-document it as the sole startup-resolution budget). Keep `clearRaftState` (and its defensive `peers.json` removal — harmless cleanup of legacy PVCs from prior releases; optional but recommended to retain).

### Phase C — Helm: NodeLocal DNSCache bypass (fix #3; Decision 1)

5. **`deploy/helm/dagger-kubernetes/values.yaml`** — add a `supervisor.dns` block (defaults ON):

   ```yaml
   supervisor:
     dns:
       ## @param supervisor.dns.enabled Bypass NodeLocal DNSCache for the Raft pods (fix #3).
       ##   When true (default), the StatefulSet renders dnsPolicy: None + dnsConfig pointing
       ##   directly at the cluster's kube-dns (CoreDNS) service, avoiding the ~30s stale
       ##   positive-A-record window after a pod is recreated. Set false to restore the
       ##   cluster's default DNS (NodeLocal DNSCache when installed).
       enabled: true
       ## @param supervisor.dns.nameserver kube-dns (CoreDNS) service ClusterIP. Helm cannot
       ##   query the cluster, so set this to your cluster's value:
       ##     kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}'
       ##   k3s: 10.43.0.10 (this repo's reference cluster); kubeadm/standard: 10.96.0.10.
       nameserver: "10.43.0.10"
       ## @param supervisor.dns.ndots resolv.conf ndots for the supervisor pod (FQDN Raft peers).
       ndots: 1
   ```

6. **`deploy/helm/dagger-kubernetes/templates/statefulset.yaml`** — two additions:
   - Fail-closed guard (with the existing `{{- fail ... }}` guards near the top):
     ```yaml
     {{- if and .Values.supervisor.dns.enabled (not .Values.supervisor.dns.nameserver) }}
     {{- fail "supervisor.dns.nameserver is required when supervisor.dns.enabled is true: set it to your kube-dns ClusterIP (kubectl -n kube-system get svc kube-dns; k3s=10.43.0.10, kubeadm=10.96.0.10), or set supervisor.dns.enabled=false to keep NodeLocal DNSCache." }}
     {{- end }}
     ```
   - Pod-spec blocks (inside `spec.template.spec`, immediately after `serviceAccountName` and before `terminationGracePeriodSeconds`):
     ```yaml
     {{- if .Values.supervisor.dns.enabled }}
     dnsPolicy: None
     dnsConfig:
       nameservers:
         - {{ .Values.supervisor.dns.nameserver | quote }}
       searches:
         - {{ printf "%s.svc.%s" (include "dagger-kubernetes.namespace" .) (.Values.supervisor.config.raft.clusterDomain | default "cluster.local") | quote }}
         - {{ printf "svc.%s" (.Values.supervisor.config.raft.clusterDomain | default "cluster.local") | quote }}
         - {{ .Values.supervisor.config.raft.clusterDomain | default "cluster.local" | quote }}
       options:
         - name: ndots
           value: {{ .Values.supervisor.dns.ndots | quote }}
     {{- end }}
     ```
   - Namespace injection reuses the existing `dagger-kubernetes.namespace` helper (`.Release.Namespace` default) and the search suffix reuses `supervisor.config.raft.clusterDomain` (default `cluster.local`), so no new `_helpers.tpl` entries are strictly required.

### Phase D — Docs + config comments

7. **`docs/README.md`** (~lines 1038–1047 and the `cluster_domain` row ~412): fix the stale `default ""` claim; state FQDN default (`cluster.local`), DNS-only discovery (no K8s API, no pod IPs), and the NodeLocal DNSCache bypass (`supervisor.dns.*`) + `cache 5` recommendation.
8. **`deploy/helm/dagger-kubernetes/README.md`** — Raft section (≈lines 467–525), both `clusterDomain` table rows (≈483, 608), and the Parameters tables: document `supervisor.dns.enabled`/`.nameserver`/`.ndots`, the k3s vs kubeadm ClusterIP values, the fail-closed guard, the escape hatch (`enabled: false`), and the complementary `cache 5` (fix #1) + retry (fix #2) notes.
9. **`config/config.app.yaml` + `config/config.app.yaml.sample`** — comment-only: advertise/peer discovery is DNS-FQDN only (drop "pod IP" hints). No new keys, no defaults change (the DNS bypass is a pod-spec concern, not an app-config key).
10. **`DAGGER.md`** — **no change** (no `dagger/`, CI-script, or workflow change). Note: the existing local helm template matrix now also renders the new `dnsConfig`/`dnsPolicy` blocks with default values (which succeed because `nameserver` defaults non-empty).

---

## 3. Data structures & function signatures

No *new* Go data structures; the change is a contraction. Relevant surfaces after the change:

```go
// internal/repository/raft_discovery.go — unchanged public surface
func NewPeerResolver(cfg *RaftDiscoveryConfig) PeerResolver   // SOLE constructor now
type PeerResolver interface {
    Resolve() ([]RaftPeer, error)
    Self() (RaftPeer, error)
}
type RaftDiscoveryConfig struct { ... }  // unchanged (NodeID, AdvertiseAddr, BindAddr, Peers, Replicas,
                                         // StatefulSetName, HeadlessService, Namespace, ClusterDomain, RaftPort)
func DeriveAdvertiseAddr(cfg *RaftDiscoveryConfig, hostname string) (string, error) // unchanged, FQDN-only
func PodSANs(cfg *RaftDiscoveryConfig, hostname string) (dnsNames []string, ipAddrs []net.IP) // unchanged (DNS forms + 127.0.0.1 only)
```

```go
// internal/repository/raft_store.go — after cleanup
type RaftStoreConfig struct {
    // ... Dir, NodeID, BindAddr, AdvertiseAddr, Peers, Resolver, ApplyTimeout, ...
    AdvertiseResolveTimeout time.Duration  // kept; sole startup-resolution budget (default 2m)
    // Backoff: DELETED
    // ... SnapshotThreshold, SnapshotInterval, TrailingLogs, TLS, ...
}

// DELETED:
//   DNSResolver, retryDNSResolver, BackoffConfig, DefaultBackoffConfig, NewRetryDNSResolver,
//   UpdateAdvertiseAddr,
//   WritePeersJSON, RecoverFromPeersJSON, StartPeersJSONWriter, StartRemovedChecker,
//   RetryJoin, JoinConfig, joinAttempt, isStalePeersJSON, peersJSONPath
// SIMPLIFIED:
//   func clearStaleRaftState(cfg *RaftStoreConfig, dir string, logger *logrus.Logger) // RecoveryMode-only now
```

TLS SAN builder is **unchanged** (`PodSANs`): the FQDN + `.svc`-form + short-form coverage it already produces is the correct, complete SAN set; the only removal is the *runtime pod-IP append* in `main.go`.

New Helm surface (no Go counterpart):

```yaml
supervisor:
  dns:
    enabled: true            # default ON (dnsPolicy: None + dnsConfig)
    nameserver: "10.43.0.10" # kube-dns ClusterIP (k3s); kubeadm = 10.96.0.10
    ndots: 1
```

---

## 4. Edge cases

- **Pod recreation during rolling update:** stable StatefulSet DNS means the FQDN stays the same; the Raft `ServerAddress` never changes, so no `remove-peer`/`add-peer` is needed. The surviving peers' next dial re-resolves to the new pod IP.
- **Stale positive DNS cache (the user's real bug):** the supervisor now bypasses NodeLocal DNSCache (fix #3) and queries `kube-dns` directly. CoreDNS's own `cache` plugin still holds a stale positive A record briefly, but the kube-dns Corefile's default `cache 30` is the remaining window; the transport + `joinLoop` retries outlast it, and startup resolution retries in-process for 2 minutes. Fix #1 (`cache 5` on the `nodelocaldns` addon) remains the way to shorten it on a cluster that keeps NodeLocal.
- **Negative cache (NXDOMAIN) during bootstrap:** FQDN already avoids the `ndots:5` search-path penalty and the catch-all `.:53` zone's `cache 30`; `cluster.local` is served with the short `denial 5` window. `ndots: 1` (set explicitly by fix #3) removes the default `ndots:5` search-path cost for short names the supervisor dials.
- **`dnsPolicy: None` loses NodeLocal for ALL supervisor lookups:** other in-cluster endpoints (telemetry, registry, loki/tempo/victoria) are dialed as `<service>.<ns>.svc` short names. With `ndots:1` + the `cluster.local` search suffix these still resolve, but pay one extra NXDOMAIN round-trip (absolute `<svc>.svc.` is tried first, then `<svc>.<ns>.svc.cluster.local` via the search path). External names (GitHub Releases, OIDC issuer) still resolve through CoreDNS's forwarding. Acceptable; document it.
- **Single-node vs 3-node:** both flow through `singleNodeResolver` / `dnsPeerResolver`; both are FQDN-based. Single-node non-K8s (bare binary) still advertises the bind host via `deriveBindHost`.
- **Cert SAN coverage when flipping to `cluster.local` (Decision 2):** `PodSANs` now emits the FQDN form. Leaves that carry both forms (original `cluster.local`-era) are reused; leaves minted in the `.svc`-only era are re-issued in place under the same CA — no trust split, no manual PVC deletion. See §1.3 and §8.
- **Cluster-domain override:** `raft.cluster_domain` remains honored end-to-end (loader default → `raftDiscoveryConfig` → `podAddress`/`PodSANs`/`DeriveAdvertiseAddr` → chart ConfigMap). The new `dnsConfig.searches` also derive from it (falling back to `cluster.local`), so they stay consistent.
- **No K8s clientset / bare-binary multi-node:** falls back to `dnsPeerResolver` via `NewPeerResolver` (or explicit `raft.peers`); the removal of `NewPeerResolverWithClientset` drops the IP preference but not the DNS fallback.
- **`supervisor.dns.enabled: false` (escape hatch):** renders no `dnsPolicy`/`dnsConfig`; the pod uses the cluster default (NodeLocal DNSCache when installed), and fix #1 (`cache 5`) is then the operator's responsibility.

---

## 5. Error handling & validation

- **New Helm validation (fail-closed):** `statefulset.yaml` `{{- fail }}`s when `supervisor.dns.enabled: true` and `supervisor.dns.nameserver` is empty/unset, with a message naming the k3s/kubeadm values and the `kubectl -n kube-system get svc kube-dns` command. This matches the chart's existing fail-closed guards for `autoscaling`/`raft.replicas`.
- **No new Go config keys:** `config/loader.go` needs no new `v.SetDefault`/validation. `validateRaftConfig` (`cmd/api/main.go`) is unchanged (routable `advertise_addr` for multi-node + TLS CA-sharing requirements).
- **TLS SAN validation:** `coversSANs`/`reusableNodeCert` continue to fail-closed — a persisted leaf not covering the required DNS SAN set is re-issued in place. This is exactly what makes the `clusterDomain` flip safe (Decision 2).
- **Fail-closed behavior preserved:** advertise-resolution failure retries in-process (`resolveAdvertiseAddr`, 2-minute budget) and errors with the last `%w`-wrapped DNS error; `joinLoop` logs and continues on resolve failure; TLS CA Secret poll still fails closed on timeout.
- **Dead-symbol safety (AGENTS.md):** every deleted helper in Phase A/B must be preceded by `grep` confirming zero references. Deleting `NewPeerResolverWithClientset` orphans `NewK8sPeerResolver`/`k8sPeerResolver`/`PodIP` (whole-file delete); deleting `RetryJoin` orphans `JoinConfig`/`joinAttempt`; deleting the peers.json family orphans `isStalePeersJSON`/`peersJSONPath`. All deletions land in one changeset to keep `golangci-lint`/`unused` green.

---

## 6. Tests

- **Delete** from `internal/repository/raft_discovery_test.go`:
  - `TestNewPeerResolverWithClientset` and `TestK8sPeerResolver` (the pod-IP matrix).
  - Remove now-unused imports (`corev1`, `metav1`, `k8s.io/client-go/kubernetes/fake`) from this file.
- **Keep/strengthen:** `TestNewPeerResolverSelection`, `TestDNSPeerResolverResolve`, `TestDNSPeerResolverSelf`, `TestDeriveAdvertiseAddr`, `TestPodSANs`. Add a table-driven case to `TestPodSANs` asserting the DNS set contains the FQDN and the IP set is exactly `[127.0.0.1]` (no pod IPs) — locks in the FQDN-only invariant.
- **`internal/repository/raft_store_test.go`:** keep `TestResolveAdvertiseAddrSuccess`/`TestResolveAdvertiseAddrRetriesThenFails`. Remove/adjust any test touching deleted symbols (`UpdateAdvertiseAddr`, `RetryJoin`, peers.json family — none currently found in the test file). Add a case asserting `clearStaleRaftState` with `RecoveryMode: false` is a no-op and with `RecoveryMode: true` clears state (covers the simplified signature).
- **`cmd/api/main_test.go`:** the `newTestRaftResolver` helper uses `repository.NewPeerResolver` (still valid). Remove/adjust any test touching `detectPodIP` or the pod-IP advertise/SAN path. Add a table-driven `validateRaftConfig` case proving multi-node + `statefulset_name` (no explicit peers) is accepted without any K8s clientset/IP requirement.
- **Helm template matrix (CI `dagger` module):** the existing `helm template` matrix renders the StatefulSet with defaults — which now include `supervisor.dns.enabled: true` + `nameserver: "10.43.0.10"`, so it must pass. Optionally add a case for `supervisor.dns.enabled: false` (escape hatch renders no `dnsPolicy`/`dnsConfig`) and one for `nameserver: ""` (must fail). No `dagger/` code change required unless a new matrix case is added (then update `DAGGER.md`).
- **Integration (per AGENTS.md):** no new integration test required; the change is internal to discovery. Any new black-box test must use `freeAddr(t)` (`tests/integration/net_helpers_test.go`) and `t.Cleanup` with a timed `Shutdown`.

---

## 7. Docs

- **New ADR** `docs/design/ADR-017-raft-fqdn-only-discovery.md` (status Accepted): records (a) FQDN-only discovery (drop `k8sPeerResolver`/pod-IP addressing and SANs), (b) the NodeLocal DNSCache stale-positive window and the adopted mitigation — chart `dnsPolicy: None` bypass (fix #3, defaults ON) + complementary `cache 5` (fix #1) and existing retries (fix #2) — and (c) the `clusterDomain` flip to full FQDN with the cert re-issue behavior.
- **Update** `docs/design/ADR-016-raft-multinode-tls.md`: revision note pointing to ADR-017, stating `k8sPeerResolver` is removed (DNS is the sole discovery path) and the live `.svc`→`cluster.local` migration.
- **Update** `docs/README.md` (lines ~1038–1047 + `cluster_domain` row ~412): fix the stale `default ""` claim; state FQDN-only + DNS bypass (`supervisor.dns.*`) + `cache 5` note.
- **Update** `deploy/helm/dagger-kubernetes/README.md` Raft section + `clusterDomain` rows + Parameters tables (add `supervisor.dns.*`).
- **Update** `config/config.app.yaml.sample` and `config/config.app.yaml` comments.
- **Update** `AGENTS.local.md` §7: **drop** the `.svc`-only `clusterDomain` note (lines ~194–197) and record the new live value `supervisor.config.raft.clusterDomain: "cluster.local"` plus `supervisor.dns.nameserver: "10.43.0.10"` (k3s). §3/§4.3 already capture values via `helm get values`; note the new `supervisor.dns` keys there.
- **`DAGGER.md`** — no change (unless a new helm matrix case is added in `dagger/`, see §6).

---

## 8. Verification

**CI gate (mandatory, AGENTS.md):**
```bash
dagger call -m ./dagger --src . ci export --path out
```
Minimum when no Docker daemon: `go build ./... && go vet ./... && go test ./...` plus `dagger call -m ./dagger --src . lint`.

**Local-cluster redeploy (AGENTS.local.md §4–5, mandatory) — includes Decision 2 (`clusterDomain` flip) and Decision 1 (`supervisor.dns`):**
1. `docker build -t docker.io/disaster/dagger-kubernetes:dev .` and `docker push ...`.
2. Recapture current values: `helm --kubeconfig /home/user/.kube/home get values dagger-kubernetes-test -n dagger-kubernetes-test -o yaml > /tmp/dagger-kubernetes-test.values.yaml`.
3. **Edit the captured values file before upgrade:**
   - Set (or change from `""`) `supervisor.config.raft.clusterDomain: "cluster.local"` — full FQDN peer addresses.
   - Set `supervisor.dns.nameserver: "10.43.0.10"` (k3s home cluster `kube-dns`; verify with `kubectl --kubeconfig /home/user/.kube/home -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}'`).
   - Leave `supervisor.dns.enabled` at its default `true`.
4. `helm --kubeconfig /home/user/.kube/home upgrade --install dagger-kubernetes-test ./deploy/helm/dagger-kubernetes --namespace dagger-kubernetes-test -f /tmp/dagger-kubernetes-test.values.yaml --set supervisor.image.tag=dev --set supervisor.image.pullPolicy=Always --set supervisor.image.repository=docker.io/disaster/dagger-kubernetes`.
5. `kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test rollout restart statefulset/dagger-kubernetes-test-dagger-kubernetes` then `rollout status ... --timeout=300s`.
6. Agent checks (§5.1): pods `Running`/`Ready`; `port-forward` + `curl -sk /healthz`/`/readyz` 200; authed `/api/v1/status` and `/api/v1/cache`; supervisor logs free of fatal errors. Confirm `kubectl get endpoints <release>-control/<release>-data` still shows exactly the leader pod.
7. Human verification (§5.2): live UI login + global status + relevant views.

**Specific functional checks for this changeset:**
- **FQDN peer addresses:** on the leader, confirm the Raft configuration stores full FQDNs (`<pod>.<headless>.<ns>.svc.cluster.local:8081`), e.g. via supervisor logs or the membership reconcile logs — no bare `.svc` addresses, no pod IPs.
- **Cert re-issue is clean:** after the `clusterDomain` flip, pods whose leaves lacked the FQDN SAN re-issue them under the same CA; confirm no `x509: certificate signed by unknown authority` errors and all 3 pods reach `Leader`/`Follower` clean state.
- **DNS bypass works:** exec into a supervisor pod (`kubectl ... exec <pod> -- cat /etc/resolv.conf`) and confirm `nameserver 10.43.0.10`, the `cluster.local` search list, and `options ndots:1`.
- **Follower rejoin:** `kubectl ... delete pod <release>-dagger-kubernetes-<ordinal>`, wait for Ready, confirm 3/3 voters and no stale-IP dial errors persist beyond the (now CoreDNS-only) cache window.

---

## 9. Open questions / risks (human decision required before/at implementation)

- **OQ-NEW-1 — public default for `supervisor.dns.nameserver`:** the plan defaults `"10.43.0.10"` (k3s) because the reference/home cluster is k3s, and a non-empty default keeps `helm template`/lint green out-of-the-box. Risk: a kubeadm/EKS operator who enables the default without overriding gets a silently-wrong nameserver and broken DNS. Alternatives: default `"10.96.0.10"` (kubeadm — more common) or default `""` with fail-closed (safest, but makes the default chart install fail until set). *Recommendation:* keep `10.43.0.10` + prominent docs + the fail-closed guard; confirm the operator audience.
- **OQ-NEW-2 — `dnsPolicy: None` for non-Raft lookups:** bypassing NodeLocal DNSCache affects every supervisor DNS lookup (telemetry/registry short names pay one extra NXDOMAIN hop; external names go through CoreDNS forwarding). Confirm this is acceptable, or scope the bypass more narrowly (not possible per-pod without NodeLocal changes).
- **OQ-NEW-3 — `.svc`-only certs will be re-issued during the flip:** as corrected in §1.3, the superset-reuse check keeps *both-form* certs but transparently re-issues `.svc`-only leaves under the same CA. This is safe (no trust split) but is a leaf re-issue during rollout — confirm acceptable versus a pre-rotation step.
- **R1 — clientset remains a hard dependency** for TLS CA + minting CA + leader labeling; removing IP discovery does not remove it (unchanged, correct).
- **R2 — fix #1 (`cache 5`) remains an out-of-chart operator change** and is the only way to shorten the TTL on a cluster that keeps NodeLocal DNSCache for other workloads; the chart bypass only covers the supervisor's own lookups.
- **R3 — dead-symbol lint risk** is the main CI hazard: all Phase A/B deletions must land in one changeset (deleting a call site without its orphaned helper fails `golangci-lint`/`unused`).
