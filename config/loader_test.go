package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
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
	if cfg.Auth.Internal.TokensFile != "/etc/dagger-kubernetes/tokens" {
		t.Fatalf("tokens_file default = %q", cfg.Auth.Internal.TokensFile)
	}
	if cfg.Fleet.Namespace != "dagger-kubernetes" {
		t.Fatalf("fleet.namespace default = %q, want dagger-kubernetes", cfg.Fleet.Namespace)
	}
	if cfg.Cache.InternalAddr != "" {
		t.Fatalf("cache.internal_addr default = %q, want empty", cfg.Cache.InternalAddr)
	}
	if cfg.TLS.CertPath != "/etc/dagger-kubernetes/tls/tls.crt" {
		t.Fatalf("tls.cert_path default = %q", cfg.TLS.CertPath)
	}
	if cfg.TLS.KeyPath != "/etc/dagger-kubernetes/tls/tls.key" {
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
	if cfg.Database.Dir != "/var/lib/dagger-kubernetes" {
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
	if cfg.Auth.OAuth.CookieSecure {
		t.Fatal("auth.oauth.cookie_secure default should be false")
	}
	if cfg.Auth.OAuth.Provider != "github" {
		t.Fatalf("auth.oauth.provider default = %q, want github", cfg.Auth.OAuth.Provider)
	}
	if cfg.Auth.OAuth.IssuerURL != "" {
		t.Fatalf("auth.oauth.issuer_url default should be empty, got %q", cfg.Auth.OAuth.IssuerURL)
	}
	if len(cfg.Auth.OAuth.Scopes) != 3 || cfg.Auth.OAuth.Scopes[0] != "openid" || cfg.Auth.OAuth.Scopes[1] != "profile" || cfg.Auth.OAuth.Scopes[2] != "email" {
		t.Fatalf("auth.oauth.scopes default = %v, want [openid profile email]", cfg.Auth.OAuth.Scopes)
	}
	if cfg.Auth.OAuth.UsernameClaim != "preferred_username" {
		t.Fatalf("auth.oauth.username_claim default = %q, want preferred_username", cfg.Auth.OAuth.UsernameClaim)
	}
	if cfg.Auth.OAuth.GroupsClaim != "groups" {
		t.Fatalf("auth.oauth.groups_claim default = %q, want groups", cfg.Auth.OAuth.GroupsClaim)
	}
	if cfg.Auth.OAuth.AllowedTeams == nil || len(cfg.Auth.OAuth.AllowedTeams) != 0 {
		t.Fatalf("auth.oauth.allowed_teams default should be non-nil and empty, got %v", cfg.Auth.OAuth.AllowedTeams)
	}
	if cfg.Auth.OAuth.AllowedGroups == nil || len(cfg.Auth.OAuth.AllowedGroups) != 0 {
		t.Fatalf("auth.oauth.allowed_groups default should be non-nil and empty, got %v", cfg.Auth.OAuth.AllowedGroups)
	}
	if cfg.Auth.OAuth.GroupMappings == nil || len(cfg.Auth.OAuth.GroupMappings) != 0 {
		t.Fatalf("auth.oauth.group_mappings default should be non-nil and empty, got %v", cfg.Auth.OAuth.GroupMappings)
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
	if cfg.Fleet.VersionRetention != 24*time.Hour {
		t.Fatalf("fleet.version_retention default = %v, want 24h", cfg.Fleet.VersionRetention)
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
	if cfg.History.GC.Enabled {
		t.Fatal("history.gc.enabled default should be false")
	}
	if cfg.History.GC.MaxAge != 720*time.Hour {
		t.Fatalf("history.gc.max_age default = %v, want 720h", cfg.History.GC.MaxAge)
	}
	if cfg.History.GC.Schedule != time.Hour {
		t.Fatalf("history.gc.schedule default = %v, want 1h", cfg.History.GC.Schedule)
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
  version_retention: "2h"
  engine_extra_env:
    http_proxy: "http://proxy.corp.example:3128"
    https_proxy: "http://proxy.corp.example:3128"
  engine_extra_env_from:
    http_proxy:
      secretName: "proxy-credentials"
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
	if cfg.Fleet.VersionRetention != 2*time.Hour {
		t.Fatalf("version_retention = %v, want 2h", cfg.Fleet.VersionRetention)
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
	t.Setenv("DAGGER_KUBERNETES_SERVER_CONTROL_ADDR", ":7070")
	t.Setenv("DAGGER_KUBERNETES_LOG_LEVEL", "error")
	t.Setenv("DAGGER_KUBERNETES_CACHE_GC_ENABLED", "true")
	t.Setenv("DAGGER_KUBERNETES_FLEET_VERSION_RETENTION", "90m")

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
	if cfg.Fleet.VersionRetention != 90*time.Minute {
		t.Fatalf("env override fleet.version_retention = %v, want 90m", cfg.Fleet.VersionRetention)
	}
}

func TestLoadHistoryEnvOverride(t *testing.T) {
	t.Setenv("DAGGER_KUBERNETES_HISTORY_GC_ENABLED", "true")

	cfg, err := Load(filepath.Join(t.TempDir(), "config.app.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.History.GC.Enabled {
		t.Fatal("env override history.gc.enabled = false, want true")
	}
}

func TestPipelineDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "config.app.yaml"))
	if err != nil {
		t.Fatalf("Load with missing file: %v", err)
	}

	if cfg.Pipeline.DisconnectGrace != 0 {
		t.Fatalf("pipeline.disconnect_grace default = %v, want 0", cfg.Pipeline.DisconnectGrace)
	}
	if !cfg.Pipeline.StaleSweep.Enabled {
		t.Fatal("pipeline.stale_sweep.enabled default should be true")
	}
	if cfg.Pipeline.StaleSweep.Schedule != time.Minute {
		t.Fatalf("pipeline.stale_sweep.schedule default = %v, want 1m", cfg.Pipeline.StaleSweep.Schedule)
	}
	if cfg.Pipeline.StaleSweep.StaleAfter != 5*time.Minute {
		t.Fatalf("pipeline.stale_sweep.stale_after default = %v, want 5m", cfg.Pipeline.StaleSweep.StaleAfter)
	}
}

func TestPipelineEnvOverride(t *testing.T) {
	t.Setenv("DAGGER_KUBERNETES_PIPELINE_DISCONNECT_GRACE", "10s")
	t.Setenv("DAGGER_KUBERNETES_PIPELINE_STALE_SWEEP_ENABLED", "false")
	t.Setenv("DAGGER_KUBERNETES_PIPELINE_STALE_SWEEP_SCHEDULE", "30s")
	t.Setenv("DAGGER_KUBERNETES_PIPELINE_STALE_SWEEP_STALE_AFTER", "2m")

	cfg, err := Load(filepath.Join(t.TempDir(), "config.app.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Pipeline.DisconnectGrace != 10*time.Second {
		t.Fatalf("env override disconnect_grace = %v, want 10s", cfg.Pipeline.DisconnectGrace)
	}
	if cfg.Pipeline.StaleSweep.Enabled {
		t.Fatal("env override stale_sweep.enabled = true, want false")
	}
	if cfg.Pipeline.StaleSweep.Schedule != 30*time.Second {
		t.Fatalf("env override stale_sweep.schedule = %v, want 30s", cfg.Pipeline.StaleSweep.Schedule)
	}
	if cfg.Pipeline.StaleSweep.StaleAfter != 2*time.Minute {
		t.Fatalf("env override stale_sweep.stale_after = %v, want 2m", cfg.Pipeline.StaleSweep.StaleAfter)
	}
}

func TestValidateAuthConfig(t *testing.T) {
	githubSet := func(cfg *domain.Config) {
		cfg.Auth.OAuth.ClientID = "cid"
		cfg.Auth.OAuth.ClientSecret = "csec"
		cfg.Auth.OAuth.RedirectURL = "https://supv.example.com/cb"
	}
	oidcSet := func(cfg *domain.Config) {
		cfg.Auth.OAuth.IssuerURL = "https://dex.example.com"
		githubSet(cfg)
	}

	tests := []struct {
		name    string
		cfg     *domain.Config
		wantErr string
	}{
		{name: "internal enabled oauth off unknown provider tolerated", cfg: &domain.Config{
			Auth: domain.AuthConfig{
				Internal: domain.InternalAuthConfig{Enabled: true},
				OAuth:    domain.OAuthConfig{Enabled: false, Provider: "gitlab"},
			},
		}},
		{name: "internal enabled github configured", cfg: func() *domain.Config {
			c := &domain.Config{Auth: domain.AuthConfig{
				Internal: domain.InternalAuthConfig{Enabled: true},
				OAuth:    domain.OAuthConfig{Enabled: true, Provider: "github"},
			}}
			githubSet(c)
			return c
		}()},
		{name: "internal enabled github missing fields", cfg: &domain.Config{
			Auth: domain.AuthConfig{
				Internal: domain.InternalAuthConfig{Enabled: true},
				OAuth:    domain.OAuthConfig{Enabled: true, Provider: "github"},
			},
		}, wantErr: "auth.oauth.enabled: true requires client_id"},
		{name: "internal enabled oidc configured", cfg: func() *domain.Config {
			c := &domain.Config{Auth: domain.AuthConfig{
				Internal: domain.InternalAuthConfig{Enabled: true},
				OAuth:    domain.OAuthConfig{Enabled: true, Provider: "oidc"},
			}}
			oidcSet(c)
			return c
		}()},
		{name: "internal enabled oidc missing issuer", cfg: func() *domain.Config {
			c := &domain.Config{Auth: domain.AuthConfig{
				Internal: domain.InternalAuthConfig{Enabled: true},
				OAuth:    domain.OAuthConfig{Enabled: true, Provider: "oidc"},
			}}
			githubSet(c)
			return c
		}(), wantErr: `provider "oidc" requires issuer_url`},
		{name: "internal enabled oidc missing client_id", cfg: &domain.Config{
			Auth: domain.AuthConfig{
				Internal: domain.InternalAuthConfig{Enabled: true},
				OAuth: domain.OAuthConfig{Enabled: true, Provider: "oidc",
					IssuerURL: "https://dex.example.com", ClientSecret: "csec", RedirectURL: "https://supv.example.com/cb"},
			},
		}, wantErr: `provider "oidc" requires issuer_url`},
		{name: "internal enabled oauth unsupported provider", cfg: &domain.Config{
			Auth: domain.AuthConfig{
				Internal: domain.InternalAuthConfig{Enabled: true},
				OAuth:    domain.OAuthConfig{Enabled: true, Provider: "gitlab"},
			},
		}, wantErr: `only "github" and "oidc" are supported`},
		{name: "internal disabled oauth off", cfg: &domain.Config{
			Auth: domain.AuthConfig{
				Internal: domain.InternalAuthConfig{Enabled: false},
				OAuth:    domain.OAuthConfig{Enabled: false},
			},
		}, wantErr: "auth.internal.enabled: false requires auth.oauth.enabled"},
		{name: "internal disabled github missing fields", cfg: &domain.Config{
			Auth: domain.AuthConfig{
				Internal: domain.InternalAuthConfig{Enabled: false},
				OAuth:    domain.OAuthConfig{Enabled: true, Provider: "github"},
			},
		}, wantErr: "auth.oauth.enabled: true requires client_id"},
		{name: "internal disabled github configured", cfg: func() *domain.Config {
			c := &domain.Config{Auth: domain.AuthConfig{
				Internal: domain.InternalAuthConfig{Enabled: false},
				OAuth:    domain.OAuthConfig{Enabled: true, Provider: "github"},
			}}
			githubSet(c)
			return c
		}()},
		{name: "internal disabled oidc configured", cfg: func() *domain.Config {
			c := &domain.Config{Auth: domain.AuthConfig{
				Internal: domain.InternalAuthConfig{Enabled: false},
				OAuth:    domain.OAuthConfig{Enabled: true, Provider: "oidc"},
			}}
			oidcSet(c)
			return c
		}()},
		{name: "internal disabled oidc missing issuer", cfg: func() *domain.Config {
			c := &domain.Config{Auth: domain.AuthConfig{
				Internal: domain.InternalAuthConfig{Enabled: false},
				OAuth:    domain.OAuthConfig{Enabled: true, Provider: "oidc"},
			}}
			githubSet(c)
			return c
		}(), wantErr: `provider "oidc" requires issuer_url`},
		{name: "internal disabled unsupported provider", cfg: &domain.Config{
			Auth: domain.AuthConfig{
				Internal: domain.InternalAuthConfig{Enabled: false},
				OAuth:    domain.OAuthConfig{Enabled: true, Provider: "gitlab"},
			},
		}, wantErr: `only "github" and "oidc" are supported`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAuthConfig(tt.cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateAuthConfig = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateAuthConfig = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateAuthConfig = %q, want containing %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestValidateGroupMappings(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *domain.Config
		wantErr string
	}{
		{name: "empty valid", cfg: &domain.Config{}},
		{name: "valid rules", cfg: &domain.Config{Auth: domain.AuthConfig{OAuth: domain.OAuthConfig{
			GroupMappings: []domain.GroupMappingRule{
				{Pattern: `^dex:(.*)$`, Replacement: `$1`},
				{Pattern: `^github\.com/acme-(.*)$`, Replacement: `acme-${1}`},
			},
			AllowedTeams: []string{"acme/eng"},
		}}}},
		{name: "bad pattern", cfg: &domain.Config{Auth: domain.AuthConfig{OAuth: domain.OAuthConfig{
			GroupMappings: []domain.GroupMappingRule{{Pattern: "[", Replacement: "x"}},
		}}}, wantErr: "auth.oauth.group_mappings[0].pattern"},
		{name: "empty pattern", cfg: &domain.Config{Auth: domain.AuthConfig{OAuth: domain.OAuthConfig{
			GroupMappings: []domain.GroupMappingRule{{Pattern: "", Replacement: "x"}},
		}}}, wantErr: "auth.oauth.group_mappings[0].pattern must not be empty"},
		{name: "empty replacement", cfg: &domain.Config{Auth: domain.AuthConfig{OAuth: domain.OAuthConfig{
			GroupMappings: []domain.GroupMappingRule{{Pattern: "^x$", Replacement: ""}},
		}}}, wantErr: "auth.oauth.group_mappings[0].replacement must not be empty"},
		{name: "allowed teams no slash", cfg: &domain.Config{Auth: domain.AuthConfig{OAuth: domain.OAuthConfig{
			AllowedTeams: []string{"noslash"},
		}}}, wantErr: "auth.oauth.allowed_teams[0] must be a non-empty \"org/team\" string"},
		{name: "allowed teams empty", cfg: &domain.Config{Auth: domain.AuthConfig{OAuth: domain.OAuthConfig{
			AllowedTeams: []string{""},
		}}}, wantErr: "auth.oauth.allowed_teams[0] must be a non-empty \"org/team\" string"},
		{name: "allowed teams too many slashes", cfg: &domain.Config{Auth: domain.AuthConfig{OAuth: domain.OAuthConfig{
			AllowedTeams: []string{"a/b/c"},
		}}}, wantErr: "auth.oauth.allowed_teams[0] must be a non-empty \"org/team\" string"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGroupMappings(tt.cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateGroupMappings = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateGroupMappings = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateGroupMappings = %q, want containing %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestLoadRejectsInvalidGroupMappings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.app.yaml")
	content := []byte("auth:\n  oauth:\n    group_mappings:\n      - pattern: \"[\"\n        replacement: \"x\"\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load = nil, want validation error")
	}
	if !strings.Contains(err.Error(), "validate group mappings") ||
		!strings.Contains(err.Error(), "auth.oauth.group_mappings[0].pattern") {
		t.Fatalf("Load error = %q, want wrapped group-mappings validation message", err.Error())
	}
}

func TestLoadRejectsInternalDisabledWithoutOAuth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.app.yaml")
	content := []byte("auth:\n  internal:\n    enabled: false\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load = nil, want validation error")
	}
	if !strings.Contains(err.Error(), "validate auth config") ||
		!strings.Contains(err.Error(), "auth.internal.enabled: false requires auth.oauth.enabled") {
		t.Fatalf("Load error = %q, want wrapped validation message", err.Error())
	}
}

func TestLoadInvalidPublicURLScheme(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.app.yaml")
	content := []byte("server:\n  public_url: \"ftp://x\"\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load = nil, want validation error")
	}
	if !strings.Contains(err.Error(), "server.public_url") ||
		!strings.Contains(err.Error(), "pipeline url base must be http(s)") {
		t.Fatalf("Load error = %q, want public_url scheme validation message", err.Error())
	}
}

func TestLoadEmptyPublicURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.app.yaml")
	content := []byte("server:\n  public_url: \"\"\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load = nil, want validation error")
	}
	if !strings.Contains(err.Error(), "server.public_url must be set so a pipeline view URL can be derived") {
		t.Fatalf("Load error = %q, want empty public_url message", err.Error())
	}
}

func TestLoadPublicURLValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.app.yaml")
	content := []byte("server:\n  public_url: \"https://supv.example.com\"\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestLoadVersionRetentionRejectsTinyPositive(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{name: "unquoted integer decodes to nanoseconds", content: "fleet:\n  version_retention: 24\n"},
		{name: "quoted nanosecond string", content: "fleet:\n  version_retention: \"1ns\"\n"},
		{name: "seconds below floor", content: "fleet:\n  version_retention: \"30s\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.app.yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			_, err := Load(path)
			if err == nil {
				t.Fatal("Load = nil, want validation error")
			}
			if !strings.Contains(err.Error(), "fleet.version_retention") ||
				!strings.Contains(err.Error(), "too small") {
				t.Fatalf("Load error = %q, want version_retention validation message", err.Error())
			}
		})
	}
}

func TestLoadVersionRetentionDisabledAndValid(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{name: "zero disables", content: "fleet:\n  version_retention: \"0s\"\n"},
		{name: "negative disables", content: "fleet:\n  version_retention: \"-1h\"\n"},
		{name: "valid duration", content: "fleet:\n  version_retention: \"2h\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.app.yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			if _, err := Load(path); err != nil {
				t.Fatalf("Load: %v", err)
			}
		})
	}
}

func TestLoadPublicURLWithUserinfo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.app.yaml")
	content := []byte("server:\n  public_url: \"https://user:pass@supv.example.com\"\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load = nil, want validation error for userinfo in public_url")
	}
	if !strings.Contains(err.Error(), "must not contain userinfo") {
		t.Fatalf("Load error = %q, want userinfo rejection message", err.Error())
	}
}

func TestLoadCLIDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "config.app.yaml"))
	if err != nil {
		t.Fatalf("Load with missing file: %v", err)
	}

	if !cfg.CLI.Enabled {
		t.Fatal("cli.enabled default should be true")
	}
	if cfg.CLI.CacheDir != "" {
		t.Fatalf("cli.cache_dir default = %q, want empty", cfg.CLI.CacheDir)
	}
	if cfg.CLI.ReleaseListTTL != time.Hour {
		t.Fatalf("cli.release_list_ttl default = %v, want 1h", cfg.CLI.ReleaseListTTL)
	}
	if cfg.CLI.DownloadTimeout != 5*time.Minute {
		t.Fatalf("cli.download_timeout default = %v, want 5m", cfg.CLI.DownloadTimeout)
	}
	if cfg.CLI.Upstream.ReleasesURL != "https://api.github.com/repos/dagger/dagger/releases" {
		t.Fatalf("cli.upstream.releases_url default = %q", cfg.CLI.Upstream.ReleasesURL)
	}
	if cfg.CLI.Upstream.DownloadBase != "https://github.com/dagger/dagger/releases/download" {
		t.Fatalf("cli.upstream.download_base default = %q", cfg.CLI.Upstream.DownloadBase)
	}
	if cfg.CLI.Upstream.GitHubToken != "" {
		t.Fatalf("cli.upstream.github_token default should be empty, got %q", cfg.CLI.Upstream.GitHubToken)
	}
}

func TestLoadCLIEnvOverride(t *testing.T) {
	t.Setenv("DAGGER_KUBERNETES_CLI_RELEASE_LIST_TTL", "30m")
	t.Setenv("DAGGER_KUBERNETES_CLI_UPSTREAM_GITHUB_TOKEN", "ghp_testtoken")
	t.Setenv("DAGGER_KUBERNETES_CLI_UPSTREAM_RELEASES_URL", "https://mirror.example.com/releases")

	cfg, err := Load(filepath.Join(t.TempDir(), "config.app.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.CLI.ReleaseListTTL != 30*time.Minute {
		t.Fatalf("cli.release_list_ttl = %v, want 30m", cfg.CLI.ReleaseListTTL)
	}
	if cfg.CLI.Upstream.GitHubToken != "ghp_testtoken" {
		t.Fatalf("cli.upstream.github_token = %q, want ghp_testtoken", cfg.CLI.Upstream.GitHubToken)
	}
	if cfg.CLI.Upstream.ReleasesURL != "https://mirror.example.com/releases" {
		t.Fatalf("cli.upstream.releases_url = %q, want mirror", cfg.CLI.Upstream.ReleasesURL)
	}
}

func TestValidateCLIConfig(t *testing.T) {
	base := func() *domain.Config {
		return &domain.Config{
			CLI: domain.CLIConfig{
				Enabled:         true,
				ReleaseListTTL:  time.Hour,
				DownloadTimeout: 5 * time.Minute,
				Upstream: domain.CLIUpstreamConfig{
					ReleasesURL:  "https://api.github.com/repos/dagger/dagger/releases",
					DownloadBase: "https://github.com/dagger/dagger/releases/download",
				},
			},
		}
	}

	tests := []struct {
		name    string
		mut     func(c *domain.Config)
		wantErr string
	}{
		{name: "valid", mut: func(c *domain.Config) {}},
		{name: "disabled skips validation", mut: func(c *domain.Config) {
			c.CLI.Enabled = false
			c.CLI.Upstream.ReleasesURL = "not-a-url"
			c.CLI.ReleaseListTTL = 0
			c.CLI.DownloadTimeout = 0
		}},
		{name: "bad releases_url", mut: func(c *domain.Config) {
			c.CLI.Upstream.ReleasesURL = "ftp://x"
		}, wantErr: "cli.upstream.releases_url must be an absolute http(s) URL"},
		{name: "relative releases_url", mut: func(c *domain.Config) {
			c.CLI.Upstream.ReleasesURL = "/releases"
		}, wantErr: "cli.upstream.releases_url must be an absolute http(s) URL"},
		{name: "bad download_base", mut: func(c *domain.Config) {
			c.CLI.Upstream.DownloadBase = "not a url"
		}, wantErr: "cli.upstream.download_base must be an absolute http(s) URL"},
		{name: "zero release_list_ttl", mut: func(c *domain.Config) {
			c.CLI.ReleaseListTTL = 0
		}, wantErr: "cli.release_list_ttl must be > 0"},
		{name: "negative download_timeout", mut: func(c *domain.Config) {
			c.CLI.DownloadTimeout = -time.Second
		}, wantErr: "cli.download_timeout must be > 0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base()
			tt.mut(cfg)
			err := validateCLIConfig(cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateCLIConfig = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateCLIConfig = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateCLIConfig = %q, want containing %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestLoadRejectsInvalidCLIConfig(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{name: "bad releases url", content: "cli:\n  enabled: true\n  upstream:\n    releases_url: \"ftp://x\"\n"},
		{name: "zero ttl", content: "cli:\n  enabled: true\n  release_list_ttl: \"0s\"\n"},
		{name: "disabled bad url tolerated", content: "cli:\n  enabled: false\n  upstream:\n    releases_url: \"ftp://x\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.app.yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			_, err := Load(path)
			if tc.name == "disabled bad url tolerated" {
				if err != nil {
					t.Fatalf("Load = %v, want nil (disabled)", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Load = nil, want validation error")
			}
			if !strings.Contains(err.Error(), "validate cli config") {
				t.Fatalf("Load error = %q, want wrapped cli validation message", err.Error())
			}
		})
	}
}
