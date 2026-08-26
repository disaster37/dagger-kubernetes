package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestRaftStoreSingleNodeLeader(t *testing.T) {
	store := newTestRaftStore(t)
	if !store.IsLeader() {
		t.Fatal("single node should be leader")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.WaitForLeader(ctx); err != nil {
		t.Fatalf("WaitForLeader: %v", err)
	}
	if !store.IsCleanState() {
		t.Fatal("single node leader should be in clean state")
	}
}

func TestRaftStoreIsCleanStateAfterApply(t *testing.T) {
	store := newTestRaftStore(t)
	cmd, err := newCommand(kindSetMeta, cmdSetMeta{Key: "k", Value: "v"})
	if err != nil {
		t.Fatalf("newCommand: %v", err)
	}
	if err := store.apply(cmd); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !store.IsCleanState() {
		t.Fatal("single node after apply should be in clean state")
	}
}

func TestRaftStoreWaitForCleanState(t *testing.T) {
	store := newTestRaftStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.WaitForCleanState(ctx); err != nil {
		t.Fatalf("WaitForCleanState: %v", err)
	}
}

func TestRaftStoreWaitForCleanStateShutdown(t *testing.T) {
	store := newTestRaftStore(t)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.WaitForCleanState(ctx); err == nil {
		t.Fatal("WaitForCleanState after shutdown should error")
	}
}

func TestRaftStoreApplySuccess(t *testing.T) {
	store := newTestRaftStore(t)
	cmd, err := newCommand(kindSetMeta, cmdSetMeta{Key: "k", Value: "v"})
	if err != nil {
		t.Fatalf("newCommand: %v", err)
	}
	if err := store.apply(cmd); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if v, err := store.fsmRead().readMeta("k"); err != nil || v != "v" {
		t.Fatalf("readMeta = %q err=%v", v, err)
	}
}

func TestRaftStoreApplyNotLeader(t *testing.T) {
	a, b := newTwoNodeRaftCluster(t)
	var leader, follower *RaftStore
	switch {
	case a.IsLeader():
		leader, follower = a, b
	case b.IsLeader():
		leader, follower = b, a
	default:
		t.Fatal("no leader elected in two-node cluster")
	}

	cmd, _ := newCommand(kindSetMeta, cmdSetMeta{Key: "k", Value: "v"})
	if err := follower.apply(cmd); !errors.Is(err, domain.ErrNotLeader) {
		t.Fatalf("follower apply = %v, want ErrNotLeader", err)
	}
	if err := leader.apply(cmd); err != nil {
		t.Fatalf("leader apply: %v", err)
	}
}

func TestRaftStoreApplyMarshalError(t *testing.T) {
	store := newTestRaftStore(t)
	cmd := &command{Kind: kindSetMeta, Data: json.RawMessage("{bad")}
	if err := store.apply(cmd); err == nil {
		t.Fatal("expected marshal error")
	}
}

func TestRaftStoreApplyFSMResponseError(t *testing.T) {
	store := newTestRaftStore(t)
	u := &cmdUser{ID: "u", Username: "alice", Create: true}
	if err := store.apply(mustCommand(t, kindUpsertUser, u)); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	dup := &cmdUser{ID: "u2", Username: "ALICE", Create: true}
	if err := store.apply(mustCommand(t, kindUpsertUser, dup)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate apply = %v, want ErrConflict", err)
	}
}

func TestMapApplyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"not leader", raft.ErrNotLeader, domain.ErrNotLeader},
		{"leadership lost", raft.ErrLeadershipLost, domain.ErrNotLeader},
		{"enqueue timeout", raft.ErrEnqueueTimeout, domain.ErrRaftTimeout},
		{"deadline exceeded", context.DeadlineExceeded, domain.ErrRaftTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapApplyError(tc.err)
			if !errors.Is(got, tc.want) {
				t.Fatalf("mapApplyError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
	// Generic errors are wrapped.
	if err := mapApplyError(errors.New("boom")); err == nil || err.Error() != "raft apply: boom" {
		t.Fatalf("generic error = %v", err)
	}
}

func TestRaftStoreCloseIdempotent(t *testing.T) {
	store := newTestRaftStore(t)
	if err := store.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestRaftStoreLeaderCh(t *testing.T) {
	store := newTestRaftStore(t)
	ch := store.LeaderCh()
	if ch == nil {
		t.Fatal("LeaderCh should be non-nil")
	}
}

func TestRaftStoreNewRaftStoreSingleNode(t *testing.T) {
	store, err := NewRaftStore(&RaftStoreConfig{
		Dir:      t.TempDir(),
		BindAddr: freeTCPAddr(t),
	}, testLogger())
	if err != nil {
		t.Fatalf("NewRaftStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WaitForLeader(ctx); err != nil {
		t.Fatalf("WaitForLeader: %v", err)
	}
	cmd, _ := newCommand(kindSetMeta, cmdSetMeta{Key: "k", Value: "v"})
	if err := store.apply(cmd); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if v, _ := store.fsmRead().readMeta("k"); v != "v" {
		t.Fatalf("readMeta = %q", v)
	}
}

// TestRaftStoreOnlyBootstrapNodeSeedsConfig verifies that only the bootstrap
// node (first peer in the resolved voter list) calls raft.BootstrapCluster,
// and that it seeds the cluster with ONLY itself as the initial voter. Other
// nodes start with no config and join via the leader's AddVoter (joinLoop).
// This prevents the deadlock where the bootstrap node includes all peers in
// the initial configuration, requiring a majority (2 of 3) to elect a leader
// while the non-bootstrap peers have no config and may not be ready to vote
// (CWE-693, ADR-016).
func TestRaftStoreOnlyBootstrapNodeSeedsConfig(t *testing.T) {
	addr1 := freeTCPAddr(t)
	addr2 := freeTCPAddr(t)
	peers := []RaftPeer{
		{ID: "node-1", Address: addr1},
		{ID: "node-2", Address: addr2},
	}

	// Node-1 is the first peer → bootstraps with only itself.
	s1, err := NewRaftStore(&RaftStoreConfig{
		Dir:           filepath.Join(t.TempDir(), "node-1"),
		NodeID:        "node-1",
		BindAddr:      addr1,
		AdvertiseAddr: addr1,
		Resolver:      &staticPeerResolver{cfg: RaftDiscoveryConfig{NodeID: "node-1", Peers: peers}},
	}, testLogger())
	if err != nil {
		t.Fatalf("NewRaftStore node-1: %v", err)
	}
	defer func() { _ = s1.Close() }()

	// Node-2 is NOT the first peer → must NOT bootstrap. Its configuration
	// must be empty until the leader (node-1) adds it via AddVoter.
	s2, err := NewRaftStore(&RaftStoreConfig{
		Dir:           filepath.Join(t.TempDir(), "node-2"),
		NodeID:        "node-2",
		BindAddr:      addr2,
		AdvertiseAddr: addr2,
		Resolver:      &staticPeerResolver{cfg: RaftDiscoveryConfig{NodeID: "node-2", Peers: peers}},
	}, testLogger())
	if err != nil {
		t.Fatalf("NewRaftStore node-2: %v", err)
	}
	defer func() { _ = s2.Close() }()

	// Node-2 must not have seeded a local configuration (no split-brain).
	cfg2, err := s2.GetConfiguration()
	if err != nil {
		t.Fatalf("GetConfiguration node-2: %v", err)
	}
	if len(cfg2.Servers) != 0 {
		t.Fatalf("non-bootstrap node-2 seeded a local config (split-brain risk): %v", cfg2.Servers)
	}

	// Node-1 must have bootstrapped with only itself (single-node quorum).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s1.WaitForLeader(ctx); err != nil {
		t.Fatalf("WaitForLeader node-1: %v", err)
	}
	cfg1, err := s1.GetConfiguration()
	if err != nil {
		t.Fatalf("GetConfiguration node-1: %v", err)
	}
	if len(cfg1.Servers) != 1 {
		t.Fatalf("bootstrap node-1 config has %d servers, want 1 (single-node bootstrap)", len(cfg1.Servers))
	}
	if string(cfg1.Servers[0].ID) != "node-1" {
		t.Fatalf("bootstrap node-1 config server = %s, want node-1", cfg1.Servers[0].ID)
	}

	// Node-2 joins via AddVoter (simulating the joinLoop). Once added, the
	// leader replicates the configuration and node-2 discovers the leader
	// through heartbeats.
	if err := s1.AddVoter("node-2", addr2, 5*time.Second); err != nil {
		t.Fatalf("AddVoter node-2: %v", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()
	if err := s2.WaitForLeader(ctx2); err != nil {
		t.Fatalf("WaitForLeader node-2 after AddVoter: %v", err)
	}

	// Verify the cluster now has both voters.
	cfgFinal, err := s1.GetConfiguration()
	if err != nil {
		t.Fatalf("GetConfiguration final: %v", err)
	}
	if len(cfgFinal.Servers) != 2 {
		t.Fatalf("final config has %d servers, want 2", len(cfgFinal.Servers))
	}
}

// TestRaftStoreSnapshotDirPermissions verifies that the snapshots directory is
// tightened to 0o700 so snapshot files (which contain password hashes, token
// hashes/ciphertexts, the JWT secret, and the token-encryption key in
// cleartext JSON) are not world-readable (CWE-922).
func TestRaftStoreSnapshotDirPermissions(t *testing.T) {
	dir := t.TempDir()
	store, err := NewRaftStore(&RaftStoreConfig{
		Dir:      dir,
		BindAddr: freeTCPAddr(t),
	}, testLogger())
	if err != nil {
		t.Fatalf("NewRaftStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	info, err := os.Stat(filepath.Join(dir, "snapshots"))
	if err != nil {
		t.Fatalf("stat snapshots dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("snapshots dir perm = %o, want 700 (CWE-922)", perm)
	}
}

func TestNewRaftStoreBadBindAddr(t *testing.T) {
	if _, err := NewRaftStore(&RaftStoreConfig{Dir: t.TempDir(), BindAddr: "not-an-addr"}, testLogger()); err == nil {
		t.Fatal("expected error for bad bind addr")
	}
}

func TestNewRaftStoreTLS(t *testing.T) {
	dir := t.TempDir()
	_, tlsCfg, err := LoadOrBuildRaftTLS(testRaftTLSCfg(filepath.Join(dir, "tls")), false, nil, "", nil, nil, "node-0", testLogger())
	if err != nil {
		t.Fatalf("build raft TLS config: %v", err)
	}

	store, err := NewRaftStore(&RaftStoreConfig{
		Dir:      filepath.Join(dir, "data"),
		BindAddr: freeTCPAddr(t),
		TLS:      tlsCfg,
	}, testLogger())
	if err != nil {
		t.Fatalf("NewRaftStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WaitForLeader(ctx); err != nil {
		t.Fatalf("WaitForLeader: %v", err)
	}
	cmd, _ := newCommand(kindSetMeta, cmdSetMeta{Key: "k", Value: "v"})
	if err := store.apply(cmd); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if v, _ := store.fsmRead().readMeta("k"); v != "v" {
		t.Fatalf("readMeta = %q", v)
	}
}

func TestWaitForLeaderAnyVsSelf(t *testing.T) {
	a, b := newTwoNodeRaftCluster(t)
	var leader, follower *RaftStore
	switch {
	case a.IsLeader():
		leader, follower = a, b
	case b.IsLeader():
		leader, follower = b, a
	default:
		t.Fatal("no leader elected in two-node cluster")
	}

	// WaitForLeader (any leader) returns on BOTH nodes.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := leader.WaitForLeader(ctx); err != nil {
		t.Fatalf("leader WaitForLeader: %v", err)
	}
	if err := follower.WaitForLeader(ctx); err != nil {
		t.Fatalf("follower WaitForLeader: %v", err)
	}

	// WaitForSelfLeadership returns on the leader, times out on the follower.
	if err := leader.WaitForSelfLeadership(ctx); err != nil {
		t.Fatalf("leader WaitForSelfLeadership: %v", err)
	}
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer shortCancel()
	if err := follower.WaitForSelfLeadership(shortCtx); err == nil {
		t.Fatal("follower WaitForSelfLeadership should time out")
	}
}

// TestWaitForLeaderBeforeElection exercises the startup wait path: a follower
// that starts waiting before any leader is elected must still observe the
// leader once it appears (raft.LeaderCh only fires on self leadership changes,
// so the wait must poll — ADR-016 D6).
func TestWaitForLeaderBeforeElection(t *testing.T) {
	a, b := newTwoNodeRaftClusterOpt(t, false)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.WaitForLeader(ctx); err != nil {
		t.Fatalf("node-a WaitForLeader: %v", err)
	}
	if err := b.WaitForLeader(ctx); err != nil {
		t.Fatalf("node-b WaitForLeader: %v", err)
	}
	if !a.IsLeader() && !b.IsLeader() {
		t.Fatal("expected a leader to be elected")
	}
}

func TestRaftStoreAddVoterAndConfiguration(t *testing.T) {
	a, b := newTwoNodeRaftCluster(t)
	var leader *RaftStore
	switch {
	case a.IsLeader():
		leader = a
	case b.IsLeader():
		leader = b
	default:
		t.Fatal("no leader elected")
	}

	cfg, err := leader.GetConfiguration()
	if err != nil {
		t.Fatalf("GetConfiguration: %v", err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("config servers = %d, want 2", len(cfg.Servers))
	}

	// Re-adding an existing voter is idempotent.
	if err := leader.AddVoter("node-1", string(cfg.Servers[0].Address), 5*time.Second); err != nil {
		t.Fatalf("AddVoter (idempotent): %v", err)
	}
}

func TestReconcileMembershipAddVoter(t *testing.T) {
	a, b := newTwoNodeRaftCluster(t)
	var leader *RaftStore
	switch {
	case a.IsLeader():
		leader = a
	case b.IsLeader():
		leader = b
	default:
		t.Fatal("no leader elected")
	}
	cfg, err := leader.GetConfiguration()
	if err != nil {
		t.Fatalf("GetConfiguration: %v", err)
	}
	addrByID := make(map[string]string, len(cfg.Servers))
	for _, s := range cfg.Servers {
		addrByID[string(s.ID)] = string(s.Address)
	}

	// A third (not running) voter is added: AddVoter only records the config
	// entry and the 2 running nodes still satisfy the old quorum.
	desired := []RaftPeer{
		{ID: "node-1", Address: addrByID["node-1"]},
		{ID: "node-2", Address: addrByID["node-2"]},
		{ID: "node-3", Address: "127.0.0.1:19999"},
	}
	added, removed, err := leader.ReconcileMembership(desired, 5*time.Second)
	if err != nil {
		t.Fatalf("ReconcileMembership: %v", err)
	}
	if len(added) != 1 || added[0] != "node-3" {
		t.Fatalf("added = %v, want [node-3]", added)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want empty", removed)
	}
}

func TestReconcileMembershipRemoveServer(t *testing.T) {
	a, b := newTwoNodeRaftCluster(t)
	var leader *RaftStore
	switch {
	case a.IsLeader():
		leader = a
	case b.IsLeader():
		leader = b
	default:
		t.Fatal("no leader elected")
	}
	cfg, err := leader.GetConfiguration()
	if err != nil {
		t.Fatalf("GetConfiguration: %v", err)
	}
	selfID := "node-2"
	if leader == a {
		selfID = "node-1"
	}
	selfAddr := ""
	for _, s := range cfg.Servers {
		if string(s.ID) == selfID {
			selfAddr = string(s.Address)
		}
	}
	if selfAddr == "" {
		t.Fatal("self address not found in configuration")
	}

	// Shrink to a single voter (self); both running nodes satisfy the old
	// 2-node quorum so the removal commits.
	desired := []RaftPeer{{ID: selfID, Address: selfAddr}}
	_, removed, err := leader.ReconcileMembership(desired, 5*time.Second)
	if err != nil {
		t.Fatalf("ReconcileMembership: %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("removed = %v, want 1 voter", removed)
	}
}

func TestNodeIDGeneratedAndReused(t *testing.T) {
	dir := t.TempDir()
	id1, err := loadOrGenerateNodeID(dir)
	if err != nil {
		t.Fatalf("loadOrGenerateNodeID: %v", err)
	}
	if id1 == "" {
		t.Fatal("empty node id")
	}
	id2, err := loadOrGenerateNodeID(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("node id not stable: %q vs %q", id1, id2)
	}
	info, err := os.Stat(filepath.Join(dir, "node-id"))
	if err != nil {
		t.Fatalf("stat node-id: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("node-id perm = %o, want 600", info.Mode().Perm())
	}
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// newTwoNodeRaftCluster builds two connected in-memory raft nodes and waits
// for one to win the election.
func newTwoNodeRaftCluster(t *testing.T) (a, b *RaftStore) {
	t.Helper()
	return newTwoNodeRaftClusterOpt(t, true)
}

// newTwoNodeRaftClusterOpt builds two connected in-memory raft nodes. When
// waitForLeader is false it returns immediately after bootstrap (before any
// leader is elected), exercising the startup wait paths.
func newTwoNodeRaftClusterOpt(t *testing.T, waitForLeader bool) (a, b *RaftStore) {
	t.Helper()

	node := func(id string) (*RaftStore, *raft.Raft, *raft.InmemTransport, raft.ServerAddress) {
		fsm := NewFSM()
		config := raft.DefaultConfig()
		config.LocalID = raft.ServerID(id)
		config.TrailingLogs = 256
		config.SnapshotInterval = 10 * time.Minute
		config.SnapshotThreshold = 1000
		config.LogOutput = io.Discard
		config.LogLevel = "WARN"
		config.HeartbeatTimeout = 200 * time.Millisecond
		config.ElectionTimeout = 200 * time.Millisecond
		config.LeaderLeaseTimeout = 100 * time.Millisecond
		config.CommitTimeout = 10 * time.Millisecond

		logs := raft.NewInmemStore()
		stable := raft.NewInmemStore()
		snaps := raft.NewInmemSnapshotStore()
		addr, trans := raft.NewInmemTransport(raft.NewInmemAddr())
		r, err := raft.NewRaft(config, fsm, logs, stable, snaps, trans)
		if err != nil {
			t.Fatalf("NewRaft %s: %v", id, err)
		}
		return &RaftStore{raft: r, fsm: fsm, timeout: 5 * time.Second}, r, trans, addr
	}

	store1, r1, t1, a1 := node("node-1")
	store2, r2, t2, a2 := node("node-2")
	t1.Connect(a2, t2)
	t2.Connect(a1, t1)

	configuration := raft.Configuration{Servers: []raft.Server{
		{Suffrage: raft.Voter, ID: raft.ServerID("node-1"), Address: a1},
		{Suffrage: raft.Voter, ID: raft.ServerID("node-2"), Address: a2},
	}}
	if err := r1.BootstrapCluster(configuration).Error(); err != nil {
		t.Fatalf("BootstrapCluster: %v", err)
	}

	if waitForLeader {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if r1.State() == raft.Leader || r2.State() == raft.Leader {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	t.Cleanup(func() {
		_ = r1.Shutdown().Error()
		_ = r2.Shutdown().Error()
		_ = t1.Close()
		_ = t2.Close()
	})
	return store1, store2
}

// messageFormatter emits only the entry message, one per line, so tests can
// assert on the exact set of flushed log lines.
type messageFormatter struct{}

func (messageFormatter) Format(e *logrus.Entry) ([]byte, error) {
	return []byte(e.Message + "\n"), nil
}

func TestLogrusWriterLineBuffering(t *testing.T) {
	var buf bytes.Buffer
	logger := logrus.New()
	logger.SetOutput(&buf)
	logger.SetFormatter(messageFormatter{})

	w := &logrusWriter{logger: logger}

	// A line split across Write calls must not be logged until '\n' arrives.
	if _, err := w.Write([]byte("hello ")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := w.Write([]byte("world")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := buf.String(); got != "" {
		t.Fatalf("partial line must be buffered, got %q", got)
	}

	if _, err := w.Write([]byte("\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, want := buf.String(), "hello world\n"; got != want {
		t.Fatalf("flushed line = %q, want %q", got, want)
	}
	buf.Reset()

	// Multiple complete lines in a single Write are flushed separately.
	if _, err := w.Write([]byte("line1\nline2\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, want := buf.String(), "line1\nline2\n"; got != want {
		t.Fatalf("multi-line flush = %q, want %q", got, want)
	}
	buf.Reset()

	// CRLF is stripped, surrounding whitespace trimmed, empty lines skipped.
	if _, err := w.Write([]byte("  spaced  \r\n\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, want := buf.String(), "spaced\n"; got != want {
		t.Fatalf("CRLF/empty-line handling = %q, want %q", got, want)
	}
}

func TestResolveAdvertiseAddrSuccess(t *testing.T) {
	addr, err := resolveAdvertiseAddr("127.0.0.1:8081", time.Second, io.Discard)
	if err != nil {
		t.Fatalf("resolveAdvertiseAddr: %v", err)
	}
	if addr == nil || !addr.IP.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("addr = %v, want 127.0.0.1", addr)
	}
}

func TestResolveAdvertiseAddrRetriesThenFails(t *testing.T) {
	// A fresh cluster's DNS may not resolve the pod FQDN yet. The pod must
	// retry within the budget instead of exiting (each exit restarts the
	// whole raft bootstrap sequence), then fail with the last resolve error.
	start := time.Now()
	if _, err := resolveAdvertiseAddr("pod-0.headless.ns.svc.cluster.local:8081", 500*time.Millisecond, io.Discard); err != nil {
		t.Logf("resolution failed after retry budget as expected: %v", err)
	} else {
		t.Log("advertise host unexpectedly resolvable in this environment; retry loop not exercised")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("retry budget not enforced: waited %v for a 500ms budget", elapsed)
	}
}
