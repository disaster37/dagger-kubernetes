package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "config.app.yaml"))
	if err != nil {
		t.Fatalf("Load with missing file: %v", err)
	}

	if cfg.Server.ControlAddr != ":8080" {
		t.Fatalf("control_addr default = %q, want :8080", cfg.Server.ControlAddr)
	}
	if cfg.Server.DataAddr != ":8443" {
		t.Fatalf("data_addr default = %q, want :8443", cfg.Server.DataAddr)
	}
	if cfg.Auth.Internal.Enabled != true {
		t.Fatal("auth.internal.enabled default should be true")
	}
	if cfg.Auth.Internal.TokensFile != "/etc/dagger-cache/tokens" {
		t.Fatalf("tokens_file default = %q", cfg.Auth.Internal.TokensFile)
	}
	if cfg.Fleet.Namespace != "dagger-cache" {
		t.Fatalf("fleet.namespace default = %q, want dagger-cache", cfg.Fleet.Namespace)
	}
	if cfg.Cache.InternalAddr != "" {
		t.Fatalf("cache.internal_addr default = %q, want empty", cfg.Cache.InternalAddr)
	}
	if cfg.TLS.CertPath != "/etc/dagger-cache/tls/tls.crt" {
		t.Fatalf("tls.cert_path default = %q", cfg.TLS.CertPath)
	}
	if cfg.TLS.KeyPath != "/etc/dagger-cache/tls/tls.key" {
		t.Fatalf("tls.key_path default = %q", cfg.TLS.KeyPath)
	}
	if cfg.LeaseTTL != 2*time.Minute {
		t.Fatalf("lease_ttl default = %v, want 2m", cfg.LeaseTTL)
	}
	if len(cfg.Version.Allowlist) != 0 {
		t.Fatalf("version.allowlist default should be empty, got %v", cfg.Version.Allowlist)
	}
	if cfg.OTel.OTLPEndpoint != "" {
		t.Fatalf("otel.otlp_endpoint default should be empty, got %q", cfg.OTel.OTLPEndpoint)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("log_level default = %q, want info", cfg.LogLevel)
	}
	if cfg.Database.Dir != "/var/lib/dagger-cache" {
		t.Fatalf("database.dir default = %q", cfg.Database.Dir)
	}
	if cfg.Raft.BindAddr != ":8081" {
		t.Fatalf("raft.bind_addr default = %q, want :8081", cfg.Raft.BindAddr)
	}
	if cfg.Raft.ApplyTimeout != 5*time.Second {
		t.Fatalf("raft.apply_timeout default = %v, want 5s", cfg.Raft.ApplyTimeout)
	}
	if cfg.Raft.LeaderWaitTimeout != 30*time.Second {
		t.Fatalf("raft.leader_wait_timeout default = %v, want 30s", cfg.Raft.LeaderWaitTimeout)
	}
	if cfg.Raft.SnapshotThreshold != 1000 {
		t.Fatalf("raft.snapshot_threshold default = %d, want 1000", cfg.Raft.SnapshotThreshold)
	}
	if cfg.Raft.SnapshotInterval != 10*time.Minute {
		t.Fatalf("raft.snapshot_interval default = %v, want 10m", cfg.Raft.SnapshotInterval)
	}
	if cfg.Raft.TrailingLogs != 256 {
		t.Fatalf("raft.trailing_logs default = %d, want 256", cfg.Raft.TrailingLogs)
	}
	if cfg.Raft.TLS.Enabled {
		t.Fatal("raft.tls.enabled default should be false")
	}
	if cfg.Auth.JWT.Secret != "" {
		t.Fatalf("auth.jwt.secret default should be empty, got %q", cfg.Auth.JWT.Secret)
	}
	if cfg.Auth.JWT.AccessTTL != 15*time.Minute {
		t.Fatalf("auth.jwt.access_ttl default = %v, want 15m", cfg.Auth.JWT.AccessTTL)
	}
	if cfg.Auth.JWT.RefreshTTL != 168*time.Hour {
		t.Fatalf("auth.jwt.refresh_ttl default = %v, want 168h", cfg.Auth.JWT.RefreshTTL)
	}
	if cfg.Auth.BootstrapAdmin.Username != "admin" {
		t.Fatalf("auth.bootstrap_admin.username default = %q, want admin", cfg.Auth.BootstrapAdmin.Username)
	}
	if cfg.Auth.BootstrapAdmin.Password != "" {
		t.Fatalf("auth.bootstrap_admin.password default should be empty, got %q", cfg.Auth.BootstrapAdmin.Password)
	}
	if cfg.Auth.OAuth.DefaultGroup != "" {
		t.Fatalf("auth.oauth.default_group default should be empty, got %q", cfg.Auth.OAuth.DefaultGroup)
	}
	if cfg.LogFormat != "json" {
		t.Fatalf("log_format default = %q, want json", cfg.LogFormat)
	}
	if cfg.Fleet.EngineLogFormat != "json" {
		t.Fatalf("fleet.engine_log_format default = %q, want json", cfg.Fleet.EngineLogFormat)
	}
	if cfg.Fleet.EngineCASecretKey != "ca.crt" {
		t.Fatalf("fleet.engine_ca_secret_key default = %q, want ca.crt", cfg.Fleet.EngineCASecretKey)
	}
	if cfg.Fleet.EngineCASecret != "" {
		t.Fatalf("fleet.engine_ca_secret default should be empty, got %q", cfg.Fleet.EngineCASecret)
	}
	if cfg.Fleet.EngineDebug {
		t.Fatal("fleet.engine_debug default should be false")
	}
	if cfg.Fleet.EngineExtraEnv == nil || len(cfg.Fleet.EngineExtraEnv) != 0 {
		t.Fatalf("fleet.engine_extra_env default should be non-nil and empty, got %v", cfg.Fleet.EngineExtraEnv)
	}
	if cfg.Fleet.EngineExtraEnvFrom == nil || len(cfg.Fleet.EngineExtraEnvFrom) != 0 {
		t.Fatalf("fleet.engine_extra_env_from default should be non-nil and empty, got %v", cfg.Fleet.EngineExtraEnvFrom)
	}
	if cfg.Fleet.EngineRegistryMirrors == nil || len(cfg.Fleet.EngineRegistryMirrors) != 0 {
		t.Fatalf("fleet.engine_registry_mirrors default should be non-nil and empty, got %v", cfg.Fleet.EngineRegistryMirrors)
	}
	if cfg.Cache.GC.Enabled {
		t.Fatal("cache.gc.enabled default should be false")
	}
	if cfg.Cache.GC.MaxAge != 168*time.Hour {
		t.Fatalf("cache.gc.max_age default = %v, want 168h", cfg.Cache.GC.MaxAge)
	}
	if cfg.Cache.GC.Schedule != time.Hour {
		t.Fatalf("cache.gc.schedule default = %v, want 1h", cfg.Cache.GC.Schedule)
	}
	if cfg.Cache.GC.MinRefsToKeep != 3 {
		t.Fatalf("cache.gc.min_refs_to_keep default = %d, want 3", cfg.Cache.GC.MinRefsToKeep)
	}
	if !cfg.Cache.GC.ProtectActiveVersions {
		t.Fatal("cache.gc.protect_active_versions default should be true")
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.app.yaml")
	content := []byte(`
server:
  control_addr: ":9090"
fleet:
  namespace: "custom-ns"
  max_replicas_per_version: 7
  engine_extra_env:
    http_proxy: "http://proxy.corp.example:3128"
    https_proxy: "http://proxy.corp.example:3128"
  engine_extra_env_from:
    http_proxy:
      secret_name: "proxy-credentials"
      key: "http_proxy"
  engine_registry_mirrors:
    acme-registry:
      - "mirror.gcr.io"
      - "hm-registry.hm.dm.ad/docker-hub"
  engine_ca_secret: "custom-ca-bundle"
  engine_debug: true
version:
  allowlist: ["0.21"]
log_level: "debug"
log_format: "text"
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.ControlAddr != ":9090" {
		t.Fatalf("control_addr = %q, want :9090", cfg.Server.ControlAddr)
	}
	if cfg.Fleet.Namespace != "custom-ns" {
		t.Fatalf("fleet.namespace = %q, want custom-ns", cfg.Fleet.Namespace)
	}
	if cfg.Fleet.MaxReplicasPerVersion != 7 {
		t.Fatalf("max_replicas_per_version = %d, want 7", cfg.Fleet.MaxReplicasPerVersion)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("log_level = %q, want debug", cfg.LogLevel)
	}
	if cfg.LogFormat != "text" {
		t.Fatalf("log_format = %q, want text", cfg.LogFormat)
	}
	if len(cfg.Version.Allowlist) != 1 || cfg.Version.Allowlist[0] != "0.21" {
		t.Fatalf("allowlist = %v", cfg.Version.Allowlist)
	}
	if len(cfg.Fleet.EngineExtraEnv) != 2 {
		t.Fatalf("engine_extra_env = %v, want 2 entries", cfg.Fleet.EngineExtraEnv)
	}
	if cfg.Fleet.EngineExtraEnv["http_proxy"] != "http://proxy.corp.example:3128" {
		t.Errorf("engine_extra_env.http_proxy = %q", cfg.Fleet.EngineExtraEnv["http_proxy"])
	}
	if len(cfg.Fleet.EngineExtraEnvFrom) != 1 {
		t.Fatalf("engine_extra_env_from = %v, want 1 entry", cfg.Fleet.EngineExtraEnvFrom)
	}
	src := cfg.Fleet.EngineExtraEnvFrom["http_proxy"]
	if src.SecretName != "proxy-credentials" || src.Key != "http_proxy" {
		t.Errorf("engine_extra_env_from.http_proxy = %+v", src)
	}
	if len(cfg.Fleet.EngineRegistryMirrors) != 1 {
		t.Fatalf("engine_registry_mirrors = %v, want 1 entry", cfg.Fleet.EngineRegistryMirrors)
	}
	mirrors := cfg.Fleet.EngineRegistryMirrors["acme-registry"]
	if len(mirrors) != 2 || mirrors[0] != "mirror.gcr.io" || mirrors[1] != "hm-registry.hm.dm.ad/docker-hub" {
		t.Errorf("engine_registry_mirrors.acme-registry = %v", mirrors)
	}
	if cfg.Fleet.EngineCASecret != "custom-ca-bundle" {
		t.Errorf("engine_ca_secret = %q, want custom-ca-bundle", cfg.Fleet.EngineCASecret)
	}
	if !cfg.Fleet.EngineDebug {
		t.Error("engine_debug = false, want true")
	}
}

func TestLoadEnvOverride(t *testing.T) {
	t.Setenv("DAGGER_CACHE_SERVER_CONTROL_ADDR", ":7070")
	t.Setenv("DAGGER_CACHE_LOG_LEVEL", "error")
	t.Setenv("DAGGER_CACHE_CACHE_GC_ENABLED", "true")

	cfg, err := Load(filepath.Join(t.TempDir(), "config.app.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.ControlAddr != ":7070" {
		t.Fatalf("env override control_addr = %q, want :7070", cfg.Server.ControlAddr)
	}
	if cfg.LogLevel != "error" {
		t.Fatalf("env override log_level = %q, want error", cfg.LogLevel)
	}
	if !cfg.Cache.GC.Enabled {
		t.Fatal("env override cache.gc.enabled = false, want true")
	}
}
