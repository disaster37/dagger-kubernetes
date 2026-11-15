package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"testing"
	"time"

	raftconfig "github.com/disaster37/go-raft/config"
	"github.com/disaster37/go-raft/discovery"
	"github.com/disaster37/go-raft/pki"
	"github.com/disaster37/go-raft/testutil"
	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// testLogger returns a logrus logger that discards all output.
func testLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return l
}

// newTestRaftStore builds a single-node Raft cluster backed entirely by
// in-memory stores and transport (fast, no disk).
func newTestRaftStore(t *testing.T) *RaftStore {
	t.Helper()
	return newTestRaftStoreTimeout(t, 5*time.Second)
}

// newTestRaftStoreTimeout is newTestRaftStore with a custom apply timeout.
func newTestRaftStoreTimeout(t *testing.T, timeout time.Duration) *RaftStore {
	t.Helper()
	store, err := NewInmemRaftStore("test-node", testLogger(), timeout)
	if err != nil {
		t.Fatalf("NewInmemRaftStore: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WaitForLeader(ctx); err != nil {
		t.Fatalf("wait for leader: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newID returns a fresh 32-char hex id (16 random bytes).
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("rand read: %v", err))
	}
	return hex.EncodeToString(b)
}

// seedUser inserts and returns a user with the given username (always RoleUser).
func seedUser(t *testing.T, store *RaftStore, username string) *domain.User {
	t.Helper()
	u := &domain.User{
		ID:       newID(),
		Username: username,
		Role:     domain.RoleUser,
	}
	if err := NewUserRepo(store).Create(context.Background(), u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return u
}

// mustCommand marshals payload into a command of the given kind, failing the
// test on error (used by FSM unit tests).
func mustCommand(t *testing.T, kind commandKind, payload any) *command {
	t.Helper()
	cmd, err := newCommand(kind, payload)
	if err != nil {
		t.Fatalf("newCommand: %v", err)
	}
	return cmd
}

// testBootstrapProvider returns a fixed shared CA material for TLS test
// clusters (no Kubernetes clientset required).
type testBootstrapProvider struct {
	mat *pki.BootstrapMaterial
}

func (p *testBootstrapProvider) EnsureBootstrapMaterial(context.Context) (*pki.BootstrapMaterial, error) {
	return p.mat, nil
}

// newTestBootstrapProvider builds a shared CA material for TLS test clusters.
func newTestBootstrapProvider(t *testing.T) *testBootstrapProvider {
	t.Helper()
	ca, err := pki.NewMintingCA("test-raft-ca", "dagger-kubernetes-raft", 10*365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCA: %v", err)
	}
	certPEM, keyPEM, err := ca.EncodePEM()
	if err != nil {
		t.Fatalf("EncodePEM: %v", err)
	}
	return &testBootstrapProvider{mat: &pki.BootstrapMaterial{CACertPEM: certPEM, CAKeyPEM: keyPEM}}
}

// newRaftCluster builds an n-node loopback Raft cluster via NewRaftStore with
// static peer discovery. When tlsEnabled is true every node shares a local CA
// and uses mTLS.
func newRaftCluster(t *testing.T, n int, tlsEnabled bool) []*RaftStore {
	t.Helper()
	return newRaftClusterOpts(t, n, tlsEnabled, false)
}

// newRaftClusterWithRotation builds a TLS cluster with in-band keyring
// rotation enabled (fast period) and an at-rest sealer.
func newRaftClusterWithRotation(t *testing.T, n int) []*RaftStore {
	t.Helper()
	return newRaftClusterOpts(t, n, true, true)
}

func newRaftClusterOpts(t *testing.T, n int, tlsEnabled, rotation bool) []*RaftStore {
	t.Helper()
	addrs := make([]string, n)
	listeners := make([]net.Listener, n)
	for i := range addrs {
		listeners[i] = testutil.FreeListener(t)
		addrs[i] = listeners[i].Addr().String()
	}
	peers := make([]discovery.Peer, n)
	for i := range peers {
		peers[i] = discovery.Peer{ID: fmt.Sprintf("node-%d", i), Address: addrs[i]}
	}
	var bootstrap pki.BootstrapProvider
	if tlsEnabled {
		bootstrap = newTestBootstrapProvider(t)
	}
	stores := make([]*RaftStore, n)
	for i := range stores {
		nodeID := fmt.Sprintf("node-%d", i)
		cfg := &RaftStoreConfig{
			Dir:                    t.TempDir(),
			NodeID:                 nodeID,
			BindAddr:               addrs[i],
			AdvertiseAddr:          addrs[i],
			Peers:                  peers,
			Replicas:               n,
			Resolver:               discovery.NewPeerResolver(&discovery.Config{NodeID: nodeID, AdvertiseAddr: addrs[i], Peers: peers, Replicas: n}),
			StorageBackend:         raftconfig.StorageBackendInmem,
			CustomListener:         listeners[i],
			JoinInterval:           100 * time.Millisecond,
			JoinReconcileTimeout:   2 * time.Second,
			PerformanceMultiplier:  0.2,
			AutopilotEnabled:       true,
			CleanupDeadServers:     true,
			MinQuorum:              1,
			TLSEnabled:             tlsEnabled,
			TLSDir:                 t.TempDir(),
			TLSOrganization:        "dagger-kubernetes-raft",
			CASecret:               "test-ca",
			ClientAuth:             true,
			BootstrapProvider:      bootstrap,
			AllowInsecureMultiNode: true,
		}
		if rotation {
			// Rotation applies Raft writes; use more forgiving election
			// timings so the leader stays stable under -race load. The rotation
			// period is deliberately well above the check interval: a 300ms period
			// generates a fresh RSA CA key every few hundred milliseconds, which
			// starves the FSM apply loop under -race -covermode=atomic and makes
			// TestRaftStoreAutoRenewsTLSKeyringAndLeaf flaky.
			cfg.PerformanceMultiplier = 2.0
			cfg.KeyringRotationEnabled = true
			cfg.RotationPeriod = 2 * time.Second
			cfg.RotationCheckInterval = 50 * time.Millisecond
			cfg.SafetyMargin = 7 * 24 * time.Hour
			cfg.EncryptionKey = "0123456789abcdef0123456789abcdef"
		}
		store, err := NewRaftStore(cfg, testLogger())
		if err != nil {
			t.Fatalf("NewRaftStore node-%d: %v", i, err)
		}
		stores[i] = store
	}
	t.Cleanup(func() {
		for _, s := range stores {
			_ = s.Close()
		}
	})
	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i, s := range stores {
		if err := s.WaitForLeader(waitCtx); err != nil {
			t.Fatalf("node-%d WaitForLeader: %v", i, err)
		}
	}
	for i, s := range stores {
		if err := s.WaitForCleanState(waitCtx); err != nil {
			t.Fatalf("node-%d WaitForCleanState: %v", i, err)
		}
	}
	return stores
}

// newThreeNodeRaftCluster builds a 3-node in-memory (no TLS) raft cluster.
func newThreeNodeRaftCluster(t *testing.T) []*RaftStore {
	t.Helper()
	return newRaftCluster(t, 3, false)
}

// newThreeNodeTLSRaftCluster builds a 3-node cluster over loopback TCP with a
// shared CA + per-node leaf certs + mTLS.
func newThreeNodeTLSRaftCluster(t *testing.T) []*RaftStore {
	t.Helper()
	stores := newRaftCluster(t, 3, true)
	waitForClusterSettled(t, stores, 3, 30*time.Second)
	return stores
}

// waitForClusterSettled blocks until every node's latest configuration
// contains the full voter set (len(cfg.Servers) == n).
func waitForClusterSettled(t *testing.T, stores []*RaftStore, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		settled := true
		for _, s := range stores {
			cfg, err := s.GetConfiguration()
			if err != nil || len(cfg.Servers) != n {
				settled = false
				break
			}
		}
		if settled {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cluster configuration never replicated to all nodes")
}

// findLeader returns the current leader store, failing if none is elected.
func findLeader(t *testing.T, stores []*RaftStore) *RaftStore {
	t.Helper()
	return findLeaderTimeout(t, stores, 30*time.Second)
}

// findLeaderTimeout is findLeader with a caller-chosen deadline.
func findLeaderTimeout(t *testing.T, stores []*RaftStore, timeout time.Duration) *RaftStore {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, s := range stores {
			if s.IsLeader() {
				return s
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	stack := make([]byte, 1<<20)
	n := runtime.Stack(stack, true)
	t.Logf("goroutine dump:\n%s", stack[:n])
	t.Fatal("no leader elected")
	return nil
}

// waitForMetaValue polls a node's local FSM until the meta key reaches want.
func waitForMetaValue(t *testing.T, store *RaftStore, key, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if v, err := store.fsmRead().readMeta(key); err == nil && v == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("meta %q never replicated to %q", key, want)
}

func TestThreeNodeInmemClusterReplication(t *testing.T) {
	stores := newThreeNodeRaftCluster(t)
	leader := findLeader(t, stores)

	cmd, _ := newCommand(kindSetMeta, cmdSetMeta{Key: "k", Value: "v"})
	if err := leader.apply(cmd); err != nil {
		t.Fatalf("leader apply: %v", err)
	}

	for _, s := range stores {
		if s == leader {
			continue
		}
		if err := s.apply(cmd); !errors.Is(err, domain.ErrNotLeader) {
			t.Fatalf("follower apply = %v, want ErrNotLeader", err)
		}
		// Followers serve the replicated value from their local FSM.
		waitForMetaValue(t, s, "k", "v")
	}
}

func TestThreeNodeTLSClusterReplication(t *testing.T) {
	stores := newThreeNodeTLSRaftCluster(t)
	leader := findLeader(t, stores)

	cmd, _ := newCommand(kindSetMeta, cmdSetMeta{Key: "tls-k", Value: "v"})
	if err := leader.apply(cmd); err != nil {
		t.Fatalf("leader apply: %v", err)
	}

	followers := 0
	for _, s := range stores {
		if s == leader {
			continue
		}
		followers++
		if err := s.apply(cmd); !errors.Is(err, domain.ErrNotLeader) {
			t.Fatalf("follower apply = %v, want ErrNotLeader", err)
		}
		waitForMetaValue(t, s, "tls-k", "v")
	}
	if followers != 2 {
		t.Fatalf("followers = %d, want 2", followers)
	}
}

func TestThreeNodeTLSClusterLeaderFailover(t *testing.T) {
	stores := newThreeNodeTLSRaftCluster(t)
	leader := findLeader(t, stores)

	// Kill the leader: a new one must be elected and writes must resume.
	if err := leader.Close(); err != nil {
		t.Fatalf("close leader: %v", err)
	}

	newLeader := findLeaderTimeout(t, stores, 60*time.Second)
	if newLeader == leader {
		t.Fatal("expected a different leader after failover")
	}
	cmd, _ := newCommand(kindSetMeta, cmdSetMeta{Key: "failover", Value: "ok"})
	if err := newLeader.apply(cmd); err != nil {
		t.Fatalf("new leader apply: %v", err)
	}
}

func TestThreeNodeTLSClusterAddVoterJoin(t *testing.T) {
	stores := newThreeNodeTLSRaftCluster(t)
	leader := findLeader(t, stores)

	cfg, err := leader.GetConfiguration()
	if err != nil {
		t.Fatalf("GetConfiguration: %v", err)
	}
	if len(cfg.Servers) != 3 {
		t.Fatalf("config servers = %d, want 3", len(cfg.Servers))
	}

	// A write on the leader commits against the full quorum.
	cmd, _ := newCommand(kindSetMeta, cmdSetMeta{Key: "joined", Value: "yes"})
	if err := leader.apply(cmd); err != nil {
		t.Fatalf("apply after join: %v", err)
	}
	for _, s := range stores {
		if s == leader {
			continue
		}
		waitForMetaValue(t, s, "joined", "yes")
	}
}
