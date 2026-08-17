package config

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func Load(configFile string) (*domain.Config, error) {
	v := viper.New()

	v.SetConfigFile(configFile)
	v.SetConfigType("yaml")

	v.SetEnvPrefix("DAGGER_CACHE")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	v.SetDefault("server.control_addr", ":8080")
	v.SetDefault("server.data_addr", ":8443")
	v.SetDefault("server.data_hostname", "data.supv.example.com")
	v.SetDefault("server.public_url", "https://supv.example.com")

	v.SetDefault("auth.internal.enabled", true)
	v.SetDefault("auth.internal.tokens_file", "/etc/dagger-cache/tokens")
	v.SetDefault("auth.oauth.enabled", false)
	v.SetDefault("auth.oauth.provider", "github")
	v.SetDefault("auth.oauth.client_id", "")
	v.SetDefault("auth.oauth.client_secret", "")
	v.SetDefault("auth.oauth.redirect_url", "")
	v.SetDefault("auth.oauth.allowed_orgs", []string{})
	v.SetDefault("auth.oauth.default_group", "")

	v.SetDefault("auth.jwt.secret", "")
	v.SetDefault("auth.jwt.access_ttl", 15*time.Minute)
	v.SetDefault("auth.jwt.refresh_ttl", 168*time.Hour) // 7d

	v.SetDefault("auth.bootstrap_admin.username", "admin")
	v.SetDefault("auth.bootstrap_admin.password", "")

	v.SetDefault("database.path", "/var/lib/dagger-cache/dagger-cache.db")

	v.SetDefault("telemetry.collector_url", "http://otel-collector:4318")
	v.SetDefault("telemetry.tempo_url", "http://tempo:3200")
	v.SetDefault("telemetry.loki_url", "http://loki:3100")
	v.SetDefault("telemetry.victoria_url", "http://victoria:8428")

	v.SetDefault("cache.backend", "registry")
	v.SetDefault("cache.registry", "cache.reg/dagger-cache")
	v.SetDefault("cache.public_host", "")
	v.SetDefault("cache.internal_addr", "")
	v.SetDefault("cache.s3.bucket", "")
	v.SetDefault("cache.s3.region", "")
	v.SetDefault("cache.ref_per_version", true)

	v.SetDefault("cache.gc.enabled", false)
	v.SetDefault("cache.gc.max_age", "168h") // 7d
	v.SetDefault("cache.gc.schedule", "1h")
	v.SetDefault("cache.gc.min_refs_to_keep", 3)
	v.SetDefault("cache.gc.protect_active_versions", true)

	v.SetDefault("fleet.namespace", "dagger-cache")
	v.SetDefault("fleet.max_replicas_per_version", 3)
	v.SetDefault("fleet.max_sessions_per_replica", 8)
	v.SetDefault("fleet.replica_idle_ttl", 5*time.Minute)
	v.SetDefault("fleet.version_retention", 24*time.Hour)
	v.SetDefault("fleet.min_replicas_per_version", 0)
	v.SetDefault("fleet.engine_image_registry", "registry.dagger.io/engine")
	v.SetDefault("fleet.engine_storage_class", "")
	v.SetDefault("fleet.engine_storage_size", "50Gi")
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

	v.SetDefault("tls.server_cert_secret", "supervisor-tls")
	v.SetDefault("tls.provider", "embedded")
	v.SetDefault("tls.ca_path", "/var/lib/dagger-cache/ca")
	v.SetDefault("tls.cert_path", "/etc/dagger-cache/tls/tls.crt")
	v.SetDefault("tls.key_path", "/etc/dagger-cache/tls/tls.key")

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

	return &cfg, nil
}
