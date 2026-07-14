package config

import (
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Server    ServerConfig    `mapstructure:"server"`
	Auth      AuthConfig      `mapstructure:"auth"`
	Telemetry TelemetryConfig `mapstructure:"telemetry"`
	Cache     CacheConfig     `mapstructure:"cache"`
	Fleet     FleetConfig     `mapstructure:"fleet"`
	CA        CAConfig        `mapstructure:"ca"`
	TLS       TLSConfig       `mapstructure:"tls"`
	Version   VersionConfig   `mapstructure:"version"`
	LeaseTTL  time.Duration   `mapstructure:"lease_ttl"`
	CI        CIConfig        `mapstructure:"ci"`
	UI        UIConfig        `mapstructure:"ui"`
	LogLevel  string          `mapstructure:"log_level"`
	OTel      OTelConfig      `mapstructure:"otel"`
}

type ServerConfig struct {
	ControlAddr string `mapstructure:"control_addr"`
	DataAddr    string `mapstructure:"data_addr"`
	DataHost    string `mapstructure:"data_hostname"`
	PublicURL   string `mapstructure:"public_url"`
	UIURL       string `mapstructure:"ui_url"`
}

type AuthConfig struct {
	Internal InternalAuthConfig `mapstructure:"internal"`
	OAuth    OAuthConfig        `mapstructure:"oauth"`
}

type InternalAuthConfig struct {
	Enabled    bool   `mapstructure:"enabled"`
	TokensFile string `mapstructure:"tokens_file"`
}

type OAuthConfig struct {
	Enabled      bool     `mapstructure:"enabled"`
	Provider     string   `mapstructure:"provider"`
	ClientID     string   `mapstructure:"client_id"`
	ClientSecret string   `mapstructure:"client_secret"`
	RedirectURL  string   `mapstructure:"redirect_url"`
	AllowedOrgs  []string `mapstructure:"allowed_orgs"`
}

type TelemetryConfig struct {
	CollectorURL  string `mapstructure:"collector_url"`
	TempoURL      string `mapstructure:"tempo_url"`
	LokiURL       string `mapstructure:"loki_url"`
	PrometheusURL string `mapstructure:"prometheus_url"`
}

type CacheConfig struct {
	Backend       string   `mapstructure:"backend"`
	Registry      string   `mapstructure:"registry"`
	S3            S3Config `mapstructure:"s3"`
	RefPerVersion bool     `mapstructure:"ref_per_version"`
}

type S3Config struct {
	Bucket string `mapstructure:"bucket"`
	Region string `mapstructure:"region"`
}

type FleetConfig struct {
	Namespace             string        `mapstructure:"namespace"`
	MaxReplicasPerVersion int           `mapstructure:"max_replicas_per_version"`
	MaxSessionsPerReplica int           `mapstructure:"max_sessions_per_replica"`
	ReplicaIdleTTL        time.Duration `mapstructure:"replica_idle_ttl"`
	VersionRetention      time.Duration `mapstructure:"version_retention"`
	MinReplicasPerVersion int           `mapstructure:"min_replicas_per_version"`
}

type CAConfig struct {
	MintingCASecret string        `mapstructure:"minting_ca_secret"`
	ClientCertTTL   time.Duration `mapstructure:"client_cert_ttl"`
}

type TLSConfig struct {
	ServerCertSecret string `mapstructure:"server_cert_secret"`
}

type VersionConfig struct {
	Floor     string   `mapstructure:"floor"`
	Allowlist []string `mapstructure:"allowlist"`
}

type CIConfig struct {
	GitHub  GHAConfig     `mapstructure:"github"`
	Jenkins JenkinsConfig `mapstructure:"jenkins"`
	Drone   DroneConfig   `mapstructure:"drone"`
}

type GHAConfig struct {
	JobSummary bool `mapstructure:"job_summary"`
	CheckRuns  bool `mapstructure:"check_runs"`
}

type JenkinsConfig struct {
	DynamicStages bool `mapstructure:"dynamic_stages"`
}

type DroneConfig struct {
	ConfigExtension bool `mapstructure:"config_extension"`
}

type UIConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	SPADir  string `mapstructure:"spa_dir"`
}

type OTelConfig struct {
	OTLPEndpoint string `mapstructure:"otlp_endpoint"`
}

func Load(configFile string) (*Config, error) {
	v := viper.New()

	v.SetConfigFile(configFile)
	v.SetConfigType("yaml")

	v.SetEnvPrefix("DAGGER_CACHE")
	v.AutomaticEnv()

	v.SetDefault("server.control_addr", ":8080")
	v.SetDefault("server.data_addr", ":8443")
	v.SetDefault("server.data_hostname", "data.supv.example.com")
	v.SetDefault("server.public_url", "https://supv.example.com")
	v.SetDefault("server.ui_url", "https://ui.supv.example.com")

	v.SetDefault("auth.internal.enabled", true)
	v.SetDefault("auth.internal.tokens_file", "/etc/dagger-cache/tokens")
	v.SetDefault("auth.oauth.enabled", false)
	v.SetDefault("auth.oauth.provider", "github")

	v.SetDefault("telemetry.collector_url", "http://otel-collector:4318")
	v.SetDefault("telemetry.tempo_url", "http://tempo:3200")
	v.SetDefault("telemetry.loki_url", "http://loki:3100")
	v.SetDefault("telemetry.prometheus_url", "http://prometheus:9090")

	v.SetDefault("cache.backend", "registry")
	v.SetDefault("cache.registry", "cache.reg/dagger-cache")
	v.SetDefault("cache.ref_per_version", true)

	v.SetDefault("fleet.namespace", "dagger-cache")
	v.SetDefault("fleet.max_replicas_per_version", 3)
	v.SetDefault("fleet.max_sessions_per_replica", 8)
	v.SetDefault("fleet.replica_idle_ttl", 5*time.Minute)
	v.SetDefault("fleet.version_retention", 24*time.Hour)
	v.SetDefault("fleet.min_replicas_per_version", 0)

	v.SetDefault("ca.minting_ca_secret", "supervisor-minting-ca")
	v.SetDefault("ca.client_cert_ttl", 2*time.Hour)

	v.SetDefault("lease_ttl", 2*time.Minute)

	v.SetDefault("tls.server_cert_secret", "supervisor-tls")

	v.SetDefault("version.floor", "v0.19.0")

	v.SetDefault("ci.github.job_summary", true)
	v.SetDefault("ci.github.check_runs", true)
	v.SetDefault("ci.jenkins.dynamic_stages", true)
	v.SetDefault("ci.drone.config_extension", true)

	v.SetDefault("ui.enabled", true)
	v.SetDefault("ui.spa_dir", "/opt/dagger-cache/ui")

	v.SetDefault("log_level", "info")

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, err
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}
