package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"strings"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

func newMetaStore(t *testing.T) *repository.MetaStore {
	t.Helper()
	db, err := repository.OpenSQLite(t.TempDir() + "/jwt.db")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := repository.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return repository.NewMetaStore(db)
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
			name: "DAGGER_CACHE_TOKEN in engine_extra_env",
			fleet: domain.FleetConfig{
				EngineExtraEnv: map[string]string{
					"DAGGER_CACHE_TOKEN": "should-not-be-set",
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
					"DAGGER_CACHE_TOKEN": {SecretName: "s", Key: "k"},
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
