package domain

import "time"

type Config struct {
	Server      ServerConfig      `mapstructure:"server"`
	Supervisor  SupervisorConfig  `mapstructure:"supervisor"`
	Auth        AuthConfig        `mapstructure:"auth"`
	Telemetry   TelemetryConfig   `mapstructure:"telemetry"`
	Cache       CacheConfig       `mapstructure:"cache"`
	ImageCache  ImageCacheConfig  `mapstructure:"image_cache"`
	History     HistoryConfig     `mapstructure:"history"`
	Fleet       FleetConfig       `mapstructure:"fleet"`
	CA          CAConfig          `mapstructure:"ca"`
	Version     VersionConfig     `mapstructure:"version"`
	LeaseTTL    time.Duration     `mapstructure:"lease_ttl"`
	Pipeline    PipelineConfig    `mapstructure:"pipeline"`
	CI          CIConfig          `mapstructure:"ci"`
	LogLevel    string            `mapstructure:"log_level"`
	LogFormat   string            `mapstructure:"log_format"` // "json" (default) | "text"
	OTel        OTelConfig        `mapstructure:"otel"`
	Database    DatabaseConfig    `mapstructure:"database"`
	Raft        RaftConfig        `mapstructure:"raft"`
	CLI         CLIConfig         `mapstructure:"cli"`
	Attribution AttributionConfig `mapstructure:"attribution"`
	Org         OrgConfig         `mapstructure:"org"`
	Checks      ChecksConfig      `mapstructure:"checks"`
	Workflows   WorkflowConfig    `mapstructure:"workflows"`
	Forge       ForgeConfig       `mapstructure:"forge"`
	GraphQL     GraphQLConfig     `mapstructure:"graphql"`
}

// SupervisorConfig groups the supervisor's own subcomponent configuration.
// The data plane subsection replaces the former top-level `tls` section: it
// holds everything about the data-plane server certificate (provider, keypair
// paths) plus the embedded minting-CA persistence path, so the section name
// states exactly what it configures (see ADR-005).
type SupervisorConfig struct {
	Dataplane    SupervisorDataplaneConfig `mapstructure:"dataplane"`
	CacheScratch CacheScratchConfig        `mapstructure:"cache_scratch"`
	GoMemLimit   string                    `mapstructure:"gomemlimit"`
}

// CacheScratchConfig configures the dedicated scratch emptyDir used by the
// rebuildable remote-cache bbolt index (ADR-051/ADR-055). It is isolated from
// the Raft PVC so cache churn never competes with raft.db/snapshots.
type CacheScratchConfig struct {
	SizeLimit string `mapstructure:"size_limit"`
}

// SupervisorDataplaneConfig configures the mTLS L4 data plane served by the
// supervisor (the endpoint Dagger clients dial to reach engine sessions).
type SupervisorDataplaneConfig struct {
	TLS TLSConfig `mapstructure:"tls"`
}

type ServerConfig struct {
	ControlAddr string `mapstructure:"control_addr"`
	DataAddr    string `mapstructure:"data_addr"`
	DataHost    string `mapstructure:"data_hostname"`
	PublicURL   string `mapstructure:"public_url"`
	// MaxDataConns bounds concurrent data-plane tunnels (default 2048).
	MaxDataConns int `mapstructure:"max_data_conns"`
	// InternalControlURL is the in-cluster control-plane URL injected into
	// engine/runner pods (e.g. https://<release>-control.<ns>.svc:8080).
	InternalControlURL string `mapstructure:"internal_control_url"`
	// InternalDataHost is the in-cluster data-plane host returned to
	// in-cluster engine/runner callers (e.g. <release>-data.<ns>.svc:8443).
	InternalDataHost string `mapstructure:"internal_data_host"`
}

type AuthConfig struct {
	Internal       InternalAuthConfig   `mapstructure:"internal"`
	OAuth          OAuthConfig          `mapstructure:"oauth"`
	JWT            JWTConfig            `mapstructure:"jwt"`
	Token          TokenConfig          `mapstructure:"token"`
	BootstrapAdmin BootstrapAdminConfig `mapstructure:"bootstrap_admin"`
	Cookie         CookieConfig         `mapstructure:"cookie"`
	CORS           CORSConfig           `mapstructure:"cors"`
	Security       SecurityConfig       `mapstructure:"security"`
	Device         DeviceAuthConfig     `mapstructure:"device"`
}

// DeviceAuthConfig configures the RFC 8628 device authorization flow.
type DeviceAuthConfig struct {
	CodeTTL      time.Duration `mapstructure:"code_ttl"`
	PollInterval time.Duration `mapstructure:"poll_interval"`
}

// SecurityConfig configures the go-secguard guardrails: per-account lockout,
// per-IP auth rate limiting, credential-stuffing IP bans, and trusted-proxy
// client IP resolution. The struct mirrors go-secguard/config.Config so it can
// be converted without importing the library into the stdlib-only domain
// package.
type SecurityConfig struct {
	Lockout   SecurityLockoutConfig   `mapstructure:"lockout"`
	RateLimit SecurityRateLimitConfig `mapstructure:"rate_limit"`
	Ban       SecurityBanConfig       `mapstructure:"ban"`
	Proxy     SecurityProxyConfig     `mapstructure:"proxy"`
}

// SecurityLockoutConfig configures the exponential per-account lockout limiter.
type SecurityLockoutConfig struct {
	Enabled           bool          `mapstructure:"enabled"`
	MaxFailedAttempts int           `mapstructure:"max_failed_attempts"`
	BaseLockout       time.Duration `mapstructure:"base_lockout"`
	MaxLockout        time.Duration `mapstructure:"max_lockout"`
	EntryTTL          time.Duration `mapstructure:"entry_ttl"`
	MaxEntries        int           `mapstructure:"max_entries"`
}

// SecurityRateLimitConfig configures the per-IP auth DoS limiter and the
// distributed per-account login throttle.
type SecurityRateLimitConfig struct {
	Enabled               bool          `mapstructure:"enabled"`
	MaxRequests           int           `mapstructure:"max_requests"`
	Window                time.Duration `mapstructure:"window"`
	LoginThrottleAttempts int           `mapstructure:"login_throttle_attempts"`
	LoginThrottleWindow   time.Duration `mapstructure:"login_throttle_window"`
	MaxEntries            int           `mapstructure:"max_entries"`
}

// SecurityBanConfig configures the credential-stuffing IP ban tracker.
type SecurityBanConfig struct {
	Enabled           bool          `mapstructure:"enabled"`
	MaxUniqueFailures int           `mapstructure:"max_unique_failures"`
	BanDuration       time.Duration `mapstructure:"ban_duration"`
	Window            time.Duration `mapstructure:"window"`
	MaxEntries        int           `mapstructure:"max_entries"`
}

// SecurityProxyConfig configures trusted reverse-proxy client IP resolution.
type SecurityProxyConfig struct {
	BehindReverseProxy bool     `mapstructure:"behind_reverse_proxy"`
	TrustedProxyCIDRs  []string `mapstructure:"trusted_proxy_cidrs"`
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
	// MappedGroupMaxRunnerSessions is the max_runner_sessions (concurrent engine
	// sessions per group; 0 = unlimited) applied to supervisor groups that are
	// AUTO-CREATED by group_mappings when the mapped target group does not exist.
	// It is applied only at creation time — a pre-existing group's quota is never
	// modified. Applied on both login and IdP revalidation.
	MappedGroupMaxRunnerSessions int    `mapstructure:"mapped_group_max_runner_sessions"`
	DefaultGroup                 string `mapstructure:"default_group"` // auto-membership for new OAuth users; empty = none
	// AdminGroups, when non-empty, promotes OAuth users to RoleAdmin when any of
	// their RAW upstream provider groups (pre-mapping) exactly matches an entry.
	// Case-sensitive. Empty = feature disabled (no OAuth user is auto-promoted).
	AdminGroups []string `mapstructure:"admin_groups"`
	// CookieSecure forces the Secure flag on the oauth_state cookie; set true
	// when TLS terminates at an ingress/proxy in front of the supervisor.
	CookieSecure bool `mapstructure:"cookie_secure"`

	// OIDC-only fields (ignored when provider: github).
	IssuerURL     string   `mapstructure:"issuer_url"`     // required for provider: oidc
	Scopes        []string `mapstructure:"scopes"`         // default ["openid","profile","email","groups","offline_access"]
	UsernameClaim string   `mapstructure:"username_claim"` // default "preferred_username"; fallback "email"
	GroupsClaim   string   `mapstructure:"groups_claim"`   // default "groups"
	CACertPath    string   `mapstructure:"ca_cert_path"`   // optional PEM CA cert for verifying the OIDC issuer TLS

	// Revalidation of IdP group membership (ADR-027).
	RevalidateInterval time.Duration `mapstructure:"revalidate_interval"`  // default 5m; per-user cache TTL
	RevalidateGrace    time.Duration `mapstructure:"revalidate_grace"`     // default 1h; offline grace window
	RevalidateFailOpen bool          `mapstructure:"revalidate_fail_open"` // default false (fail closed)
	SessionMaxAge      time.Duration `mapstructure:"session_max_age"`      // default 0 (disabled); hard refresh-token bound for OAuth users
}

// GroupMappingRule maps an upstream provider group name to a supervisor group
// name. Pattern is a Go regexp matched against the incoming group name;
// Replacement is the target supervisor group name and may reference capture
// groups via $1 / ${name} (Go regexp.Expand semantics; $$ = literal $).
type GroupMappingRule struct {
	Pattern     string `mapstructure:"pattern"`
	Replacement string `mapstructure:"replacement"`
	// Org targets the organization slug or "org-<16hex>" ID. Empty selects the
	// bootstrap organization.
	Org string `mapstructure:"org"`
}

// AttributionConfig governs project → group attribution at OTLP ingest.
type AttributionConfig struct {
	// ProjectMappings maps a project name (CI repo slug) to a supervisor group
	// name. Ordered; first-match-wins. Empty = disabled (per-group
	// AutoAssignPattern and explicit assignment still apply).
	ProjectMappings []ProjectMappingRule `mapstructure:"project_mappings"`
}

// ProjectMappingRule maps a project name to a supervisor group name. Pattern is
// a Go regexp matched against the project name (case-sensitive; (?i) opt-in);
// Group is the literal target supervisor group name (no capture substitution).
type ProjectMappingRule struct {
	Pattern string `mapstructure:"pattern"`
	Group   string `mapstructure:"group"`
	// Org optionally restricts the rule to one organization slug or ID. Empty
	// applies to the trace's resolved organization.
	Org string `mapstructure:"org"`
}

// OrgConfig configures organization-level defaults. The bootstrap
// organization's storage ID is generated; only its presentation title and the
// cluster-wide quota defaults are configurable.
type OrgConfig struct {
	Default       DefaultOrgConfig `mapstructure:"default"`
	DefaultQuotas OrgQuotas        `mapstructure:"default_quotas"`
}

// DefaultOrgConfig configures the bootstrap organization's presentation.
type DefaultOrgConfig struct {
	DisplayName string `mapstructure:"display_name"`
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
	// NoSnapshotRestoreOnStart disables auto snapshot restore on start.
	// When true, the FSM replays only the Raft log on restart. After a snapshot
	// has been taken and the log compacted, this silently loses all data that
	// was in the snapshot — set to true only when snapshots are managed externally.
	// Default: false.
	NoSnapshotRestoreOnStart bool `mapstructure:"no_snapshot_restore_on_start"`
	// TerminationGracePeriod for the pod. Default: 60s.
	TerminationGracePeriod time.Duration `mapstructure:"termination_grace_period"`
	// RecoveryMode clears stale raft state (raft.db, snapshots, node-id) on
	// startup and bootstraps a fresh cluster. Manual quorum-loss recovery
	// only. Default: false.
	RecoveryMode bool `mapstructure:"recovery_mode"`

	// TransportMaxPool controls the Raft transport connection pool size per
	// peer (default: 1). In Kubernetes StatefulSet environments where pod IPs
	// change across restarts, a smaller pool forces fresh DNS resolution on
	// each connection, reducing the window where stale cached connections
	// point to old pod IPs (Cilium eBPF DNS proxy delays).
	TransportMaxPool int `mapstructure:"transport_max_pool"`

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
	// KeyringRotationEnabled enables in-band phased CA/leaf auto-renewal.
	// Default: true.
	KeyringRotationEnabled bool `mapstructure:"keyring_rotation_enabled"`
	// RotationCheckInterval is how often the leader checks whether a rotation
	// is due. Default: 30s.
	RotationCheckInterval time.Duration `mapstructure:"rotation_check_interval"`
	// ReloadPollInterval is how often manual PEM files are polled for
	// cert-manager/external renewal. Default: 5s.
	ReloadPollInterval time.Duration `mapstructure:"reload_poll_interval"`
	// SafetyMargin is the re-issue window before a leaf expires. Default: 7d.
	SafetyMargin time.Duration `mapstructure:"safety_margin"`
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
	// PasswordFile is where a GENERATED password is written (0600) so the
	// operator can recover it without it ever being logged (CWE-532). Empty
	// derives <database.dir>/bootstrap-admin-password. Ignored when Password
	// is configured explicitly.
	PasswordFile string `mapstructure:"password_file"`
}

type TelemetryConfig struct {
	CollectorURL string `mapstructure:"collector_url"`
	TempoURL     string `mapstructure:"tempo_url"`
	LokiURL      string `mapstructure:"loki_url"`
	VictoriaURL  string `mapstructure:"victoria_url"`
}

type CacheConfig struct {
	S3     S3Config          `mapstructure:"s3"`
	Remote RemoteCacheConfig `mapstructure:"remote"`
}

// RemoteCacheConfig configures the read-through remote cache service
// (GET /v1/engine, ADR-055).
type RemoteCacheConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// Bucket overrides cache.s3.bucket for remote-cache objects.
	Bucket string `mapstructure:"bucket"`
	// Prefix is the S3 key prefix for bundles/blobs/index checkpoints.
	Prefix string `mapstructure:"prefix"`
	// S3EngineEndpoint is the endpoint used to sign presigned URLs handed to
	// engine pods (must match the Host they dial in-cluster).
	S3EngineEndpoint string `mapstructure:"s3_engine_endpoint"`
	// IndexPath is the bbolt file path (default /var/cache/dagger-kubernetes/remotecache.db).
	IndexPath string `mapstructure:"index_path"`
	// TTL bounds how long an index entry/object is retained.
	TTL time.Duration `mapstructure:"ttl"`
	// MaxBytes bounds the total stored bytes before GC evicts oldest entries.
	MaxBytes int64 `mapstructure:"max_bytes"`
	// Workers bounds concurrent bundle export/upload work (default 16).
	Workers int `mapstructure:"workers"`
	// PackMaxMemberBytes is the largest blob packed into a dagger-pack (default 4 MiB).
	PackMaxMemberBytes int64 `mapstructure:"pack_max_member_bytes"`
	// GCInterval is the background GC/checkpoint cadence (default 5m).
	GCInterval time.Duration `mapstructure:"gc_interval"`
}

// S3Config is the shared S3 client configuration consumed by the CLI cache
// (cli.enabled). The bucket is also the default bucket for the CLI cache
// (cli.s3_bucket).
type S3Config struct {
	Bucket    string `mapstructure:"bucket"`
	Region    string `mapstructure:"region"`
	Endpoint  string `mapstructure:"endpoint"`   // S3-compatible endpoint, e.g. "minio.example.com:9000"
	UseSSL    bool   `mapstructure:"use_ssl"`    // use HTTPS for the S3 endpoint
	AccessKey string `mapstructure:"access_key"` // set via env/secret in production
	SecretKey string `mapstructure:"secret_key"` // set via env/secret in production
}

// ImageCacheConfig describes the local image mirrors the supervisor manages
// over their OCI Distribution v2 API. It is admin-visible/read-only and
// rendered by the Helm chart from what the chart deploys; the supervisor never
// sees the mirror's own S3 credentials. TLSCAPath is an optional PEM CA bundle
// the supervisor uses to verify HTTPS mirrors (empty = system trust pool).
type ImageCacheConfig struct {
	Mirrors   []ImageCacheMirror `mapstructure:"mirrors"`
	TLSCAPath string             `mapstructure:"tls_ca_path"`
}

// ImageCacheMirror is one local Zot mirror endpoint.
type ImageCacheMirror struct {
	ID           string `mapstructure:"id"`            // slug, e.g. "docker-io"
	Host         string `mapstructure:"host"`          // upstream host, e.g. "docker.io"
	Upstream     string `mapstructure:"upstream"`      // upstream base URL
	InternalAddr string `mapstructure:"internal_addr"` // "<release>-<slug>-mirror.<ns>.svc:5000" (in-cluster BuildKit)
	ExternalAddr string `mapstructure:"external_addr"` // node-reachable ingress host for the engine-image pull (ADR-046); "" = no ingress
	Backend      string `mapstructure:"backend"`       // "s3" | "pvc" (informational)
	TLS          bool   `mapstructure:"tls"`           // true = mirror is served over HTTPS (imageCache.tls.enabled)
}

// HistoryConfig governs pipeline-history retention (trace_meta + logs +
// metrics).
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
	DisconnectGrace time.Duration         `mapstructure:"disconnect_grace"`
	StaleSweep      PipelineStaleSweep    `mapstructure:"stale_sweep"`
	Metrics         PipelineMetricsConfig `mapstructure:"metrics"`
}

// PipelineMetricsConfig governs the trace-scoped engine resource metrics
// endpoint (GET /api/v1/traces/:traceID/metrics) that queries cAdvisor
// container_* series from VictoriaMetrics.
type PipelineMetricsConfig struct {
	Enabled         bool          `mapstructure:"enabled"`
	Step            time.Duration `mapstructure:"step"`              // query_range step; default 15s
	RateWindow      time.Duration `mapstructure:"rate_window"`       // rate() lookback; default 1m
	RecordInterval  time.Duration `mapstructure:"record_interval"`   // recorder sampling tick; default 15s
	MaxRecordWindow time.Duration `mapstructure:"max_record_window"` // per-trace recording cap; default 24h
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
	// EngineRegistryMirrorsHTTP lists mirror host[:port] values that are
	// dialed over plaintext HTTP. engine.toml emits a
	// [registry."<mirror>"] http = true section per entry (BuildKit requires
	// the mirror host itself to be marked http for in-cluster plaintext
	// mirrors).
	EngineRegistryMirrorsHTTP []string `mapstructure:"engine_registry_mirrors_http"`
	// Isolation is "per_org" (default) or "shared". per_org scopes engine
	// StatefulSets/Services/PVCs and remote-cache namespaces by org.
	Isolation string `mapstructure:"isolation"`
}

type CAConfig struct {
	MintingCASecret string `mapstructure:"minting_ca_secret"`
	// MintingCAConfigMap names the Kubernetes ConfigMap the leader keeps in
	// sync with the public Minting CA trust bundle (key ca.crt, certificate(s)
	// only — never the private key). Gateway API BackendTLSPolicy reads it as a
	// Core ConfigMap caCertificateRef. Empty disables ConfigMap publishing.
	MintingCAConfigMap string        `mapstructure:"minting_ca_configmap"`
	ClientCertTTL      time.Duration `mapstructure:"client_cert_ttl"`
	// KeyringRotationEnabled enables zero-downtime phased Minting CA rotation
	// for the engine/data-plane/internal-control mTLS plane. Default: true.
	KeyringRotationEnabled bool `mapstructure:"keyring_rotation_enabled"`
	// RotationPeriod is how often the Minting CA is rotated. Default: 24h.
	// Must be >= 2 * client_cert_ttl when rotation is enabled.
	RotationPeriod time.Duration `mapstructure:"rotation_period"`
	// RotationCheckInterval is how often the leader checks whether a rotation
	// is due. Default: 30s.
	RotationCheckInterval time.Duration `mapstructure:"rotation_check_interval"`
	// Validity is the leaf certificate validity. Default: 8760h (1y).
	Validity time.Duration `mapstructure:"validity"`
	// CAValidity is the CA certificate validity. Default: 262800h (30y).
	CAValidity time.Duration `mapstructure:"ca_validity"`
	// SafetyMargin is the re-issue window before a leaf expires. Default: 7d.
	SafetyMargin time.Duration `mapstructure:"safety_margin"`
}

// TLSConfig configures the server certificate the supervisor serves on the
// control and data planes, and the path where the embedded provider persists
// the auto-bootstrapped minting CA + self-signed server keypair.
type TLSConfig struct {
	Provider string `mapstructure:"provider"` // "embedded" | "cert-manager" | "external"
	CAPath   string `mapstructure:"ca_path"`
	CertPath string `mapstructure:"cert_path"`
	KeyPath  string `mapstructure:"key_path"`
	// ReloadPollInterval is how often cert-manager/external server cert files
	// are polled for renewal. Default: 5s.
	ReloadPollInterval time.Duration `mapstructure:"reload_poll_interval"`
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
	// MaxInflightBytes caps the total bytes of concurrently buffered OTLP
	// bodies (default 256 MiB). Saturation returns 503 instead of OOM-killing
	// the Raft leader.
	MaxInflightBytes int64 `mapstructure:"max_inflight_bytes"`
	// IngestMaxBodySize caps OTLP ingest request bodies (bytes). It is
	// deliberately separate from the 4 MiB control-API cap: OTLP log/trace
	// batches are legitimately large. 0 = use the handler default (64 MiB).
	IngestMaxBodySize int64 `mapstructure:"ingest_max_body_size"`
}

// CLIConfig configures the on-the-fly Dagger CLI provisioning addon.
type CLIConfig struct {
	Enabled         bool              `mapstructure:"enabled"`
	CacheRepo       string            `mapstructure:"cache_repo"` // OCI repo for CLI tarballs, default "dagger-kubernetes/cli-cache" (registry backend)
	ReleaseListTTL  time.Duration     `mapstructure:"release_list_ttl"`
	DownloadTimeout time.Duration     `mapstructure:"download_timeout"`
	Upstream        CLIUpstreamConfig `mapstructure:"upstream"`
	CIWrapperPath   string            `mapstructure:"ci_wrapper_path"` // path to pre-built dagger-kubernetes-ci binary
	// IncludePrereleases includes vX.Y.Z-beta.* releases in the "latest"
	// resolution. Explicit pre-release downloads always work regardless.
	IncludePrereleases bool `mapstructure:"include_prereleases"`

	// S3-specific settings for the CLI cache. The rest of the S3 client
	// (endpoint, region, SSL, credentials) is configured via cache.s3.*.
	S3Bucket string `mapstructure:"s3_bucket"` // CLI-cache bucket; empty = cache.s3.bucket
	S3Prefix string `mapstructure:"s3_prefix"` // S3 key prefix for CLI tarballs, default "cli-cache"
}

// CLIUpstreamConfig points at the Dagger release source (mirror-able for
// self-hosted/offline deployments).
type CLIUpstreamConfig struct {
	ReleasesURL  string `mapstructure:"releases_url"`  // https://api.github.com/repos/dagger/dagger/releases
	DownloadBase string `mapstructure:"download_base"` // https://github.com/dagger/dagger/releases/download
	GitHubToken  string `mapstructure:"github_token"`  // optional, raises API rate limit; set via env only
}

// ChecksConfig configures Cloud Checks (ADR-056).
type ChecksConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// Namespace is where check-run Jobs are created (default fleet.namespace).
	Namespace string `mapstructure:"namespace"`
	// JobImage is the runner image used for check Jobs.
	JobImage string `mapstructure:"job_image"`
	// JobImagePullSecrets are image pull secrets attached to check-run Job pods.
	JobImagePullSecrets []string `mapstructure:"job_image_pull_secrets"`
	// Timeout bounds a single check-run Job.
	Timeout time.Duration `mapstructure:"timeout"`
	// GitHubOIDCIssuer is the GitHub Actions OIDC issuer URL.
	GitHubOIDCIssuer string `mapstructure:"github_oidc_issuer"`
	// GitLabOIDCIssuer is the default GitLab CI OIDC issuer URL.
	GitLabOIDCIssuer string `mapstructure:"gitlab_oidc_issuer"`
	// OIDCAudience is the expected audience of CI OIDC ID tokens.
	OIDCAudience string `mapstructure:"oidc_audience"`
}

// WorkflowConfig configures Dagger-native CD workflows (ADR-057).
type WorkflowConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// Namespace is where workflow Jobs are created (default fleet.namespace).
	Namespace string `mapstructure:"namespace"`
	// JobImage is the runner image used for workflow Jobs.
	JobImage string `mapstructure:"job_image"`
	// JobImagePullSecrets are image pull secrets attached to workflow Job pods.
	JobImagePullSecrets []string `mapstructure:"job_image_pull_secrets"`
	// DefaultTimeout bounds a workflow job when it declares no timeout.
	DefaultTimeout time.Duration `mapstructure:"default_timeout"`
	// MaxParallel bounds concurrently running jobs in one workflow run.
	MaxParallel int `mapstructure:"max_parallel"`
}

// ForgeConfig configures outbound forge API endpoints (ADR-056).
type ForgeConfig struct {
	GitHubAPIURL string `mapstructure:"github_api_url"`
	GitLabAPIURL string `mapstructure:"gitlab_api_url"`
	GiteaAPIURL  string `mapstructure:"gitea_api_url"`
	// WebhookSecret is the fallback HMAC secret when a source has none.
	WebhookSecret string            `mapstructure:"webhook_secret"`
	GitHub        ForgeGitHubConfig `mapstructure:"github"`
}

// ForgeGitHubConfig configures the outbound GitHub API and OAuth app used for
// credential linking (ADR-059). It is intentionally independent of auth.oauth.
type ForgeGitHubConfig struct {
	AllowedAPIURLs []string               `mapstructure:"allowed_api_urls"`
	CACertPath     string                 `mapstructure:"ca_cert_path"`
	HTTPTimeout    time.Duration          `mapstructure:"http_timeout"`
	OAuth          ForgeGitHubOAuthConfig `mapstructure:"oauth"`
}

// ForgeGitHubOAuthConfig configures the GitHub OAuth app used to link GitHub
// repository credentials. It is NOT a supervisor login provider.
type ForgeGitHubOAuthConfig struct {
	Enabled             bool          `mapstructure:"enabled"`
	ClientID            string        `mapstructure:"client_id"`
	ClientSecret        string        `mapstructure:"client_secret"`
	OAuthBaseURL        string        `mapstructure:"oauth_base_url"` // default "https://github.com"
	Scopes              []string      `mapstructure:"scopes"`         // default ["repo","read:org","admin:repo_hook"]
	StateTTL            time.Duration `mapstructure:"state_ttl"`      // default 10m
	DeviceFlowEnabled   bool          `mapstructure:"device_flow_enabled"`
	AllowedRedirectURIs []string      `mapstructure:"allowed_redirect_uris"`
	// RefreshEnabled rotates expiring OAuth App tokens (kill switch). When
	// false, an expiring token behaves like a non-expiring one (401 → revoke).
	RefreshEnabled bool `mapstructure:"refresh_enabled"` // default true
	// RefreshSkew is the proactive-rotation window before access-token expiry.
	RefreshSkew time.Duration `mapstructure:"refresh_skew"` // default 5m, allowed 0..1h
}

// GraphQLConfig bounds Cloud GraphQL query complexity and introspection.
type GraphQLConfig struct {
	IntrospectionEnabled bool `mapstructure:"introspection_enabled"`
	MaxDepth             int  `mapstructure:"max_depth"`
	MaxSelections        int  `mapstructure:"max_selections"`
	MaxQueryBytes        int  `mapstructure:"max_query_bytes"`
	MaxParallelism       int  `mapstructure:"max_parallelism"`
}
