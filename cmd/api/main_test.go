package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	peers := []repository.RaftPeer{
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

func mustResolver(t *testing.T, nodeID, advertise string, peers []repository.RaftPeer) repository.PeerResolver {
	t.Helper()
	return repository.NewPeerResolver(&repository.RaftDiscoveryConfig{
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
			name: "DAGGER_KUBERNETES_TOKEN in engine_extra_env",
			fleet: domain.FleetConfig{
				EngineExtraEnv: map[string]string{
					"DAGGER_KUBERNETES_TOKEN": "should-not-be-set",
				},
			},
			wantErr: true,
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
			name: "SSL_CERT_FILE in engine_extra_env with CA secret set",
			fleet: domain.FleetConfig{
				EngineCASecret: "custom-ca-bundle",
				EngineExtraEnv: map[string]string{
					"SSL_CERT_FILE": "/etc/ssl/certs/other.pem",
				},
			},
			wantErr: true,
		},
		{
			name: "SSL_CERT_FILE in engine_extra_env without CA secret",
			fleet: domain.FleetConfig{
				EngineExtraEnv: map[string]string{
					"SSL_CERT_FILE": "/etc/ssl/certs/other.pem",
				},
			},
			wantErr: false,
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
			name: "reserved name in engine_extra_env_from",
			fleet: domain.FleetConfig{
				EngineExtraEnvFrom: map[string]domain.EnvVarSource{
					"DAGGER_KUBERNETES_TOKEN": {SecretName: "s", Key: "k"},
				},
			},
			wantErr: true,
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
		{
			name: "SSL_CERT_FILE in engine_extra_env_from with CA secret set",
			fleet: domain.FleetConfig{
				EngineCASecret: "custom-ca-bundle",
				EngineExtraEnvFrom: map[string]domain.EnvVarSource{
					"SSL_CERT_FILE": {SecretName: "s", Key: "k"},
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

func TestValidateCacheConfig(t *testing.T) {
	base := func() *domain.Config {
		return &domain.Config{
			Server: domain.ServerConfig{PublicURL: "https://supv.example.com"},
			Cache:  domain.CacheConfig{Backend: "registry"},
		}
	}

	tests := []struct {
		name         string
		mut          func(*domain.Config)
		wantHost     string
		wantBackends int
		wantErr      bool
	}{
		{
			name:         "default-derivation",
			mut:          func(c *domain.Config) { c.Cache.Registry = "cache.reg/dagger-cache" },
			wantHost:     "cache.supv.example.com",
			wantBackends: 1,
		},
		{
			name: "explicit-public-host",
			mut: func(c *domain.Config) {
				c.Cache.PublicHost = "cache.custom.example"
				c.Cache.Registry = "cache.reg/dagger-cache"
			},
			wantHost:     "cache.custom.example",
			wantBackends: 1,
		},
		{
			name: "public-host-collision",
			mut: func(c *domain.Config) {
				c.Cache.PublicHost = "supv.example.com"
				c.Cache.Registry = "cache.reg/dagger-cache"
			},
			wantErr: true,
		},
		{
			name:    "empty-backends",
			mut:     func(c *domain.Config) {},
			wantErr: true,
		},
		{
			name: "duplicate-id",
			mut: func(c *domain.Config) {
				c.Cache.Registries = []domain.RegistryBackend{{ID: "a", InternalAddr: "r1:5000"}, {ID: "a", InternalAddr: "r2:5000"}}
			},
			wantErr: true,
		},
		{
			name: "empty-id",
			mut: func(c *domain.Config) {
				c.Cache.Registries = []domain.RegistryBackend{{ID: "", InternalAddr: "r1:5000"}}
			},
			wantErr: true,
		},
		{
			name:    "empty-internal-addr",
			mut:     func(c *domain.Config) { c.Cache.Registries = []domain.RegistryBackend{{ID: "a", InternalAddr: ""}} },
			wantErr: true,
		},
		{
			name: "bad-internal-addr-scheme",
			mut: func(c *domain.Config) {
				c.Cache.Registries = []domain.RegistryBackend{{ID: "a", InternalAddr: "https://r1:5000"}}
			},
			wantErr: true,
		},
		{
			name:         "synthesize-from-internal-addr",
			mut:          func(c *domain.Config) { c.Cache.InternalAddr = "reg.internal:5000" },
			wantHost:     "cache.supv.example.com",
			wantBackends: 1,
		},
		{
			name:    "synthesized-addr-with-scheme",
			mut:     func(c *domain.Config) { c.Cache.InternalAddr = "https://reg.internal:5000" },
			wantErr: true,
		},
		{
			name:    "empty-public-url",
			mut:     func(c *domain.Config) { c.Server.PublicURL = "" },
			wantErr: true,
		},
		{
			name: "multi-backend-valid",
			mut: func(c *domain.Config) {
				c.Cache.Registries = []domain.RegistryBackend{{ID: "a", InternalAddr: "r1:5000"}, {ID: "b", InternalAddr: "r2:5000"}}
			},
			wantHost:     "cache.supv.example.com",
			wantBackends: 2,
		},
		{
			name:         "s3-skip",
			mut:          func(c *domain.Config) { c.Cache.Backend = "s3" },
			wantHost:     "",
			wantBackends: 0,
			wantErr:      false,
		},
		{
			name: "public-url-with-explicit-port",
			mut: func(c *domain.Config) {
				c.Server.PublicURL = "https://supv.example.com:443"
				c.Cache.Registry = "cache.reg/dagger-cache"
			},
			wantHost:     "cache.supv.example.com",
			wantBackends: 1,
		},
		{
			name: "public-host-collision-with-port-in-control-host",
			mut: func(c *domain.Config) {
				c.Server.PublicURL = "https://supv.example.com:443"
				c.Cache.PublicHost = "supv.example.com"
				c.Cache.Registry = "cache.reg/dagger-cache"
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mut(cfg)
			host, backends, err := validateCacheConfig(cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if host != tc.wantHost {
				t.Fatalf("host = %q, want %q", host, tc.wantHost)
			}
			if len(backends) != tc.wantBackends {
				t.Fatalf("backends = %d, want %d", len(backends), tc.wantBackends)
			}
		})
	}
}

func TestValidateCacheConfigSynthesizesFromRegistry(t *testing.T) {
	cfg := &domain.Config{
		Server: domain.ServerConfig{PublicURL: "https://supv.example.com"},
		Cache:  domain.CacheConfig{Backend: "registry", Registry: "cache.reg/dagger-cache"},
	}
	_, backends, err := validateCacheConfig(cfg)
	if err != nil {
		t.Fatalf("validateCacheConfig: %v", err)
	}
	if backends[0].ID != "default" || backends[0].InternalAddr != "cache.reg" {
		t.Fatalf("backend = %+v, want id=default addr=cache.reg", backends[0])
	}
}

func TestHostOfStripsPort(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"with-explicit-port", "https://supv.example.com:443", "supv.example.com"},
		{"without-port", "https://supv.example.com", "supv.example.com"},
		{"with-path", "https://supv.example.com/foo/bar", "supv.example.com"},
		{"parse-failure-returns-raw", "://not-a-url", "://not-a-url"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostOf(tc.raw); got != tc.want {
				t.Fatalf("hostOf(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestResolveRegistryBackendSecrets(t *testing.T) {
	logger := observ.NewTestLogger()
	ctx := context.Background()
	clientset := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "reg-auth", Namespace: "ns"},
		Data:       map[string][]byte{"password": []byte("s3cr3t")},
	})

	tests := []struct {
		name      string
		clientset kubernetes.Interface
		namespace string
		backends  []domain.RegistryBackend
		wantPass  []string
		wantErr   bool
	}{
		{
			name:      "resolves password from secret",
			clientset: clientset,
			namespace: "ns",
			backends:  []domain.RegistryBackend{{ID: "b1", PasswordSecret: &domain.SecretRef{Name: "reg-auth", Key: "password"}}},
			wantPass:  []string{"s3cr3t"},
		},
		{
			name:      "explicit password wins",
			clientset: clientset,
			namespace: "ns",
			backends:  []domain.RegistryBackend{{ID: "b1", Password: "direct", PasswordSecret: &domain.SecretRef{Name: "reg-auth", Key: "password"}}},
			wantPass:  []string{"direct"},
		},
		{
			name:      "nil ref skipped",
			clientset: clientset,
			namespace: "ns",
			backends:  []domain.RegistryBackend{{ID: "b1"}},
			wantPass:  []string{""},
		},
		{
			name:      "empty secret name skipped",
			clientset: clientset,
			namespace: "ns",
			backends:  []domain.RegistryBackend{{ID: "b1", PasswordSecret: &domain.SecretRef{Name: ""}}},
			wantPass:  []string{""},
		},
		{
			name:      "missing secret errors but leaves password empty",
			clientset: clientset,
			namespace: "ns",
			backends:  []domain.RegistryBackend{{ID: "b1", PasswordSecret: &domain.SecretRef{Name: "nope", Key: "password"}}},
			wantPass:  []string{""},
			wantErr:   true,
		},
		{
			// One missing Secret must NOT abort resolution for the rest
			// (availability/degradation): the failing backend is left
			// empty (it will 401) while subsequent backends still resolve.
			name:      "missing secret for one backend does not block the rest",
			clientset: clientset,
			namespace: "ns",
			backends: []domain.RegistryBackend{
				{ID: "b1", PasswordSecret: &domain.SecretRef{Name: "nope", Key: "password"}},
				{ID: "b2", PasswordSecret: &domain.SecretRef{Name: "reg-auth", Key: "password"}},
			},
			wantPass: []string{"", "s3cr3t"},
			wantErr:  true,
		},
		{
			name:      "nil clientset leaves password empty",
			clientset: nil,
			namespace: "ns",
			backends:  []domain.RegistryBackend{{ID: "b1", PasswordSecret: &domain.SecretRef{Name: "reg-auth", Key: "password"}}},
			wantPass:  []string{""},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := resolveRegistryBackendSecrets(ctx, tc.clientset, tc.namespace, tc.backends, logger)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for i, want := range tc.wantPass {
				if tc.backends[i].Password != want {
					t.Fatalf("backend %d password = %q, want %q", i, tc.backends[i].Password, want)
				}
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
	t.Run("embedded returns provider that shares minting CA via secret", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		cfg := &domain.Config{
			CA:         domain.CAConfig{MintingCASecret: "minting-ca", ClientCertTTL: 2 * time.Hour},
			Supervisor: domain.SupervisorConfig{Dataplane: domain.SupervisorDataplaneConfig{TLS: domain.TLSConfig{Provider: "embedded", CAPath: t.TempDir()}}},
			Raft:       domain.RaftConfig{Replicas: 1},
			Fleet:      domain.FleetConfig{Namespace: "ns"},
			Server:     domain.ServerConfig{DataHost: "data.example.com"},
		}
		p, err := selectTLSProvider(cfg, clientset)
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
		p, err := selectTLSProvider(cfg, clientset)
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
