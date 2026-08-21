package config

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func Load(configFile string) (*domain.Config, error) {
	v := viper.New()

	v.SetConfigFile(configFile)
	v.SetConfigType("yaml")

	v.SetEnvPrefix("DAGGER_KUBERNETES")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	v.SetDefault("server.control_addr", ":8080")
	v.SetDefault("server.data_addr", ":8443")
	v.SetDefault("server.data_hostname", "data.supv.example.com")
	v.SetDefault("server.public_url", "https://supv.example.com")

	v.SetDefault("auth.internal.enabled", true)
	v.SetDefault("auth.internal.tokens_file", "/etc/dagger-kubernetes/tokens")
	v.SetDefault("auth.oauth.enabled", false)
	v.SetDefault("auth.oauth.provider", "github")
	v.SetDefault("auth.oauth.client_id", "")
	v.SetDefault("auth.oauth.client_secret", "")
	v.SetDefault("auth.oauth.redirect_url", "")
	v.SetDefault("auth.oauth.allowed_orgs", []string{})
	v.SetDefault("auth.oauth.allowed_teams", []string{})
	v.SetDefault("auth.oauth.allowed_groups", []string{})
	v.SetDefault("auth.oauth.group_mappings", []domain.GroupMappingRule{})
	v.SetDefault("auth.oauth.default_group", "")
	v.SetDefault("auth.oauth.cookie_secure", false)
	v.SetDefault("auth.oauth.issuer_url", "")
	v.SetDefault("auth.oauth.scopes", []string{"openid", "profile", "email"})
	v.SetDefault("auth.oauth.username_claim", "preferred_username")
	v.SetDefault("auth.oauth.groups_claim", "groups")

	v.SetDefault("auth.jwt.secret", "")
	v.SetDefault("auth.jwt.access_ttl", 15*time.Minute)
	v.SetDefault("auth.jwt.refresh_ttl", 168*time.Hour) // 7d

	v.SetDefault("auth.token.encryption_key", "")

	v.SetDefault("auth.bootstrap_admin.username", "admin")
	v.SetDefault("auth.bootstrap_admin.password", "")

	v.SetDefault("auth.cookie.access_name", "dagger_kubernetes_access")
	v.SetDefault("auth.cookie.refresh_name", "dagger_kubernetes_refresh")
	v.SetDefault("auth.cookie.secure", false)
	v.SetDefault("auth.cors.allowed_origins", []string{})

	v.SetDefault("database.dir", "/var/lib/dagger-kubernetes")

	v.SetDefault("raft.node_id", "")
	v.SetDefault("raft.bind_addr", ":8081")
	v.SetDefault("raft.advertise_addr", "")
	v.SetDefault("raft.peers", []domain.RaftPeer{})
	v.SetDefault("raft.replicas", 1)
	v.SetDefault("raft.statefulset_name", "")
	v.SetDefault("raft.headless_service", "")
	v.SetDefault("raft.namespace", "")
	v.SetDefault("raft.cluster_domain", "cluster.local")
	v.SetDefault("raft.apply_timeout", 5*time.Second)
	v.SetDefault("raft.leader_wait_timeout", 30*time.Second)
	v.SetDefault("raft.snapshot_threshold", uint64(1000))
	v.SetDefault("raft.snapshot_interval", 10*time.Minute)
	v.SetDefault("raft.trailing_logs", uint64(256))
	v.SetDefault("raft.tls.enabled", false)
	v.SetDefault("raft.tls.dir", "")
	v.SetDefault("raft.tls.validity", 8760*time.Hour)
	v.SetDefault("raft.tls.organization", "dagger-kubernetes-raft")
	v.SetDefault("raft.tls.ca_cert", "")
	v.SetDefault("raft.tls.cert", "")
	v.SetDefault("raft.tls.key", "")
	v.SetDefault("raft.tls.ca_secret", "")
	v.SetDefault("raft.tls.ca_bootstrap", false)
	v.SetDefault("raft.tls.client_auth", true)

	v.SetDefault("telemetry.collector_url", "http://otel-collector:4318")
	v.SetDefault("telemetry.tempo_url", "http://tempo:3200")
	v.SetDefault("telemetry.loki_url", "http://loki:3100")
	v.SetDefault("telemetry.victoria_url", "http://victoria:8428")

	v.SetDefault("cache.backend", "registry")
	v.SetDefault("cache.registry", "cache.reg/dagger-cache")
	v.SetDefault("cache.public_host", "")
	v.SetDefault("cache.internal_addr", "")
	v.SetDefault("cache.auth_token", "")
	v.SetDefault("cache.registries", []domain.RegistryBackend{})
	v.SetDefault("cache.s3.bucket", "")
	v.SetDefault("cache.s3.region", "")

	v.SetDefault("cache.gc.enabled", false)
	v.SetDefault("cache.gc.max_age", "168h") // 7d
	v.SetDefault("cache.gc.schedule", "1h")
	v.SetDefault("cache.gc.min_refs_to_keep", 3)
	v.SetDefault("cache.gc.protect_active_versions", true)

	v.SetDefault("history.gc.enabled", false)
	v.SetDefault("history.gc.max_age", "720h") // 30d
	v.SetDefault("history.gc.schedule", "1h")

	v.SetDefault("pipeline.disconnect_grace", 0) // 0 = immediate
	v.SetDefault("pipeline.stale_sweep.enabled", true)
	v.SetDefault("pipeline.stale_sweep.schedule", time.Minute)
	v.SetDefault("pipeline.stale_sweep.stale_after", 5*time.Minute)

	v.SetDefault("fleet.namespace", "dagger-kubernetes")
	v.SetDefault("fleet.max_replicas_per_version", 3)
	v.SetDefault("fleet.max_sessions_per_replica", 8)
	v.SetDefault("fleet.replica_idle_ttl", 5*time.Minute)
	v.SetDefault("fleet.version_retention", 24*time.Hour)
	v.SetDefault("fleet.engine_image_registry", "registry.dagger.io/engine")
	v.SetDefault("fleet.engine_storage_class", "")
	v.SetDefault("fleet.engine_storage_size", "50Gi")
	v.SetDefault("fleet.engine_pvc_labels", map[string]string{})
	v.SetDefault("fleet.engine_cpu_request", "500m")
	v.SetDefault("fleet.engine_cpu_limit", "2000m")
	v.SetDefault("fleet.engine_memory_request", "1Gi")
	v.SetDefault("fleet.engine_memory_limit", "8Gi")
	v.SetDefault("fleet.engine_termination_grace_seconds", 120)
	v.SetDefault("fleet.engine_node_selector", map[string]string{})
	v.SetDefault("fleet.engine_tolerations", []string{})
	v.SetDefault("fleet.engine_extra_args", []string{})
	v.SetDefault("fleet.engine_pull_policy", "IfNotPresent")
	v.SetDefault("fleet.engine_privileged", false)
	v.SetDefault("fleet.engine_extra_env", map[string]string{})
	v.SetDefault("fleet.engine_ca_secret", "")
	v.SetDefault("fleet.engine_ca_secret_key", "ca.crt")
	v.SetDefault("fleet.engine_debug", false)
	v.SetDefault("fleet.engine_log_format", "json")
	v.SetDefault("fleet.engine_registry_mirrors", map[string][]string{})
	v.SetDefault("fleet.engine_extra_env_from", map[string]domain.EnvVarSource{})

	v.SetDefault("ca.minting_ca_secret", "supervisor-minting-ca")
	v.SetDefault("ca.client_cert_ttl", 2*time.Hour)

	v.SetDefault("lease_ttl", 2*time.Minute)

	v.SetDefault("tls.provider", "embedded")
	v.SetDefault("tls.ca_path", "/var/lib/dagger-kubernetes/ca")
	v.SetDefault("tls.cert_path", "/etc/dagger-kubernetes/tls/tls.crt")
	v.SetDefault("tls.key_path", "/etc/dagger-kubernetes/tls/tls.key")

	v.SetDefault("version.floor", "v0.19.0")
	v.SetDefault("version.allowlist", []string{})

	v.SetDefault("ci.github.job_summary", true)
	v.SetDefault("ci.github.check_runs", true)
	v.SetDefault("ci.jenkins.dynamic_stages", true)
	v.SetDefault("ci.drone.config_extension", true)

	v.SetDefault("log_level", "info")
	v.SetDefault("log_format", "json")

	v.SetDefault("otel.otlp_endpoint", "")

	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		var nf viper.ConfigFileNotFoundError
		if !errors.As(err, &nf) && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("read config: %w", err)
		}
	}

	var cfg domain.Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	if err := validateAuthConfig(&cfg); err != nil {
		return nil, fmt.Errorf("validate auth config: %w", err)
	}

	if err := validateGroupMappings(&cfg); err != nil {
		return nil, fmt.Errorf("validate group mappings: %w", err)
	}

	if err := validateServerConfig(&cfg); err != nil {
		return nil, fmt.Errorf("validate server config: %w", err)
	}

	if err := validateFleetConfig(&cfg); err != nil {
		return nil, fmt.Errorf("validate fleet config: %w", err)
	}

	return &cfg, nil
}

// validateServerConfig ensures server.public_url is an absolute http(s) URL
// with a host (validated via PipelineViewURL with a placeholder trace ID),
// since it doubles as the pipeline-view base URL. Fails fast at startup when
// it is not.
func validateServerConfig(cfg *domain.Config) error {
	if cfg.Server.PublicURL == "" {
		return fmt.Errorf("server.public_url must be set so a pipeline view URL can be derived")
	}
	if _, err := domain.PipelineViewURL(cfg.Server.PublicURL, "traceid-placeholder"); err != nil {
		return fmt.Errorf("server.public_url: %w", err)
	}
	return nil
}

// validateAuthConfig enforces the auth-provider gating rules:
//   - auth is always required (no fully-disabled mode);
//   - auth.internal.enabled: false is only allowed when OAuth is enabled and
//     fully configured for the chosen provider (github: client_id+secret+redirect_url;
//     oidc: issuer_url+client_id+secret+redirect_url);
//   - when auth.oauth.enabled: true, the provider must be "github" or "oidc"
//     and the per-provider required fields must be non-empty.
func validateAuthConfig(cfg *domain.Config) error {
	o := cfg.Auth.OAuth
	if o.Enabled {
		switch o.Provider {
		case "github":
			if anyEmpty(o.ClientID, o.ClientSecret, o.RedirectURL) {
				return fmt.Errorf("auth.oauth.enabled: true requires client_id, client_secret, and redirect_url")
			}
		case "oidc":
			if anyEmpty(o.IssuerURL, o.ClientID, o.ClientSecret, o.RedirectURL) {
				return fmt.Errorf(`auth.oauth.enabled: true with provider "oidc" requires issuer_url, client_id, client_secret, and redirect_url`)
			}
		default:
			return fmt.Errorf(`auth.oauth.provider: only "github" and "oidc" are supported`)
		}
	}

	if !cfg.Auth.Internal.Enabled && !o.Enabled {
		return fmt.Errorf("auth.internal.enabled: false requires auth.oauth.enabled: true with a fully configured provider")
	}

	return nil
}

// validateGroupMappings fails fast on invalid group-mapping rules: each rule
// needs a non-empty pattern that compiles as a Go regexp, and a non-empty
// replacement. allowed_teams entries must be non-empty "org/team" strings.
// Errors name the offending config path and regex.
func validateGroupMappings(cfg *domain.Config) error {
	for i, rule := range cfg.Auth.OAuth.GroupMappings {
		if rule.Pattern == "" {
			return fmt.Errorf("auth.oauth.group_mappings[%d].pattern must not be empty", i)
		}
		if _, err := regexp.Compile(rule.Pattern); err != nil {
			return fmt.Errorf("auth.oauth.group_mappings[%d].pattern %q: %w", i, rule.Pattern, err)
		}
		if rule.Replacement == "" {
			return fmt.Errorf("auth.oauth.group_mappings[%d].replacement must not be empty", i)
		}
	}

	for i, team := range cfg.Auth.OAuth.AllowedTeams {
		if team == "" || strings.Count(team, "/") != 1 {
			return fmt.Errorf("auth.oauth.allowed_teams[%d] must be a non-empty \"org/team\" string", i)
		}
	}

	return nil
}

// validateFleetConfig guards against a misconfigured version_retention that
// would silently cause irreversible engine-fleet deletion (StatefulSet +
// Service, and its PVCs via the StatefulSet retention policy). version_retention
// <= 0 is the documented "disabled" value; a positive value below one minute is
// almost always a unit mistake — in particular, an unquoted YAML integer (e.g.
// `version_retention: 24`) is decoded by Viper/mapstructure as *nanoseconds*,
// not hours, which would GC idle fleets within one sweep tick.
func validateFleetConfig(cfg *domain.Config) error {
	if cfg.Fleet.VersionRetention > 0 && cfg.Fleet.VersionRetention < time.Minute {
		return fmt.Errorf("fleet.version_retention = %v is too small; use a duration with an explicit unit of at least 1m (or <= 0 to disable GC)", cfg.Fleet.VersionRetention)
	}
	return nil
}

// anyEmpty reports whether any of the supplied strings is empty.
func anyEmpty(fields ...string) bool {
	for _, f := range fields {
		if f == "" {
			return true
		}
	}
	return false
}
