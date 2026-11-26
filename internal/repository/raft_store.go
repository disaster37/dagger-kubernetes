package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	goraft "github.com/disaster37/go-raft"
	raftconfig "github.com/disaster37/go-raft/config"
	"github.com/disaster37/go-raft/discovery"
	raftk8s "github.com/disaster37/go-raft/k8s"
	"github.com/disaster37/go-raft/keyring"
	raftobserv "github.com/disaster37/go-raft/observ"
	"github.com/disaster37/go-raft/pki"
	"github.com/disaster37/go-raft/storage"
	rafttransport "github.com/disaster37/go-raft/transport"
	"github.com/hashicorp/raft"
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// RaftStore wraps a go-raft Node. Reads are served from the local FSM; writes
// are serialized through the node's Apply. The node owns storage, transport,
// discovery, TLS, autopilot, and in-band keyring rotation.
type RaftStore struct {
	node   *goraft.Node
	fsm    *FSM
	logger *logrus.Logger
	sealer storage.Sealer
}

// RaftStoreConfig configures NewRaftStore. It mirrors the Raft-relevant subset
// of the supervisor configuration and is mapped onto go-raft's config via
// raftconfig.FromDaggerKubernetes.
type RaftStoreConfig struct {
	Dir                      string
	NodeID                   string
	BindAddr                 string
	AdvertiseAddr            string
	Peers                    []discovery.Peer
	Resolver                 discovery.PeerResolver
	Replicas                 int
	StatefulSetName          string
	HeadlessService          string
	Namespace                string
	ClusterDomain            string
	RaftPort                 int
	ApplyTimeout             time.Duration
	LeaderWaitTimeout        time.Duration
	SnapshotThreshold        uint64
	SnapshotInterval         time.Duration
	TrailingLogs             uint64
	PerformanceMultiplier    float64
	RaftLogCacheSize         int
	TransportMaxPool         int
	NoSnapshotRestoreOnStart bool
	RecoveryMode             bool

	// SessionSink receives replicated session-lease state (domain.SessionStateSink,
	// usually the pod-local session store). Set before NewRaftStore so log
	// replay restores sessions into the local store on every pod.
	SessionSink domain.SessionStateSink

	// OrgQuotaDefaults are the cluster-wide org quota defaults mirrored into
	// the FSM so hard-cap guards are deterministic on every peer.
	OrgQuotaDefaults domain.OrgQuotas

	// TLS configures the Raft transport mTLS + in-band keyring.
	TLSEnabled             bool
	TLSDir                 string
	TLSValidity            time.Duration
	TLSOrganization        string
	CACertPath             string
	CertPath               string
	KeyPath                string
	CASecret               string
	CABootstrap            bool
	ClientAuth             bool
	SecretPollTimeout      time.Duration
	KeyringRotationEnabled bool
	RotationPeriod         time.Duration
	RotationCheckInterval  time.Duration
	SafetyMargin           time.Duration
	ReloadPollInterval     time.Duration

	// EncryptionKey enables at-rest sealing of the replicated keyring private
	// keys (HKDF-SHA256-derived AES-256-GCM).
	EncryptionKey string

	// Autopilot configuration.
	AutopilotEnabled   bool
	CleanupDeadServers bool
	MinQuorum          int

	// Clientset enables Kubernetes Secret CA bootstrap, stale-PVC generation
	// fencing, and StatefulSet replica-count authorization.
	Clientset kubernetes.Interface

	// BootstrapProvider overrides the CA bootstrap provider (tests / custom
	// deployments). When nil and a clientset + CA Secret are configured, a
	// Kubernetes Secret provider is used.
	BootstrapProvider pki.BootstrapProvider

	// AllowInsecureMultiNode permits a multi-node cluster without TLS (or with
	// a loopback advertise address). It is implied when TLS is disabled.
	AllowInsecureMultiNode bool

	// MetaHook, when set, is invoked for every replicated meta key/value on
	// Apply and Restore (used to observe meta["minting_tls_keyring"]).
	MetaHook func(key, value string)

	// Test hooks.
	StorageBackend       raftconfig.StorageBackendType
	CustomListener       net.Listener
	JoinInterval         time.Duration
	JoinReconcileTimeout time.Duration
	Metrics              raftobserv.MetricsCollector
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
	if cfg.LeaderWaitTimeout == 0 {
		cfg.LeaderWaitTimeout = 120 * time.Second
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
	if cfg.TransportMaxPool == 0 {
		cfg.TransportMaxPool = 1
	}
	if cfg.Replicas == 0 {
		cfg.Replicas = 1
	}
	if cfg.TLSValidity == 0 {
		cfg.TLSValidity = 8760 * time.Hour
	}
	if cfg.TLSOrganization == "" {
		cfg.TLSOrganization = "dagger-kubernetes-raft"
	}
	if cfg.RotationPeriod == 0 {
		cfg.RotationPeriod = 24 * time.Hour
	}
	if cfg.RotationCheckInterval == 0 {
		cfg.RotationCheckInterval = 30 * time.Second
	}
	if cfg.SafetyMargin == 0 {
		cfg.SafetyMargin = 7 * 24 * time.Hour
	}
}

// NewRaftStore constructs and starts a go-raft Node. It maps the supervisor
// config onto go-raft's config, wires the Kubernetes Secret CA bootstrap /
// generation fencing / replica authorization when a clientset is available,
// derives the at-rest sealer, and starts the node (storage, transport, TLS,
// discovery, autopilot, keyring rotation, join loop). The caller must call
// WaitForLeader before issuing writes.
func NewRaftStore(cfg *RaftStoreConfig, logger *logrus.Logger) (*RaftStore, error) {
	withDefaults(cfg)

	rcfg := raftconfig.FromDaggerKubernetes(raftconfig.DaggerKubernetesConfig{
		Dir:                   cfg.Dir,
		NodeID:                cfg.NodeID,
		BindAddr:              cfg.BindAddr,
		AdvertiseAddr:         cfg.AdvertiseAddr,
		Peers:                 toPeerConfigs(cfg.Peers),
		Replicas:              cfg.Replicas,
		StatefulSetName:       cfg.StatefulSetName,
		HeadlessService:       cfg.HeadlessService,
		Namespace:             cfg.Namespace,
		ClusterDomain:         cfg.ClusterDomain,
		RaftPort:              cfg.RaftPort,
		ApplyTimeout:          cfg.ApplyTimeout,
		LeaderWaitTimeout:     cfg.LeaderWaitTimeout,
		SnapshotThreshold:     cfg.SnapshotThreshold,
		SnapshotInterval:      cfg.SnapshotInterval,
		TrailingLogs:          cfg.TrailingLogs,
		PerformanceMultiplier: cfg.PerformanceMultiplier,
		RaftLogCacheSize:      cfg.RaftLogCacheSize,
		TransportMaxPool:      cfg.TransportMaxPool,
		TLSEnabled:            cfg.TLSEnabled,
		TLSDir:                cfg.TLSDir,
		TLSValidity:           cfg.TLSValidity,
		TLSOrganization:       cfg.TLSOrganization,
		CACertPath:            cfg.CACertPath,
		CertPath:              cfg.CertPath,
		KeyPath:               cfg.KeyPath,
		CASecret:              cfg.CASecret,
		CABootstrap:           cfg.CABootstrap,
		ClientAuth:            cfg.ClientAuth,
		SecretPollTimeout:     cfg.SecretPollTimeout,
		EncryptionKey:         cfg.EncryptionKey,
		AutopilotEnabled:      cfg.AutopilotEnabled,
		CleanupDeadServers:    cfg.CleanupDeadServers,
		MinQuorum:             cfg.MinQuorum,
	})

	// Overrides not covered by FromDaggerKubernetes.
	rcfg.AllowInsecureMultiNode = cfg.AllowInsecureMultiNode || !cfg.TLSEnabled
	rcfg.NoSnapshotRestoreOnStart = cfg.NoSnapshotRestoreOnStart
	rcfg.RecoveryMode = cfg.RecoveryMode
	rcfg.TLS.KeyringRotationEnabled = cfg.KeyringRotationEnabled
	rcfg.TLS.RotationPeriod = cfg.RotationPeriod
	rcfg.TLS.RotationCheckInterval = cfg.RotationCheckInterval
	rcfg.TLS.SafetyMargin = cfg.SafetyMargin
	if cfg.ReloadPollInterval > 0 {
		rcfg.TLS.ReloadPollInterval = cfg.ReloadPollInterval
	}
	// Manual PEM mode is renewed by external tooling (cert-manager); in-band
	// keyring rotation would fight it, so disable it unless explicitly wanted.
	if cfg.CACertPath != "" || cfg.CertPath != "" || cfg.KeyPath != "" {
		rcfg.TLS.KeyringRotationEnabled = false
	}
	if cfg.StorageBackend != "" {
		rcfg.Storage.Backend = cfg.StorageBackend
	}
	if cfg.JoinInterval > 0 {
		rcfg.Join.Interval = cfg.JoinInterval
	}
	if cfg.JoinReconcileTimeout > 0 {
		rcfg.Join.ReconcileTimeout = cfg.JoinReconcileTimeout
	}

	fsm := NewFSM()
	fsm.state.sessionSink = cfg.SessionSink
	fsm.state.quotaDefaults = cfg.OrgQuotaDefaults
	if cfg.MetaHook != nil {
		fsm.SetMetaHook(cfg.MetaHook)
	}

	sealer, err := DeriveSealer(cfg.EncryptionKey)
	if err != nil {
		return nil, err
	}

	bootstrapProvider := cfg.BootstrapProvider
	if bootstrapProvider == nil {
		bootstrapProvider = buildBootstrapProvider(cfg, logger)
	}
	genFencer, replicaReader := buildK8sHooks(cfg)

	metrics := cfg.Metrics
	if metrics == nil {
		metrics = raftobserv.NewNoopMetrics()
	}

	node, err := goraft.NewNode(context.Background(), &rcfg, goraft.NodeOptions{
		FSM:                    fsm,
		Resolver:               cfg.Resolver,
		BootstrapProvider:      bootstrapProvider,
		GenerationFencer:       genFencer,
		ReplicaCountAuthorizer: replicaReader,
		Sealer:                 sealer,
		Logger:                 raftobserv.NewLogrusAdapter(logger),
		Metrics:                metrics,
		CustomListener:         cfg.CustomListener,
	})
	if err != nil {
		return nil, fmt.Errorf("create raft node: %w", err)
	}
	if err := node.Start(context.Background()); err != nil {
		_ = node.Close()
		return nil, fmt.Errorf("start raft node: %w", err)
	}

	return &RaftStore{
		node:   node,
		fsm:    fsm,
		logger: logger,
		sealer: sealer,
	}, nil
}

// deriveSealer builds the at-rest sealer from the encryption key, or returns
// nil when no key is configured.
func DeriveSealer(encryptionKey string) (storage.Sealer, error) {
	if encryptionKey == "" {
		return nil, nil
	}
	sealer, err := storage.DeriveAESGCMKeeper([]byte(encryptionKey), storage.DefaultHKDFInfo, storage.DefaultHKDFInfo)
	if err != nil {
		return nil, fmt.Errorf("derive at-rest sealer: %w", err)
	}
	return sealer, nil
}

// buildBootstrapProvider returns the Kubernetes Secret CA bootstrap provider
// when a clientset + CA Secret are configured; otherwise nil (the node falls
// back to manual PEM or local CA bootstrap).
func buildBootstrapProvider(cfg *RaftStoreConfig, logger *logrus.Logger) pki.BootstrapProvider {
	if cfg.Clientset == nil || cfg.CASecret == "" {
		return nil
	}
	return raftk8s.NewSecretBootstrapProvider(raftk8s.SecretBootstrapConfig{
		Clientset:    cfg.Clientset,
		Namespace:    cfg.Namespace,
		SecretName:   cfg.CASecret,
		CABootstrap:  cfg.CABootstrap,
		CACommonName: "dagger-kubernetes-raft-ca",
		Organization: cfg.TLSOrganization,
		CAValidity:   10 * 365 * 24 * time.Hour,
		PollTimeout:  cfg.SecretPollTimeout,
		CacheDir:     cfg.TLSDir,
		Logger:       raftobserv.NewLogrusAdapter(logger),
	})
}

// buildK8sHooks returns the generation fencer and replica-count authorizer when
// a clientset is available.
func buildK8sHooks(cfg *RaftStoreConfig) (goraft.GenerationFencer, goraft.ReplicaCountAuthorizer) {
	if cfg.Clientset == nil {
		return nil, nil
	}
	var fencer goraft.GenerationFencer
	if cfg.CASecret != "" {
		fencer = raftk8s.NewGenerationStore(cfg.Clientset, cfg.Namespace, cfg.CASecret)
	}
	var replicas goraft.ReplicaCountAuthorizer
	if cfg.StatefulSetName != "" {
		replicas = raftk8s.NewStatefulSetReplicaReader(cfg.Clientset, cfg.Namespace, cfg.StatefulSetName)
	}
	return fencer, replicas
}

// toPeerConfigs converts discovery peers to go-raft config peers.
func toPeerConfigs(peers []discovery.Peer) []raftconfig.PeerConfig {
	out := make([]raftconfig.PeerConfig, 0, len(peers))
	for _, p := range peers {
		out = append(out, raftconfig.PeerConfig{ID: p.ID, Address: p.Address, NonVoter: p.NonVoter})
	}
	return out
}

// NewInmemRaftStore constructs a single-node Raft store backed entirely by
// in-memory log/stable/snapshot stores and an in-memory transport. Intended
// for tests (fast, no disk/network). Call WaitForLeader before writes.
func NewInmemRaftStore(nodeID string, logger *logrus.Logger, applyTimeout time.Duration) (*RaftStore, error) {
	fsm := NewFSM()
	node, err := goraft.NewInmemNode(nodeID, fsm, applyTimeout, raftobserv.NewLogrusAdapter(logger))
	if err != nil {
		return nil, fmt.Errorf("create inmem raft node: %w", err)
	}
	return &RaftStore{node: node, fsm: fsm, logger: logger}, nil
}

// WaitForLeader blocks until A leader exists in the cluster (not necessarily
// this node) or ctx expires.
func (s *RaftStore) WaitForLeader(ctx context.Context) error {
	return s.node.WaitForLeader(ctx)
}

// WaitForSelfLeadership blocks until this node is the leader or ctx expires.
func (s *RaftStore) WaitForSelfLeadership(ctx context.Context) error {
	return s.node.WaitForSelfLeadership(ctx)
}

// WaitForCleanState blocks until IsCleanState reports true or ctx expires.
func (s *RaftStore) WaitForCleanState(ctx context.Context) error {
	return s.node.WaitForCleanState(ctx)
}

// GetConfiguration returns the current raft cluster configuration.
func (s *RaftStore) GetConfiguration() (raft.Configuration, error) {
	return s.node.GetConfiguration()
}

// AddVoter adds a new voter to the running cluster (idempotent). Leader-only.
func (s *RaftStore) AddVoter(id, address string, timeout time.Duration) error {
	return s.node.AddVoter(id, address, timeout)
}

// RemoveServer removes a server from the running cluster. Leader-only.
func (s *RaftStore) RemoveServer(id string, timeout time.Duration) error {
	return s.node.RemoveServer(id, timeout)
}

// ReconcileMembership ensures the cluster configuration matches the desired
// voter list (idempotent). Leader-only.
func (s *RaftStore) ReconcileMembership(desired []discovery.Peer, timeout time.Duration) (added, updated, removed []string, err error) {
	return s.node.ReconcileMembership(desired, timeout)
}

// apply marshals cmd, calls Apply, and maps the result to a Go error.
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

// applyResponse is the shared apply path; it also returns the FSM response.
func (s *RaftStore) applyResponse(cmd *command) (interface{}, error) {
	data, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("marshal raft command: %w", err)
	}
	resp, err := s.node.Apply(context.Background(), data)
	if err != nil {
		return nil, mapApplyError(err)
	}
	return resp, nil
}

// mapApplyError translates raft apply-future errors into domain sentinels.
func mapApplyError(err error) error {
	switch {
	case errors.Is(err, goraft.ErrNotLeader), errors.Is(err, raft.ErrNotLeader), errors.Is(err, raft.ErrLeadershipLost):
		return domain.ErrNotLeader
	case errors.Is(err, goraft.ErrRaftTimeout), errors.Is(err, raft.ErrEnqueueTimeout), errors.Is(err, context.DeadlineExceeded):
		return domain.ErrRaftTimeout
	default:
		return fmt.Errorf("raft apply: %w", err)
	}
}

// SetMetaHook installs the FSM meta-change callback (used by tests and by the
// Minting CA rotator wiring).
func (s *RaftStore) SetMetaHook(hook func(key, value string)) {
	s.fsm.SetMetaHook(hook)
}

// SetSessionSink wires the pod-local session-state sink into the FSM.
func (s *RaftStore) SetSessionSink(sink domain.SessionStateSink) {
	s.fsm.state.mu.Lock()
	defer s.fsm.state.mu.Unlock()
	s.fsm.state.sessionSink = sink
}

// fsmRead returns the FSM for direct reads.
func (s *RaftStore) fsmRead() *FSM {
	return s.fsm
}

// IsLeader reports whether this node is the Raft leader.
func (s *RaftStore) IsLeader() bool {
	return s.node.IsLeader()
}

// LeaderAddress returns the current leader's Raft transport address, or "".
func (s *RaftStore) LeaderAddress() string {
	return s.node.LeaderAddress()
}

// IsCleanState reports whether the Raft consensus layer is in a clean state.
func (s *RaftStore) IsCleanState() bool {
	return s.node.IsCleanState()
}

// Close shuts the raft node down. Idempotent.
func (s *RaftStore) Close() error {
	if s.node == nil {
		return nil
	}
	return s.node.Close()
}

// StepDown causes the leader to step down to follower status.
func (s *RaftStore) StepDown(ctx context.Context) error {
	return s.node.StepDown(ctx)
}

// IsStarted returns true when the Raft node has joined the cluster.
func (s *RaftStore) IsStarted() bool {
	return s.node.IsStarted()
}

// IsVoter returns true if this node is a voting member of the cluster.
func (s *RaftStore) IsVoter() bool {
	return s.node.IsVoter()
}

// IsRemoved checks if this node has been removed from the Raft configuration.
func (s *RaftStore) IsRemoved() bool {
	return s.node.IsRemoved()
}

// TransferLeadership attempts to transfer leadership to another node.
func (s *RaftStore) TransferLeadership(ctx context.Context, timeout time.Duration) error {
	return s.node.TransferLeadership(ctx, timeout)
}

// LeaveCluster removes this node from the voter configuration gracefully.
func (s *RaftStore) LeaveCluster(ctx context.Context, timeout time.Duration) error {
	return s.node.LeaveCluster(ctx, timeout)
}

// MinFollowerAppliedIndex returns the minimum applied index across followers.
func (s *RaftStore) MinFollowerAppliedIndex() uint64 {
	return s.node.MinFollowerAppliedIndex()
}

// AppliedIndex returns the FSM applied index.
func (s *RaftStore) AppliedIndex() uint64 {
	return s.node.AppliedIndex()
}

// NodeID returns the stable node ID.
func (s *RaftStore) NodeID() string {
	return s.node.NodeID()
}

// TLSProvider returns the dynamic TLS provider used by the Raft transport.
func (s *RaftStore) TLSProvider() *rafttransport.DynamicTLSProvider {
	return s.node.TLSProvider()
}

// KeyringManager returns the in-band TLS keyring manager.
func (s *RaftStore) KeyringManager() *keyring.Manager {
	return s.node.KeyringManager()
}

// ActiveCAPEM returns the settled active CA (key ID, keyring term, cert PEM,
// unsealed key PEM) from the replicated keyring, or an error when no keyring is
// available yet.
func (s *RaftStore) ActiveCAPEM() (keyID string, term uint64, certPEM, keyPEM []byte, err error) {
	km := s.node.KeyringManager()
	if km == nil {
		return "", 0, nil, nil, fmt.Errorf("keyring manager unavailable")
	}
	kr := km.CurrentKeyring()
	if kr == nil {
		return "", 0, nil, nil, fmt.Errorf("keyring unavailable")
	}
	active := kr.GetActive()
	if active == nil {
		return "", 0, nil, nil, fmt.Errorf("keyring has no active key")
	}
	keyPEM, err = s.unseal(active.KeyPEM)
	if err != nil {
		return "", 0, nil, nil, err
	}
	return active.ID, kr.Term, active.CertPEM, keyPEM, nil
}

// unseal opens a stored keyring private key, or returns it unchanged when no
// sealer is configured.
func (s *RaftStore) unseal(stored string) ([]byte, error) {
	if stored == "" {
		return nil, nil
	}
	if s.sealer == nil {
		return []byte(stored), nil
	}
	plain, err := s.sealer.OpenString(stored)
	if err != nil {
		return nil, fmt.Errorf("unseal keyring private key: %w", err)
	}
	return []byte(plain), nil
}
