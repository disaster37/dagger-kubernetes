package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
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
	// "" = peer addresses end at .svc (no cluster suffix), e.g.
	// <pod>.<headless>.<ns>.svc:8081.
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
	v.SetDefault("ci.jenkins.steps_poll_interval", 2*time.Second)
	v.SetDefault("ci.jenkins.steps_max_depth", 8)
	v.SetDefault("ci.drone.config_extension", true)

	v.SetDefault("cli.enabled", true)
	v.SetDefault("cli.cache_dir", "")
	v.SetDefault("cli.release_list_ttl", time.Hour)
	v.SetDefault("cli.download_timeout", 5*time.Minute)
	v.SetDefault("cli.upstream.releases_url", "https://api.github.com/repos/dagger/dagger/releases")
	v.SetDefault("cli.upstream.download_base", "https://github.com/dagger/dagger/releases/download")
	v.SetDefault("cli.upstream.github_token", "")

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
	if err := unmarshalConfig(v, &cfg); err != nil {
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

	if err := validateCLIConfig(&cfg); err != nil {
		return nil, fmt.Errorf("validate cli config: %w", err)
	}

	if err := validateCIConfig(&cfg); err != nil {
		return nil, fmt.Errorf("validate ci config: %w", err)
	}

	return &cfg, nil
}

// unmarshalConfig decodes the merged Viper settings into cfg. It deliberately
// does NOT use viper.Unmarshal: Viper rebuilds the settings tree by splitting
// every flat key on "." (viper.getSettings), which corrupts map keys that
// themselves contain dots — Longhorn PVC label keys such as
// "recurring-job-group.longhorn.io/nobackup", registry hostnames such as
// "docker.io"/"ghcr.io" in fleet.engine_registry_mirrors, env names such as
// "http.proxy" in fleet.engine_extra_env_from, etc. Instead the tree is
// rebuilt leaf-by-leaf following the real nested structure of the sources
// (see collectSettings) and then decoded with mapstructure using the same
// weak-typing hooks Viper uses, plus a duration parser that also accepts day
// ("d") and week ("w") units, which Go's time.ParseDuration rejects with
// "unknown unit".
func unmarshalConfig(v *viper.Viper, cfg *domain.Config) error {
	return decodeSettings(collectSettings(v), cfg)
}

// decodeSettings decodes the nested settings map into result using the same
// weak-typing semantics Viper uses. See unmarshalConfig for why this replaces
// viper.Unmarshal.
func decodeSettings(settings map[string]any, result any) error {
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		WeaklyTypedInput: true,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			stringToDurationHookFunc(),
			stringToWeakSliceHookFunc(","),
		),
		Result: result,
	})
	if err != nil {
		return err
	}
	return decoder.Decode(settings)
}

// collectSettings rebuilds the merged settings as a nested map. Each leaf key
// reported by viper.AllKeys is resolved with v.Get (which applies the
// override/flags/env/config/defaults priority and resolves map keys
// containing dots via prefix search) and inserted by walking the real key
// structure of the sources, so dotted map keys are preserved instead of being
// split into nested maps.
func collectSettings(v *viper.Viper) map[string]any {
	settings := map[string]any{}
	for _, key := range v.AllKeys() {
		val := v.Get(key)
		if val == nil {
			continue
		}
		insertSetting(v, settings, key, val)
	}
	return settings
}

// insertSetting inserts val for the dot-joined flat key into dst. At each
// level the walk consults the source subtree (via v.Get of the prefix
// traversed so far) to find the longest remaining prefix that is an actual
// map key, so keys containing dots stay intact. When no structural hint is
// available the walk falls back to consuming a single path element.
func insertSetting(v *viper.Viper, dst map[string]any, key string, val any) {
	parts := strings.Split(key, ".")
	cur := dst
	prefix := ""
	i := 0
	for i < len(parts) {
		rest := strings.Join(parts[i:], ".")
		keyPart := exactKeyPrefix(v.Get(prefix), rest)
		if keyPart == "" {
			keyPart = parts[i]
		}
		consumed := strings.Split(keyPart, ".")
		if i+len(consumed) >= len(parts) {
			cur[keyPart] = val
			return
		}
		next, ok := cur[keyPart].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[keyPart] = next
		}
		cur = next
		if prefix != "" {
			prefix += "."
		}
		prefix += keyPart
		i += len(consumed)
	}
}

// exactKeyPrefix returns the longest prefix of the dot-joined key that exists
// as an actual map key in src, or "" if none does. It is the structural probe
// that keeps map keys containing dots intact during insertSetting.
func exactKeyPrefix(src any, key string) string {
	m, ok := src.(map[string]any)
	if !ok {
		// Viper stores defaults declared with a concrete Go map type (e.g.
		// map[string]string for fleet.engine_pvc_labels) with that type;
		// normalize so probing works there too.
		typed, okTyped := src.(map[string]string)
		if !okTyped {
			return ""
		}
		m = make(map[string]any, len(typed))
		for k, val := range typed {
			m[k] = val
		}
	}
	parts := strings.Split(key, ".")
	for i := len(parts); i >= 1; i-- {
		candidate := strings.Join(parts[:i], ".")
		if _, ok := m[candidate]; ok {
			return candidate
		}
	}
	return ""
}

// extendedDurationSegmentRE matches one `<number><unit>` segment where the
// unit extends time.ParseDuration's set with "d" (day) and "w" (week).
var extendedDurationSegmentRE = regexp.MustCompile(`([+-]?(\d*\.)?\d+)(ns|us|µs|ms|s|m|h|d|w)`)

// parseExtendedDuration parses a duration like time.ParseDuration but
// additionally accepts day ("d") and week ("w") units, so values such as
// "7d", "1w" or "1.5d12h" work. If the input also fails the extended grammar,
// the error from time.ParseDuration is returned unchanged.
func parseExtendedDuration(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}

	matches := extendedDurationSegmentRE.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		_, err := time.ParseDuration(s)
		return 0, err
	}
	covered := 0
	for _, m := range matches {
		if m[0] != covered {
			_, err := time.ParseDuration(s)
			return 0, err
		}
		covered = m[1]
	}
	if covered != len(s) {
		_, err := time.ParseDuration(s)
		return 0, err
	}

	var total time.Duration
	for _, m := range matches {
		num, unit := s[m[2]:m[3]], s[m[6]:m[7]]
		switch unit {
		case "d", "w":
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, fmt.Errorf("time: invalid duration %q", s)
			}
			unitDur := 24 * time.Hour
			if unit == "w" {
				unitDur = 7 * 24 * time.Hour
			}
			total += time.Duration(f * float64(unitDur))
		default:
			d, err := time.ParseDuration(num + unit)
			if err != nil {
				return 0, err
			}
			total += d
		}
	}
	return total, nil
}

// stringToDurationHookFunc is a mapstructure decode hook converting strings
// to time.Duration with parseExtendedDuration (time.ParseDuration semantics
// plus "d"/"w" units). It replaces viper's built-in hook, which errors with
// "time: unknown unit" on "7d"-style values.
func stringToDurationHookFunc() mapstructure.DecodeHookFuncType {
	return func(f reflect.Type, t reflect.Type, data any) (any, error) {
		if f.Kind() != reflect.String {
			return data, nil
		}
		if t != reflect.TypeOf(time.Duration(0)) {
			return data, nil
		}
		d, err := parseExtendedDuration(data.(string))
		if err != nil {
			return nil, err
		}
		return d, nil
	}
}

// stringToWeakSliceHookFunc is the same weak string->slice hook Viper composes
// into its default decoder: a plain string value (e.g. an env override) is
// split on sep so it can feed slice-typed config fields.
func stringToWeakSliceHookFunc(sep string) mapstructure.DecodeHookFuncType {
	return func(f reflect.Type, t reflect.Type, data any) (any, error) {
		if f.Kind() != reflect.String || t.Kind() != reflect.Slice {
			return data, nil
		}
		raw := data.(string)
		if raw == "" {
			return []string{}, nil
		}
		return strings.Split(raw, sep), nil
	}
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

// validateCLIConfig guards the on-the-fly Dagger CLI provisioning addon. When
// enabled, the upstream release-discovery and download URLs must be absolute
// http(s) URLs and the two TTL/timeouts must be positive (a 0 TTL would hot-loop
// the GitHub Releases API).
func validateCLIConfig(cfg *domain.Config) error {
	if !cfg.CLI.Enabled {
		return nil
	}

	for _, u := range []struct {
		key string
		val string
	}{
		{key: "cli.upstream.releases_url", val: cfg.CLI.Upstream.ReleasesURL},
		{key: "cli.upstream.download_base", val: cfg.CLI.Upstream.DownloadBase},
	} {
		parsed, err := url.ParseRequestURI(u.val)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("%s must be an absolute http(s) URL", u.key)
		}
	}

	if cfg.CLI.ReleaseListTTL <= 0 {
		return fmt.Errorf("cli.release_list_ttl must be > 0")
	}
	if cfg.CLI.DownloadTimeout <= 0 {
		return fmt.Errorf("cli.download_timeout must be > 0")
	}

	return nil
}

// validateCIConfig guards the CI step-stream feature flags. When
// ci.jenkins.dynamic_stages is enabled, the step-stream poll interval must be
// positive (0 would hot-loop the supervisor API) and the depth clamp must not
// be negative.
func validateCIConfig(cfg *domain.Config) error {
	if cfg.CI.Jenkins.DynamicStages {
		if cfg.CI.Jenkins.StepsPollInterval <= 0 {
			return fmt.Errorf("ci.jenkins.steps_poll_interval must be > 0 when ci.jenkins.dynamic_stages is enabled")
		}
	}
	if cfg.CI.Jenkins.StepsMaxDepth < 0 {
		return fmt.Errorf("ci.jenkins.steps_max_depth must be >= 0")
	}
	return nil
}
