# ADR-029: Raft FQDN-only discovery + NodeLocal DNSCache bypass

- **Status:** Accepted
- **Date:** 2026-09-03
- **Builds on:** ADR-016 (multi-node Raft with TLS + StatefulSet discovery)
- **Related:** ADR-015 (Raft replaces SQLite)

## Context

ADR-016 made peers advertise their stable StatefulSet pod DNS name and
discover voters via headless-Service DNS. Two leftovers remained:

1. **Pod-IP discovery preference.** `cmd/api/main.go` called
   `repository.NewPeerResolverWithClientset`, which preferred a
   `k8sPeerResolver` (pod IPs straight from the Kubernetes API, Vault
   go-discover style) whenever a clientset was available — always true
   in-cluster. It then (a) overrode the advertise address with the pod IP,
   (b) injected the pod IP into the TLS SAN set, and (c) kept an interface-
   probing `detectPodIP()` fallback. This is the classic pod-IP anti-pattern:
   ephemeral identity, per-IP certificate concerns, and stale addresses after
   restart. `raft_store.go` also carried a half-built DNS-retry machinery
   (`DNSResolver`/`retryDNSResolver`/`BackoffConfig`) plus an unwired
   peers.json family (`WritePeersJSON`, `RecoverFromPeersJSON`,
   `StartPeersJSONWriter`, `StartRemovedChecker`, `RetryJoin`) and an
   `isStalePeersJSON` auto-clear path that only ever made sense when peers
   were addressed by IP — all dead code with zero call sites.

2. **Stale positive DNS cache (NodeLocal DNSCache).** When a cluster runs the
   NodeLocal DNSCache add-on, its `cache 30` serves a stale positive A record
   for up to 30 s after a pod is recreated. Even though Raft peer addresses
   are stable FQDNs, a recreated pod's fresh IP is invisible to its peers for
   that window, delaying re-election/re-join beyond the intended failure
   detection times.

Separately, docs and one live-cluster override still suggested
`raft.cluster_domain: ""` (`.svc`-only peer addresses) as a convention, while
every default (`loader.go`, sample configs, chart values) is `"cluster.local"`.

## Decision

### 1. FQDN-only discovery (pod-IP path deleted)

- `NewPeerResolver` is the **sole** constructor. Selection order unchanged:
  explicit `raft.peers` → static resolver; else `statefulset_name` +
  `headless_service` → DNS resolver (FQDN); else single-node.
- `internal/repository/raft_discovery_k8s.go` (`k8sPeerResolver`,
  `NewK8sPeerResolver`, `PodIP`) is deleted, along with
  `NewPeerResolverWithClientset` and the `detectPodIP()` fallback,
  the pod-IP advertise override, and the pod-IP SAN injection in
  `initRaftStore`. Advertise addresses come solely from
  `DeriveAdvertiseAddr` (pod FQDN); TLS SANs solely from `PodSANs`
  (pod DNS forms + `127.0.0.1`, never pod IPs).
- The Kubernetes clientset remains a hard dependency — it is still required
  for the TLS CA Secret, the minting CA, and leader pod labeling.
- Dead Raft code is deleted in the same changeset: `DNSResolver`,
  `retryDNSResolver`, `BackoffConfig`, `DefaultBackoffConfig`,
  `NewRetryDNSResolver`, `UpdateAdvertiseAddr`, `WritePeersJSON`,
  `RecoverFromPeersJSON`, `StartPeersJSONWriter`, `StartRemovedChecker`,
  `RetryJoin`/`JoinConfig`/`joinAttempt`, `isStalePeersJSON`/`peersJSONPath`,
  and the `RaftStoreConfig.Backoff` field. `clearStaleRaftState` handles only
  `RecoveryMode` (manual wipe); with FQDN peers the stored addresses never
  change, so staleness detection is a no-op by construction.
- `RaftStoreConfig.AdvertiseResolveTimeout` is kept as the sole
  startup-resolution budget (2-minute in-process retry, no CrashLoopBackOff on
  fresh clusters). Self-healing afterwards is covered by the existing
  mechanisms: the `joinLoop` membership reconcile (5 s) and the Raft
  `NetworkTransport` re-dialing (re-resolving) the FQDN after a connection
  breaks — Hashicorp raft exposes no reconnect-backoff knob, so nothing is
  built for that.

### 2. NodeLocal DNSCache bypass in the chart (defaults ON)

The supervisor StatefulSet renders, by default
(`supervisor.dns.enabled: true`):

```yaml
dnsPolicy: None
dnsConfig:
  nameservers:
    - "<supervisor.dns.nameserver>"   # kube-dns ClusterIP
  searches:
    - "<ns>.svc.<clusterDomain>"
    - "svc.<clusterDomain>"
    - "<clusterDomain>"
  options:
    - name: ndots
      value: "1"
```

so Raft pods query the cluster's kube-dns (CoreDNS) directly, bypassing
NodeLocal DNSCache and its `cache 30` stale-positive window; `ndots: 1`
removes the search-path penalty for the FQDN dials. `supervisor.dns.nameserver`
defaults to `10.43.0.10` (k3s, this project's reference cluster; kubeadm is
`10.96.0.10`) and is **fail-closed**: rendering fails when it is empty while
the bypass is enabled. `supervisor.dns.enabled: false` is the escape hatch
back to the cluster default DNS.

Trade-off (accepted): `dnsPolicy: None` affects every supervisor lookup, not
just Raft. In-cluster `<service>.<ns>.svc` short names still resolve via the
search suffix (one extra NXDOMAIN round-trip); external names forward through
CoreDNS as usual.

Complementary operator change (documented, not automated): reduce the
`nodelocaldns` Corefile's `cache 30` → `cache 5` for the `cluster.local`
zone. This remains the only way to shorten the TTL on a cluster that keeps
NodeLocal DNSCache for *other* workloads; the chart bypass covers only the
supervisor's own lookups.

### 3. `clusterDomain` is the FQDN default everywhere

`raft.cluster_domain` stays `"cluster.local"` by default (peer addresses
`<pod>.<headless>.<ns>.svc.cluster.local:<port>`); the `.svc`-only form was a
live-cluster override plus one stale docs sentence, both corrected. Because
`PodSANs` emits the FQDN form, leaves minted while the `.svc`-only override
was active do **not** cover the new FQDN SAN and are re-issued in place at
boot under the same shared CA (the `<release>-raft-ca` Secret) — no trust
split and no manual PVC deletion. Certs that already carry both forms are
reused as-is.

## Consequences

- One discovery path (DNS FQDN), one advertise form, one SAN set — the
  resolver surface shrinks to `NewPeerResolver`, and ~700 lines of dead or
  unwired Raft code are gone.
- Pod identity is durable across restarts: same FQDN, same cert, same Raft
  address; a recreated pod is reachable as soon as CoreDNS serves its record.
- `helm template`/`lint` fail loudly on an unset `supervisor.dns.nameserver`
  instead of silently shipping a broken `resolv.conf`.
- Tests updated: pod-IP resolver tests removed; `TestPodSANs` locks in the
  FQDN-only invariant (DNS set contains the FQDN, IP set exactly
  `[127.0.0.1]`); `clearStaleRaftState` covered for both `RecoveryMode`
  values; `validateRaftConfig` proves multi-node + `statefulset_name` needs
  no clientset or IP.
