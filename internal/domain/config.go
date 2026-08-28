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
	CLI       CLIConfig       `mapstructure:"cli"`
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
	Token          TokenConfig          `mapstructure:"token"`
	BootstrapAdmin BootstrapAdminConfig `mapstructure:"bootstrap_admin"`
	Cookie         CookieConfig         `mapstructure:"cookie"`
	CORS           CORSConfig           `mapstructure:"cors"`
}

// CookieConfig configures the httpOnly session cookies (access + refresh JWTs)
// used by the SPA. Bearer-token auth for CI is unaffected (additive).
type CookieConfig struct {
	AccessName  string `mapstructure:"access_name"`  // default dagger_kubernetes_access
	RefreshName string `mapstructure:"refresh_name"` // default dagger_kubernetes_refresh
	Secure      bool   `mapstructure:"secure"`       // force Secure; else auto-detect TLS
}

// CORSConfig configures cross-origin access for split UI deployments. An empty
// allowlist means same-origin only (no Access-Control-Allow-Origin header).
type CORSConfig struct {
	AllowedOrigins []string `mapstructure:"allowed_origins"`
}

type InternalAuthConfig struct {
	Enabled    bool   `mapstructure:"enabled"`
	TokensFile string `mapstructure:"tokens_file"`
}

// OAuthConfig configures OAuth for human/UI login. A single provider is active
// per deployment, selected by the `provider` discriminator ("github" | "oidc").
// The OIDC-only fields (IssuerURL, Scopes, UsernameClaim, GroupsClaim,
// AllowedGroups) are ignored when provider is "github"; the github-only fields
// (AllowedOrgs as org membership, AllowedTeams) are ignored by oidc (which
// matches AllowedOrgs against the groups claim instead — a deprecated alias of
// AllowedGroups).
type OAuthConfig struct {
	Enabled       bool               `mapstructure:"enabled"`
	Provider      string             `mapstructure:"provider"` // "github" | "oidc"
	ClientID      string             `mapstructure:"client_id"`
	ClientSecret  string             `mapstructure:"client_secret"`
	RedirectURL   string             `mapstructure:"redirect_url"`
	AllowedOrgs   []string           `mapstructure:"allowed_orgs"`   // github: org membership allowlist; oidc: deprecated alias for allowed_groups
	AllowedTeams  []string           `mapstructure:"allowed_teams"`  // github only: "org/team" slug allowlist
	AllowedGroups []string           `mapstructure:"allowed_groups"` // oidc only: groups-claim allowlist (canonical)
	GroupMappings []GroupMappingRule `mapstructure:"group_mappings"` // provider group -> supervisor group regex mapping
	DefaultGroup  string             `mapstructure:"default_group"`  // auto-membership for new OAuth users; empty = none
	// CookieSecure forces the Secure flag on the oauth_state cookie; set true
	// when TLS terminates at an ingress/proxy in front of the supervisor.
	CookieSecure bool `mapstructure:"cookie_secure"`

	// OIDC-only fields (ignored when provider: github).
	IssuerURL     string   `mapstructure:"issuer_url"`     // required for provider: oidc
	Scopes        []string `mapstructure:"scopes"`         // default ["openid","profile","email"]
	UsernameClaim string   `mapstructure:"username_claim"` // default "preferred_username"; fallback "email"
	GroupsClaim   string   `mapstructure:"groups_claim"`   // default "groups"
	CACertPath    string   `mapstructure:"ca_cert_path"`   // optional PEM CA cert for verifying the OIDC issuer TLS
}

// GroupMappingRule maps an upstream provider group name to a supervisor group
// name. Pattern is a Go regexp matched against the incoming group name;
// Replacement is the target supervisor group name and may reference capture
// groups via $1 / ${name} (Go regexp.Expand semantics; $$ = literal $).
type GroupMappingRule struct {
	Pattern     string `mapstructure:"pattern"`
	Replacement string `mapstructure:"replacement"`
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

	// PerformanceMultiplier scales election/heartbeat/lease timeouts. Default: 5.0.
	PerformanceMultiplier float64 `mapstructure:"performance_multiplier"`
	// RaftLogCacheSize is the in-memory log cache size. Default: 512.
	RaftLogCacheSize int `mapstructure:"raft_log_cache_size"`
	// NoSnapshotRestoreOnStart disables auto snapshot restore on start. Default: true.
	NoSnapshotRestoreOnStart bool `mapstructure:"no_snapshot_restore_on_start"`
	// TerminationGracePeriod for the pod. Default: 60s.
	TerminationGracePeriod time.Duration `mapstructure:"termination_grace_period"`
	// RecoveryMode enables peers.json auto-recovery. Default: false.
	RecoveryMode bool `mapstructure:"recovery_mode"`

	// Autopilot configuration.
	Autopilot AutopilotConfig `mapstructure:"autopilot"`
	// Join configuration.
	Join JoinDomainConfig `mapstructure:"join"`
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

	// RotationPeriod is how often to rotate TLS certificates. Default: 24h.
	RotationPeriod time.Duration `mapstructure:"rotation_period"`
	// CAValidity is the CA certificate validity period. Default: 262800h (30 years).
	CAValidity time.Duration `mapstructure:"ca_validity"`
}

// AutopilotConfig is the domain-level autopilot configuration.
type AutopilotConfig struct {
	Enabled                        bool          `mapstructure:"enabled"`
	CleanupDeadServers             bool          `mapstructure:"cleanup_dead_servers"`
	DeadServerLastContactThreshold time.Duration `mapstructure:"dead_server_last_contact_threshold"`
	MinQuorum                      int           `mapstructure:"min_quorum"`
	StabilizationTime              time.Duration `mapstructure:"stabilization_time"`
	HeartbeatInterval              time.Duration `mapstructure:"heartbeat_interval"`
}

// JoinDomainConfig holds join-related configuration.
type JoinDomainConfig struct {
	RetryInterval time.Duration `mapstructure:"retry_interval"`
	MaxConcurrent int           `mapstructure:"max_concurrent"`
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
	ID             string     `mapstructure:"id"`
	InternalAddr   string     `mapstructure:"internal_addr"` // host[:port], no scheme
	Username       string     `mapstructure:"username"`
	Password       string     `mapstructure:"password"`
	PasswordSecret *SecretRef `mapstructure:"password_secret"` // K8s Secret ref; resolves Password when empty
}

// SecretRef names one key of a K8s Secret in the fleet namespace.
type SecretRef struct {
	Name string `mapstructure:"name"`
	Key  string `mapstructure:"key"`
}

type CacheConfig struct {
	Backend      string            `mapstructure:"backend"`       // "registry" | "s3"
	Registry     string            `mapstructure:"registry"`      // legacy single ref "host/repo"
	PublicHost   string            `mapstructure:"public_host"`   // dedicated cache vhost
	InternalAddr string            `mapstructure:"internal_addr"` // legacy single backend addr
	AuthToken    string            `mapstructure:"auth_token"`    // engine→proxy bearer
	Registries   []RegistryBackend `mapstructure:"registries"`    // multi-backend list
	S3           S3Config          `mapstructure:"s3"`
	GC           GCConfig          `mapstructure:"gc"`
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
	SecretName string `mapstructure:"secretName"`
	Key        string `mapstructure:"key"`
}

type FleetConfig struct {
	Namespace              string                  `mapstructure:"namespace"`
	MaxReplicasPerVersion  int                     `mapstructure:"max_replicas_per_version"`
	MaxSessionsPerReplica  int                     `mapstructure:"max_sessions_per_replica"`
	ReplicaIdleTTL         time.Duration           `mapstructure:"replica_idle_ttl"`
	VersionRetention       time.Duration           `mapstructure:"version_retention"`
	EngineImageRegistry    string                  `mapstructure:"engine_image_registry"`
	EngineStorageClass     string                  `mapstructure:"engine_storage_class"`
	EngineStorageSize      string                  `mapstructure:"engine_storage_size"`
	EnginePVCLabels        map[string]string       `mapstructure:"engine_pvc_labels"`
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
	Provider string `mapstructure:"provider"`
	CAPath   string `mapstructure:"ca_path"`
	CertPath string `mapstructure:"cert_path"`
	KeyPath  string `mapstructure:"key_path"`
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
	DynamicStages     bool          `mapstructure:"dynamic_stages"`
	StepsPollInterval time.Duration `mapstructure:"steps_poll_interval"` // poll cadence for the CI step stream
	StepsMaxDepth     int           `mapstructure:"steps_max_depth"`     // 0 = unlimited
}

type DroneConfig struct {
	ConfigExtension bool `mapstructure:"config_extension"`
}

type OTelConfig struct {
	OTLPEndpoint string `mapstructure:"otlp_endpoint"`
}

// CLIConfig configures the on-the-fly Dagger CLI provisioning addon.
type CLIConfig struct {
	Enabled         bool              `mapstructure:"enabled"`
	CacheRepo       string            `mapstructure:"cache_repo"` // OCI repo for CLI tarballs, default "dagger-kubernetes/cli-cache"
	ReleaseListTTL  time.Duration     `mapstructure:"release_list_ttl"`
	DownloadTimeout time.Duration     `mapstructure:"download_timeout"`
	Upstream        CLIUpstreamConfig `mapstructure:"upstream"`
}

// CLIUpstreamConfig points at the Dagger release source (mirror-able for
// self-hosted/offline deployments).
type CLIUpstreamConfig struct {
	ReleasesURL  string `mapstructure:"releases_url"`  // https://api.github.com/repos/dagger/dagger/releases
	DownloadBase string `mapstructure:"download_base"` // https://github.com/dagger/dagger/releases/download
	GitHubToken  string `mapstructure:"github_token"`  // optional, raises API rate limit; set via env only
}
