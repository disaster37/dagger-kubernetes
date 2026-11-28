package repository

import (
	"encoding/json"
	"fmt"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// engineConfigKey is the projected ConfigMap key and the file name the Dagger
// engine reads from its config directory (/etc/dagger/engine.json).
const engineConfigKey = "engine.json"

// engineRemoteCacheMinVersion is the first engine release that has both a
// RemoteCache field in engine/config.Config and the engine/remotecache package
// this platform speaks to. Any version below it (all 0.x releases and the 1.0
// betas before beta.28) must not receive a remoteCache key: the engine loads
// engine.json with DisallowUnknownFields and exits fatally on an unknown key.
// The gate is deliberately a capability threshold, never Major >= 1.
var engineRemoteCacheMinVersion = &domain.Version{Major: 1, Minor: 0, Patch: 0, PreRelease: "beta.28"}

// engineConfig is the /etc/dagger/engine.json shape (a subset of the Dagger
// engine's engine/config.Config). Only the keys this platform manages are
// rendered; every other engine default is preserved.
type engineConfig struct {
	LogLevel    string                    `json:"logLevel,omitempty"`
	Registries  map[string]engineRegistry `json:"registries,omitempty"`
	RemoteCache *engineRemoteCache        `json:"remoteCache,omitempty"`
}

// engineRegistry configures one registry host.
type engineRegistry struct {
	Mirrors []string `json:"mirrors,omitempty"`
	HTTP    *bool    `json:"http,omitempty"` // plaintext mirror/registry
}

// engineRemoteCache configures the remote cache service URL.
type engineRemoteCache struct {
	URL string `json:"url"`
}

// render returns the engine.json content, or nil when the configuration is
// empty (no log level, no mirrors, no remote cache). encoding/json sorts map
// keys, so the output is deterministic.
func (c engineConfig) render() ([]byte, error) {
	if c.LogLevel == "" && len(c.Registries) == 0 && c.RemoteCache == nil {
		return nil, nil
	}
	data, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshal engine config: %w", err)
	}
	return data, nil
}

// engineSupportsRemoteCache reports whether version is new enough to accept the
// remoteCache key and speak the protocol served here. Unparseable or custom
// tags fail closed: the remote cache is omitted rather than guessed.
func engineSupportsRemoteCache(version string) bool {
	v, err := domain.Parse(version)
	if err != nil {
		return false
	}
	return v.Compare(engineRemoteCacheMinVersion) >= 0
}

// buildEngineConfig maps the provider configuration to the engine.json shape.
// LogFormat has no engine.json equivalent (the engine hardcodes its text
// formatter), so it is not rendered. RemoteCacheURL is rendered only when the
// engine version passes the capability threshold.
func buildEngineConfig(cfg *K8sProviderConfig, version string) engineConfig {
	out := engineConfig{}
	if cfg.Debug {
		out.LogLevel = "debug"
	}
	if registries := buildEngineRegistries(cfg); len(registries) > 0 {
		out.Registries = registries
	}
	if cfg.RemoteCacheURL != "" && engineSupportsRemoteCache(version) {
		out.RemoteCache = &engineRemoteCache{URL: cfg.RemoteCacheURL}
	}
	return out
}

// buildEngineRegistries merges the configured mirrors and plaintext-mirror
// hosts into the per-host registries map. Hosts without mirrors and without a
// plaintext flag are omitted.
func buildEngineRegistries(cfg *K8sProviderConfig) map[string]engineRegistry {
	registries := make(map[string]engineRegistry, len(cfg.RegistryMirrors)+len(cfg.MirrorHTTP))
	for host, mirrors := range cfg.RegistryMirrors {
		kept := make([]string, 0, len(mirrors))
		for _, mirror := range mirrors {
			if mirror != "" {
				kept = append(kept, mirror)
			}
		}
		if len(kept) > 0 {
			registries[host] = engineRegistry{Mirrors: kept}
		}
	}
	plaintext := true
	for _, host := range cfg.MirrorHTTP {
		if host == "" {
			continue
		}
		registry := registries[host]
		registry.HTTP = &plaintext
		registries[host] = registry
	}
	return registries
}
