# Raft Stability Fix — Implementation Plan

## 1. Root Cause Analysis

### Problem A: CrashLoop during bootstrap / rolling restart (DNS timeout)
**Root cause:** `resolveAdvertiseAddr` has a hard 2-minute timeout budget (1s interval × 120 retries). When a pod starts before its headless service DNS entry is registered (common with `publishNotReadyAddresses: true` and `podManagementPolicy: Parallel`), the entire Raft store creation fails. The process exits, Kubernetes restarts it, and the cycle repeats until DNS finally resolves. The DNS resolution is also coupled directly to transport creation — there is no way to create the Raft store without a resolved address.

### Problem B: DB unavailable after supervisor starts (Raft instability)
**Root cause:** `initRaftStore` blocks on `WaitForCleanState` before serving any HTTP endpoint. If the cluster is in a degraded state (e.g., 2 of 3 nodes restarting simultaneously with `podManagementPolicy: Parallel`), no node becomes clean, so no node starts serving. The readiness probe fails on all nodes, and no traffic is routed, creating a deadlock where the cluster can never recover. Additionally, there is no startup probe — Kubernetes kills pods before Raft has time to stabilize.

### Problem C: Leadership transitions during rolling restart cause disruptions
**Root cause:** No graceful leave mechanism exists. When a pod is terminated (SIGTERM), the Raft node disappears from the cluster without transferring leadership or removing itself from the voter configuration. The leader's log entries may be lost. The remaining nodes must wait for an election timeout before detecting the failure and electing a new leader. With `podManagementPolicy: Parallel`, multiple nodes can disappear simultaneously, potentially losing quorum.

### Problem D: No autopilot / dead server cleanup
**Root cause:** There is no mechanism to detect and remove dead or unhealthy servers from the Raft configuration. Stale voter entries accumulate. There is no stabilization period after membership changes to prevent cascading reconfigurations.

### Problem E: Short-lived TLS certificates
**Root cause:** TLS certificate validity is likely short (standard defaults). No rotation mechanism exists. Certificate expiry during long-running clusters would cause transport failures.

---

## 2. Design Decisions

### Decision 1: Decouple DNS from Raft store creation
**Approach:** Move DNS resolution into a background goroutine with infinite retry and exponential backoff. The Raft store is created with a placeholder (empty) address initially, then updated once DNS resolves. This matches Vault's approach of retrying `retry_join` indefinitely.

### Decision 2: Separate bootstrap from ready-serving
**Approach:** Split the startup sequence into phases:
1. Create Raft store (don't block on DNS).
2. Start HTTP server immediately (health endpoints respond).
3. Add a `startupProbe` that waits for Raft stability (up to 5 minutes).
4. `readinessProbe` accepts Follower state, not just clean state.
5. `livenessProbe` checks basic process health only.

### Decision 3: Graceful leave on SIGTERM
**Approach:** On SIGTERM:
1. If leader, transfer leadership to another node.
2. Remove self from voter configuration.
3. Wait for commit index to propagate.
4. Shutdown Raft.
This matches Vault's `StartRemovedChecker` pattern but initiated on shutdown signal.

### Decision 4: Performance multiplier for election/heartbeat timeouts
**Approach:** Add `performance_multiplier` config (default 5x) that scales `ElectionTimeout`, `HeartbeatTimeout`, and `LeaderLeaseTimeout`. This prevents false-positive leader elections in high-latency environments (matches Vault's approach).

### Decision 5: Autopilot for dead server detection
**Approach:** Implement a lightweight autopilot that:
- Tracks follower heartbeats from the leader's perspective.
- Detects servers that haven't heartbeated within a configurable threshold.
- After a stabilization period, removes dead servers from the voter config.
- This is simpler than Vault's full `raft-autopilot` library and sufficient for our scale.

### Decision 6: TLS rotation with keyring
**Approach:** Extend certificate validity to 30 years for the CA. Implement a 2-key keyring (active + pending) for TLS certificates. Rotate on a 24-hour timer using a 4-phase commit pattern keyed off applied indexes.

### Decision 7: peers.json recovery
**Approach:** On startup, if the Raft log exists but the node can't join, attempt to recover from a `peers.json` backup written on every successful membership change.

---

## 3. File-by-File Changes

### Phase 1: DNS & Transport Resilience

#### `internal/repository/raft_store.go`

**ADD — New types:**

```go
// DNSResolver is an interface for resolving advertise addresses.
// Decoupled from transport creation to allow retry loops.
type DNSResolver interface {
    // Resolve blocks until an address is resolved or ctx is cancelled.
    // Returns the resolved *net.TCPAddr.
    Resolve(ctx context.Context) (*net.TCPAddr, error)
    // Resolved returns the last successfully resolved address, or nil.
    Resolved() *net.TCPAddr
}

// retryDNSResolver implements DNSResolver with exponential backoff.
type retryDNSResolver struct {
    addr       string
    timeout    time.Duration
    logger     *logrus.Logger
    mu         sync.RWMutex
    resolved   *net.TCPAddr
    backoffCfg BackoffConfig
}

// BackoffConfig controls the retry backoff behavior.
type BackoffConfig struct {
    Initial  time.Duration // initial retry interval (default: 1s)
    Max      time.Duration // max retry interval (default: 30s)
    Multiplier float64     // backoff multiplier (default: 1.5)
}

// DefaultBackoffConfig returns the default backoff configuration.
func DefaultBackoffConfig() BackoffConfig {
    return BackoffConfig{
        Initial:    1 * time.Second,
        Max:        30 * time.Second,
        Multiplier: 1.5,
    }
}
```

**ADD — New functions:**

```go
// NewRetryDNSResolver creates a DNSResolver that retries indefinitely
// with exponential backoff. It does NOT have a timeout budget.
func NewRetryDNSResolver(addr string, timeout time.Duration, logger *logrus.Logger, cfg BackoffConfig) DNSResolver

// Start begins background DNS resolution. Non-blocking.
// Call Resolved() to check if an address has been found.
func (r *retryDNSResolver) Start(ctx context.Context)

// Resolve blocks until a *net.TCPAddr is resolved or ctx is cancelled.
func (r *retryDNSResolver) Resolve(ctx context.Context) (*net.TCPAddr, error)

// Resolved returns the last successfully resolved address, or nil.
func (r *retryDNSResolver) Resolved() *net.TCPAddr

// resolveAttempt performs a single DNS resolution attempt.
func (r *retryDNSResolver) resolveAttempt() (*net.TCPAddr, error)
```

**ADD — BoltDB custom options:**

```go
// defaultBoltOptions returns optimized BoltDB options matching Vault's settings.
func defaultBoltOptions() *bbolt.Options {
    return &bbolt.Options{
        Timeout:         1 * time.Second,
        NoFreelistSync:  true,
        FreelistType:    bbolt.FreelistMapType,
        InitialMmapSize: 100 * 1024 * 1024, // 100GB virtual, Linux MAP_POPULATE
    }
}
```

**ADD — Raft log cache:**

```go
const defaultRaftLogCacheSize = 512
```

**MODIFY — `RaftStoreConfig` struct:**

Add fields:
```go
type RaftStoreConfig struct {
    // ... existing fields ...

    // Backoff overrides the default DNS retry backoff. Zero value = defaults.
    Backoff BackoffConfig

    // PerformanceMultiplier scales election/heartbeat/lease timeouts.
    // Default: 5.0. Must be >= 1.0.
    PerformanceMultiplier float64

    // RaftLogCacheSize is the in-memory log cache size. Default: 512.
    RaftLogCacheSize int

    // NoSnapshotRestoreOnStart disables automatic snapshot restore on start.
    // Default: true (we manage snapshots ourselves).
    NoSnapshotRestoreOnStart bool

    // BoltOptions overrides the default BoltDB options. Nil = use defaults.
    BoltOptions *bbolt.Options
}
```

**MODIFY — `NewRaftStore` function:**

Before (current flow):
1. Resolve advertise addr (blocks 2 min max)
2. Create bolt store
3. Create snapshot store
4. Create transport (needs resolved addr)
5. Bootstrap cluster
6. Create raft.NewRaft

After (new flow):
1. Create DNS resolver (starts background goroutine)
2. Wait for initial DNS resolution with a short timeout (30s) — if fails, proceed with empty addr
3. Create bolt store with custom options
4. Create snapshot store
5. Create transport with a placeholder listener (or skip if no addr yet)
6. Set NoSnapshotRestoreOnStart = true
7. Create raft config with PerformanceMultiplier applied to timeouts
8. Create raft.NewRaft
9. If DNS resolved after raft creation, update transport address
10. Return RaftStore (caller retries bootstrap when DNS ready)

**MODIFY — `resolveAdvertiseAddr` function:**

- Mark as **DEPRECATED**. Replace with `NewRetryDNSResolver`.
- Keep for backward compatibility but add deprecation comment.
- Remove the 2-minute retry budget. Make it call the new resolver.

**ADD — `UpdateAdvertiseAddr` method:**

```go
// UpdateAdvertiseAddr updates the transport's advertise address.
// Called when DNS resolution completes after initial store creation.
func (s *RaftStore) UpdateAdvertiseAddr(addr *net.TCPAddr) error
```

**ADD — `ApplyPerformanceMultiplier` function:**

```go
// ApplyPerformanceMultiplier applies the multiplier to a Raft config.
func ApplyPerformanceMultiplier(cfg *raft.Config, multiplier float64) {
    if multiplier < 1.0 {
        multiplier = 1.0
    }
    cfg.ElectionTimeout = time.Duration(float64(cfg.ElectionTimeout) * multiplier)
    cfg.HeartbeatTimeout = time.Duration(float64(cfg.HeartbeatTimeout) * multiplier)
    cfg.LeaderLeaseTimeout = time.Duration(float64(cfg.LeaderLeaseTimeout) * multiplier)
}
```

**Edge cases handled:**
- DNS never resolves: Raft store is created but stays as a non-voter (can't join cluster). HTTP server still starts for health checks. Startup probe will fail, preventing traffic.
- DNS resolves after long delay: `UpdateAdvertiseAddr` is called, transport recreated if needed.
- DNS changes mid-run: Cached resolved address is returned; health check detects and triggers re-resolution.
- Context cancelled during resolution: `Resolve()` returns error, caller handles.

**Error handling:**
- DNS resolution errors logged at Warn level (not Error — expected during bootstrap).
- After 10 consecutive failures, log at Error level.
- `Resolve()` returns `context.Canceled` or `context.DeadlineExceeded` wrapped with `%w`.

---

#### `internal/repository/raft_discovery.go` (NEW FILE)

**ADD — Full file contents:**

```go
package repository

import (
    "context"
    "net"
    "sync"
    "time"

    "github.com/sirupsen/logrus"
)

// PeerDiscovery discovers and health-checks Raft peers.
type PeerDiscovery struct {
    resolver DNSResolver
    logger   *logrus.Logger
    mu       sync.RWMutex
    peers    map[string]*discoveredPeer
}

// discoveredPeer tracks a single peer's state.
type discoveredPeer struct {
    ID        string
    Addr      *net.TCPAddr
    LastSeen  time.Time
    Healthy   bool
}

// NewPeerDiscovery creates a new PeerDiscovery.
func NewPeerDiscovery(resolver DNSResolver, logger *logrus.Logger) *PeerDiscovery

// Start begins periodic peer discovery and health checking.
// interval is how often to re-resolve and health-check peers.
func (d *PeerDiscovery) Start(ctx context.Context, interval time.Duration)

// Peers returns the current list of discovered peers.
func (d *PeerDiscovery) Peers() []discoveredPeer

// HealthyPeers returns only peers that passed the last health check.
func (d *PeerDiscovery) HealthyPeers() []discoveredPeer

// ResolvePeer resolves a single peer by ID or address.
func (d *PeerDiscovery) ResolvePeer(ctx context.Context, addr string) (*net.TCPAddr, error)

// healthCheck performs a TCP dial health check on a peer.
func (d *PeerDiscovery) healthCheck(peer *discoveredPeer) bool
```

**Edge cases:**
- Empty peer list: Returns empty slice, no error.
- Peer address changes: Updated in-place, LastSeen updated.
- Health check timeout: Peer marked unhealthy, not removed.

---

### Phase 2: Bootstrap & Join Lifecycle

#### `internal/repository/raft_store.go`

**ADD — New types:**

```go
// JoinConfig controls the retry-join behavior.
type JoinConfig struct {
    // LeaderAddr is the address of a known leader to join.
    LeaderAddr string
    // RetryInterval is the interval between join attempts. Default: 2s.
    RetryInterval time.Duration
    // MaxConcurrent is the max number of concurrent join attempts. Default: 20.
    MaxConcurrent int
}
```

**ADD — New methods on RaftStore:**

```go
// RetryJoin continuously attempts to join the cluster via the given leader address.
// Blocks until join succeeds or ctx is cancelled.
// Uses 2-second interval between retries with exponential backoff capped at 30s.
// Respects MaxConcurrent for concurrent join workers.
func (s *RaftStore) RetryJoin(ctx context.Context, cfg JoinConfig) error

// joinAttempt performs a single join attempt against a leader.
func (s *RaftStore) joinAttempt(ctx context.Context, leaderAddr string) error

// IsRemoved checks if this node has been removed from the Raft configuration.
// Returns true if the node's ID is not in the current configuration.
// Used by the removed-checker goroutine to detect eviction.
func (s *RaftStore) IsRemoved() bool

// IsLeader returns true if this node is the current Raft leader.
func (s *RaftStore) IsLeader() bool

// IsVoter returns true if this node is a voting member of the cluster.
func (s *RaftStore) IsVoter() bool

// SetBootstrapConfig sets the bootstrap configuration for the cluster.
// Separated from SetupCluster to allow deferred bootstrapping.
// Only effective if the node is the bootstrap node.
func (s *RaftStore) SetBootstrapConfig(config raft.Configuration) *raft.BootstrapConfigurationFuture

// TransferLeadership attempts to transfer leadership to another node.
// Blocks until transfer completes or timeout.
func (s *RaftStore) TransferLeadership(ctx context.Context, timeout time.Duration) error

// LeaveCluster removes this node from the voter configuration gracefully.
// If this node is the leader, transfers leadership first.
func (s *RaftStore) LeaveCluster(ctx context.Context, timeout time.Duration) error
```

**MODIFY — Bootstrap in `NewRaftStore`:**

Current: Bootstrap node seeds cluster with ONLY itself as initial voter.
New: Call `SetBootstrapConfig` with the initial configuration. Allow the initial config to include multiple voters if provided via config (for recovery scenarios). Default to self-only.

**ADD — `raftLogCacheSize` to raft.Config:**

```go
raftConfig := raft.DefaultConfig()
raftConfig.LocalID = raft.ServerID(cfg.NodeID)
raftConfig.LogCacheSize = cfg.RaftLogCacheSize
if raftConfig.LogCacheSize == 0 {
    raftConfig.LogCacheSize = defaultRaftLogCacheSize
}
```

**ADD — `NoSnapshotRestoreOnStart` to raft.Config:**

```go
if cfg.NoSnapshotRestoreOnStart {
    raftConfig.NoSnapshotRestoreOnStart = true
}
```

---

#### `cmd/api/main.go`

**MODIFY — `initRaftStore` function:**

Before (current flow):
1. Validate config
2. Build peer resolver
3. Derive advertise addr (blocks)
4. Build TLS
5. Create RaftStore (blocks on DNS inside NewRaftStore)
6. WaitForLeader
7. Start observeLeadership + joinLoop
8. Resolve secrets
9. WaitForCleanState (BLOCKS — no HTTP until this passes)
10. Serve

After (new flow):
1. Validate config
2. Build peer resolver
3. Create DNSResolver (starts background goroutine, non-blocking)
4. Build TLS (can use placeholder certs if DNS not resolved)
5. Create RaftStore (non-blocking — uses placeholder addr if needed)
6. **Start HTTP server immediately** (health endpoints only)
7. Start `observeLeadership` goroutine
8. Start `retryJoin` goroutine (non-blocking, retries until join succeeds)
9. Start `startupComplete` goroutine that waits for:
   - DNS resolved
   - Raft joined cluster
   - Clean state reached
10. When startup complete, mark ready for traffic
11. Resolve secrets
12. Serve all endpoints

**ADD — `retryJoin` goroutine:**

```go
// retryJoinLoop continuously attempts to join the cluster.
// Runs in a background goroutine. On success, marks the node as joined.
func retryJoinLoop(ctx context.Context, store *repository.RaftStore, cfg JoinConfig, logger *logrus.Logger) {
    ticker := time.NewTicker(5 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            if store.IsVoter() {
                return // already joined
            }
            if err := store.RetryJoin(ctx, cfg); err != nil {
                logger.WithError(err).Warn("retry join failed")
            }
        }
    }
}
```

**ADD — Graceful shutdown handler:**

```go
// handleShutdown performs graceful Raft shutdown on SIGTERM/SIGINT.
func handleShutdown(ctx context.Context, store *repository.RaftStore, logger *logrus.Logger, shutdownTimeout time.Duration) {
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
    <-sigCh

    logger.Info("received shutdown signal, leaving Raft cluster")

    leaveCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
    defer cancel()

    if err := store.LeaveCluster(leaveCtx, shutdownTimeout); err != nil {
        logger.WithError(err).Error("failed to leave Raft cluster gracefully")
    }

    logger.Info("Raft shutdown complete")
}
```

**MODIFY — `WaitForCleanState` call:**

Remove the blocking `WaitForCleanState` from the critical path. Instead, move it to the `startupComplete` goroutine. The HTTP server starts serving immediately.

**ADD — Startup probe endpoint (or modify `/healthz`):**

```go
// handleStartup returns 200 when the Raft node has joined the cluster
// and DNS is resolved. Used by Kubernetes startupProbe.
func handleStartup(store *repository.RaftStore, resolver repository.DNSResolver) app.HandlerFunc {
    return func(ctx context.Context, c *app.RequestContext) {
        if resolver.Resolved() == nil {
            c.JSON(consts.StatusServiceUnavailable, map[string]string{
                "status": "dns_not_resolved",
            })
            return
        }
        if !store.IsVoter() && !store.IsLeader() {
            c.JSON(consts.StatusServiceUnavailable, map[string]string{
                "status": "not_joined",
            })
            return
        }
        c.JSON(consts.StatusOK, map[string]string{
            "status": "started",
        })
    }
}
```

**MODIFY — `readinessProbe` handler (`/readyz`):**

Current: Returns 503 when Raft not clean.
New: Accepts Follower state too. Only returns 503 if node is not a voter and not the leader (i.e., hasn't joined yet).

```go
func handleReadyz(store *repository.RaftStore) app.HandlerFunc {
    return func(ctx context.Context, c *app.RequestContext) {
        if store.IsLeader() || store.IsVoter() {
            c.JSON(consts.StatusOK, map[string]string{"status": "ready"})
            return
        }
        c.JSON(consts.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
    }
}
```

**MODIFY — `livenessProbe` handler (`/healthz`):**

Keep as-is: basic process health check, returns 200 always.

---

### Phase 3: Graceful Shutdown & K8s Stability

#### `deploy/helm/dagger-kubernetes/templates/statefulset.yaml`

**ADD — preStop hook:**

```yaml
lifecycle:
  preStop:
    exec:
      command:
        - /bin/sh
        - -c
        - |
          curl -s -X POST http://localhost:{{ .Values.service.controlPort }}/v1/cluster/leave \
            --max-time 30 || true
```

**ADD — startupProbe:**

```yaml
startupProbe:
  httpGet:
    path: /startup
    port: {{ .Values.service.controlPort }}
  initialDelaySeconds: 10
  periodSeconds: 5
  failureThreshold: 60   # 5 minutes total (10 + 5*60 = 310s)
  timeoutSeconds: 5
```

**MODIFY — readinessProbe:**

```yaml
readinessProbe:
  httpGet:
    path: /readyz
    port: {{ .Values.service.controlPort }}
  initialDelaySeconds: 5
  periodSeconds: 5
  failureThreshold: 3
  timeoutSeconds: 5
  # No change to structure, but handler logic changed (see Phase 2).
```

**MODIFY — livenessProbe:**

```yaml
livenessProbe:
  httpGet:
    path: /healthz
    port: {{ .Values.service.controlPort }}
  initialDelaySeconds: 30
  periodSeconds: 10
  failureThreshold: 3
  timeoutSeconds: 5
```

**ADD — terminationGracePeriodSeconds:**

```yaml
spec:
  terminationGracePeriodSeconds: {{ .Values.raft.terminationGracePeriodSeconds | default 60 }}
```

**ADD — `/v1/cluster/leave` endpoint reference in container ports / service.**

#### NEW `deploy/helm/dagger-kubernetes/templates/pdb.yaml`

```yaml
{{- if .Values.podDisruptionBudget.enabled }}
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: {{ include "dagger-kubernetes.fullname" . }}
  labels:
    {{- include "dagger-kubernetes.labels" . | nindent 4 }}
spec:
  {{- if .Values.podDisruptionBudget.minAvailable }}
  minAvailable: {{ .Values.podDisruptionBudget.minAvailable }}
  {{- else }}
  maxUnavailable: {{ .Values.podDisruptionBudget.maxUnavailable | default 1 }}
  {{- end }}
  selector:
    matchLabels:
      {{- include "dagger-kubernetes.selectorLabels" . | nindent 6 }}
{{- end }}
```

#### `deploy/helm/dagger-kubernetes/values.yaml`

**ADD — New values:**

```yaml
raft:
  # ... existing raft values ...

  # performanceMultiplier scales election/heartbeat/lease timeouts.
  # Higher values reduce false-positive leader elections at the cost of
  # slower failure detection. Default: 5.0
  performanceMultiplier: 5.0

  # raftLogCacheSize is the in-memory log cache size. Default: 512
  raftLogCacheSize: 512

  # terminationGracePeriodSeconds for the pod. Default: 60
  terminationGracePeriodSeconds: 60

  # autopilot configuration
  autopilot:
    # enabled enables the autopilot subsystem.
    enabled: true
    # cleanupDeadServers enables automatic removal of dead servers.
    cleanupDeadServers: true
    # deadServerLastContactThreshold is how long before a server is considered dead.
    deadServerLastContactThreshold: "24h"
    # minQuorum is the minimum number of voters before autopilot stops removing.
    minQuorum: 3
    # stabilizationTime is how long to wait after a membership change before
    # allowing another change. Default: 10s
    stabilizationTime: "10s"

  # join configuration
  join:
    # retryInterval is the interval between join attempts. Default: 2s
    retryInterval: "2s"
    # maxConcurrent is the max number of concurrent join workers. Default: 20
    maxConcurrent: 20

  # tls configuration
  tls:
    # rotationPeriod is how often to rotate TLS certificates. Default: 24h
    rotationPeriod: "24h"
    # caValidity is the CA certificate validity period. Default: 30 years
    caValidity: "262800h"  # 30 years

# PodDisruptionBudget
podDisruptionBudget:
  enabled: true
  maxUnavailable: 1
```

---

### Phase 4: Leader Election & Autopilot

#### `internal/repository/raft_store.go`

**ADD — `WithPerformanceMultiplier` to RaftStoreConfig defaults:**

In `NewRaftStore`:
```go
if cfg.PerformanceMultiplier == 0 {
    cfg.PerformanceMultiplier = 5.0
}
ApplyPerformanceMultiplier(raftConfig, cfg.PerformanceMultiplier)
```

**ADD — `StepDown` method:**

```go
// StepDown causes the leader to step down to follower status.
// Used during graceful shutdown to allow a clean leadership transfer.
func (s *RaftStore) StepDown(ctx context.Context) error
```

**MODIFY — `LeaveCluster` implementation:**

```go
func (s *RaftStore) LeaveCluster(ctx context.Context, timeout time.Duration) error {
    // 1. If leader, transfer leadership first
    if s.IsLeader() {
        if err := s.TransferLeadership(ctx, timeout/2); err != nil {
            s.logger.WithError(err).Warn("leadership transfer failed, stepping down")
            if err := s.StepDown(ctx); err != nil {
                return fmt.Errorf("step down: %w", err)
            }
        }
    }

    // 2. Remove self from voter configuration
    removeFuture := s.raft.RemoveServer(raft.ServerID(s.nodeID), 0, 0)
    if err := removeFuture.Error(); err != nil {
        return fmt.Errorf("remove self from cluster: %w", err)
    }

    // 3. Wait for commit index to catch up
    // (RemoveServer is a log entry; wait for it to be committed)
    ctx, cancel := context.WithTimeout(ctx, timeout/2)
    defer cancel()
    if err := s.waitForCommit(ctx); err != nil {
        s.logger.WithError(err).Warn("timeout waiting for remove-server commit")
    }

    return nil
}
```

#### NEW `internal/repository/raft_autopilot.go`

**ADD — Full file contents:**

```go
package repository

import (
    "context"
    "sync"
    "time"

    "github.com/hashicorp/raft"
    "github.com/sirupsen/logrus"
)

// AutopilotConfig controls the autopilot subsystem behavior.
type AutopilotConfig struct {
    // Enabled enables the autopilot subsystem.
    Enabled bool
    // CleanupDeadServers enables automatic removal of dead servers.
    CleanupDeadServers bool
    // DeadServerLastContactThreshold is how long since last contact before
    // a server is considered dead. Default: 24h.
    DeadServerLastContactThreshold time.Duration
    // MinQuorum is the minimum number of voters before autopilot stops
    // removing servers. Default: 3.
    MinQuorum int
    // StabilizationTime is how long to wait after a membership change
    // before allowing another change. Default: 10s.
    StabilizationTime time.Duration
    // HeartbeatInterval is how often the leader checks follower heartbeats.
    // Default: 1s.
    HeartbeatInterval time.Duration
}

// DefaultAutopilotConfig returns the default autopilot configuration.
func DefaultAutopilotConfig() AutopilotConfig {
    return AutopilotConfig{
        Enabled:                        true,
        CleanupDeadServers:             true,
        DeadServerLastContactThreshold: 24 * time.Hour,
        MinQuorum:                      3,
        StabilizationTime:              10 * time.Second,
        HeartbeatInterval:              1 * time.Second,
    }
}

// FollowerState tracks the health of a single follower.
type FollowerState struct {
    ID                raft.ServerID
    Address           raft.ServerAddress
    LastHeartbeat     time.Time
    LastHeartbeatAck  time.Time
    AppliedIndexDelta int64
    Term              uint64
    Healthy           bool
}

// Autopilot manages Raft cluster health, dead server detection, and stabilization.
type Autopilot struct {
    cfg       AutopilotConfig
    raft      *raft.Raft
    logger    *logrus.Logger

    mu            sync.RWMutex
    followers     map[raft.ServerID]*FollowerState
    lastChange    time.Time
    leaderCh      <-chan bool
}

// NewAutopilot creates a new Autopilot instance.
func NewAutopilot(cfg AutopilotConfig, r *raft.Raft, leaderCh <-chan bool, logger *logrus.Logger) *Autopilot

// Start begins the autopilot control loop. Blocks until ctx is cancelled.
func (a *Autopilot) Start(ctx context.Context)

// GetFollowerState returns the state for a specific follower.
func (a *Autopilot) GetFollowerState(id raft.ServerID) (*FollowerState, bool)

// GetAllFollowerStates returns all tracked follower states.
func (a *Autopilot) GetAllFollowerStates() map[raft.ServerID]*FollowerState

// DeadServers returns servers that exceed the dead-server threshold.
func (a *Autopilot) DeadServers() []raft.ServerID

// RemoveDeadServers removes all dead servers from the cluster configuration.
// Respects MinQuorum and StabilizationTime.
func (a *Autopilot) RemoveDeadServers(ctx context.Context) error

// stabilize waits for the stabilization period before allowing changes.
func (a *Autopilot) stabilize()

// heartbeatTracker runs on the leader, tracking follower heartbeats.
func (a *Autopilot) heartbeatTracker(ctx context.Context)

// isDead returns true if a follower exceeds the dead-server threshold.
func (a *Autopilot) isDead(state *FollowerState) bool
```

**Edge cases:**
- Not the leader: `heartbeatTracker` is a no-op. Only the leader tracks heartbeats.
- MinQuorum not met: `RemoveDeadServers` returns an error and logs a warning.
- During stabilization: `RemoveDeadServers` is a no-op until stabilization period passes.
- Node rejoins after being marked dead: `FollowerState` is recreated with fresh timestamps.
- All followers dead: No removal if MinQuorum would be violated.

---

#### `internal/domain/config.go`

**ADD — New config structs:**

```go
// AutopilotConfig is the domain-level autopilot configuration.
type AutopilotConfig struct {
    Enabled                        bool          `mapstructure:"enabled"`
    CleanupDeadServers             bool          `mapstructure:"cleanup_dead_servers"`
    DeadServerLastContactThreshold time.Duration `mapstructure:"dead_server_last_contact_threshold"`
    MinQuorum                      int           `mapstructure:"min_quorum"`
    StabilizationTime              time.Duration `mapstructure:"stabilization_time"`
    HeartbeatInterval              time.Duration `mapstructure:"heartbeat_interval"`
}

// RaftConfig holds all Raft-related configuration.
type RaftConfig struct {
    // ... existing fields ...

    PerformanceMultiplier   float64          `mapstructure:"performance_multiplier"`
    RaftLogCacheSize        int              `mapstructure:"raft_log_cache_size"`
    NoSnapshotRestoreOnStart bool            `mapstructure:"no_snapshot_restore_on_start"`
    Autopilot               AutopilotConfig  `mapstructure:"autopilot"`
    Join                    JoinDomainConfig `mapstructure:"join"`
    TLS                     RaftTLSConfig    `mapstructure:"tls"`
    TerminationGracePeriod  time.Duration    `mapstructure:"termination_grace_period"`
}

// JoinDomainConfig holds join-related configuration.
type JoinDomainConfig struct {
    RetryInterval time.Duration `mapstructure:"retry_interval"`
    MaxConcurrent int           `mapstructure:"max_concurrent"`
}

// RaftTLSConfig holds TLS-related configuration.
type RaftTLSConfig struct {
    RotationPeriod time.Duration `mapstructure:"rotation_period"`
    CAValidity     time.Duration `mapstructure:"ca_validity"`
}
```

---

#### `config/loader.go`

**ADD — `v.SetDefault` calls for new config keys:**

```go
// Raft defaults
v.SetDefault("raft.performance_multiplier", 5.0)
v.SetDefault("raft.raft_log_cache_size", 512)
v.SetDefault("raft.no_snapshot_restore_on_start", true)
v.SetDefault("raft.termination_grace_period", "60s")

// Autopilot defaults
v.SetDefault("raft.autopilot.enabled", true)
v.SetDefault("raft.autopilot.cleanup_dead_servers", true)
v.SetDefault("raft.autopilot.dead_server_last_contact_threshold", "24h")
v.SetDefault("raft.autopilot.min_quorum", 3)
v.SetDefault("raft.autopilot.stabilization_time", "10s")
v.SetDefault("raft.autopilot.heartbeat_interval", "1s")

// Join defaults
v.SetDefault("raft.join.retry_interval", "2s")
v.SetDefault("raft.join.max_concurrent", 20)

// TLS defaults
v.SetDefault("raft.tls.rotation_period", "24h")
v.SetDefault("raft.tls.ca_validity", "262800h") // 30 years
```

---

### Phase 5: TLS & Certificates

#### `internal/repository/raft_tls.go`

**ADD — New types:**

```go
// Keyring manages TLS certificate rotation with up to 2 keys (active + pending).
type Keyring struct {
    mu     sync.RWMutex
    active  *tls.Certificate
    pending *tls.Certificate
    caCert  *x509.Certificate
    caKey   crypto.PrivateKey
    logger  *logrus.Logger
}

// NewKeyring creates a new Keyring with auto-generated ECDSA P-521 keys.
func NewKeyring(logger *logrus.Logger) (*Keyring, error)

// Active returns the currently active TLS certificate.
func (k *Keyring) Active() *tls.Certificate

// Pending returns the pending TLS certificate, or nil.
func (k *Keyring) Pending() *tls.Certificate

// GetCertificate is a tls.Config.GetCertificate callback that returns
// the active certificate, falling back to pending.
func (k *Keyring) GetCertificate(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error)

// Rotate generates a new pending certificate and begins the rotation.
// Phase 1: Generate new cert, set as pending.
// Phase 2: Wait for applied index to advance (new cert propagated).
// Phase 3: Promote pending to active, old active is discarded.
// Blocks until rotation is complete or ctx is cancelled.
func (k *Keyring) Rotate(ctx context.Context, appliedIndexFuture func() uint64) error

// generateCA generates a self-signed CA certificate with 30-year validity.
func (k *Keyring) generateCA() error

// generateLeaf generates a leaf certificate signed by the CA.
func (k *Keyring) generateLeaf(nodeID string) (*tls.Certificate, error)

// CACertPool returns a cert pool containing the CA certificate.
func (k *Keyring) CACertPool() *x509.CertPool

// StartRotationLoop periodically rotates certificates.
// rotationPeriod is the interval between rotations.
func (k *Keyring) StartRotationLoop(ctx context.Context, rotationPeriod time.Duration, appliedIndexFuture func() uint64)
```

**MODIFY — TLS stream layer creation:**

Current: Creates TLS config with whatever certificate is provided.
New: Uses Keyring's `GetCertificate` callback so certificates can rotate without restarting the transport.

```go
// NewRaftTLSStreamLayer creates a TLS stream layer with rotating certificates.
func NewRaftTLSStreamLayer(addr net.Addr, keyring *Keyring, logger *logrus.Logger) (*raft.NetworkTransport, error) {
    tlsConfig := &tls.Config{
        GetCertificate: keyring.GetCertificate,
        ClientAuth:     tls.RequireAndVerifyClientCert,
        ClientCAs:      keyring.CACertPool(),
        MinVersion:     tls.VersionTLS12,
        // SNI-based peer identification
        VerifyPeerCertificate: verifyPeerID,
    }
    // ...
}

// verifyPeerID extracts the peer ID from the client certificate's SAN.
func verifyPeerID(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error
```

**MODIFY — Certificate generation:**

Current: Uses default validity (likely 1 year).
New: Use 30-year CA validity (262800 hours). Leaf certificates: 90 days (they rotate frequently).

---

### Phase 6: Observability & Recovery

#### `internal/observ/raft_metrics.go` (NEW FILE)

**ADD — Full file contents:**

```go
package observ

import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
)

// RaftMetrics holds Prometheus metrics for Raft cluster health.
type RaftMetrics struct {
    LeaderChanges          prometheus.Counter
    FollowerHeartbeatAgeMs prometheus.GaugeVec
    AppliedIndexDelta      prometheus.GaugeVec
    RaftState              prometheus.Gauge
    LastLogIndex           prometheus.Gauge
    CommitIndex            prometheus.Gauge
    AppliedIndex           prometheus.Gauge
    NumPeers               prometheus.Gauge
    JoinAttempts           prometheus.Counter
    JoinFailures           prometheus.Counter
    DNSResolutionFailures  prometheus.Counter
    LeadershipTransfers    prometheus.Counter
    DeadServersDetected    prometheus.Counter
}

// NewRaftMetrics creates and registers Raft metrics.
func NewRaftMetrics() *RaftMetrics {
    return &RaftMetrics{
        LeaderChanges: promauto.NewCounter(prometheus.CounterOpts{
            Name: "raft_leader_changes_total",
            Help: "Total number of leader changes observed.",
        }),
        FollowerHeartbeatAgeMs: *promauto.NewGaugeVec(prometheus.GaugeOpts{
            Name: "raft_follower_heartbeat_age_ms",
            Help: "Milliseconds since last heartbeat from each follower.",
        }, []string{"follower_id"}),
        AppliedIndexDelta: *promauto.NewGaugeVec(prometheus.GaugeOpts{
            Name: "raft_applied_index_delta",
            Help: "Difference between leader and follower applied index.",
        }, []string{"follower_id"}),
        RaftState: promauto.NewGauge(prometheus.GaugeOpts{
            Name: "raft_state",
            Help: "Current Raft state (0=Follower, 1=Candidate, 2=Leader).",
        }),
        LastLogIndex: promauto.NewGauge(prometheus.GaugeOpts{
            Name: "raft_last_log_index",
            Help: "Last log index.",
        }),
        CommitIndex: promauto.NewGauge(prometheus.GaugeOpts{
            Name: "raft_commit_index",
            Help: "Commit index.",
        }),
        AppliedIndex: promauto.NewGauge(prometheus.GaugeOpts{
            Name: "raft_applied_index",
            Help: "Applied index.",
        }),
        NumPeers: promauto.NewGauge(prometheus.GaugeOpts{
            Name: "raft_num_peers",
            Help: "Number of peers in the cluster.",
        }),
        JoinAttempts: promauto.NewCounter(prometheus.CounterOpts{
            Name: "raft_join_attempts_total",
            Help: "Total number of join attempts.",
        }),
        JoinFailures: promauto.NewCounter(prometheus.CounterOpts{
            Name: "raft_join_failures_total",
            Help: "Total number of failed join attempts.",
        }),
        DNSResolutionFailures: promauto.NewCounter(prometheus.CounterOpts{
            Name: "raft_dns_resolution_failures_total",
            Help: "Total number of DNS resolution failures.",
        }),
        LeadershipTransfers: promauto.NewCounter(prometheus.CounterOpts{
            Name: "raft_leadership_transfers_total",
            Help: "Total number of leadership transfers.",
        }),
        DeadServersDetected: promauto.NewCounter(prometheus.CounterOpts{
            Name: "raft_dead_servers_detected_total",
            Help: "Total number of dead servers detected by autopilot.",
        }),
    }
}

// UpdateFromStats updates metrics from raft.Stats().
func (m *RaftMetrics) UpdateFromStats(stats map[string]string)

// UpdateFromAutopilot updates metrics from autopilot follower states.
func (m *RaftMetrics) UpdateFromAutopilot(states map[string]*FollowerState) // import cycle — use interface
```

**Note on import cycle:** Define a `FollowerStateExporter` interface in `internal/domain/` to avoid `observ -> repository` import cycle. `repository.FollowerState` implements this interface.

#### `internal/repository/raft_store.go`

**ADD — peers.json recovery:**

```go
// peersJSONPath returns the path to the peers.json recovery file.
func peersJSONPath(dataDir string) string {
    return filepath.Join(dataDir, "peers.json")
}

// WritePeersJSON writes the current Raft configuration to peers.json.
// Called after every successful membership change.
func (s *RaftStore) WritePeersJSON() error

// RecoverFromPeersJSON attempts to recover cluster configuration from peers.json.
// Used during startup if the Raft log exists but the node can't join.
func (s *RaftStore) RecoverFromPeersJSON() error

// StartPeersJSONWriter starts a goroutine that periodically writes peers.json.
func (s *RaftStore) StartPeersJSONWriter(ctx context.Context, interval time.Duration)
```

**MODIFY — `ReconcileMembership`:**

After each successful add/update/remove, call `WritePeersJSON()`.

**ADD — `StartRemovedChecker`:**

```go
// StartRemovedChecker polls for removal from the cluster configuration.
// If this node is removed, it shuts down the Raft store.
// Matches Vault's StartRemovedChecker pattern.
func (s *RaftStore) StartRemovedChecker(ctx context.Context, interval time.Duration)
```

---

### Phase 7: Tests

#### `internal/repository/raft_store_test.go`

**ADD — New test functions:**

```go
// TestRetryJoin_EventuallySucceeds verifies that RetryJoin succeeds after
// initial failures when the leader becomes available.
func TestRetryJoin_EventuallySucceeds(t *testing.T)

// TestRetryJoin_ContextCancelled verifies that RetryJoin returns when
// the context is cancelled.
func TestRetryJoin_ContextCancelled(t *testing.T)

// TestLeaveCluster_AsLeader verifies that LeaveCluster transfers leadership
// and removes the node when called on the leader.
func TestLeaveCluster_AsLeader(t *testing.T)

// TestLeaveCluster_AsFollower verifies that LeaveCluster removes the node
// when called on a follower.
func TestLeaveCluster_AsFollower(t *testing.T)

// TestDNSResolver_RetriesIndefinitely verifies that the DNS resolver
// retries indefinitely with exponential backoff.
func TestDNSResolver_RetriesIndefinitely(t *testing.T)

// TestDNSResolver_ResolvedAfterRetries verifies that the resolver eventually
// returns the address after DNS becomes available.
func TestDNSResolver_ResolvedAfterRetries(t *testing.T)

// TestDNSResolver_ContextCancelled verifies that Resolve() returns
// context.Canceled when the context is cancelled.
func TestDNSResolver_ContextCancelled(t *testing.T)

// TestBoltDBOptions_Applied verifies that custom BoltDB options are applied.
func TestBoltDBOptions_Applied(t *testing.T)

// TestRaftLogCacheSize_Applied verifies that the log cache size is set.
func TestRaftLogCacheSize_Applied(t *testing.T)

// TestPerformanceMultiplier_Applied verifies that the multiplier affects
// election/heartbeat/lease timeouts.
func TestPerformanceMultiplier_Applied(t *testing.T)

// TestPeersJSON_WriteAndRecover verifies the peers.json write/recovery cycle.
func TestPeersJSON_WriteAndRecover(t *testing.T)

// TestIsRemoved_AfterLeave verifies IsRemoved returns true after leaving.
func TestIsRemoved_AfterLeave(t *testing.T)

// TestStartRemovedChecker_DetectsRemoval verifies the removed checker
// detects removal and shuts down.
func TestStartRemovedChecker_DetectsRemoval(t *testing.T)
```

#### `internal/repository/raft_tls_test.go`

**ADD — New test functions:**

```go
// TestKeyring_Rotate verifies the full 4-phase certificate rotation.
func TestKeyring_Rotate(t *testing.T)

// TestKeyring_GetCertificate_ReturnsActive verifies GetCertificate behavior.
func TestKeyring_GetCertificate_ReturnsActive(t *testing.T)

// TestKeyring_CAValidity_30Years verifies the CA has 30-year validity.
func TestKeyring_CAValidity_30Years(t *testing.T)

// TestKeyring_RotationLoop verifies the periodic rotation goroutine.
func TestKeyring_RotationLoop(t *testing.T)

// TestKeyring_ConcurrentAccess verifies thread safety.
func TestKeyring_ConcurrentAccess(t *testing.T)
```

#### `internal/repository/raft_autopilot_test.go` (NEW FILE)

```go
// TestAutopilot_DetectsDeadServers verifies dead server detection.
func TestAutopilot_DetectsDeadServers(t *testing.T)

// TestAutopilot_RemovesDeadServers verifies dead server removal.
func TestAutopilot_RemovesDeadServers(t *testing.T)

// TestAutopilot_RespectsMinQuorum verifies min_quorum enforcement.
func TestAutopilot_RespectsMinQuorum(t *testing.T)

// TestAutopilot_StabilizationPeriod verifies stabilization between changes.
func TestAutopilot_StabilizationPeriod(t *testing.T)

// TestAutopilot_NonLeaderNoop verifies autopilot is no-op on followers.
func TestAutopilot_NonLeaderNoop(t *testing.T)
```

#### NEW `tests/integration/raft_stability_test.go`

```go
// TestRollingRestart_NoDataLoss verifies no data loss during rolling restart.
// - Start 3-node cluster
// - Write data continuously
// - Restart pods one at a time
// - Verify all data is readable
func TestRollingRestart_NoDataLoss(t *testing.T)

// TestPodFailure_LeaderElection verifies leader election after pod failure.
// - Start 3-node cluster
// - Kill the leader pod
// - Verify new leader elected within election timeout * multiplier
// - Verify no writes lost
func TestPodFailure_LeaderElection(t *testing.T)

// TestDNSUnavailability_BootstrapDelay verifies bootstrap succeeds after
// DNS becomes available.
// - Start cluster with delayed DNS resolution
// - Verify all nodes eventually join
func TestDNSUnavailability_BootstrapDelay(t *testing.T)

// TestGracefulLeave_NoDisruption verifies graceful leave doesn't disrupt
// the cluster.
// - Start 3-node cluster
// - Gracefully remove one node
// - Verify remaining nodes continue serving
func TestGracefulLeave_NoDisruption(t *testing.T)

// TestAutopilot_CleansUpDeadNode verifies autopilot removes a dead node.
// - Start 3-node cluster
// - Hard-kill one node (no graceful leave)
// - Wait for dead server threshold
// - Verify dead node is removed from configuration
func TestAutopilot_CleansUpDeadNode(t *testing.T)

// TestConcurrentRestart_QuorumPreserved verifies quorum is preserved
// during concurrent restarts (Parallel pod management).
// - Start 3-node cluster
// - Restart 2 pods simultaneously
// - Verify remaining node maintains leadership
// - Verify restarted pods rejoin
func TestConcurrentRestart_QuorumPreserved(t *testing.T)
```

---

## 4. Implementation Order

Each phase can be tested independently. Phases 1-3 are the highest priority (fix crashloop and instability). Phases 4-6 add resilience and observability. Phase 7 runs in parallel with its corresponding implementation phase.

```
Step 1:  Phase 1 — DNS & Transport Resilience
Step 2:  Phase 2 — Bootstrap & Join Lifecycle
Step 3:  Phase 3 — Graceful Shutdown & K8s Stability (depends on Phase 2 for /v1/cluster/leave endpoint)
Step 4:  Phase 5 — TLS & Certificates (independent, but transport changes depend on Phase 1)
Step 5:  Phase 4 — Autopilot (depends on Phase 2 for stable cluster operations)
Step 6:  Phase 6 — Observability & Recovery (depends on Phase 4 for follower states)
Step 7:  Phase 7 — Tests (written alongside each phase, final integration tests last)
```

Dependency graph:
```
Phase 1 ──→ Phase 2 ──→ Phase 3
                │
                └──→ Phase 4 ──→ Phase 6
Phase 1 ──→ Phase 5
                │
                └──→ Phase 7 (unit tests alongside, integration tests after Phase 4)
```

---

## 5. Verification Strategy

### Phase 1 Verification
- Unit test: `TestDNSResolver_RetriesIndefinitely` — mock DNS that fails 50 times then succeeds. Verify resolver returns correct address.
- Unit test: `TestDNSResolver_ContextCancelled` — cancel context during resolution, verify `context.Canceled`.
- Unit test: `TestBoltDBOptions_Applied` — verify BoltDB is opened with custom options.
- Unit test: `TestRaftLogCacheSize_Applied` — verify `raft.Config.LogCacheSize` is set.
- Manual: Start a pod without DNS. Verify it doesn't crashloop. Verify it eventually resolves when DNS appears.

### Phase 2 Verification
- Unit test: `TestRetryJoin_EventuallySucceeds` — start 2-node cluster, verify join succeeds.
- Unit test: `TestRetryJoin_ContextCancelled` — cancel context, verify return.
- Unit test: `TestIsRemoved_AfterLeave` — leave cluster, verify `IsRemoved()` returns true.
- Manual: Start 3-node cluster. Verify all nodes show as joined in startup probe.
- Manual: Kill leader. Verify `/readyz` returns 200 on followers during election.

### Phase 3 Verification
- Integration test: `TestGracefulLeave_NoDisruption` — remove node, verify cluster stable.
- Integration test: `TestConcurrentRestart_QuorumPreserved` — restart 2 pods simultaneously, verify quorum.
- Manual: `kubectl delete pod <leader>`. Verify preStop hook runs. Verify new leader elected. Verify no 5xx errors during transition.
- Manual: Verify PDB prevents eviction of >1 pod during voluntary disruption.

### Phase 4 Verification
- Unit test: `TestAutopilot_DetectsDeadServers` — simulate dead server, verify detection.
- Unit test: `TestAutopilot_RemovesDeadServers` — verify dead server removed from config.
- Unit test: `TestAutopilot_RespectsMinQuorum` — verify no removal below min_quorum.
- Unit test: `TestAutopilot_StabilizationPeriod` — verify delay between changes.
- Integration test: `TestAutopilot_CleansUpDeadNode` — hard-kill node, verify auto-cleanup.

### Phase 5 Verification
- Unit test: `TestKeyring_Rotate` — trigger rotation, verify new cert active.
- Unit test: `TestKeyring_CAValidity_30Years` — verify CA NotAfter is ~30 years from now.
- Unit test: `TestKeyring_RotationLoop` — start rotation loop, verify periodic rotation.
- Manual: Run cluster for >24h (or set rotationPeriod=30s), verify cert rotation without disruption.

### Phase 6 Verification
- Unit test: `TestPeersJSON_WriteAndRecover` — write peers.json, restart, verify recovery.
- Manual: Scrape `/metrics`, verify all new metrics present and updating.
- Manual: Kill a node, verify `raft_follower_heartbeat_age_ms` increases for that node.
- Manual: Trigger leadership transfer, verify `raft_leadership_transfers_total` increments.

### Phase 7 Verification
- Integration test: `TestRollingRestart_NoDataLoss` — full rolling restart, verify data integrity.
- Integration test: `TestPodFailure_LeaderElection` — kill leader, verify election.
- Integration test: `TestDNSUnavailability_BootstrapDelay` — delayed DNS, verify bootstrap.
- Full CI gate: `dagger call -m ./dagger --src . ci export --path out` must pass.

---

## 6. Config Keys Reference

| Config Key | Type | Default | Helm Values Path | Description |
|---|---|---|---|---|
| `raft.performance_multiplier` | float64 | 5.0 | `.Values.raft.performanceMultiplier` | Scales election/heartbeat/lease timeouts |
| `raft.raft_log_cache_size` | int | 512 | `.Values.raft.raftLogCacheSize` | In-memory Raft log cache size |
| `raft.no_snapshot_restore_on_start` | bool | true | `.Values.raft.noSnapshotRestoreOnStart` | Disable auto snapshot restore on start |
| `raft.termination_grace_period` | duration | 60s | `.Values.raft.terminationGracePeriodSeconds` | Pod termination grace period |
| `raft.autopilot.enabled` | bool | true | `.Values.raft.autopilot.enabled` | Enable autopilot subsystem |
| `raft.autopilot.cleanup_dead_servers` | bool | true | `.Values.raft.autopilot.cleanupDeadServers` | Auto-remove dead servers |
| `raft.autopilot.dead_server_last_contact_threshold` | duration | 24h | `.Values.raft.autopilot.deadServerLastContactThreshold` | Time before server considered dead |
| `raft.autopilot.min_quorum` | int | 3 | `.Values.raft.autopilot.minQuorum` | Minimum voters before stopping removal |
| `raft.autopilot.stabilization_time` | duration | 10s | `.Values.raft.autopilot.stabilizationTime` | Wait time between membership changes |
| `raft.autopilot.heartbeat_interval` | duration | 1s | `.Values.raft.autopilot.heartbeatInterval` | Leader heartbeat check interval |
| `raft.join.retry_interval` | duration | 2s | `.Values.raft.join.retryInterval` | Interval between join attempts |
| `raft.join.max_concurrent` | int | 20 | `.Values.raft.join.maxConcurrent` | Max concurrent join workers |
| `raft.tls.rotation_period` | duration | 24h | `.Values.raft.tls.rotationPeriod` | TLS certificate rotation period |
| `raft.tls.ca_validity` | duration | 262800h (30y) | `.Values.raft.tls.caValidity` | CA certificate validity period |
| `podDisruptionBudget.enabled` | bool | true | `.Values.podDisruptionBudget.enabled` | Enable PodDisruptionBudget |
| `podDisruptionBudget.maxUnavailable` | int | 1 | `.Values.podDisruptionBudget.maxUnavailable` | Max unavailable pods during disruption |

All `v.SetDefault()` calls are in `config/loader.go` under the Raft defaults section.

---

## 7. Rollback Plan

### Before Deploying
1. Take a snapshot of the Raft data directory on all nodes:
   ```bash
   kubectl exec <pod> -- tar czf /tmp/raft-backup-$(date +%s).tgz /var/lib/dagger-kubernetes/raft
   kubectl cp <pod>:/tmp/raft-backup-*.tgz ./backups/
   ```
2. Backup the current Helm release values:
   ```bash
   helm get values dagger-kubernetes-test > backup-values.yaml
   ```

### If Rollback Is Needed
1. **Revert Helm chart**: Deploy the previous chart version with the backed-up values.
2. **Restore Raft data** (if data corruption detected):
   ```bash
   kubectl exec <pod> -- rm -rf /var/lib/dagger-kubernetes/raft
   kubectl cp ./backups/raft-backup-*.tgz <pod>:/tmp/
   kubectl exec <pod> -- tar xzf /tmp/raft-backup-*.tgz -C /
   kubectl rollout restart statefulset/dagger-kubernetes
   ```
3. **Manual recovery** (if cluster can't form quorum):
   - Use `peers.json` on the node with the most recent data.
   - Start a single-node cluster with `raft.recovery_mode=true` (new config flag).
   - Once the single node is stable, add remaining nodes.

### Feature Flags for Gradual Rollout
All new behaviors can be disabled via config:

| Feature | Disable Via |
|---|---|
| Autopilot | `raft.autopilot.enabled: false` |
| Auto-cleanup dead servers | `raft.autopilot.cleanup_dead_servers: false` |
| No-snapshot-restore | `raft.no_snapshot_restore_on_start: false` |
| New DNS resolver | Remove `raft.dns` config block (falls back to old `resolveAdvertiseAddr`) |
| TLS rotation | `raft.tls.rotation_period: "0s"` (disables rotation) |

### Recovery Mode Config
Add a new config flag for emergency recovery:
```go
// In domain/config.go
type RaftConfig struct {
    // ...
    // RecoveryMode starts Raft in recovery mode (single-node, ignores peers.json).
    // Only use for manual recovery when quorum is lost.
    RecoveryMode bool `mapstructure:"recovery_mode"`
}

// In config/loader.go
v.SetDefault("raft.recovery_mode", false)
```

---

## Open Questions (for follow-up)

1. **Should the preStop hook use the `/v1/cluster/leave` endpoint or a lower-level Raft remove-peer?** The leave endpoint is cleaner but requires the HTTP server to be running. If the server shuts down before the preStop hook runs, the hook fails. Consider adding a dedicated Raft-only leave command-line tool or having the main process handle SIGTERM directly (not via HTTP).

2. **What is the exact `min_quorum` for a 3-node cluster?** The default of 3 means autopilot will never remove a server from a 3-node cluster (removing one would bring it to 2, below min_quorum). This might be intentional (only remove from 5+ node clusters) or should be 2 for 3-node clusters.

3. **Should `peers.json` recovery be automatic or require a flag?** Automatic recovery could cause split-brain if a node with stale peers.json starts and forms its own cluster. A `--recover-from-peers` CLI flag might be safer.

4. **What happens to in-flight requests during leadership transfer?** Requests sent to the old leader during transfer may fail. The client needs retry logic. Document this behavior.

5. **Should the TLS rotation period be configurable per-environment?** 24h rotation in production, but for development clusters, 30s might be useful for testing. The config key supports this.
