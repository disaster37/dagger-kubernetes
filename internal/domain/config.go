package domain

import "time"

type Config struct {
	Server    ServerConfig    `mapstructure:"server"`
	Auth      AuthConfig      `mapstructure:"auth"`
	Telemetry TelemetryConfig `mapstructure:"telemetry"`
	Cache     CacheConfig     `mapstructure:"cache"`
	History   HistoryConfig   `mapstructure:"history"`
	Fleet     FleetConfig     `mapstructure:"fleet"`
	CA        CAConfig        `mapstructure:"ca"`
	TLS       TLSConfig       `mapstructure:"tls"`
	Version   VersionConfig   `mapstructure:"version"`
	LeaseTTL  time.Duration   `mapstructure:"lease_ttl"`
	Pipeline  PipelineConfig  `mapstructure:"pipeline"`
	CI        CIConfig        `mapstructure:"ci"`
	LogLevel  string          `mapstructure:"log_level"`
	LogFormat string          `mapstructure:"log_format"` // "json" (default) | "text"
	OTel      OTelConfig      `mapstructure:"otel"`
	Database  DatabaseConfig  `mapstructure:"database"`
	Raft      RaftConfig      `mapstructure:"raft"`
}

type ServerConfig struct {
	ControlAddr string `mapstructure:"control_addr"`
	DataAddr    string `mapstructure:"data_addr"`
	DataHost    string `mapstructure:"data_hostname"`
	PublicURL   string `mapstructure:"public_url"`
	PipelineURL string `mapstructure:"pipeline_url"` // base for pipeline-view links; "" => fall back to PublicURL
}

type AuthConfig struct {
	Internal       InternalAuthConfig   `mapstructure:"internal"`
	OAuth          OAuthConfig          `mapstructure:"oauth"`
	JWT            JWTConfig            `mapstructure:"jwt"`
	Token          TokenConfig          `mapstructure:"token"`
	BootstrapAdmin BootstrapAdminConfig `mapstructure:"bootstrap_admin"`
}

type InternalAuthConfig struct {
	Enabled    bool   `mapstructure:"enabled"`
	TokensFile string `mapstructure:"tokens_file"`
}

// OAuthConfig configures OAuth for human/UI login. A single provider is active
// per deployment, selected by the `provider` discriminator ("github" | "oidc").
// The OIDC-only fields (IssuerURL, Scopes, UsernameClaim, GroupsClaim) are
// ignored when provider is "github"; the github-only fields (AllowedOrgs as org
// membership) are ignored by oidc (which matches AllowedOrgs against the groups
// claim instead).
type OAuthConfig struct {
	Enabled      bool     `mapstructure:"enabled"`
	Provider     string   `mapstructure:"provider"` // "github" | "oidc"
	ClientID     string   `mapstructure:"client_id"`
	ClientSecret string   `mapstructure:"client_secret"`
	RedirectURL  string   `mapstructure:"redirect_url"`
	AllowedOrgs  []string `mapstructure:"allowed_orgs"`  // github: org membership; oidc: groups claim intersection
	DefaultGroup string   `mapstructure:"default_group"` // auto-membership for new OAuth users; empty = none
	// CookieSecure forces the Secure flag on the oauth_state cookie; set true
	// when TLS terminates at an ingress/proxy in front of the supervisor.
	CookieSecure bool `mapstructure:"cookie_secure"`

	// OIDC-only fields (ignored when provider: github).
	IssuerURL     string   `mapstructure:"issuer_url"`     // required for provider: oidc
	Scopes        []string `mapstructure:"scopes"`         // default ["openid","profile","email"]
	UsernameClaim string   `mapstructure:"username_claim"` // default "preferred_username"; fallback "email"
	GroupsClaim   string   `mapstructure:"groups_claim"`   // default "groups"
}

// DatabaseConfig configures the Raft data directory backing the multi-user
// store (raft.db bolt log, snapshots/, node-id). The store starts fresh; there
// is no migration from the legacy SQLite store.
type DatabaseConfig struct {
	Dir string `mapstructure:"dir"`
}

// RaftConfig configures the always-on Hashicorp Raft replicated state machine
// that replaced SQLite as the persistence engine (see ADR-015). Multi-node
// discovery and transport TLS are covered by ADR-016.
type RaftConfig struct {
	NodeID            string        `mapstructure:"node_id"`
	BindAddr          string        `mapstructure:"bind_addr"`
	AdvertiseAddr     string        `mapstructure:"advertise_addr"`
	Peers             []RaftPeer    `mapstructure:"peers"`
	Replicas          int           `mapstructure:"replicas"`
	StatefulSetName   string        `mapstructure:"statefulset_name"`
	HeadlessService   string        `mapstructure:"headless_service"`
	Namespace         string        `mapstructure:"namespace"`
	ClusterDomain     string        `mapstructure:"cluster_domain"`
	ApplyTimeout      time.Duration `mapstructure:"apply_timeout"`
	LeaderWaitTimeout time.Duration `mapstructure:"leader_wait_timeout"`
	SnapshotThreshold uint64        `mapstructure:"snapshot_threshold"`
	SnapshotInterval  time.Duration `mapstructure:"snapshot_interval"`
	TrailingLogs      uint64        `mapstructure:"trailing_logs"`
	TLS               RaftTLSConfig `mapstructure:"tls"`
}

// RaftPeer is one voter in the Raft cluster.
type RaftPeer struct {
	ID      string `mapstructure:"id"`
	Address string `mapstructure:"address"`
}

// RaftTLSConfig configures transport TLS for the Raft StreamLayer. Peers
// present a per-node leaf certificate and verify each other against a shared
// internal CA generated with goca (ADR-016).
type RaftTLSConfig struct {
	Enabled      bool          `mapstructure:"enabled"`
	Dir          string        `mapstructure:"dir"`
	Validity     time.Duration `mapstructure:"validity"`
	Organization string        `mapstructure:"organization"`
	CACertPath   string        `mapstructure:"ca_cert"`
	CertPath     string        `mapstructure:"cert"`
	KeyPath      string        `mapstructure:"key"`
	CASecret     string        `mapstructure:"ca_secret"`
	CABootstrap  bool          `mapstructure:"ca_bootstrap"`
	ClientAuth   bool          `mapstructure:"client_auth"`
}

// JWTConfig configures HS256 JWT issuance (access + refresh).
type JWTConfig struct {
	Secret     string        `mapstructure:"secret"`
	AccessTTL  time.Duration `mapstructure:"access_ttl"`
	RefreshTTL time.Duration `mapstructure:"refresh_ttl"`
}

// TokenConfig configures API-token plaintext recovery (Connect-env UI).
type TokenConfig struct {
	EncryptionKey string `mapstructure:"encryption_key"` // >= 32 bytes; empty = auto-generated + persisted in meta
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

// RegistryBackend is one backend OCI registry the Supervisor proxies to.
type RegistryBackend struct {
	ID           string `mapstructure:"id"`
	InternalAddr string `mapstructure:"internal_addr"` // host[:port], no scheme
	Username     string `mapstructure:"username"`
	Password     string `mapstructure:"password"`
}

type CacheConfig struct {
	Backend       string            `mapstructure:"backend"`       // "registry" | "s3"
	Registry      string            `mapstructure:"registry"`      // legacy single ref "host/repo"
	PublicHost    string            `mapstructure:"public_host"`   // dedicated cache vhost
	InternalAddr  string            `mapstructure:"internal_addr"` // legacy single backend addr
	AuthToken     string            `mapstructure:"auth_token"`    // engine→proxy bearer
	Registries    []RegistryBackend `mapstructure:"registries"`    // multi-backend list
	S3            S3Config          `mapstructure:"s3"`
	RefPerVersion bool              `mapstructure:"ref_per_version"`
	GC            GCConfig          `mapstructure:"gc"`
}

type S3Config struct {
	Bucket string `mapstructure:"bucket"`
	Region string `mapstructure:"region"`
}

// GCConfig governs the cache auto-clean background sweeper.
type GCConfig struct {
	Enabled               bool          `mapstructure:"enabled"`
	MaxAge                time.Duration `mapstructure:"max_age"`
	Schedule              time.Duration `mapstructure:"schedule"`
	MinRefsToKeep         int           `mapstructure:"min_refs_to_keep"`
	ProtectActiveVersions bool          `mapstructure:"protect_active_versions"`
}

// HistoryConfig governs pipeline-history retention (trace_meta + logs +
// metrics). Mirrors CacheConfig.GC.
type HistoryConfig struct {
	GC HistoryGCConfig `mapstructure:"gc"`
}

// HistoryGCConfig governs the history auto-purge background sweeper.
type HistoryGCConfig struct {
	Enabled  bool          `mapstructure:"enabled"`
	MaxAge   time.Duration `mapstructure:"max_age"`
	Schedule time.Duration `mapstructure:"schedule"`
}

// PipelineConfig governs client-disconnect detection for pipelines (see
// ADR-019): when the owning L4 data-plane tunnel closes, or when a running
// trace has no active lease beyond a staleness threshold, the supervisor
// transitions the trace to "failed" with a recorded reason.
type PipelineConfig struct {
	DisconnectGrace time.Duration      `mapstructure:"disconnect_grace"`
	StaleSweep      PipelineStaleSweep `mapstructure:"stale_sweep"`
}

// PipelineStaleSweep governs the background staleness sweeper that recovers
// orphaned running traces after a supervisor restart/crash (in-memory leases
// are lost, so the disconnect handler cannot run).
type PipelineStaleSweep struct {
	Enabled    bool          `mapstructure:"enabled"`
	Schedule   time.Duration `mapstructure:"schedule"`
	StaleAfter time.Duration `mapstructure:"stale_after"`
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
