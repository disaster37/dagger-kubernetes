package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/disaster37/go-raft/discovery"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

func newRaftStoreForTest(t *testing.T) *repository.RaftStore {
	t.Helper()
	store, err := repository.NewRaftStore(&repository.RaftStoreConfig{
		Dir:      t.TempDir(),
		BindAddr: freeAddr(t),
	}, observ.NewTestLogger())
	if err != nil {
		t.Fatalf("NewRaftStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.WaitForLeader(ctx); err != nil {
		t.Fatalf("WaitForLeader: %v", err)
	}
	return store
}

// freeAddr returns a loopback TCP address that is free at call time.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func newMetaStore(t *testing.T) *repository.MetaStore {
	t.Helper()
	return repository.NewMetaStore(newRaftStoreForTest(t))
}

// newTwoNodeRaftStores builds a real two-node raft cluster over loopback TCP
// (plaintext) and waits for a leader to be elected. The bootstrap node seeds
// the cluster with only itself; the follower joins via AddVoter (simulating the
// production joinLoop).
func newTwoNodeRaftStores(t *testing.T) (s1, s2 *repository.RaftStore) {
	t.Helper()
	addr1 := freeAddr(t)
	addr2 := freeAddr(t)
	peers := []discovery.Peer{
		{ID: "node-1", Address: addr1},
		{ID: "node-2", Address: addr2},
	}
	logger := observ.NewTestLogger()

	s1, err := repository.NewRaftStore(&repository.RaftStoreConfig{
		Dir:           filepath.Join(t.TempDir(), "node-1"),
		NodeID:        "node-1",
		BindAddr:      addr1,
		AdvertiseAddr: addr1,
		Resolver:      mustResolver(t, "node-1", addr1, peers),
	}, logger)
	if err != nil {
		t.Fatalf("NewRaftStore node-1: %v", err)
	}
	s2, err = repository.NewRaftStore(&repository.RaftStoreConfig{
		Dir:           filepath.Join(t.TempDir(), "node-2"),
		NodeID:        "node-2",
		BindAddr:      addr2,
		AdvertiseAddr: addr2,
		Resolver:      mustResolver(t, "node-2", addr2, peers),
	}, logger)
	if err != nil {
		t.Fatalf("NewRaftStore node-2: %v", err)
	}
	t.Cleanup(func() {
		_ = s1.Close()
		_ = s2.Close()
	})

	// Node-1 bootstraps with only itself (single-node quorum). Wait for it
	// to become leader, then add node-2 via AddVoter (simulating joinLoop).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s1.WaitForLeader(ctx); err != nil {
		t.Fatalf("WaitForLeader node-1: %v", err)
	}
	if err := s1.AddVoter("node-2", addr2, 5*time.Second); err != nil {
		t.Fatalf("AddVoter node-2: %v", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()
	if err := s2.WaitForLeader(ctx2); err != nil {
		t.Fatalf("WaitForLeader node-2: %v", err)
	}
	return s1, s2
}

func mustResolver(t *testing.T, nodeID, advertise string, peers []discovery.Peer) discovery.PeerResolver {
	t.Helper()
	return discovery.NewPeerResolver(&discovery.Config{
		NodeID:        nodeID,
		AdvertiseAddr: advertise,
		Peers:         peers,
	})
}

func TestLoadOrCreateJWTSecretConfiguredOK(t *testing.T) {
	ms := newMetaStore(t)
	secret := strings.Repeat("k", minJWTSecretLen)
	got, err := loadOrCreateJWTSecret(context.Background(), ms, secret, observ.NewTestLogger())
	if err != nil {
		t.Fatalf("loadOrCreateJWTSecret: %v", err)
	}
	if string(got) != secret {
		t.Fatalf("secret = %q, want configured", got)
	}
}

func TestLoadOrCreateJWTSecretTooShortRejected(t *testing.T) {
	ms := newMetaStore(t)
	if _, err := loadOrCreateJWTSecret(context.Background(), ms, "short", observ.NewTestLogger()); err == nil {
		t.Fatal("expected error for short secret (CWE-326)")
	}
}

func TestLoadOrCreateJWTSecretGeneratedAndPersisted(t *testing.T) {
	ms := newMetaStore(t)
	logger := observ.NewTestLogger()

	got, err := loadOrCreateJWTSecret(context.Background(), ms, "", logger)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(got) < minJWTSecretLen {
		t.Fatalf("generated secret too short: %d", len(got))
	}

	// Second call returns the persisted value (stable across restarts).
	again, err := loadOrCreateJWTSecret(context.Background(), ms, "", logger)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !bytes.Equal(again, got) {
		t.Fatal("generated secret must be persisted and reused")
	}
}

func TestLoadOrCreateTokenEncryptionKeyConfiguredDerived(t *testing.T) {
	ms := newMetaStore(t)
	// A configured key longer than 32 bytes must still yield a valid 32-byte
	// AES-256 key (AES-256 requires exactly 32 bytes).
	configured := strings.Repeat("k", 40)
	got, err := loadOrCreateTokenEncryptionKey(context.Background(), ms, configured, observ.NewTestLogger())
	if err != nil {
		t.Fatalf("loadOrCreateTokenEncryptionKey: %v", err)
	}
	if len(got) != 32 {
		t.Fatalf("derived key length = %d, want 32", len(got))
	}
	if _, err := aes.NewCipher(got); err != nil {
		t.Fatalf("derived key is not a valid AES key: %v", err)
	}
	if !bytes.Equal(got, deriveAESKey([]byte(configured))) {
		t.Fatal("configured key must be SHA-256-derived deterministically")
	}
}

func TestLoadOrCreateTokenEncryptionKeyTooShortRejected(t *testing.T) {
	ms := newMetaStore(t)
	if _, err := loadOrCreateTokenEncryptionKey(context.Background(), ms, "short", observ.NewTestLogger()); err == nil {
		t.Fatal("expected error for short encryption key (CWE-326)")
	}
}

func TestLoadOrCreateTokenEncryptionKeyGeneratedAndPersisted(t *testing.T) {
	ms := newMetaStore(t)
	logger := observ.NewTestLogger()

	got, err := loadOrCreateTokenEncryptionKey(context.Background(), ms, "", logger)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// The auto-generated meta value is a 64-char hex string; the returned AES
	// key must be a fixed 32 bytes usable by AES-256.
	if len(got) != 32 {
		t.Fatalf("generated key length = %d, want 32", len(got))
	}
	if _, err := aes.NewCipher(got); err != nil {
		t.Fatalf("generated key is not a valid AES key: %v", err)
	}

	// Second call returns the same derived key (stable across restarts).
	again, err := loadOrCreateTokenEncryptionKey(context.Background(), ms, "", logger)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !bytes.Equal(again, got) {
		t.Fatal("generated key must be persisted and reused")
	}
}

func TestValidateFleetEnv(t *testing.T) {
	tests := []struct {
		name    string
		fleet   domain.FleetConfig
		wantErr bool
	}{
		{
			name: "valid proxy map",
			fleet: domain.FleetConfig{
				EngineExtraEnv: map[string]string{
					"HTTP_PROXY": "http://proxy.corp.example:3128",
				},
			},
			wantErr: false,
		},
		{
			name: "DAGGER_KUBERNETES_TOKEN in engine_extra_env is no longer reserved",
			fleet: domain.FleetConfig{
				EngineExtraEnv: map[string]string{
					"DAGGER_KUBERNETES_TOKEN": "operator-supplied",
				},
			},
			wantErr: false,
		},
		{
			name: "empty env name in engine_extra_env",
			fleet: domain.FleetConfig{
				EngineExtraEnv: map[string]string{
					"": "no-name",
				},
			},
			wantErr: true,
		},
		{
			name: "EngineCASecret set with empty EngineCASecretKey",
			fleet: domain.FleetConfig{
				EngineCASecret:    "custom-ca-bundle",
				EngineCASecretKey: "",
			},
			wantErr: true,
		},
		{
			name: "valid engine_extra_env_from entry",
			fleet: domain.FleetConfig{
				EngineExtraEnvFrom: map[string]domain.EnvVarSource{
					"HTTP_PROXY": {SecretName: "proxy-credentials", Key: "http_proxy"},
				},
			},
			wantErr: false,
		},
		{
			name: "DAGGER_KUBERNETES_TOKEN in engine_extra_env_from is no longer reserved",
			fleet: domain.FleetConfig{
				EngineExtraEnvFrom: map[string]domain.EnvVarSource{
					"DAGGER_KUBERNETES_TOKEN": {SecretName: "s", Key: "k"},
				},
			},
			wantErr: false,
		},
		{
			name: "same name in both engine_extra_env and engine_extra_env_from",
			fleet: domain.FleetConfig{
				EngineExtraEnv: map[string]string{
					"HTTP_PROXY": "http://proxy:3128",
				},
				EngineExtraEnvFrom: map[string]domain.EnvVarSource{
					"HTTP_PROXY": {SecretName: "proxy-credentials", Key: "http_proxy"},
				},
			},
			wantErr: true,
		},
		{
			name: "engine_extra_env_from with empty SecretName",
			fleet: domain.FleetConfig{
				EngineExtraEnvFrom: map[string]domain.EnvVarSource{
					"HTTP_PROXY": {SecretName: "", Key: "http_proxy"},
				},
			},
			wantErr: true,
		},
		{
			name: "engine_extra_env_from with empty Key",
			fleet: domain.FleetConfig{
				EngineExtraEnvFrom: map[string]domain.EnvVarSource{
					"HTTP_PROXY": {SecretName: "proxy-credentials", Key: ""},
				},
			},
			wantErr: true,
		},
		{
			name: "empty name in engine_extra_env_from",
			fleet: domain.FleetConfig{
				EngineExtraEnvFrom: map[string]domain.EnvVarSource{
					"": {SecretName: "s", Key: "k"},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFleetEnv(&tt.fleet)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateFleetEnv() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestWaitForMetaSecret(t *testing.T) {
	t.Run("returns existing value", func(t *testing.T) {
		ms := newMetaStore(t)
		if err := ms.Set(context.Background(), "k", "v"); err != nil {
			t.Fatalf("Set: %v", err)
		}
		got, err := waitForMetaSecret(context.Background(), ms, "k", 2*time.Second)
		if err != nil {
			t.Fatalf("waitForMetaSecret: %v", err)
		}
		if string(got) != "v" {
			t.Fatalf("got %q, want v", got)
		}
	})

	t.Run("times out when absent", func(t *testing.T) {
		ms := newMetaStore(t)
		start := time.Now()
		if _, err := waitForMetaSecret(context.Background(), ms, "missing", 200*time.Millisecond); err == nil {
			t.Fatal("expected timeout")
		} else if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
			t.Fatalf("returned too early: %v", elapsed)
		}
	})
}

func TestResolveJWTSecretLeaderAndConfigured(t *testing.T) {
	logger := observ.NewTestLogger()

	t.Run("leader provisions", func(t *testing.T) {
		store := newRaftStoreForTest(t)
		if !store.IsLeader() {
			t.Fatal("single-node store should be leader")
		}
		ms := repository.NewMetaStore(store)
		got, err := resolveJWTSecret(context.Background(), store, ms, "", 5*time.Second, logger)
		if err != nil {
			t.Fatalf("resolveJWTSecret: %v", err)
		}
		if len(got) < minJWTSecretLen {
			t.Fatalf("secret too short: %d", len(got))
		}
	})

	t.Run("configured short-circuits", func(t *testing.T) {
		store := newRaftStoreForTest(t)
		ms := repository.NewMetaStore(store)
		secret := strings.Repeat("k", minJWTSecretLen)
		got, err := resolveJWTSecret(context.Background(), store, ms, secret, 5*time.Second, logger)
		if err != nil {
			t.Fatalf("resolveJWTSecret: %v", err)
		}
		if string(got) != secret {
			t.Fatalf("got %q, want configured", got)
		}
	})
}

func TestResolveJWTSecretFollowerWaitsForMeta(t *testing.T) {
	logger := observ.NewTestLogger()
	s1, s2 := newTwoNodeRaftStores(t)
	var leader, follower *repository.RaftStore
	switch {
	case s1.IsLeader():
		leader, follower = s1, s2
	case s2.IsLeader():
		leader, follower = s2, s1
	default:
		t.Fatal("no leader elected")
	}

	// The leader provisions the JWT secret; the follower waits for replication
	// instead of writing (and must not exit).
	leaderMS := repository.NewMetaStore(leader)
	provisioned, err := loadOrCreateJWTSecret(context.Background(), leaderMS, "", logger)
	if err != nil {
		t.Fatalf("leader provision: %v", err)
	}

	followerMS := repository.NewMetaStore(follower)
	got, err := resolveJWTSecret(context.Background(), follower, followerMS, "", 10*time.Second, logger)
	if err != nil {
		t.Fatalf("follower resolveJWTSecret: %v", err)
	}
	if !bytes.Equal(got, provisioned) {
		t.Fatal("follower JWT secret must equal the leader's provisioned value")
	}

	// Token-encryption key follows the same path.
	leaderKey, err := loadOrCreateTokenEncryptionKey(context.Background(), leaderMS, "", logger)
	if err != nil {
		t.Fatalf("leader key: %v", err)
	}
	gotKey, err := resolveTokenEncryptionKey(context.Background(), follower, followerMS, "", 10*time.Second, logger)
	if err != nil {
		t.Fatalf("follower resolveTokenEncryptionKey: %v", err)
	}
	if !bytes.Equal(gotKey, leaderKey) {
		t.Fatal("follower token key must equal the leader's provisioned key")
	}
}

func TestRaftCABootstrap(t *testing.T) {
	tests := []struct {
		name     string
		tlsCfg   domain.RaftTLSConfig
		replicas int
		hostname string
		want     bool
	}{
		{
			name:     "single node always bootstraps",
			tlsCfg:   domain.RaftTLSConfig{},
			replicas: 1,
			hostname: "a1b2c3d4-0000-0000-0000-000000000000",
			want:     true,
		},
		{
			name:     "ordinal zero auto-detected",
			tlsCfg:   domain.RaftTLSConfig{},
			replicas: 3,
			hostname: "dagger-kubernetes-supervisor-0",
			want:     true,
		},
		{
			name:     "higher ordinal not bootstrap",
			tlsCfg:   domain.RaftTLSConfig{},
			replicas: 3,
			hostname: "dagger-kubernetes-supervisor-1",
			want:     false,
		},
		{
			name:     "two digit ordinal not bootstrap",
			tlsCfg:   domain.RaftTLSConfig{},
			replicas: 3,
			hostname: "dagger-kubernetes-supervisor-10",
			want:     false,
		},
		{
			name:     "explicit ca_bootstrap wins",
			tlsCfg:   domain.RaftTLSConfig{CABootstrap: true},
			replicas: 3,
			hostname: "dagger-kubernetes-supervisor-1",
			want:     true,
		},
		{
			name:     "non-k8s uuid not bootstrap (multi-node)",
			tlsCfg:   domain.RaftTLSConfig{},
			replicas: 3,
			hostname: "a1b2c3d4-0000-0000-0000-000000000000",
			want:     false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &domain.Config{Raft: domain.RaftConfig{Replicas: tc.replicas, TLS: tc.tlsCfg}}
			if got := raftCABootstrap(cfg, tc.hostname); got != tc.want {
				t.Fatalf("raftCABootstrap(%q) = %v, want %v", tc.hostname, got, tc.want)
			}
		})
	}
}

func TestMintingCABootstrap(t *testing.T) {
	tests := []struct {
		name     string
		replicas int
		peers    []domain.RaftPeer
		hostname string
		want     bool
	}{
		{
			name:     "single node always bootstraps",
			replicas: 1,
			hostname: "supervisor-5f8c9d7b6-abc12",
			want:     true,
		},
		{
			name:     "single explicit peer bootstraps",
			replicas: 1,
			peers:    []domain.RaftPeer{{ID: "self", Address: "self:8081"}},
			hostname: "supervisor-5f8c9d7b6-abc12",
			want:     true,
		},
		{
			name:     "ordinal zero auto-detected",
			replicas: 3,
			hostname: "dagger-kubernetes-supervisor-0",
			want:     true,
		},
		{
			name:     "higher ordinal not bootstrap",
			replicas: 3,
			hostname: "dagger-kubernetes-supervisor-1",
			want:     false,
		},
		{
			name:     "two digit ordinal not bootstrap",
			replicas: 3,
			hostname: "dagger-kubernetes-supervisor-10",
			want:     false,
		},
		{
			name:     "non-k8s pod name not bootstrap (multi-node)",
			replicas: 3,
			hostname: "supervisor-5f8c9d7b6-abc12",
			want:     false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &domain.Config{Raft: domain.RaftConfig{Replicas: tc.replicas, Peers: tc.peers}}
			if got := mintingCABootstrap(cfg, tc.hostname); got != tc.want {
				t.Fatalf("mintingCABootstrap(%q) = %v, want %v", tc.hostname, got, tc.want)
			}
		})
	}
}

func TestSelectTLSProvider(t *testing.T) {
	newDynamicCA := func(t *testing.T, cfg *domain.Config, clientset kubernetes.Interface) *repository.DynamicMintingCA {
		t.Helper()
		minting := mintingProvider(cfg, clientset)
		dynamicCA, err := repository.NewDynamicMintingCA(repository.DynamicMintingCAConfig{
			Provider: minting,
			Logger:   observ.NewTestLogger(),
		})
		if err != nil {
			t.Fatalf("NewDynamicMintingCA: %v", err)
		}
		return dynamicCA
	}

	t.Run("embedded returns provider that shares minting CA via secret", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		cfg := &domain.Config{
			CA:         domain.CAConfig{MintingCASecret: "minting-ca", ClientCertTTL: 2 * time.Hour},
			Supervisor: domain.SupervisorConfig{Dataplane: domain.SupervisorDataplaneConfig{TLS: domain.TLSConfig{Provider: "embedded", CAPath: t.TempDir()}}},
			Raft:       domain.RaftConfig{Replicas: 1},
			Fleet:      domain.FleetConfig{Namespace: "ns"},
			Server:     domain.ServerConfig{DataHost: "data.example.com"},
		}
		p, err := selectTLSProvider(cfg, newDynamicCA(t, cfg, clientset))
		if err != nil {
			t.Fatalf("selectTLSProvider: %v", err)
		}
		if _, err := p.MintingCA(); err != nil {
			t.Fatalf("MintingCA: %v", err)
		}
		if _, err := clientset.CoreV1().Secrets("ns").Get(context.Background(), "minting-ca", metav1.GetOptions{}); err != nil {
			t.Fatalf("minting-ca secret not created: %v", err)
		}
	})

	t.Run("cert-manager still bootstraps the minting CA", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		cfg := &domain.Config{
			CA:         domain.CAConfig{MintingCASecret: "minting-ca", ClientCertTTL: 2 * time.Hour},
			Supervisor: domain.SupervisorConfig{Dataplane: domain.SupervisorDataplaneConfig{TLS: domain.TLSConfig{Provider: "cert-manager", CAPath: t.TempDir(), CertPath: "unused", KeyPath: "unused"}}},
			Raft:       domain.RaftConfig{Replicas: 1},
			Fleet:      domain.FleetConfig{Namespace: "ns"},
			Server:     domain.ServerConfig{DataHost: "data.example.com"},
		}
		p, err := selectTLSProvider(cfg, newDynamicCA(t, cfg, clientset))
		if err != nil {
			t.Fatalf("selectTLSProvider: %v", err)
		}
		if _, err := p.MintingCA(); err != nil {
			t.Fatalf("MintingCA: %v", err)
		}
		if _, err := clientset.CoreV1().Secrets("ns").Get(context.Background(), "minting-ca", metav1.GetOptions{}); err != nil {
			t.Fatalf("minting-ca secret not created: %v", err)
		}
	})

	t.Run("unknown provider errors", func(t *testing.T) {
		cfg := &domain.Config{Supervisor: domain.SupervisorConfig{Dataplane: domain.SupervisorDataplaneConfig{TLS: domain.TLSConfig{Provider: "bogus"}}}}
		if _, err := selectTLSProvider(cfg, nil); err == nil {
			t.Fatal("expected error for unknown provider")
		}
	})
}

func TestValidateMigrateTokensSingleNode(t *testing.T) {
	tests := []struct {
		name    string
		raft    domain.RaftConfig
		wantErr bool
	}{
		{
			name:    "single node ok",
			raft:    domain.RaftConfig{Replicas: 1},
			wantErr: false,
		},
		{
			name: "single explicit peer ok",
			raft: domain.RaftConfig{
				Replicas: 1,
				Peers:    []domain.RaftPeer{{ID: "a", Address: "a:8081"}},
			},
			wantErr: false,
		},
		{
			name:    "multi-node replicas rejected",
			raft:    domain.RaftConfig{Replicas: 3},
			wantErr: true,
		},
		{
			name: "multi-node peers rejected",
			raft: domain.RaftConfig{
				Replicas: 1,
				Peers:    []domain.RaftPeer{{ID: "a", Address: "a:8081"}, {ID: "b", Address: "b:8081"}},
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &domain.Config{Raft: tc.raft}
			err := validateMigrateTokensSingleNode(cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateMigrateTokensSingleNode() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateRaftConfig(t *testing.T) {
	base := func() *domain.Config {
		return &domain.Config{
			Raft: domain.RaftConfig{
				Replicas: 1,
				TLS:      domain.RaftTLSConfig{ClientAuth: true},
			},
		}
	}

	tests := []struct {
		name      string
		mut       func(*domain.Config)
		clientset kubernetes.Interface
		wantErr   bool
	}{
		{
			name: "single node plaintext ok",
		},
		{
			name: "multi-node with peers ok",
			mut: func(c *domain.Config) {
				c.Raft.Replicas = 3
				c.Raft.Peers = []domain.RaftPeer{{ID: "a", Address: "a:8081"}}
			},
		},
		{
			name:    "multi-node requires statefulset or peers",
			mut:     func(c *domain.Config) { c.Raft.Replicas = 3 },
			wantErr: true,
		},
		{
			// FQDN-only discovery: a StatefulSet name is sufficient for
			// multi-node — no K8s clientset or pod IP is required.
			name: "multi-node with statefulset and no clientset ok",
			mut: func(c *domain.Config) {
				c.Raft.Replicas = 3
				c.Raft.StatefulSetName = "sts"
				c.Raft.HeadlessService = "headless"
				c.Raft.Namespace = "ns"
			},
			clientset: nil,
		},
		{
			name: "tls manual all set",
			mut: func(c *domain.Config) {
				c.Raft.TLS.Enabled = true
				c.Raft.TLS.CACertPath = "/ca"
				c.Raft.TLS.CertPath = "/cert"
				c.Raft.TLS.KeyPath = "/key"
			},
		},
		{
			name: "tls manual missing key",
			mut: func(c *domain.Config) {
				c.Raft.TLS.Enabled = true
				c.Raft.TLS.CACertPath = "/ca"
				c.Raft.TLS.CertPath = "/cert"
			},
			wantErr: true,
		},
		{
			name: "tls auto multi-node without k8s",
			mut: func(c *domain.Config) {
				c.Raft.TLS.Enabled = true
				c.Raft.Replicas = 3
				c.Raft.Peers = []domain.RaftPeer{{ID: "a", Address: "a:8081"}}
			},
			clientset: nil,
			wantErr:   true,
		},
		{
			name: "loopback advertise multi-node rejected",
			mut: func(c *domain.Config) {
				c.Raft.Replicas = 3
				c.Raft.AdvertiseAddr = "127.0.0.1:8081"
				c.Raft.Peers = []domain.RaftPeer{{ID: "a", Address: "a:8081"}}
			},
			wantErr: true,
		},
		{
			name: "routable advertise multi-node ok",
			mut: func(c *domain.Config) {
				c.Raft.Replicas = 3
				c.Raft.AdvertiseAddr = "node-0.headless.ns.svc.cluster.local:8081"
				c.Raft.Peers = []domain.RaftPeer{{ID: "a", Address: "a:8081"}}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			if tc.mut != nil {
				tc.mut(cfg)
			}
			err := validateRaftConfig(cfg, tc.clientset)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateRaftConfig() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestRaftDiscoveryConfig(t *testing.T) {
	cfg := &domain.Config{
		Raft: domain.RaftConfig{
			NodeID:          "n",
			BindAddr:        ":9090",
			AdvertiseAddr:   "a:9090",
			Replicas:        3,
			StatefulSetName: "sts",
			HeadlessService: "headless",
			Namespace:       "ns",
			ClusterDomain:   "cluster.local",
		},
		Fleet: domain.FleetConfig{Namespace: "fleet-ns"},
	}
	d := raftDiscoveryConfig(cfg)
	// The raft port is derived from BindAddr by the repository's raftPort
	// (covered in raft_discovery_test.go), so RaftPort stays zero here.
	if d.BindAddr != ":9090" {
		t.Fatalf("BindAddr = %q, want :9090", d.BindAddr)
	}
	if d.Namespace != "ns" {
		t.Fatalf("Namespace = %q, want ns", d.Namespace)
	}
	if d.Replicas != 3 || d.StatefulSetName != "sts" || d.HeadlessService != "headless" {
		t.Fatalf("discovery config = %+v", d)
	}

	// Namespace falls back to fleet namespace.
	cfg2 := &domain.Config{
		Raft:  domain.RaftConfig{BindAddr: ":8081"},
		Fleet: domain.FleetConfig{Namespace: "fleet-ns"},
	}
	d2 := raftDiscoveryConfig(cfg2)
	if d2.Namespace != "fleet-ns" {
		t.Fatalf("Namespace fallback = %q, want fleet-ns", d2.Namespace)
	}
	if d2.BindAddr != ":8081" {
		t.Fatalf("BindAddr = %q, want :8081", d2.BindAddr)
	}
}

func TestIsMintingCAOnPerPodStorage(t *testing.T) {
	tests := []struct {
		name   string
		caPath string
		dbDir  string
		want   bool
	}{
		{"default under db dir", "/var/lib/dagger-kubernetes/ca", "/var/lib/dagger-kubernetes", true},
		{"nested under db dir", "/var/lib/dagger-kubernetes/sub/ca", "/var/lib/dagger-kubernetes", true},
		{"outside db dir", "/etc/dagger-kubernetes/ca", "/var/lib/dagger-kubernetes", false},
		{"empty ca path", "", "/var/lib/dagger-kubernetes", false},
		{"empty db dir", "/var/lib/dagger-kubernetes/ca", "", false},
		{"same path", "/var/lib/dagger-kubernetes", "/var/lib/dagger-kubernetes", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &domain.Config{
				Database:   domain.DatabaseConfig{Dir: tc.dbDir},
				Supervisor: domain.SupervisorConfig{Dataplane: domain.SupervisorDataplaneConfig{TLS: domain.TLSConfig{CAPath: tc.caPath}}},
			}
			if got := isMintingCAOnPerPodStorage(cfg); got != tc.want {
				t.Fatalf("isMintingCAOnPerPodStorage = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- bootstrap admin password file (SEC-009 / CWE-532) ---

// newBootstrapUserSvc builds a UserService over a single-node in-memory Raft
// store (leader after WaitForLeader).
func newBootstrapUserSvc(t *testing.T) *service.UserService {
	t.Helper()
	store, err := repository.NewInmemRaftStore("bootstrap-admin-test", observ.NewTestLogger(), 5*time.Second)
	if err != nil {
		t.Fatalf("NewInmemRaftStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WaitForLeader(ctx); err != nil {
		t.Fatalf("WaitForLeader: %v", err)
	}
	return service.NewUserService(repository.NewUserRepo(store), repository.NewGroupRepo(store), observ.NewTestLogger())
}

// newBootstrapTest returns a bootstrap config rooted at a temp database dir,
// a UserService, and a logger capturing output into buf.
func newBootstrapTest(t *testing.T) (*domain.Config, *service.UserService, *bytes.Buffer, *logrus.Logger) {
	t.Helper()
	cfg := &domain.Config{}
	cfg.Database.Dir = t.TempDir()
	cfg.Auth.BootstrapAdmin.Username = "admin"
	buf := &bytes.Buffer{}
	logger := logrus.New()
	logger.SetOutput(buf)
	logger.SetFormatter(&logrus.TextFormatter{DisableColors: true, DisableTimestamp: true})
	return cfg, newBootstrapUserSvc(t), buf, logger
}

func TestBootstrapAdminWritesGeneratedPasswordFile(t *testing.T) {
	cfg, users, buf, logger := newBootstrapTest(t)
	ctx := context.Background()

	if err := bootstrapAdmin(ctx, cfg, users, logger); err != nil {
		t.Fatalf("bootstrapAdmin: %v", err)
	}

	path := filepath.Join(cfg.Database.Dir, "bootstrap-admin-password")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("password file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("password file mode = %04o, want 0600", fi.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read password file: %v", err)
	}
	password := strings.TrimSuffix(string(raw), "\n")
	if password == "" {
		t.Fatal("password file is empty")
	}

	// The credential never appears in the log; the recovery path does.
	out := buf.String()
	if strings.Contains(out, password) {
		t.Fatalf("generated password leaked into log output: %s", out)
	}
	if !strings.Contains(out, "bootstrap admin created") {
		t.Fatalf("missing bootstrap WARN: %s", out)
	}
	if !strings.Contains(out, path) {
		t.Fatalf("log should name the password file path: %s", out)
	}

	// The account is usable with the file's password.
	if _, err := users.Authenticate(ctx, "admin", password); err != nil {
		t.Fatalf("authenticate with generated password: %v", err)
	}
}

func TestBootstrapAdminConfiguredPasswordNotWritten(t *testing.T) {
	cfg, users, buf, logger := newBootstrapTest(t)
	cfg.Auth.BootstrapAdmin.Password = "configuredpw123"

	if err := bootstrapAdmin(context.Background(), cfg, users, logger); err != nil {
		t.Fatalf("bootstrapAdmin: %v", err)
	}

	path := filepath.Join(cfg.Database.Dir, "bootstrap-admin-password")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("password file should not exist for a configured password: %v", err)
	}
	if strings.Contains(buf.String(), "configuredpw123") {
		t.Fatalf("configured password leaked into log output: %s", buf.String())
	}
	if _, err := users.Authenticate(context.Background(), "admin", "configuredpw123"); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
}

func TestBootstrapAdminCustomPasswordFile(t *testing.T) {
	cfg, users, buf, logger := newBootstrapTest(t)
	custom := filepath.Join(t.TempDir(), "sub", "admin.pw")
	cfg.Auth.BootstrapAdmin.PasswordFile = custom

	if err := bootstrapAdmin(context.Background(), cfg, users, logger); err != nil {
		t.Fatalf("bootstrapAdmin: %v", err)
	}

	fi, err := os.Stat(custom)
	if err != nil {
		t.Fatalf("custom password file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("custom password file mode = %04o, want 0600", fi.Mode().Perm())
	}
	if !strings.Contains(buf.String(), custom) {
		t.Fatalf("log should name the custom path: %s", buf.String())
	}
	raw, err := os.ReadFile(custom)
	if err != nil {
		t.Fatalf("read custom file: %v", err)
	}
	if strings.Contains(buf.String(), strings.TrimSuffix(string(raw), "\n")) {
		t.Fatal("generated password leaked into log output")
	}
}

func TestBootstrapAdminUnwritablePasswordFileFails(t *testing.T) {
	cfg, users, _, logger := newBootstrapTest(t)
	// Parent path is an existing regular file: MkdirAll must fail.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	cfg.Auth.BootstrapAdmin.PasswordFile = filepath.Join(blocker, "admin.pw")

	err := bootstrapAdmin(context.Background(), cfg, users, logger)
	if err == nil || !strings.Contains(err.Error(), "create bootstrap password dir") {
		t.Fatalf("err = %v, want wrapped mkdir failure", err)
	}
	// No account may exist whose password is unrecoverable.
	if _, err := users.GetByUsername(context.Background(), "admin"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("user should not be created when the recovery file fails: %v", err)
	}
}

func TestBootstrapAdminIdempotentSecondBoot(t *testing.T) {
	cfg, users, _, logger := newBootstrapTest(t)
	if err := bootstrapAdmin(context.Background(), cfg, users, logger); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	path := filepath.Join(cfg.Database.Dir, "bootstrap-admin-password")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read password file: %v", err)
	}
	// Second boot with users present: no-op, file untouched.
	if err := bootstrapAdmin(context.Background(), cfg, users, logger); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read password file: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("password file rewritten on second boot")
	}
}

// TestSyncCASecretsAfterRotation proves the settled Minting CA is written back
// to its Kubernetes Secret after rotation so future scale-up pods bootstrap
// with the current CA.
func TestSyncCASecretsAfterRotation(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	dir := t.TempDir()
	provider := repository.NewEmbeddedProvider(dir, 2*time.Hour)
	sealer, err := repository.DeriveSealer("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("DeriveSealer: %v", err)
	}
	dynamicCA, err := repository.NewDynamicMintingCA(repository.DynamicMintingCAConfig{
		Provider: provider,
		Sealer:   sealer,
		Logger:   observ.NewTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewDynamicMintingCA: %v", err)
	}

	store := newRaftStoreForTest(t)
	store.SetMetaHook(func(key, value string) {
		if key == repository.MintingKeyringMetaKey {
			if err := dynamicCA.SetKeyringJSON(value); err != nil {
				t.Errorf("SetKeyringJSON: %v", err)
			}
		}
	})

	now := time.Now()
	rotator := repository.NewMintingRotator(&repository.MintingRotatorConfig{
		Store:          store,
		CA:             dynamicCA,
		Bootstrap:      provider.BootstrapProvider(),
		Sealer:         sealer,
		ClientCertTTL:  2 * time.Hour,
		RotationPeriod: time.Hour,
		CheckInterval:  time.Second,
		CommonName:     "dagger-kubernetes-minting-ca",
		Organization:   "dagger-kubernetes",
		CAValidity:     30 * 365 * 24 * time.Hour,
		Logger:         observ.NewTestLogger(),
		Now:            func() time.Time { return now },
	})
	ctx := context.Background()
	if err := rotator.RotateOnce(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	now = now.Add(2 * time.Hour)
	if err := rotator.RotateOnce(ctx); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := rotator.RotateOnce(ctx); err != nil {
		t.Fatalf("activate: %v", err)
	}

	syncMintingCASecret(ctx, clientset, "ns", "minting-ca", dynamicCA, observ.NewTestLogger())
	secret, err := clientset.CoreV1().Secrets("ns").Get(ctx, "minting-ca", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("minting-ca secret not created: %v", err)
	}
	if len(secret.Data["ca.crt"]) == 0 || len(secret.Data["ca.key"]) == 0 {
		t.Fatal("minting-ca secret missing ca.crt/ca.key")
	}
}

// newDynamicCAForTest builds a DynamicMintingCA over a fresh provider/sealer.
func newDynamicCAForTest(t *testing.T) *repository.DynamicMintingCA {
	t.Helper()
	provider := repository.NewEmbeddedProvider(t.TempDir(), 2*time.Hour)
	sealer, err := repository.DeriveSealer("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("DeriveSealer: %v", err)
	}
	dynamicCA, err := repository.NewDynamicMintingCA(repository.DynamicMintingCAConfig{
		Provider: provider,
		Sealer:   sealer,
		Logger:   observ.NewTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewDynamicMintingCA: %v", err)
	}
	return dynamicCA
}

// TestSyncMintingCAConfigMapBootstrap proves the ConfigMap is created with the
// single bootstrap CA certificate (key ca.crt) when no keyring is installed.
func TestSyncMintingCAConfigMapBootstrap(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	dynamicCA := newDynamicCAForTest(t)
	ctx := context.Background()

	syncMintingCAConfigMap(ctx, clientset, "ns", "minting-ca-bundle", dynamicCA, observ.NewTestLogger())
	cm, err := clientset.CoreV1().ConfigMaps("ns").Get(ctx, "minting-ca-bundle", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("minting-ca-bundle configmap not created: %v", err)
	}
	if cm.Data["ca.crt"] == "" {
		t.Fatal("minting-ca-bundle missing ca.crt")
	}
	if strings.Contains(cm.Data["ca.crt"], "PRIVATE KEY") {
		t.Fatal("minting-ca-bundle must never contain a private key")
	}
}

// TestSyncMintingCAConfigMapPreservesHelmShell proves the sync populates ca.crt
// on a pre-existing Helm-rendered shell (with labels/annotations, nil Data),
// preserves those labels/annotations, and is a no-op on a second call.
func TestSyncMintingCAConfigMapPreservesHelmShell(t *testing.T) {
	shell := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "minting-ca-bundle",
			Namespace:   "ns",
			Labels:      map[string]string{"app.kubernetes.io/managed-by": "Helm", "helm.sh/chart": "dagger-kubernetes-0.1.1"},
			Annotations: map[string]string{"meta.helm.sh/release-name": "dagger-kubernetes"},
		},
	}
	clientset := fake.NewSimpleClientset(shell)
	dynamicCA := newDynamicCAForTest(t)
	ctx := context.Background()

	syncMintingCAConfigMap(ctx, clientset, "ns", "minting-ca-bundle", dynamicCA, observ.NewTestLogger())
	cm, err := clientset.CoreV1().ConfigMaps("ns").Get(ctx, "minting-ca-bundle", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	if cm.Data["ca.crt"] == "" {
		t.Fatal("ca.crt not populated on the Helm shell")
	}
	if cm.Labels["app.kubernetes.io/managed-by"] != "Helm" || cm.Labels["helm.sh/chart"] != "dagger-kubernetes-0.1.1" {
		t.Fatalf("Helm labels not preserved: %v", cm.Labels)
	}
	if cm.Annotations["meta.helm.sh/release-name"] != "dagger-kubernetes" {
		t.Fatalf("Helm annotations not preserved: %v", cm.Annotations)
	}

	// Second call with the same bundle must be an idempotent no-op (no new
	// ResourceVersion, so the API server performs no write).
	rv := cm.ResourceVersion
	syncMintingCAConfigMap(ctx, clientset, "ns", "minting-ca-bundle", dynamicCA, observ.NewTestLogger())
	cm2, err := clientset.CoreV1().ConfigMaps("ns").Get(ctx, "minting-ca-bundle", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-get configmap: %v", err)
	}
	if cm2.ResourceVersion != rv {
		t.Fatalf("second sync rewrote the ConfigMap (rv %q -> %q)", rv, cm2.ResourceVersion)
	}
}

// TestSyncMintingCAConfigMapEmptyNameNoOp proves an empty config-map name
// disables publishing (mirrors ca.minting_ca_secret == "").
func TestSyncMintingCAConfigMapEmptyNameNoOp(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	dynamicCA := newDynamicCAForTest(t)
	ctx := context.Background()

	syncMintingCAConfigMap(ctx, clientset, "ns", "", dynamicCA, observ.NewTestLogger())
	list, err := clientset.CoreV1().ConfigMaps("ns").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list configmaps: %v", err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("empty name must not create any ConfigMap, got %d", len(list.Items))
	}
}

// TestSyncMintingCAConfigMapRotation proves the bundle tracks the phased
// rotation: [M0] seeded, [M0, M1] during stage/activate, [M1] after pruning.
func TestSyncMintingCAConfigMapRotation(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	provider := repository.NewEmbeddedProvider(t.TempDir(), 2*time.Hour)
	sealer, err := repository.DeriveSealer("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("DeriveSealer: %v", err)
	}
	dynamicCA, err := repository.NewDynamicMintingCA(repository.DynamicMintingCAConfig{
		Provider: provider,
		Sealer:   sealer,
		Logger:   observ.NewTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewDynamicMintingCA: %v", err)
	}

	store := newRaftStoreForTest(t)
	store.SetMetaHook(func(key, value string) {
		if key == repository.MintingKeyringMetaKey {
			if err := dynamicCA.SetKeyringJSON(value); err != nil {
				t.Errorf("SetKeyringJSON: %v", err)
			}
		}
	})

	now := time.Now()
	rotator := repository.NewMintingRotator(&repository.MintingRotatorConfig{
		Store:          store,
		CA:             dynamicCA,
		Bootstrap:      provider.BootstrapProvider(),
		Sealer:         sealer,
		ClientCertTTL:  2 * time.Hour,
		RotationPeriod: time.Hour,
		CheckInterval:  time.Second,
		CommonName:     "dagger-kubernetes-minting-ca",
		Organization:   "dagger-kubernetes",
		CAValidity:     30 * 365 * 24 * time.Hour,
		Logger:         observ.NewTestLogger(),
		Now:            func() time.Time { return now },
	})
	ctx := context.Background()

	certCount := func(t *testing.T) int {
		t.Helper()
		cm, err := clientset.CoreV1().ConfigMaps("ns").Get(ctx, "minting-ca-bundle", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get configmap: %v", err)
		}
		return strings.Count(cm.Data["ca.crt"], "BEGIN CERTIFICATE")
	}

	// Seed [M0].
	if err := rotator.RotateOnce(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	syncMintingCAConfigMap(ctx, clientset, "ns", "minting-ca-bundle", dynamicCA, observ.NewTestLogger())
	if got := certCount(t); got != 1 {
		t.Fatalf("seed bundle certs = %d, want 1", got)
	}

	// Stage + Activate [M0, M1].
	now = now.Add(2 * time.Hour)
	if err := rotator.RotateOnce(ctx); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := rotator.RotateOnce(ctx); err != nil {
		t.Fatalf("activate: %v", err)
	}
	syncMintingCAConfigMap(ctx, clientset, "ns", "minting-ca-bundle", dynamicCA, observ.NewTestLogger())
	if got := certCount(t); got != 2 {
		t.Fatalf("overlap bundle certs = %d, want 2", got)
	}

	// Prune [M1].
	now = now.Add(2*2*time.Hour + time.Hour)
	if err := rotator.RotateOnce(ctx); err != nil {
		t.Fatalf("prune: %v", err)
	}
	syncMintingCAConfigMap(ctx, clientset, "ns", "minting-ca-bundle", dynamicCA, observ.NewTestLogger())
	if got := certCount(t); got != 1 {
		t.Fatalf("pruned bundle certs = %d, want 1", got)
	}
}

// TestSyncCASecretLoopImmediateAndNotify proves the loop runs an immediate
// leader pass on entry (populating the bundle before any tick) and re-syncs on
// a caSyncNotify wake-up.
func TestSyncCASecretLoopImmediateAndNotify(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	store := newRaftStoreForTest(t)
	if !store.IsLeader() {
		t.Skip("single-node store did not become leader")
	}
	dynamicCA := newDynamicCAForTest(t)
	cfg := &domain.Config{}
	cfg.Raft.Namespace = "ns"
	cfg.CA.MintingCASecret = "minting-ca"
	cfg.CA.MintingCAConfigMap = "minting-ca-bundle"
	cfg.Raft.TLS.CASecret = ""

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notify := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		syncCASecretLoop(ctx, cfg, clientset, store, dynamicCA, notify, observ.NewTestLogger())
		close(done)
	}()

	// Immediate pass: the bundle appears without any ticker/notify.
	deadline := time.After(10 * time.Second)
	for {
		cm, err := clientset.CoreV1().ConfigMaps("ns").Get(ctx, "minting-ca-bundle", metav1.GetOptions{})
		if err == nil && cm.Data["ca.crt"] != "" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("immediate CA sync pass did not populate the bundle")
		case <-time.After(20 * time.Millisecond):
		}
	}

	// Wake-up: delete the bundle and notify; it must be recreated.
	if err := clientset.CoreV1().ConfigMaps("ns").Delete(ctx, "minting-ca-bundle", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete configmap: %v", err)
	}
	notify <- struct{}{}
	deadline = time.After(10 * time.Second)
	for {
		cm, err := clientset.CoreV1().ConfigMaps("ns").Get(ctx, "minting-ca-bundle", metav1.GetOptions{})
		if err == nil && cm.Data["ca.crt"] != "" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("caSyncNotify wake-up did not re-sync the bundle")
		case <-time.After(20 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("syncCASecretLoop did not return after cancel")
	}
}

// TestInternalServiceSANsCoverInternalControlHost proves the minting-CA
// internal leaf covers the hostname engines and the follower relay dial
// (server.internal_control_url) in its bare, .svc and cluster-domain forms.
func TestInternalServiceSANsCoverInternalControlHost(t *testing.T) {
	sans := internalServiceSANs(
		"https://dagger-kubernetes-test-dagger-kubernetes-control.dagger-kubernetes-test.svc:80",
		"",
		"cluster.local",
	)
	want := []string{
		"dagger-kubernetes-test-dagger-kubernetes-control.dagger-kubernetes-test.svc",
		"dagger-kubernetes-test-dagger-kubernetes-control",
		"dagger-kubernetes-test-dagger-kubernetes-control.dagger-kubernetes-test.svc.cluster.local",
	}
	for _, w := range want {
		found := false
		for _, san := range sans {
			if san == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("SAN %q missing from %v", w, sans)
		}
	}
}
