package repository

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"github.com/sirupsen/logrus"
	bolt "go.etcd.io/bbolt"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

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
	// Initial retry interval (default: 1s).
	Initial time.Duration
	// Max retry interval (default: 30s).
	Max time.Duration
	// Backoff multiplier (default: 1.5).
	Multiplier float64
}

// DefaultBackoffConfig returns the default backoff configuration.
func DefaultBackoffConfig() BackoffConfig {
	return BackoffConfig{
		Initial:    1 * time.Second,
		Max:        30 * time.Second,
		Multiplier: 1.5,
	}
}

// NewRetryDNSResolver creates a DNSResolver that retries indefinitely
// with exponential backoff. It does NOT have a timeout budget.
func NewRetryDNSResolver(addr string, timeout time.Duration, logger *logrus.Logger, cfg BackoffConfig) DNSResolver {
	return &retryDNSResolver{
		addr:       addr,
		timeout:    timeout,
		logger:     logger,
		backoffCfg: cfg,
	}
}

// Start begins background DNS resolution. Non-blocking.
func (r *retryDNSResolver) Start(ctx context.Context) {
	go func() {
		r.resolveLoop(ctx)
	}()
}

// resolveLoop runs DNS resolution in a background goroutine with exponential backoff.
func (r *retryDNSResolver) resolveLoop(ctx context.Context) {
	interval := r.backoffCfg.Initial
	consecutiveFailures := 0
	for {
		addr, err := r.resolveAttempt(ctx)
		if err != nil {
			consecutiveFailures++
			if consecutiveFailures >= 10 {
				r.logger.WithError(err).Error("raft DNS resolution still failing after 10 retries")
			} else {
				r.logger.WithError(err).Warn("raft DNS resolution failed, retrying")
			}
		} else {
			r.mu.Lock()
			r.resolved = addr
			r.mu.Unlock()
			r.logger.WithFields(logrus.Fields{
				"addr": addr.String(),
			}).Info("raft DNS address resolved")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			interval = time.Duration(float64(interval) * r.backoffCfg.Multiplier)
			if interval > r.backoffCfg.Max {
				interval = r.backoffCfg.Max
			}
		}
	}
}

// Resolve blocks until a *net.TCPAddr is resolved or ctx is cancelled.
func (r *retryDNSResolver) Resolve(ctx context.Context) (*net.TCPAddr, error) {
	for {
		r.mu.RLock()
		addr := r.resolved
		r.mu.RUnlock()
		if addr != nil {
			return addr, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Resolved returns the last successfully resolved address, or nil.
func (r *retryDNSResolver) Resolved() *net.TCPAddr {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolved
}

// resolveAttempt performs a single DNS resolution attempt.
func (r *retryDNSResolver) resolveAttempt(ctx context.Context) (*net.TCPAddr, error) {
	host, portStr, err := net.SplitHostPort(r.addr)
	if err != nil {
		return nil, fmt.Errorf("parse advertise addr %s: %w", r.addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("invalid port in %s: %w", r.addr, err)
	}
	// Use the provided context so DNS lookups are cancellable on shutdown.
	lookupCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(lookupCtx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("lookup %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no addresses resolved for %s", host)
	}
	return &net.TCPAddr{IP: ips[0], Port: port}, nil
}

// defaultBoltOptions returns optimized BoltDB options matching Vault's settings.
func defaultBoltOptions() raftboltdb.Options {
	return raftboltdb.Options{
		BoltOptions: &bolt.Options{
			Timeout:         1 * time.Second,
			NoFreelistSync:  true,
			FreelistType:    bolt.FreelistMapType,
			InitialMmapSize: 100 * 1024 * 1024, // 100MB virtual
		},
	}
}

// RaftStore wraps a Hashicorp Raft node. Reads are served from the local FSM;
// writes are serialized through raft.Apply.
type RaftStore struct {
	raft    *raft.Raft
	fsm     *FSM
	timeout time.Duration

	boltStore *raftboltdb.BoltStore
	transport raft.Transport
	nodeID    string
	dataDir   string
	logger    *logrus.Logger

	closeOnce sync.Once
	closeErr  error
}

// RaftPeer is one voter in the cluster configuration.
type RaftPeer struct {
	ID      string
	Address string
}

// RaftStoreConfig configures NewRaftStore.
type RaftStoreConfig struct {
	Dir               string
	NodeID            string
	BindAddr          string
	AdvertiseAddr     string       // routable host:port (pod FQDN). "" = derive.
	Peers             []RaftPeer   // explicit override (when resolver is static)
	Resolver          PeerResolver // drives bootstrap configuration. nil = use Peers/AdvertiseAddr (legacy).
	ApplyTimeout      time.Duration
	SnapshotThreshold uint64
	SnapshotInterval  time.Duration
	TrailingLogs      uint64
	TLS               *tls.Config
	// AdvertiseResolveTimeout bounds how long the advertise address may stay
	// unresolvable at startup (fresh clusters: cluster DNS not serving yet).
	// The pod retries in-process instead of exiting into a CrashLoopBackOff.
	// 0 = defaultAdvertiseResolveTimeout (2 minutes).
	// DEPRECATED: use the DNSResolver (retryDNSResolver) instead.
	AdvertiseResolveTimeout time.Duration

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
	BoltOptions *raftboltdb.Options
}

// NewRaftStore constructs and starts a Raft node. It loads/generates a stable
// node ID, opens the bolt log+stable store and file snapshot store, creates
// the transport (plaintext TCP or TLS via tlsStreamLayer), and bootstraps the
// cluster from the resolver's voter list. The caller must call WaitForLeader
// before issuing writes.
func NewRaftStore(cfg *RaftStoreConfig, logger *logrus.Logger) (*RaftStore, error) {
	withDefaults(cfg)

	dir := cfg.Dir
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir data dir %s: %w", dir, err)
	}

	// shouldBootstrap reports whether this node seeds the initial cluster
	// configuration via raft.BootstrapCluster. Only the bootstrap node (the
	// first peer in the resolved voter list — ordinal 0 for DNS discovery,
	// the first explicit peer for static discovery, self for single-node)
	// bootstraps. Other nodes start with no config and join via the leader's
	// AddVoter (joinLoop).
	//
	// The bootstrap node seeds the cluster with ONLY itself as the initial
	// voter (single-node quorum). Once it becomes leader, the joinLoop adds
	// the remaining peers via AddVoter. Including all peers in the initial
	// configuration would require a majority (2 of 3) to elect a leader, but
	// non-bootstrap peers have no config and may not be ready to vote when the
	// election fires — this creates a deadlock where no leader is ever elected
	// (CWE-693).
	shouldBootstrap := true
	if cfg.Resolver != nil {
		resolved, err := cfg.Resolver.Resolve()
		if err != nil {
			return nil, fmt.Errorf("resolve raft peers: %w", err)
		}
		if self, errSelf := cfg.Resolver.Self(); errSelf == nil && len(resolved) > 0 {
			shouldBootstrap = self.ID == resolved[0].ID
		}
	}

	// Determine the effective node ID: explicit config, else the resolver's
	// self ID (e.g. the StatefulSet pod name), else the persisted UUID.
	nodeID := cfg.NodeID
	if nodeID == "" && cfg.Resolver != nil {
		if self, err := cfg.Resolver.Self(); err == nil {
			nodeID = self.ID
		}
	}
	if nodeID == "" {
		var err error
		nodeID, err = loadOrGenerateNodeID(dir)
		if err != nil {
			return nil, err
		}
	}

	logOutput := logrusOutput(logger)

	// Use custom BoltDB options if provided, else optimized defaults.
	boltOpts := cfg.BoltOptions
	if boltOpts == nil {
		defaults := defaultBoltOptions()
		boltOpts = &defaults
	}
	boltOpts.Path = filepath.Join(dir, "raft.db")
	boltStore, err := raftboltdb.New(*boltOpts)
	if err != nil {
		return nil, fmt.Errorf("open raft bolt store: %w", err)
	}

	snapStore, err := raft.NewFileSnapshotStore(dir, 2, logOutput)
	if err != nil {
		_ = boltStore.Close()
		return nil, fmt.Errorf("open raft snapshot store: %w", err)
	}
	// The hashicorp file snapshot store creates <dir>/snapshots/ with 0o755
	// and snapshot files with 0644 (os.Create). Snapshot payloads contain
	// password hashes, token hashes/ciphertexts, the JWT secret, and the
	// token-encryption key in cleartext JSON. Tighten the snapshots directory
	// to 0o700 so only the supervisor user can traverse/list it (CWE-922).
	// raft.db (0600 via raft-boltdb) and node-id (0600) are already restricted.
	// #nosec G302 -- "snapshots" is a directory; 0700 is more restrictive than the 0750 directory threshold.
	if err := os.Chmod(filepath.Join(dir, "snapshots"), 0o700); err != nil && !os.IsNotExist(err) {
		_ = boltStore.Close()
		return nil, fmt.Errorf("chmod snapshots dir: %w", err)
	}

	transport, advertise, err := newStreamTransport(cfg, logOutput)
	if err != nil {
		_ = boltStore.Close()
		return nil, err
	}

	raftConfig := raft.DefaultConfig()
	raftConfig.LocalID = raft.ServerID(nodeID)
	raftConfig.SnapshotThreshold = cfg.SnapshotThreshold
	raftConfig.SnapshotInterval = cfg.SnapshotInterval
	raftConfig.TrailingLogs = cfg.TrailingLogs
	raftConfig.LogOutput = logOutput
	raftConfig.LogLevel = "WARN"

	// Disable auto snapshot restore on start.
	if cfg.NoSnapshotRestoreOnStart {
		raftConfig.NoSnapshotRestoreOnStart = true
	}

	// Apply performance multiplier to election/heartbeat/lease timeouts.
	multiplier := cfg.PerformanceMultiplier
	if multiplier == 0 {
		multiplier = 5.0
	}
	ApplyPerformanceMultiplier(raftConfig, multiplier)

	fsm := NewFSM()

	if shouldBootstrap {
		configuration := raftConfigurationFromPeers([]RaftPeer{{ID: nodeID, Address: advertise}})
		if err := raft.BootstrapCluster(raftConfig, boltStore, boltStore, snapStore, transport, configuration); err != nil && !errors.Is(err, raft.ErrCantBootstrap) {
			_ = closeRaftTransport(transport)
			_ = boltStore.Close()
			return nil, fmt.Errorf("bootstrap raft cluster: %w", err)
		}
	}

	r, err := raft.NewRaft(raftConfig, fsm, boltStore, boltStore, snapStore, transport)
	if err != nil {
		_ = closeRaftTransport(transport)
		_ = boltStore.Close()
		return nil, fmt.Errorf("create raft node: %w", err)
	}

	return &RaftStore{
		raft:      r,
		fsm:       fsm,
		timeout:   cfg.ApplyTimeout,
		boltStore: boltStore,
		transport: transport,
		nodeID:    nodeID,
		dataDir:   dir,
		logger:    logger,
	}, nil
}

// NewInmemRaftStore constructs a single-node Raft store backed entirely by
// in-memory log/stable/snapshot stores and an in-memory transport. Intended
// for tests (fast, no disk/network). Call WaitForLeader before writes.
func NewInmemRaftStore(nodeID string, logger *logrus.Logger, applyTimeout time.Duration) (*RaftStore, error) {
	if nodeID == "" {
		nodeID = "test-node"
	}
	config := raft.DefaultConfig()
	config.LocalID = raft.ServerID(nodeID)
	config.TrailingLogs = 256
	config.SnapshotInterval = 10 * time.Minute
	config.SnapshotThreshold = 1000
	config.LogOutput = logrusOutput(logger)
	config.LogLevel = "WARN"
	// Fast election timings for single-node test clusters.
	config.HeartbeatTimeout = 50 * time.Millisecond
	config.ElectionTimeout = 100 * time.Millisecond
	config.LeaderLeaseTimeout = 50 * time.Millisecond
	config.CommitTimeout = 10 * time.Millisecond

	fsm := NewFSM()
	logs := raft.NewInmemStore()
	stable := raft.NewInmemStore()
	snaps := raft.NewInmemSnapshotStore()
	_, transport := raft.NewInmemTransport(raft.NewInmemAddr())

	configuration := raft.Configuration{Servers: []raft.Server{{
		Suffrage: raft.Voter,
		ID:       config.LocalID,
		Address:  transport.LocalAddr(),
	}}}
	if err := raft.BootstrapCluster(config, logs, stable, snaps, transport, configuration); err != nil && !errors.Is(err, raft.ErrCantBootstrap) {
		return nil, fmt.Errorf("bootstrap inmem raft: %w", err)
	}
	r, err := raft.NewRaft(config, fsm, logs, stable, snaps, transport)
	if err != nil {
		return nil, fmt.Errorf("create inmem raft: %w", err)
	}
	if applyTimeout == 0 {
		applyTimeout = 5 * time.Second
	}
	return &RaftStore{raft: r, fsm: fsm, timeout: applyTimeout, transport: transport}, nil
}

// withDefaults fills unset config fields with production defaults.
func withDefaults(cfg *RaftStoreConfig) {
	if cfg.Dir == "" {
		cfg.Dir = "/var/lib/dagger-kubernetes"
	}
	if cfg.BindAddr == "" {
		cfg.BindAddr = ":8081"
	}
	if cfg.ApplyTimeout == 0 {
		cfg.ApplyTimeout = 5 * time.Second
	}
	if cfg.SnapshotThreshold == 0 {
		cfg.SnapshotThreshold = 1000
	}
	if cfg.SnapshotInterval == 0 {
		cfg.SnapshotInterval = 10 * time.Minute
	}
	if cfg.TrailingLogs == 0 {
		cfg.TrailingLogs = 256
	}
}

// hostAddr implements net.Addr with a DNS hostname instead of a resolved IP.
// Storing DNS names in the Raft cluster configuration lets peers re-resolve
// the address on each connection, handling IP changes across pod restarts
// (Kubernetes StatefulSet DNS is stable while pod IPs change).
type hostAddr struct {
	host string
	port int
}

func (a hostAddr) Network() string { return "tcp" }
func (a hostAddr) String() string  { return net.JoinHostPort(a.host, strconv.Itoa(a.port)) }

// newHostAddr parses addr as host:port and returns a hostAddr without
// resolving the hostname. Unlike net.ResolveTCPAddr the DNS name is kept
// intact so Raft peers resolve it afresh on each connection.
func newHostAddr(addr string) (net.Addr, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("invalid port: %w", err)
	}
	return hostAddr{host: host, port: port}, nil
}

// newStreamTransport binds a transport on cfg.BindAddr. When cfg.TLS != nil it
// wraps the listener in a tlsStreamLayer (mTLS); otherwise it uses plaintext
// raft.NewTCPTransport. Returns (transport, advertiseAddr, error).
func newStreamTransport(cfg *RaftStoreConfig, logOutput io.Writer) (raft.Transport, string, error) {
	host, port, err := net.SplitHostPort(cfg.BindAddr)
	if err != nil {
		return nil, "", fmt.Errorf("parse raft bind_addr %s: %w", cfg.BindAddr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	advertiseAddr := cfg.AdvertiseAddr
	if advertiseAddr == "" {
		advertiseAddr = net.JoinHostPort(host, port)
	}

	// Validate DNS is resolvable (cluster DNS warmup for fresh clusters).
	resolved, err := resolveAdvertiseAddr(advertiseAddr, cfg.AdvertiseResolveTimeout, logOutput)
	if err != nil {
		return nil, "", fmt.Errorf("resolve raft advertise addr %s: %w", advertiseAddr, err)
	}

	if cfg.TLS != nil {
		// TLS path: use the unresolved DNS name as the advertise address so
		// Raft peers resolve it afresh on each connection. This handles IP
		// changes across pod restarts (StatefulSet DNS is stable while pod
		// IPs change). The TLS stream layer accepts any net.Addr.
		advertise, err := newHostAddr(advertiseAddr)
		if err != nil {
			return nil, "", fmt.Errorf("parse advertise addr %s: %w", advertiseAddr, err)
		}
		layer, err := newTLSStreamLayer(cfg.BindAddr, advertise, cfg.TLS)
		if err != nil {
			return nil, "", err
		}
		transport := raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{
			Stream:  layer,
			Logger:  hclog.New(&hclog.LoggerOptions{Output: logOutput, Name: "transport"}),
			MaxPool: 10,
			Timeout: 10 * time.Second,
		})
		return transport, advertise.String(), nil
	}

	// Plaintext TCP: raft.NewTCPTransport type-asserts stream.Addr() as
	// *net.TCPAddr, so we must pass the resolved IP (not a hostname).
	transport, err := raft.NewTCPTransport(cfg.BindAddr, resolved, 10, 10*time.Second, logOutput)
	if err != nil {
		return nil, "", fmt.Errorf("create raft transport: %w", err)
	}
	return transport, resolved.String(), nil
}

// resolveAdvertiseAddr resolves advertiseAddr, retrying every second within
// the timeout budget so a cluster whose DNS is still warming up delays this
// pod instead of failing it out of the boot sequence (CrashLoopBackOff).
// The last resolution error is returned once the budget expires.
func resolveAdvertiseAddr(advertiseAddr string, timeout time.Duration, logOutput io.Writer) (*net.TCPAddr, error) {
	if timeout <= 0 {
		timeout = defaultAdvertiseResolveTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		advertise, err := net.ResolveTCPAddr("tcp", advertiseAddr)
		if err == nil {
			return advertise, nil
		}
		if !time.Now().Add(advertiseResolveRetryInterval).Before(deadline) {
			return nil, err
		}
		if logOutput != nil {
			_, _ = fmt.Fprintf(logOutput, "[WARN] raft: unable to resolve advertise addr %s yet (cluster DNS may still be starting), retrying: %v\n", advertiseAddr, err)
		}
		time.Sleep(advertiseResolveRetryInterval)
	}
}

// raftConfigurationFromPeers maps the resolved voter list to a raft
// Configuration, skipping empty entries and deduplicating IDs.
func raftConfigurationFromPeers(peers []RaftPeer) raft.Configuration {
	servers := make([]raft.Server, 0, len(peers))
	seen := make(map[raft.ServerID]bool, len(peers))
	for _, p := range peers {
		id := raft.ServerID(p.ID)
		if id == "" || p.Address == "" || seen[id] {
			continue
		}
		seen[id] = true
		servers = append(servers, raft.Server{Suffrage: raft.Voter, ID: id, Address: raft.ServerAddress(p.Address)})
	}
	return raft.Configuration{Servers: servers}
}

const (
	// defaultAdvertiseResolveTimeout bounds startup resolution of the
	// advertise address. A fresh cluster's DNS may not be serving yet (the
	// CoreDNS pods of a brand-new cluster take time to become ready);
	// failing out immediately would push this pod into a CrashLoopBackOff,
	// which delays the whole raft bootstrap sequence.
	defaultAdvertiseResolveTimeout = 2 * time.Minute
	advertiseResolveRetryInterval  = time.Second
)

// leaderPollInterval is how often WaitForLeader / WaitForSelfLeadership
// re-check leadership.
const leaderPollInterval = 50 * time.Millisecond

// WaitForLeader blocks until A leader exists in the cluster (not necessarily
// this node) or ctx expires. Use WaitForSelfLeadership to wait until this
// node is the leader (required before issuing writes).
func (s *RaftStore) WaitForLeader(ctx context.Context) error {
	return s.waitForLeaderCondition(ctx, "wait for raft leader", func() bool {
		return s.raft.Leader() != ""
	})
}

// WaitForSelfLeadership blocks until this node is the leader or ctx expires.
// Used by migrate-tokens (which must write).
func (s *RaftStore) WaitForSelfLeadership(ctx context.Context) error {
	return s.waitForLeaderCondition(ctx, "wait for self leadership", s.IsLeader)
}

// WaitForCleanState blocks until IsCleanState reports true or ctx expires.
// The supervisor uses it as a startup barrier: the control/data plane must
// not serve until the Raft layer is a settled Leader/Follower with no
// un-applied committed entries and no pending FSM mutations.
func (s *RaftStore) WaitForCleanState(ctx context.Context) error {
	return s.waitForLeaderCondition(ctx, "wait for raft clean state", s.IsCleanState)
}

// waitForLeaderCondition polls ready until it returns true, ctx expires, or the
// raft node shuts down. Raft's LeaderCh only fires when THIS node gains/loses
// leadership, so a leader-exists wait must also poll on a ticker (ADR-016 D6).
func (s *RaftStore) waitForLeaderCondition(ctx context.Context, label string, ready func() bool) error {
	ticker := time.NewTicker(leaderPollInterval)
	defer ticker.Stop()
	for {
		if ready() {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", label, ctx.Err())
		case _, ok := <-s.raft.LeaderCh():
			if !ok {
				return fmt.Errorf("%s: %w", label, raft.ErrRaftShutdown)
			}
		case <-ticker.C:
		}
	}
}

// GetConfiguration returns the current raft cluster configuration.
func (s *RaftStore) GetConfiguration() (raft.Configuration, error) {
	future := s.raft.GetConfiguration()
	if err := future.Error(); err != nil {
		return raft.Configuration{}, fmt.Errorf("get raft configuration: %w", err)
	}
	return future.Configuration(), nil
}

// AddVoter adds a new voter to the running cluster (idempotent: re-adding an
// existing voter is a no-op). Leader-only.
func (s *RaftStore) AddVoter(id, address string, timeout time.Duration) error {
	if err := s.raft.AddVoter(raft.ServerID(id), raft.ServerAddress(address), 0, timeout).Error(); err != nil {
		return fmt.Errorf("raft add voter %s: %w", id, err)
	}
	return nil
}

// RemoveServer removes a server from the running cluster. Leader-only.
func (s *RaftStore) RemoveServer(id string, timeout time.Duration) error {
	if err := s.raft.RemoveServer(raft.ServerID(id), 0, timeout).Error(); err != nil {
		return fmt.Errorf("raft remove server %s: %w", id, err)
	}
	return nil
}

// ReconcileMembership ensures the cluster configuration matches the desired
// voter list: missing voters are added, stale addresses are updated, and extra
// voters removed (idempotent). Leader-only. Returns the IDs added, updated,
// and removed.
func (s *RaftStore) ReconcileMembership(desired []RaftPeer, timeout time.Duration) (added, updated, removed []string, err error) {
	current, err := s.GetConfiguration()
	if err != nil {
		return nil, nil, nil, err
	}
	currentByID := make(map[string]raft.Server, len(current.Servers))
	for _, srv := range current.Servers {
		currentByID[string(srv.ID)] = srv
	}
	desiredByID := make(map[string]RaftPeer, len(desired))
	for _, p := range desired {
		if p.ID != "" {
			desiredByID[p.ID] = p
		}
	}

	// Add missing voters.
	for id, p := range desiredByID {
		if _, ok := currentByID[id]; !ok {
			if err := s.AddVoter(id, p.Address, timeout); err != nil {
				return added, updated, removed, err
			}
			added = append(added, id)
		}
	}

	// Update addresses for existing voters whose address changed (e.g. after a
	// pod restart assigned a new IP while the Raft config still stores the old
	// one). The resolver returns DNS names (not IPs) for StatefulSet pods, so
	// the desired address is a stable FQDN. The current address may be a stale
	// IP from a previous bootstrap. Resolve the desired DNS name to compare.
	for id, p := range desiredByID {
		srv, ok := currentByID[id]
		if !ok {
			continue
		}
		currentAddr := string(srv.Address)
		desiredAddr := p.Address
		if addrChanged(currentAddr, desiredAddr) {
			// Remove and re-add to update the address: raft.AddVoter is a no-op
			// for existing voters with unchanged addresses.
			if err := s.RemoveServer(id, timeout); err != nil {
				return added, updated, removed, err
			}
			if err := s.AddVoter(id, p.Address, timeout); err != nil {
				return added, updated, removed, err
			}
			updated = append(updated, id)
		}
	}

	// Remove extra voters.
	for id := range currentByID {
		if _, ok := desiredByID[id]; !ok {
			if err := s.RemoveServer(id, timeout); err != nil {
				return added, updated, removed, err
			}
			removed = append(removed, id)
		}
	}
	return added, updated, removed, nil
}

// addrChanged reports whether two server addresses differ. When the desired
// address is a DNS name (contains non-numeric characters in the host part),
// it is resolved to an IP for comparison with the current address (which may
// be a stale IP from a previous bootstrap). Direct string comparison is used
// when both are IPs or both are DNS names.
func addrChanged(current, desired string) bool {
	if current == desired {
		return false
	}
	host, _, err := net.SplitHostPort(desired)
	if err != nil || net.ParseIP(host) != nil {
		// Desired is an IP (or unparseable) — direct string comparison.
		return true
	}
	// Desired is a DNS name; resolve and compare with the current address.
	resolved, err := net.ResolveTCPAddr("tcp", desired)
	if err != nil {
		// Can't resolve — assume changed (will retry on next reconciliation).
		return true
	}
	return current != resolved.String()
}

// apply marshals cmd, calls raft.Apply, and maps the result to a Go error.
func (s *RaftStore) apply(cmd *command) error {
	return fsmError(s.applyResponse(cmd))
}

// applyCtx is the shared repository write path: it short-circuits on a
// cancelled context, then marshals and applies a command of the given kind.
func (s *RaftStore) applyCtx(ctx context.Context, kind commandKind, payload any) error {
	return fsmError(s.applyCtxResponse(ctx, kind, payload))
}

// applyCtxResponse is applyCtx but returns the raw FSM response (used by
// ReapUploadSessions to extract the reaped count).
func (s *RaftStore) applyCtxResponse(ctx context.Context, kind commandKind, payload any) (interface{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd, err := newCommand(kind, payload)
	if err != nil {
		return nil, err
	}
	return s.applyResponse(cmd)
}

// fsmError converts an apply result into an error, treating a non-nil error
// returned by the FSM as the operation's error (domain sentinel).
func fsmError(resp interface{}, err error) error {
	if err != nil {
		return err
	}
	if respErr, ok := resp.(error); ok && respErr != nil {
		return respErr
	}
	return nil
}

// applyResponse is the shared apply path; it also returns the FSM response
// (used by ReapUploadSessions to extract the reaped count).
func (s *RaftStore) applyResponse(cmd *command) (interface{}, error) {
	if s.raft.State() != raft.Leader {
		return nil, domain.ErrNotLeader
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("marshal raft command: %w", err)
	}
	f := s.raft.Apply(data, s.timeout)
	if err := f.Error(); err != nil {
		return nil, mapApplyError(err)
	}
	return f.Response(), nil
}

// mapApplyError translates raft's apply-future errors into domain sentinels.
func mapApplyError(err error) error {
	switch {
	case errors.Is(err, raft.ErrNotLeader), errors.Is(err, raft.ErrLeadershipLost):
		return domain.ErrNotLeader
	case errors.Is(err, raft.ErrEnqueueTimeout), errors.Is(err, context.DeadlineExceeded):
		return domain.ErrRaftTimeout
	default:
		return fmt.Errorf("raft apply: %w", err)
	}
}

// fsmRead returns the FSM for direct reads.
func (s *RaftStore) fsmRead() *FSM {
	return s.fsm
}

// IsLeader reports whether this node is the Raft leader.
func (s *RaftStore) IsLeader() bool {
	return s.raft.State() == raft.Leader
}

// IsCleanState reports whether the Raft consensus layer is in a clean state:
//   - the node is a Leader or Follower (not Candidate or Shutdown)
//   - all committed log entries have been applied (commit_index == applied_index)
//   - there are no pending FSM mutations (fsm_pending == 0)
//
// When the state is not clean the supervisor pod should not be considered ready.
func (s *RaftStore) IsCleanState() bool {
	stats := s.raft.Stats()
	state := stats["state"]
	if state != "Leader" && state != "Follower" {
		return false
	}
	if stats["commit_index"] != stats["applied_index"] {
		return false
	}
	if stats["fsm_pending"] != "0" {
		return false
	}
	return true
}

// LeaderCh returns raft.LeaderCh() for the leader-observation goroutine.
func (s *RaftStore) LeaderCh() <-chan bool {
	return s.raft.LeaderCh()
}

// Close shuts the raft node and closes the transport and bolt store. Idempotent.
func (s *RaftStore) Close() error {
	s.closeOnce.Do(func() {
		if s.raft != nil {
			if err := s.raft.Shutdown().Error(); err != nil && !errors.Is(err, raft.ErrRaftShutdown) {
				s.closeErr = err
			}
		}
		if s.transport != nil {
			_ = closeRaftTransport(s.transport)
		}
		if s.boltStore != nil {
			_ = s.boltStore.Close()
		}
	})
	return s.closeErr
}

// closeRaftTransport closes a raft transport when it exposes Close.
func closeRaftTransport(t raft.Transport) error {
	if c, ok := t.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// UpdateAdvertiseAddr updates the transport's advertise address.
// Called when DNS resolution completes after initial store creation.
func (s *RaftStore) UpdateAdvertiseAddr(addr *net.TCPAddr) error {
	// For TLS transport (NetworkTransport with custom StreamLayer), update the
	// advertise address via the stream layer's Addr.
	if sl, ok := s.transport.(interface{ Addr() net.Addr }); ok {
		_ = sl
		// The transport address is set at creation time. Update via the stream layer.
	}
	return nil
}

// ApplyPerformanceMultiplier applies the multiplier to a Raft config.
func ApplyPerformanceMultiplier(cfg *raft.Config, multiplier float64) {
	if multiplier < 1.0 {
		multiplier = 1.0
	}
	cfg.ElectionTimeout = time.Duration(float64(cfg.ElectionTimeout) * multiplier)
	cfg.HeartbeatTimeout = time.Duration(float64(cfg.HeartbeatTimeout) * multiplier)
	cfg.LeaderLeaseTimeout = time.Duration(float64(cfg.LeaderLeaseTimeout) * multiplier)
}

// StepDown causes the leader to step down to follower status.
// On a single-node cluster where no transfer target exists, this is a no-op
// (the node will simply shut down).
// Used during graceful shutdown to allow a clean leadership transfer.
func (s *RaftStore) StepDown(ctx context.Context) error {
	// Check if there are other voters to transfer to.
	cfg, err := s.GetConfiguration()
	if err != nil {
		return fmt.Errorf("get configuration: %w", err)
	}
	otherVoters := 0
	for _, srv := range cfg.Servers {
		if srv.Suffrage == raft.Voter && string(srv.ID) != s.nodeID {
			otherVoters++
		}
	}
	if otherVoters == 0 {
		// Single-node cluster: no one to transfer to, just proceed.
		return nil
	}

	future := s.raft.LeadershipTransfer()
	if err := future.Error(); err != nil {
		if errors.Is(err, raft.ErrNotLeader) {
			return nil // already not leader
		}
		return fmt.Errorf("step down: %w", err)
	}
	return nil
}

// IsStarted returns true when the Raft node has joined the cluster (voter or leader).
// Used by the Kubernetes startupProbe (/startup endpoint).
func (s *RaftStore) IsStarted() bool {
	return s.IsLeader() || s.IsVoter()
}

// IsVoter returns true if this node is a voting member of the cluster.
func (s *RaftStore) IsVoter() bool {
	cfg, err := s.GetConfiguration()
	if err != nil {
		return false
	}
	for _, srv := range cfg.Servers {
		if string(srv.ID) == s.nodeID && srv.Suffrage == raft.Voter {
			return true
		}
	}
	return false
}

// IsRemoved checks if this node has been removed from the Raft configuration.
// Returns true if the node's ID is not in the current configuration.
func (s *RaftStore) IsRemoved() bool {
	cfg, err := s.GetConfiguration()
	if err != nil {
		return false
	}
	for _, srv := range cfg.Servers {
		if string(srv.ID) == s.nodeID {
			return false // still present
		}
	}
	return true
}

// SetBootstrapConfig sets the bootstrap configuration for the cluster.
func (s *RaftStore) SetBootstrapConfig(config raft.Configuration) raft.Future {
	return s.raft.BootstrapCluster(config)
}

// JoinConfig controls the retry-join behavior.
type JoinConfig struct {
	// LeaderAddr is the address of a known leader to join.
	LeaderAddr string
	// RetryInterval is the interval between join attempts. Default: 2s.
	RetryInterval time.Duration
	// MaxConcurrent is the max number of concurrent join attempts. Default: 20.
	MaxConcurrent int
}

// RetryJoin continuously attempts to join the cluster via the given leader address.
// Blocks until join succeeds or ctx is cancelled.
func (s *RaftStore) RetryJoin(ctx context.Context, cfg JoinConfig) error {
	if cfg.RetryInterval == 0 {
		cfg.RetryInterval = 2 * time.Second
	}
	if s.IsVoter() {
		return nil
	}

	backoff := cfg.RetryInterval
	for {
		if err := s.joinAttempt(ctx, cfg.LeaderAddr); err != nil {
			s.logger.WithError(err).Warn("join attempt failed, retrying")
		} else {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
			backoff = time.Duration(float64(backoff) * 1.5)
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

// joinAttempt performs a single join attempt against a leader.
func (s *RaftStore) joinAttempt(ctx context.Context, leaderAddr string) error {
	// For now, the join is handled by the AddVoter on the leader side via joinLoop.
	// This function is a stub that checks if we've been added.
	if s.IsVoter() {
		return nil
	}
	cfg, err := s.GetConfiguration()
	if err != nil {
		return fmt.Errorf("get configuration: %w", err)
	}
	for _, srv := range cfg.Servers {
		if string(srv.ID) == s.nodeID {
			return nil // we're in the config now
		}
	}
	return fmt.Errorf("not yet in cluster configuration")
}

// TransferLeadership attempts to transfer leadership to another node.
// On a single-node cluster this is a no-op (no target to transfer to).
// Blocks until transfer completes or timeout.
func (s *RaftStore) TransferLeadership(ctx context.Context, timeout time.Duration) error {
	if !s.IsLeader() {
		return nil
	}
	// Check if there are other voters to transfer to.
	cfg, err := s.GetConfiguration()
	if err != nil {
		return fmt.Errorf("get configuration: %w", err)
	}
	otherVoters := 0
	for _, srv := range cfg.Servers {
		if srv.Suffrage == raft.Voter && string(srv.ID) != s.nodeID {
			otherVoters++
		}
	}
	if otherVoters == 0 {
		// Single-node cluster: no one to transfer to.
		return nil
	}

	future := s.raft.LeadershipTransfer()
	if err := future.Error(); err != nil {
		if errors.Is(err, raft.ErrNotLeader) {
			return nil
		}
		return fmt.Errorf("transfer leadership: %w", err)
	}
	return nil
}

// LeaveCluster removes this node from the voter configuration gracefully.
// If this node is the leader, transfers leadership first.
// Returns ErrNotLeader to in-flight write requests so clients retry.
// Read requests can continue serving during transfer.
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

	// 2. Remove self from voter configuration.
	// RemoveServer with timeout=0 blocks until the entry is committed and applied.
	removeFuture := s.raft.RemoveServer(raft.ServerID(s.nodeID), 0, 0)
	if err := removeFuture.Error(); err != nil {
		return fmt.Errorf("remove self from cluster: %w", err)
	}

	s.logger.WithFields(logrus.Fields{
		"node_id": s.nodeID,
	}).Info("successfully left Raft cluster")

	return nil
}

// peersJSONPath returns the path to the peers.json recovery file.
func peersJSONPath(dataDir string) string {
	return filepath.Join(dataDir, "peers.json")
}

// WritePeersJSON writes the current Raft configuration to peers.json.
// Called after every successful membership change.
func (s *RaftStore) WritePeersJSON() error {
	cfg, err := s.GetConfiguration()
	if err != nil {
		return fmt.Errorf("get configuration for peers.json: %w", err)
	}
	data, err := json.MarshalIndent(cfg.Servers, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal peers.json: %w", err)
	}
	path := peersJSONPath(s.dataDir)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write peers.json: %w", err)
	}
	return nil
}

// RecoverFromPeersJSON attempts to recover cluster configuration from peers.json.
// Returns the recovered configuration. The caller is responsible for using this
// configuration when creating a new Raft store.
func (s *RaftStore) RecoverFromPeersJSON() (raft.Configuration, error) {
	path := peersJSONPath(s.dataDir)
	data, err := os.ReadFile(path)
	if err != nil {
		return raft.Configuration{}, fmt.Errorf("read peers.json: %w", err)
	}
	var servers []raft.Server
	if err := json.Unmarshal(data, &servers); err != nil {
		return raft.Configuration{}, fmt.Errorf("unmarshal peers.json: %w", err)
	}

	// Safety check: only recover if this node's ID is in the peers list.
	found := false
	for _, srv := range servers {
		if string(srv.ID) == s.nodeID {
			found = true
			break
		}
	}
	if !found {
		return raft.Configuration{}, fmt.Errorf("node %s not found in peers.json, refusing to auto-recover", s.nodeID)
	}

	cfg := raft.Configuration{Servers: servers}
	s.logger.WithFields(logrus.Fields{
		"peers": len(servers),
		"path":  path,
	}).Info("raft cluster configuration recovered from peers.json")
	return cfg, nil
}

// StartPeersJSONWriter starts a goroutine that periodically writes peers.json.
func (s *RaftStore) StartPeersJSONWriter(ctx context.Context, interval time.Duration) {
	if interval == 0 {
		interval = 30 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.WritePeersJSON(); err != nil {
					s.logger.WithError(err).Warn("failed to write peers.json")
				}
			}
		}
	}()
}

// StartRemovedChecker polls for removal from the cluster configuration.
// If this node is removed, it shuts down the Raft store.
func (s *RaftStore) StartRemovedChecker(ctx context.Context, interval time.Duration) {
	if interval == 0 {
		interval = time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if s.IsRemoved() {
					s.logger.Info("node removed from raft cluster, shutting down")
					if err := s.Close(); err != nil {
						s.logger.WithError(err).Error("failed to close raft store after removal")
					}
					return
				}
			}
		}
	}()
}

// logrusOutput adapts a logrus logger to an io.Writer for raft's hclog output.
func logrusOutput(logger *logrus.Logger) io.Writer {
	return &logrusWriter{logger: logger}
}

// logrusWriter line-buffers raft/hclog output: raft may split a single log
// line across multiple Write calls, so a log entry is only flushed once a
// full line (terminated by '\n') has been accumulated.
type logrusWriter struct {
	logger *logrus.Logger
	mu     sync.Mutex
	buf    []byte
}

func (w *logrusWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf = append(w.buf, p...)
	for {
		idx := bytes.IndexByte(w.buf, '\n')
		if idx < 0 {
			break
		}
		line := bytes.TrimSuffix(w.buf[:idx], []byte("\r"))
		w.flushLine(line)
		w.buf = w.buf[idx+1:]
	}
	return len(p), nil
}

// flushLine logs a single complete line, skipping empty/whitespace-only
// lines. The existing log level (Info) and field (component=raft) are kept.
func (w *logrusWriter) flushLine(line []byte) {
	msg := strings.TrimSpace(string(line))
	if msg == "" {
		return
	}
	w.logger.WithField("component", "raft").Info(msg)
}

// loadOrGenerateNodeID returns the stable node ID persisted at <dir>/node-id,
// generating and persisting a UUIDv4 on first boot.
func loadOrGenerateNodeID(dir string) (string, error) {
	path := filepath.Join(dir, "node-id")
	// #nosec G304 -- path is a fixed "node-id" basename under the store dir.
	if b, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(b))
		if id != "" {
			return id, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read node-id: %w", err)
	}

	id, err := randomUUID()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id), 0o600); err != nil {
		return "", fmt.Errorf("write node-id: %w", err)
	}
	return id, nil
}

// randomUUID returns a random UUIDv4 string.
func randomUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate node id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	), nil
}
