package repository

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
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

// raftClusterNode is one node in a test raft cluster.
type raftClusterNode struct {
	store *RaftStore
	r     *raft.Raft
	trans raft.Transport
	layer *tlsStreamLayer
	addr  raft.ServerAddress
}

// raftTestConfig returns a fast election-timing raft config for test clusters.
// Timings are intentionally relaxed enough to stay deterministic under
// -race and CI container CPU contention (200ms timeouts flaked on loaded
// runners: leaders stepped down before heartbeats could commit; the later
// failover flake "no leader elected" was the same problem one level up —
// a 250ms leader lease and 500ms election timeouts can still flap under
// contention, and each stalled round waits the full transport timeout on the
// dead node).
func raftTestConfig(id string) *raft.Config {
	config := raft.DefaultConfig()
	config.LocalID = raft.ServerID(id)
	config.TrailingLogs = 256
	config.SnapshotInterval = 10 * time.Minute
	config.SnapshotThreshold = 1000
	config.LogOutput = io.Discard
	config.LogLevel = "WARN"
	config.HeartbeatTimeout = time.Second
	config.ElectionTimeout = time.Second
	config.LeaderLeaseTimeout = 500 * time.Millisecond
	config.CommitTimeout = 100 * time.Millisecond
	return config
}

// waitForClusterLeader blocks until one of the nodes becomes leader.
func waitForClusterLeader(t *testing.T, nodes []raftClusterNode) *RaftStore {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for i := range nodes {
			if nodes[i].r.State() == raft.Leader {
				return nodes[i].store
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no leader elected")
	return nil
}

// newThreeNodeRaftCluster builds a 3-node in-memory (no TLS) raft cluster and
// waits for a leader to be elected. Used for stale-read + not-leader-on-follower
// + leadership-transfer tests.
func newThreeNodeRaftCluster(t *testing.T) []*RaftStore {
	t.Helper()
	return newInmemRaftCluster(t, 3)
}

// newInmemRaftCluster builds an n-node fully-connected in-memory raft cluster.
func newInmemRaftCluster(t *testing.T, n int) []*RaftStore {
	t.Helper()
	nodes := make([]raftClusterNode, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("node-%d", i)
		fsm := NewFSM()
		logs := raft.NewInmemStore()
		stable := raft.NewInmemStore()
		snaps := raft.NewInmemSnapshotStore()
		addr, trans := raft.NewInmemTransport(raft.NewInmemAddr())
		r, err := raft.NewRaft(raftTestConfig(id), fsm, logs, stable, snaps, trans)
		if err != nil {
			t.Fatalf("NewRaft %s: %v", id, err)
		}
		nodes[i] = raftClusterNode{
			store: &RaftStore{raft: r, fsm: fsm, timeout: 5 * time.Second},
			r:     r,
			trans: trans,
			addr:  addr,
		}
	}
	for i := range nodes {
		for j := range nodes {
			if i != j {
				nodes[i].trans.(*raft.InmemTransport).Connect(nodes[j].addr, nodes[j].trans)
			}
		}
	}
	bootstrapCluster(t, nodes)
	t.Cleanup(func() {
		for i := range nodes {
			_ = nodes[i].r.Shutdown().Error()
			_ = nodes[i].trans.(*raft.InmemTransport).Close()
		}
	})
	return clusterStores(nodes)
}

// newThreeNodeTLSRaftCluster builds a 3-node cluster over loopback TCP with
// real goca CA + per-node leaf certs + mTLS. Asserts a leader is elected and
// that the initial configuration has replicated to every node (see
// waitForClusterSettled).
func newThreeNodeTLSRaftCluster(t *testing.T) []*RaftStore {
	t.Helper()
	nodes := buildTLSClusterNodes(t, 3)
	bootstrapCluster(t, nodes)
	waitForClusterLeader(t, nodes)
	stores := clusterStores(nodes)
	waitForClusterSettled(t, stores, 3, 30*time.Second)
	return stores
}

// waitForClusterSettled blocks until every node's latest configuration
// contains the full voter set (len(cfg.Servers) == n). This proves the
// bootstrap configuration entry has been committed and replicated.
//
// It exists because raft followers with an empty configuration refuse to
// start elections by design (split-brain protection: runFollower aborts with
// "no known peers, aborting election" when the config index is 0). A test
// that kills the leader before the configuration replicated leaves a cluster
// that can never elect a new leader — the exact "no leader elected" CI flake
// in TestThreeNodeTLSClusterLeaderFailover, where the kill landed in the
// window between the first election and the commit of the no-op barrier
// entry (widened by leader-lease step-down flaps under -race contention).
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

// buildTLSClusterNodes builds n mTLS raft nodes over loopback TCP sharing a
// goca CA. The nodes are NOT bootstrapped (callers choose the configuration).
func buildTLSClusterNodes(t *testing.T, n int) []raftClusterNode {
	t.Helper()

	caCertPEM, caKeyPEM, err := createRaftCAWithGoca("test-raft-ca", "dagger-kubernetes-raft")
	if err != nil {
		t.Fatalf("createRaftCAWithGoca: %v", err)
	}
	ca, err := NewMintingCAFromPEM(caCertPEM, caKeyPEM, time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCAFromPEM: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertPEM) {
		t.Fatal("append CA cert")
	}

	nodes := make([]raftClusterNode, n)
	for i := 0; i < n; i++ {
		cn := fmt.Sprintf("node-%d", i)
		leafCert, leafKey, err := ca.IssuePeerCertificate(cn, "dagger-kubernetes-raft", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, time.Hour)
		if err != nil {
			t.Fatalf("IssuePeerCertificate: %v", err)
		}
		leaf, err := tls.X509KeyPair(leafCert, leafKey)
		if err != nil {
			t.Fatalf("X509KeyPair: %v", err)
		}
		tlsCfg := &tls.Config{
			Certificates: []tls.Certificate{leaf},
			RootCAs:      pool,
			ClientCAs:    pool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			MinVersion:   tls.VersionTLS12,
		}
		layer, adv, err := newTLSStreamLayer("127.0.0.1:0", nil, tlsCfg)
		if err != nil {
			t.Fatalf("newTLSStreamLayer: %v", err)
		}
		trans := raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{
			Stream:  layer,
			Logger:  hclog.NewNullLogger(),
			MaxPool: 10,
			Timeout: time.Second,
		})
		nodes[i] = raftClusterNode{layer: layer, trans: trans, addr: raft.ServerAddress(adv.String())}
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("node-%d", i)
		fsm := NewFSM()
		logs := raft.NewInmemStore()
		stable := raft.NewInmemStore()
		snaps := raft.NewInmemSnapshotStore()
		r, err := raft.NewRaft(raftTestConfig(id), fsm, logs, stable, snaps, nodes[i].trans)
		if err != nil {
			t.Fatalf("NewRaft %s: %v", id, err)
		}
		nodes[i].store = &RaftStore{raft: r, fsm: fsm, timeout: 5 * time.Second, transport: nodes[i].trans}
		nodes[i].r = r
	}
	t.Cleanup(func() {
		for i := range nodes {
			_ = nodes[i].r.Shutdown().Error()
			_ = closeRaftTransport(nodes[i].trans)
		}
	})
	return nodes
}

// bootstrapSingle seeds node-0 with itself as the only voter.
func bootstrapSingle(t *testing.T, nodes []raftClusterNode) {
	t.Helper()
	servers := []raft.Server{{
		Suffrage: raft.Voter,
		ID:       raft.ServerID("node-0"),
		Address:  nodes[0].addr,
	}}
	if err := nodes[0].r.BootstrapCluster(raft.Configuration{Servers: servers}).Error(); err != nil {
		t.Fatalf("BootstrapCluster: %v", err)
	}
}

// bootstrapCluster seeds node-0 with the full voter configuration.
func bootstrapCluster(t *testing.T, nodes []raftClusterNode) {
	t.Helper()
	servers := make([]raft.Server, len(nodes))
	for i := range nodes {
		servers[i] = raft.Server{
			Suffrage: raft.Voter,
			ID:       raft.ServerID(fmt.Sprintf("node-%d", i)),
			Address:  nodes[i].addr,
		}
	}
	if err := nodes[0].r.BootstrapCluster(raft.Configuration{Servers: servers}).Error(); err != nil {
		t.Fatalf("BootstrapCluster: %v", err)
	}
}

// clusterStores extracts the RaftStore handles from the cluster nodes.
func clusterStores(nodes []raftClusterNode) []*RaftStore {
	stores := make([]*RaftStore, len(nodes))
	for i := range nodes {
		stores[i] = nodes[i].store
	}
	return stores
}

// findLeader returns the current leader store, failing if none is elected.
func findLeader(t *testing.T, stores []*RaftStore) *RaftStore {
	t.Helper()
	return findLeaderTimeout(t, stores, 30*time.Second)
}

// findLeaderTimeout is findLeader with a caller-chosen deadline. The failover
// test uses a longer one: after the leader dies, election rounds stall on the
// dead node for the transport timeout, and under -race/CI contention the
// default 30s has proven too tight.
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
	for i, s := range stores {
		t.Logf("node %d stats: %v", i, s.raft.Stats())
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

	// Kill the leader (raft + transport): a new one must be elected and
	// writes must resume. newThreeNodeTLSRaftCluster already waited for the
	// configuration to replicate to every node — without it the survivors
	// would hold an empty configuration and refuse to start elections
	// (raft's split-brain protection), which was the actual CI flake.
	// Closing the transport too makes dials to the dead node fail fast
	// (connection refused) instead of stalling every election round for the
	// transport timeout.
	if err := leader.raft.Shutdown().Error(); err != nil {
		t.Fatalf("shutdown leader: %v", err)
	}
	_ = closeRaftTransport(leader.transport)

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
	nodes := buildTLSClusterNodes(t, 3)
	bootstrapSingle(t, nodes)

	// Wait for node-0 to become the single-node leader.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && !nodes[0].store.IsLeader() {
		time.Sleep(10 * time.Millisecond)
	}
	if !nodes[0].store.IsLeader() {
		t.Fatal("node-0 did not become leader")
	}

	leader := nodes[0].store
	for i := 1; i < 3; i++ {
		if err := leader.AddVoter(fmt.Sprintf("node-%d", i), string(nodes[i].addr), 5*time.Second); err != nil {
			t.Fatalf("AddVoter node-%d: %v", i, err)
		}
	}

	// The cluster reaches a 3-node configuration.
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		cfg, err := leader.GetConfiguration()
		if err == nil && len(cfg.Servers) == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
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
	for i := 1; i < 3; i++ {
		waitForMetaValue(t, nodes[i].store, "joined", "yes")
	}
}
