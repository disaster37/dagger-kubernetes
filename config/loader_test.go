package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"

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
	if cfg.Supervisor.Dataplane.TLS.CertPath != "/etc/dagger-kubernetes/tls/tls.crt" {
		t.Fatalf("supervisor.dataplane.tls.cert_path default = %q", cfg.Supervisor.Dataplane.TLS.CertPath)
	}
	if cfg.Supervisor.Dataplane.TLS.KeyPath != "/etc/dagger-kubernetes/tls/tls.key" {
		t.Fatalf("supervisor.dataplane.tls.key_path default = %q", cfg.Supervisor.Dataplane.TLS.KeyPath)
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

func TestLoadRaftClusterDomain(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "default",
			content: "server:\n  control_addr: \":8080\"\n",
			want:    "cluster.local",
		},
		{
			name: "explicit empty means svc-only",
			content: `raft:
  cluster_domain: ""
`,
			want: "",
		},
		{
			name: "custom domain",
			content: `raft:
  cluster_domain: "cluster.internal"
`,
			want: "cluster.internal",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.app.yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Raft.ClusterDomain != tc.want {
				t.Fatalf("raft.cluster_domain = %q, want %q", cfg.Raft.ClusterDomain, tc.want)
			}
		})
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
	if cfg.CLI.CacheRepo != "dagger-kubernetes/cli-cache" {
		t.Fatalf("cli.cache_repo default = %q, want dagger-kubernetes/cli-cache", cfg.CLI.CacheRepo)
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
				CacheRepo:       "dagger-kubernetes/cli-cache",
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
			c.CLI.CacheRepo = ""
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
		{name: "empty cache_repo", mut: func(c *domain.Config) {
			c.CLI.CacheRepo = ""
		}, wantErr: "cli.cache_repo must not be empty"},
		{name: "invalid cache_repo", mut: func(c *domain.Config) {
			c.CLI.CacheRepo = "UPPER/INVALID"
		}, wantErr: "not a valid OCI repository name"},
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

func TestValidateCIConfig(t *testing.T) {
	base := func() *domain.Config {
		return &domain.Config{
			CI: domain.CIConfig{
				Jenkins: domain.JenkinsConfig{
					DynamicStages:     true,
					StepsPollInterval: 2 * time.Second,
					StepsMaxDepth:     8,
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
		{name: "disabled skips poll-interval check", mut: func(c *domain.Config) {
			c.CI.Jenkins.DynamicStages = false
			c.CI.Jenkins.StepsPollInterval = 0
		}},
		{name: "zero poll interval when enabled", mut: func(c *domain.Config) {
			c.CI.Jenkins.StepsPollInterval = 0
		}, wantErr: "ci.jenkins.steps_poll_interval must be > 0"},
		{name: "negative poll interval when enabled", mut: func(c *domain.Config) {
			c.CI.Jenkins.StepsPollInterval = -time.Second
		}, wantErr: "ci.jenkins.steps_poll_interval must be > 0"},
		{name: "negative max depth", mut: func(c *domain.Config) {
			c.CI.Jenkins.StepsMaxDepth = -1
		}, wantErr: "ci.jenkins.steps_max_depth must be >= 0"},
		{name: "zero max depth allowed", mut: func(c *domain.Config) {
			c.CI.Jenkins.StepsMaxDepth = 0
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base()
			tt.mut(cfg)
			err := validateCIConfig(cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateCIConfig = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateCIConfig = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateCIConfig = %q, want containing %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestLoadRejectsInvalidCIConfig(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{name: "zero poll interval", content: "ci:\n  jenkins:\n    dynamic_stages: true\n    steps_poll_interval: \"0s\"\n"},
		{name: "negative max depth", content: "ci:\n  jenkins:\n    steps_max_depth: -1\n"},
		{name: "disabled zero poll interval tolerated", content: "ci:\n  jenkins:\n    dynamic_stages: false\n    steps_poll_interval: \"0s\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.app.yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			_, err := Load(path)
			if tc.name == "disabled zero poll interval tolerated" {
				if err != nil {
					t.Fatalf("Load = %v, want nil (disabled)", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Load = nil, want validation error")
			}
			if !strings.Contains(err.Error(), "validate ci config") {
				t.Fatalf("Load error = %q, want wrapped ci validation message", err.Error())
			}
		})
	}
}

func TestLoadExtendedDurationUnits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.app.yaml")
	content := []byte(`
auth:
  jwt:
    refresh_ttl: "7d"
cache:
  gc:
    max_age: "30d"
    schedule: "1d12h"
history:
  gc:
    max_age: "7d"
fleet:
  replica_idle_ttl: "1w"
  version_retention: "1.5d"
cli:
  release_list_ttl: "2d3h"
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Auth.JWT.RefreshTTL != 168*time.Hour {
		t.Fatalf("auth.jwt.refresh_ttl = %v, want 168h (7d)", cfg.Auth.JWT.RefreshTTL)
	}
	if cfg.Cache.GC.MaxAge != 30*24*time.Hour {
		t.Fatalf("cache.gc.max_age = %v, want 720h (30d)", cfg.Cache.GC.MaxAge)
	}
	if cfg.Cache.GC.Schedule != 36*time.Hour {
		t.Fatalf("cache.gc.schedule = %v, want 36h (1d12h)", cfg.Cache.GC.Schedule)
	}
	if cfg.History.GC.MaxAge != 168*time.Hour {
		t.Fatalf("history.gc.max_age = %v, want 168h (7d)", cfg.History.GC.MaxAge)
	}
	if cfg.Fleet.ReplicaIdleTTL != 7*24*time.Hour {
		t.Fatalf("fleet.replica_idle_ttl = %v, want 168h (1w)", cfg.Fleet.ReplicaIdleTTL)
	}
	if cfg.Fleet.VersionRetention != 36*time.Hour {
		t.Fatalf("fleet.version_retention = %v, want 36h (1.5d)", cfg.Fleet.VersionRetention)
	}
	if cfg.CLI.ReleaseListTTL != 51*time.Hour {
		t.Fatalf("cli.release_list_ttl = %v, want 51h (2d3h)", cfg.CLI.ReleaseListTTL)
	}
}

func TestLoadEnvExtendedDuration(t *testing.T) {
	t.Setenv("DAGGER_KUBERNETES_AUTH_JWT_REFRESH_TTL", "7d")
	t.Setenv("DAGGER_KUBERNETES_HISTORY_GC_MAX_AGE", "14d")

	cfg, err := Load(filepath.Join(t.TempDir(), "config.app.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Auth.JWT.RefreshTTL != 168*time.Hour {
		t.Fatalf("env auth.jwt.refresh_ttl = %v, want 168h (7d)", cfg.Auth.JWT.RefreshTTL)
	}
	if cfg.History.GC.MaxAge != 14*24*time.Hour {
		t.Fatalf("env history.gc.max_age = %v, want 336h (14d)", cfg.History.GC.MaxAge)
	}
}

func TestLoadDottedMapKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.app.yaml")
	content := []byte(`
fleet:
  engine_pvc_labels:
    recurring-job-group.longhorn.io/nobackup: "enabled"
    recurring-job-group.longhorn.io/source: "enabled"
    recurring-job.longhorn.io/snapshots: "enabled"
  engine_node_selector:
    node-role.kubernetes.io/control-plane: "true"
  engine_extra_env_from:
    http.proxy:
      secretName: "proxy"
      key: "HTTP_PROXY"
  engine_registry_mirrors:
    docker.io:
      - "hm-registry.hm.dm.ad/docker-hub"
      - "mirror.gcr.io"
    public.ecr.aws:
      - "hm-registry.hm.dm.ad/docker-aws"
    ghcr.io:
      - "hm-registry.hm.dm.ad/docker-github"
    docker.elastic.co:
      - "hm-registry.hm.dm.ad/docker-elastic"
    gcr.io:
      - "hm-registry.hm.dm.ad/gcr.io"
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	wantLabels := map[string]string{
		"recurring-job-group.longhorn.io/nobackup": "enabled",
		"recurring-job-group.longhorn.io/source":   "enabled",
		"recurring-job.longhorn.io/snapshots":      "enabled",
	}
	if len(cfg.Fleet.EnginePVCLabels) != len(wantLabels) {
		t.Fatalf("engine_pvc_labels = %v, want %v", cfg.Fleet.EnginePVCLabels, wantLabels)
	}
	for k, want := range wantLabels {
		if cfg.Fleet.EnginePVCLabels[k] != want {
			t.Errorf("engine_pvc_labels[%q] = %q, want %q", k, cfg.Fleet.EnginePVCLabels[k], want)
		}
	}

	if cfg.Fleet.EngineNodeSelector["node-role.kubernetes.io/control-plane"] != "true" {
		t.Errorf("engine_node_selector = %v, want dotted key preserved", cfg.Fleet.EngineNodeSelector)
	}

	src, ok := cfg.Fleet.EngineExtraEnvFrom["http.proxy"]
	if !ok {
		t.Fatalf("engine_extra_env_from = %v, want dotted key http.proxy", cfg.Fleet.EngineExtraEnvFrom)
	}
	if src.SecretName != "proxy" || src.Key != "HTTP_PROXY" {
		t.Errorf("engine_extra_env_from[http.proxy] = %+v", src)
	}

	wantMirrors := map[string][]string{
		"docker.io":         {"hm-registry.hm.dm.ad/docker-hub", "mirror.gcr.io"},
		"public.ecr.aws":    {"hm-registry.hm.dm.ad/docker-aws"},
		"ghcr.io":           {"hm-registry.hm.dm.ad/docker-github"},
		"docker.elastic.co": {"hm-registry.hm.dm.ad/docker-elastic"},
		"gcr.io":            {"hm-registry.hm.dm.ad/gcr.io"},
	}
	if len(cfg.Fleet.EngineRegistryMirrors) != len(wantMirrors) {
		t.Fatalf("engine_registry_mirrors = %v, want %v", cfg.Fleet.EngineRegistryMirrors, wantMirrors)
	}
	for k, want := range wantMirrors {
		got := cfg.Fleet.EngineRegistryMirrors[k]
		if len(got) != len(want) {
			t.Fatalf("engine_registry_mirrors[%q] = %v, want %v", k, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("engine_registry_mirrors[%q][%d] = %q, want %q", k, i, got[i], want[i])
			}
		}
	}
}

func TestLoadRejectsInvalidDuration(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{name: "jwt refresh_ttl", content: "auth:\n  jwt:\n    refresh_ttl: \"banana\"\n"},
		{name: "history gc max_age", content: "history:\n  gc:\n    max_age: \"7x\"\n"},
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
				t.Fatal("Load = nil, want duration parse error")
			}
			if !strings.Contains(err.Error(), "unmarshal config") || !strings.Contains(err.Error(), "time:") {
				t.Fatalf("Load error = %q, want wrapped duration parse error", err.Error())
			}
		})
	}
}

func TestParseExtendedDuration(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr string
	}{
		{name: "days", in: "7d", want: 168 * time.Hour},
		{name: "weeks", in: "1w", want: 7 * 24 * time.Hour},
		{name: "two weeks", in: "2w", want: 14 * 24 * time.Hour},
		{name: "fractional days", in: "1.5d", want: 36 * time.Hour},
		{name: "negative days", in: "-7d", want: -168 * time.Hour},
		{name: "days and hours", in: "1d12h", want: 36 * time.Hour},
		{name: "mixed with minutes", in: "2d3h4m", want: 51*time.Hour + 4*time.Minute},
		{name: "std minutes", in: "90m", want: 90 * time.Minute},
		{name: "std composite", in: "1m30s", want: 90 * time.Second},
		{name: "std hours", in: "168h", want: 168 * time.Hour},
		{name: "zero", in: "0", want: 0},
		{name: "zero unit", in: "0s", want: 0},
		{name: "fractional hours", in: "1.5h", want: 90 * time.Minute},
		{name: "microseconds", in: "250us", want: 250 * time.Microsecond},
		{name: "huge days overflow float", in: strings.Repeat("9", 400) + "d", wantErr: "time:"},
		{name: "huge hours overflow int", in: strings.Repeat("9", 400) + "h", wantErr: "time:"},
		{name: "empty", in: "", wantErr: "invalid duration"},
		{name: "garbage", in: "abc", wantErr: "invalid duration"},
		{name: "unknown unit", in: "7x", wantErr: "unknown unit"},
		{name: "double unit", in: "7dd", wantErr: "time:"},
		{name: "unit first", in: "d7", wantErr: "time:"},
		{name: "bad decimal", in: "1.2.3d", wantErr: "time:"},
		{name: "space", in: "7d 1h", wantErr: "time:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseExtendedDuration(tt.in)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("parseExtendedDuration(%q) = %v, want %v", tt.in, err, tt.want)
				}
				if got != tt.want {
					t.Fatalf("parseExtendedDuration(%q) = %v, want %v", tt.in, got, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("parseExtendedDuration(%q) = %v, want error containing %q", tt.in, got, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("parseExtendedDuration(%q) error = %q, want containing %q", tt.in, err.Error(), tt.wantErr)
			}
		})
	}
}

func TestExactKeyPrefix(t *testing.T) {
	tests := []struct {
		name string
		src  any
		key  string
		want string
	}{
		{name: "nil source", src: nil, key: "a.b", want: ""},
		{name: "longest dotted match", src: map[string]any{"a.b": 1, "a": 2, "x": 3}, key: "a.b.c", want: "a.b"},
		{name: "exact dotted match", src: map[string]any{"docker.io": []any{"x"}}, key: "docker.io", want: "docker.io"},
		{name: "no match", src: map[string]any{"a": 1}, key: "x.y", want: ""},
		{name: "typed string map match", src: map[string]string{"a.b": "v"}, key: "a.b.c", want: "a.b"},
		{name: "typed string map no match", src: map[string]string{"a": "v"}, key: "b.c", want: ""},
		{name: "unhandled source type", src: []string{"a"}, key: "a.b", want: ""},
		{name: "empty key", src: map[string]any{"a": 1}, key: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exactKeyPrefix(tt.src, tt.key); got != tt.want {
				t.Fatalf("exactKeyPrefix(%#v, %q) = %q, want %q", tt.src, tt.key, got, tt.want)
			}
		})
	}
}

func TestInsertSetting(t *testing.T) {
	t.Run("fallback walk without structure", func(t *testing.T) {
		v := viper.New()
		dst := map[string]any{}
		insertSetting(v, dst, "x.y.z", "v")
		sub, ok := dst["x"].(map[string]any)
		if !ok {
			t.Fatalf("dst = %#v, want nested maps", dst)
		}
		sub2, ok := sub["y"].(map[string]any)
		if !ok || sub2["z"] != "v" {
			t.Fatalf("dst = %#v, want x.y.z = v", dst)
		}
	})

	t.Run("descends into existing subtree", func(t *testing.T) {
		v := viper.New()
		v.Set("a.b", 1)
		v.Set("a.c", 2)
		dst := map[string]any{}
		insertSetting(v, dst, "a.b", 1)
		insertSetting(v, dst, "a.c", 2)
		sub, ok := dst["a"].(map[string]any)
		if !ok || sub["b"] != 1 || sub["c"] != 2 {
			t.Fatalf("dst = %#v, want a.b = 1 and a.c = 2", dst)
		}
	})

	t.Run("dotted map key preserved", func(t *testing.T) {
		v := viper.New()
		v.Set("fleet.engine_registry_mirrors", map[string]any{"docker.io": []any{"mirror.gcr.io"}})
		dst := map[string]any{}
		insertSetting(v, dst, "fleet.engine_registry_mirrors.docker.io", []string{"mirror.gcr.io"})
		fleet, ok := dst["fleet"].(map[string]any)
		if !ok {
			t.Fatalf("dst = %#v, want fleet subtree", dst)
		}
		mirrors, ok := fleet["engine_registry_mirrors"].(map[string]any)
		if !ok {
			t.Fatalf("dst = %#v, want engine_registry_mirrors map", dst)
		}
		if _, ok := mirrors["docker.io"]; !ok {
			t.Fatalf("mirrors = %#v, want intact docker.io key", mirrors)
		}
		if _, mangled := mirrors["docker"]; mangled {
			t.Fatalf("mirrors = %#v, docker.io key was split on the dot", mirrors)
		}
	})

	t.Run("dotted key with nested map value", func(t *testing.T) {
		v := viper.New()
		v.Set("fleet.engine_extra_env_from", map[string]any{"http.proxy": map[string]any{"secretName": "proxy", "key": "HTTP_PROXY"}})
		dst := map[string]any{}
		insertSetting(v, dst, "fleet.engine_extra_env_from.http.proxy.secretname", "proxy")
		fleet, ok := dst["fleet"].(map[string]any)
		if !ok {
			t.Fatalf("dst = %#v, want fleet subtree", dst)
		}
		envFrom, ok := fleet["engine_extra_env_from"].(map[string]any)
		if !ok {
			t.Fatalf("dst = %#v, want engine_extra_env_from map", dst)
		}
		proxy, ok := envFrom["http.proxy"].(map[string]any)
		if !ok {
			t.Fatalf("envFrom = %#v, want intact http.proxy key", envFrom)
		}
		if proxy["secretname"] != "proxy" {
			t.Fatalf("envFrom = %#v, want http.proxy.secretname = proxy", envFrom)
		}
	})
}

func TestCollectSettingsSkipsShadowedKeys(t *testing.T) {
	t.Setenv("DAGGER_KUBERNETES_SERVER", "whole-tree-value")
	v := viper.New()
	v.SetEnvPrefix("DAGGER_KUBERNETES")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	v.SetDefault("server.control_addr", ":8080")
	v.SetDefault("log_level", "info")

	settings := collectSettings(v)
	if _, ok := settings["server"]; ok {
		t.Fatalf("settings = %#v, want server subtree shadowed by env and skipped", settings)
	}
	if settings["log_level"] != "info" {
		t.Fatalf("settings[log_level] = %v, want info", settings["log_level"])
	}
}

func TestStringToDurationHookFunc(t *testing.T) {
	hook := stringToDurationHookFunc()
	durType := reflect.TypeOf(time.Duration(0))
	strType := reflect.TypeOf("")

	out, err := hook(durType, durType, 5*time.Second)
	if err != nil || out != 5*time.Second {
		t.Fatalf("non-string passthrough = (%v, %v), want (5s, nil)", out, err)
	}
	out, err = hook(strType, strType, "keep")
	if err != nil || out != "keep" {
		t.Fatalf("non-duration target passthrough = (%v, %v), want (keep, nil)", out, err)
	}
	out, err = hook(strType, durType, "7d")
	if err != nil || out != 168*time.Hour {
		t.Fatalf("7d = (%v, %v), want (168h, nil)", out, err)
	}
	out, err = hook(strType, durType, "nope")
	if err == nil || out != nil {
		t.Fatalf("invalid duration = (%v, %v), want error", out, err)
	}
}

func TestStringToWeakSliceHookFunc(t *testing.T) {
	hook := stringToWeakSliceHookFunc(",")
	strType := reflect.TypeOf("")
	sliceType := reflect.TypeOf([]string{})
	intType := reflect.TypeOf(0)

	out, err := hook(strType, sliceType, "a,b")
	if err != nil {
		t.Fatalf("split = %v", err)
	}
	if got := out.([]string); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("split = %v, want [a b]", got)
	}
	out, err = hook(strType, sliceType, "")
	if err != nil {
		t.Fatalf("empty split = %v", err)
	}
	if got := out.([]string); len(got) != 0 {
		t.Fatalf("empty split = %v, want empty slice", got)
	}
	out, err = hook(strType, strType, "x")
	if err != nil || out != "x" {
		t.Fatalf("non-slice target passthrough = (%v, %v), want (x, nil)", out, err)
	}
	out, err = hook(intType, sliceType, 5)
	if err != nil || out != 5 {
		t.Fatalf("non-string passthrough = (%v, %v), want (5, nil)", out, err)
	}
}

func TestDecodeSettingsRejectsNonPointer(t *testing.T) {
	err := decodeSettings(nil, struct{}{})
	if err == nil || !strings.Contains(err.Error(), "pointer") {
		t.Fatalf("decodeSettings(nil, struct) = %v, want pointer error", err)
	}
}
