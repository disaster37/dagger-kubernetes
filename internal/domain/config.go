package domain

import "time"

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
	LogLevel  string          `mapstructure:"log_level"`
	LogFormat string          `mapstructure:"log_format"` // "json" (default) | "text"
	OTel      OTelConfig      `mapstructure:"otel"`
	Database  DatabaseConfig  `mapstructure:"database"`
}

type ServerConfig struct {
	ControlAddr string `mapstructure:"control_addr"`
	DataAddr    string `mapstructure:"data_addr"`
	DataHost    string `mapstructure:"data_hostname"`
	PublicURL   string `mapstructure:"public_url"`
}

type AuthConfig struct {
	Internal       InternalAuthConfig   `mapstructure:"internal"`
	OAuth          OAuthConfig          `mapstructure:"oauth"`
	JWT            JWTConfig            `mapstructure:"jwt"`
	BootstrapAdmin BootstrapAdminConfig `mapstructure:"bootstrap_admin"`
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
	DefaultGroup string   `mapstructure:"default_group"` // auto-membership for new OAuth users; empty = none
}

// DatabaseConfig configures the SQLite database backing the multi-user store.
type DatabaseConfig struct {
	Path string `mapstructure:"path"`
}

// JWTConfig configures HS256 JWT issuance (access + refresh).
type JWTConfig struct {
	Secret     string        `mapstructure:"secret"`
	AccessTTL  time.Duration `mapstructure:"access_ttl"`
	RefreshTTL time.Duration `mapstructure:"refresh_ttl"`
}

// BootstrapAdminConfig configures the first-boot admin user creation.
type BootstrapAdminConfig struct {
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
}

type TelemetryConfig struct {
	CollectorURL string `mapstructure:"collector_url"`
	TempoURL     string `mapstructure:"tempo_url"`
	LokiURL      string `mapstructure:"loki_url"`
	VictoriaURL  string `mapstructure:"victoria_url"`
}

type CacheConfig struct {
	Backend       string   `mapstructure:"backend"`
	Registry      string   `mapstructure:"registry"`
	PublicHost    string   `mapstructure:"public_host"`
	InternalAddr  string   `mapstructure:"internal_addr"`
	S3            S3Config `mapstructure:"s3"`
	RefPerVersion bool     `mapstructure:"ref_per_version"`
}

type S3Config struct {
	Bucket string `mapstructure:"bucket"`
	Region string `mapstructure:"region"`
}

// EnvVarSource selects one key of a Kubernetes Secret as the value of an
// engine container env var (fleet.engine_extra_env_from).
type EnvVarSource struct {
	SecretName string `mapstructure:"secret_name"`
	Key        string `mapstructure:"key"`
}

type FleetConfig struct {
	Namespace              string                  `mapstructure:"namespace"`
	MaxReplicasPerVersion  int                     `mapstructure:"max_replicas_per_version"`
	MaxSessionsPerReplica  int                     `mapstructure:"max_sessions_per_replica"`
	ReplicaIdleTTL         time.Duration           `mapstructure:"replica_idle_ttl"`
	VersionRetention       time.Duration           `mapstructure:"version_retention"`
	MinReplicasPerVersion  int                     `mapstructure:"min_replicas_per_version"`
	EngineImageRegistry    string                  `mapstructure:"engine_image_registry"`
	EngineStorageClass     string                  `mapstructure:"engine_storage_class"`
	EngineStorageSize      string                  `mapstructure:"engine_storage_size"`
	EngineCPURequest       string                  `mapstructure:"engine_cpu_request"`
	EngineCPULimit         string                  `mapstructure:"engine_cpu_limit"`
	EngineMemoryRequest    string                  `mapstructure:"engine_memory_request"`
	EngineMemoryLimit      string                  `mapstructure:"engine_memory_limit"`
	EngineTerminationGrace int                     `mapstructure:"engine_termination_grace_seconds"`
	EngineNodeSelector     map[string]string       `mapstructure:"engine_node_selector"`
	EngineTolerations      []string                `mapstructure:"engine_tolerations"`
	EngineExtraArgs        []string                `mapstructure:"engine_extra_args"`
	EnginePullPolicy       string                  `mapstructure:"engine_pull_policy"`
	EnginePrivileged       bool                    `mapstructure:"engine_privileged"`
	EngineExtraEnv         map[string]string       `mapstructure:"engine_extra_env"`
	EngineExtraEnvFrom     map[string]EnvVarSource `mapstructure:"engine_extra_env_from"`
	EngineCASecret         string                  `mapstructure:"engine_ca_secret"`
	EngineCASecretKey      string                  `mapstructure:"engine_ca_secret_key"`
	EngineDebug            bool                    `mapstructure:"engine_debug"`
	EngineLogFormat        string                  `mapstructure:"engine_log_format"`
	EngineRegistryMirrors  map[string][]string     `mapstructure:"engine_registry_mirrors"`
}

type CAConfig struct {
	MintingCASecret string        `mapstructure:"minting_ca_secret"`
	ClientCertTTL   time.Duration `mapstructure:"client_cert_ttl"`
}

type TLSConfig struct {
	Provider         string `mapstructure:"provider"`
	ServerCertSecret string `mapstructure:"server_cert_secret"`
	CAPath           string `mapstructure:"ca_path"`
	CertPath         string `mapstructure:"cert_path"`
	KeyPath          string `mapstructure:"key_path"`
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

type OTelConfig struct {
	OTLPEndpoint string `mapstructure:"otlp_endpoint"`
}
