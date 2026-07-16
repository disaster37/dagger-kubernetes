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
version:
  allowlist: ["0.21"]
log_level: "debug"
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
	if len(cfg.Version.Allowlist) != 1 || cfg.Version.Allowlist[0] != "0.21" {
		t.Fatalf("allowlist = %v", cfg.Version.Allowlist)
	}
}

func TestLoadEnvOverride(t *testing.T) {
	t.Setenv("DAGGER_CACHE_SERVER_CONTROL_ADDR", ":7070")
	t.Setenv("DAGGER_CACHE_LOG_LEVEL", "error")

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
}
