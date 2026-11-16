package repository

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/disaster37/go-raft/discovery"
	"github.com/disaster37/go-raft/keyring"
	"github.com/disaster37/go-raft/pki"
	"github.com/hashicorp/raft"

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
	if err := store.WaitForCleanState(ctx); err != nil {
		t.Fatalf("single node leader should be in clean state: %v", err)
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
	stores := newRaftCluster(t, 2, false)
	leader := findLeader(t, stores)
	var follower *RaftStore
	for _, s := range stores {
		if s != leader {
			follower = s
		}
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

func TestRaftStoreLeaderAddress(t *testing.T) {
	// In-memory transport: the leader address is an opaque inmem UUID, but it
	// must be non-empty once a leader is elected.
	inmem := newTestRaftStore(t)
	if addr := inmem.LeaderAddress(); addr == "" {
		t.Fatal("LeaderAddress should be non-empty once a leader is elected")
	}

	// Real TCP transport: the leader address is host:port.
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
	addr := store.LeaderAddress()
	if addr == "" {
		t.Fatal("LeaderAddress should be non-empty once a leader is elected")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("LeaderAddress %q is not host:port: %v", addr, err)
	}
	if host == "" || port == "" {
		t.Fatalf("LeaderAddress %q missing host/port", addr)
	}
}

func TestRaftStoreLeaderAddressNoLeader(t *testing.T) {
	store := newTestRaftStore(t)
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if addr := store.LeaderAddress(); addr != "" {
		t.Fatalf("LeaderAddress after shutdown = %q, want empty", addr)
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
	store, err := NewRaftStore(&RaftStoreConfig{
		Dir:               filepath.Join(t.TempDir(), "data"),
		BindAddr:          freeTCPAddr(t),
		TLSEnabled:        true,
		TLSDir:            t.TempDir(),
		TLSOrganization:   "dagger-kubernetes-raft",
		ClientAuth:        true,
		BootstrapProvider: newTestBootstrapProvider(t),
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
	stores := newRaftCluster(t, 2, false)
	leader := findLeader(t, stores)
	var follower *RaftStore
	for _, s := range stores {
		if s != leader {
			follower = s
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := leader.WaitForLeader(ctx); err != nil {
		t.Fatalf("leader WaitForLeader: %v", err)
	}
	if err := follower.WaitForLeader(ctx); err != nil {
		t.Fatalf("follower WaitForLeader: %v", err)
	}

	if err := leader.WaitForSelfLeadership(ctx); err != nil {
		t.Fatalf("leader WaitForSelfLeadership: %v", err)
	}
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer shortCancel()
	if err := follower.WaitForSelfLeadership(shortCtx); err == nil {
		t.Fatal("follower WaitForSelfLeadership should time out")
	}
}

func TestRaftStoreAddVoterAndConfiguration(t *testing.T) {
	stores := newRaftCluster(t, 2, false)
	leader := findLeader(t, stores)

	cfg, err := leader.GetConfiguration()
	if err != nil {
		t.Fatalf("GetConfiguration: %v", err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("config servers = %d, want 2", len(cfg.Servers))
	}

	// Re-adding an existing voter is idempotent.
	if err := leader.AddVoter("node-0", string(cfg.Servers[0].Address), 5*time.Second); err != nil {
		t.Fatalf("AddVoter (idempotent): %v", err)
	}
}

func TestReconcileMembershipAddVoter(t *testing.T) {
	stores := newRaftCluster(t, 2, false)
	leader := findLeader(t, stores)
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
	desired := []discovery.Peer{
		{ID: "node-0", Address: addrByID["node-0"]},
		{ID: "node-1", Address: addrByID["node-1"]},
		{ID: "node-2", Address: "127.0.0.1:19999"},
	}
	added, updated, removed, err := leader.ReconcileMembership(desired, 5*time.Second)
	if err != nil {
		t.Fatalf("ReconcileMembership: %v", err)
	}
	if len(added) != 1 || added[0] != "node-2" {
		t.Fatalf("added = %v, want [node-2]", added)
	}
	if len(updated) != 0 {
		t.Fatalf("updated = %v, want empty", updated)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want empty", removed)
	}
}

func TestReconcileMembershipRemoveServer(t *testing.T) {
	stores := newRaftCluster(t, 2, false)
	leader := findLeader(t, stores)
	cfg, err := leader.GetConfiguration()
	if err != nil {
		t.Fatalf("GetConfiguration: %v", err)
	}
	selfID := leader.NodeID()
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
	desired := []discovery.Peer{{ID: selfID, Address: selfAddr}}
	added, updated, removed, err := leader.ReconcileMembership(desired, 5*time.Second)
	if err != nil {
		t.Fatalf("ReconcileMembership: %v", err)
	}
	if len(added) != 0 {
		t.Fatalf("added = %v, want empty", added)
	}
	if len(updated) != 0 {
		t.Fatalf("updated = %v, want empty", updated)
	}
	if len(removed) != 1 {
		t.Fatalf("removed = %v, want 1 voter", removed)
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

// TestRaftStoreAutoRenewsTLSKeyringAndLeaf proves the in-band keyring rotation
// advances to Term >= 3, seals the active private key at rest, and keeps
// replication running.
func TestRaftStoreAutoRenewsTLSKeyringAndLeaf(t *testing.T) {
	stores := newRaftClusterWithRotation(t, 3)

	deadline := time.Now().Add(180 * time.Second)
	var kr *keyring.TLSKeyring
	var leader *RaftStore
	for time.Now().Before(deadline) {
		for _, s := range stores {
			km := s.KeyringManager()
			if km == nil {
				continue
			}
			if cur := km.CurrentKeyring(); cur != nil && cur.Term >= 3 {
				kr = cur
				leader = s
				break
			}
		}
		if kr != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if kr == nil || kr.Term < 3 {
		t.Fatalf("keyring term = %v, want >= 3", kr)
	}
	active := kr.GetActive()
	if active == nil {
		t.Fatal("keyring has no active key")
	}
	if !strings.HasPrefix(active.KeyPEM, "{") {
		t.Fatalf("active keyring private key is not sealed: %q", active.KeyPEM)
	}

	// Replication continues after rotation. Re-find the leader and retry: a
	// rotation apply under -race load can coincide with a transient
	// leadership change.
	cmd, _ := newCommand(kindSetMeta, cmdSetMeta{Key: "rot", Value: "ok"})
	var applyErr error
	for attempt := 0; attempt < 20; attempt++ {
		leader = findLeader(t, stores)
		if applyErr = leader.apply(cmd); applyErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if applyErr != nil {
		t.Fatalf("apply after rotation: %v", applyErr)
	}
	for _, s := range stores {
		if s == leader {
			continue
		}
		waitForMetaValue(t, s, "rot", "ok")
	}
}

// TestRaftStoreManualTLSFileWatcherHotReload proves manual PEM mode hot-reloads
// renewed cert/key files via transport.FileWatcher.
func TestRaftStoreManualTLSFileWatcherHotReload(t *testing.T) {
	dir := t.TempDir()
	ca, err := pki.NewMintingCA("test-ca", "org", 10*365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCA: %v", err)
	}
	caCertPEM, _, err := ca.EncodePEM()
	if err != nil {
		t.Fatalf("EncodePEM: %v", err)
	}
	caPath := filepath.Join(dir, "ca.crt")
	certPath := filepath.Join(dir, "node.crt")
	keyPath := filepath.Join(dir, "node.key")
	if err := os.WriteFile(caPath, caCertPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	writeLeaf := func() *x509.Certificate {
		certPEM, keyPEM, err := ca.IssuePeerCertificate("node-0", "org", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, time.Hour)
		if err != nil {
			t.Fatalf("IssuePeerCertificate: %v", err)
		}
		if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
			t.Fatalf("write cert: %v", err)
		}
		if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
			t.Fatalf("write key: %v", err)
		}
		parsed, err := pki.ParsePEMCert(certPEM)
		if err != nil {
			t.Fatalf("ParsePEMCert: %v", err)
		}
		return parsed
	}
	initial := writeLeaf()

	store, err := NewRaftStore(&RaftStoreConfig{
		Dir:                filepath.Join(dir, "data"),
		BindAddr:           freeTCPAddr(t),
		TLSEnabled:         true,
		CACertPath:         caPath,
		CertPath:           certPath,
		KeyPath:            keyPath,
		ReloadPollInterval: 50 * time.Millisecond,
		ClientAuth:         true,
	}, testLogger())
	if err != nil {
		t.Fatalf("NewRaftStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	provider := store.TLSProvider()
	if provider == nil {
		t.Fatal("no TLS provider")
	}
	renewed := writeLeaf()
	if renewed.SerialNumber.Cmp(initial.SerialNumber) == 0 {
		t.Fatal("test precondition: renewed cert has the same serial")
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		cur, err := provider.GetCertificate(nil)
		if err == nil && len(cur.Certificate) > 0 {
			parsed, perr := x509.ParseCertificate(cur.Certificate[0])
			if perr == nil && parsed.SerialNumber.Cmp(renewed.SerialNumber) == 0 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("file watcher did not hot-reload the renewed certificate")
}

// TestRaftStoreStartupReissuesExpiringNodeCert proves a persisted node cert
// within the safety margin is re-issued at startup.
func TestRaftStoreStartupReissuesExpiringNodeCert(t *testing.T) {
	dir := t.TempDir()
	tlsDir := filepath.Join(dir, "tls")
	if err := os.MkdirAll(tlsDir, 0o700); err != nil {
		t.Fatalf("mkdir tls: %v", err)
	}
	ca, err := pki.NewMintingCA("test-ca", "org", 10*365*24*time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCA: %v", err)
	}
	caCertPEM, caKeyPEM, err := ca.EncodePEM()
	if err != nil {
		t.Fatalf("EncodePEM: %v", err)
	}
	hostname, _ := os.Hostname()
	// A node cert that covers the required SANs but expires within the 7d
	// safety margin.
	certPEM, keyPEM, err := ca.IssuePeerCertificate("node-0", "org", []string{hostname, "localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, time.Hour)
	if err != nil {
		t.Fatalf("IssuePeerCertificate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tlsDir, "node.crt"), certPEM, 0o600); err != nil {
		t.Fatalf("write node.crt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tlsDir, "node.key"), keyPEM, 0o600); err != nil {
		t.Fatalf("write node.key: %v", err)
	}
	oldCert, err := pki.ParsePEMCert(certPEM)
	if err != nil {
		t.Fatalf("ParsePEMCert: %v", err)
	}

	store, err := NewRaftStore(&RaftStoreConfig{
		Dir:               dir,
		NodeID:            "node-0",
		BindAddr:          freeTCPAddr(t),
		TLSEnabled:        true,
		TLSDir:            tlsDir,
		TLSValidity:       8760 * time.Hour,
		SafetyMargin:      7 * 24 * time.Hour,
		ClientAuth:        true,
		BootstrapProvider: &testBootstrapProvider{mat: &pki.BootstrapMaterial{CACertPEM: caCertPEM, CAKeyPEM: caKeyPEM}},
	}, testLogger())
	if err != nil {
		t.Fatalf("NewRaftStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	newCertPEM, err := os.ReadFile(filepath.Join(tlsDir, "node.crt"))
	if err != nil {
		t.Fatalf("read node.crt: %v", err)
	}
	newCert, err := pki.ParsePEMCert(newCertPEM)
	if err != nil {
		t.Fatalf("ParsePEMCert: %v", err)
	}
	if newCert.SerialNumber.Cmp(oldCert.SerialNumber) == 0 {
		t.Fatal("expiring node cert was not re-issued at startup")
	}
}
