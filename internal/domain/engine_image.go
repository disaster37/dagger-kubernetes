package domain

import (
	"fmt"
	"strings"
)

// EngineImageRegistryViaMirror rewrites registry (the engine image registry,
// e.g. "registry.dagger.io/engine") to route through an image-cache mirror when
// mirrors contains a TLS-enabled entry whose Host equals the registry's host.
// It returns registry unchanged otherwise. The mirror's InternalAddr replaces
// the original host and the repository path is preserved, e.g.
// "registry.dagger.io/engine" -> "<release>-registry-dagger-io-mirror.<ns>.svc:5000/engine".
// A mirror that is not TLS (plaintext HTTP) never triggers a rewrite, because
// the kubelet cannot pull the engine image from a plaintext mirror without
// node-level insecure-registry configuration.
func EngineImageRegistryViaMirror(registry string, mirrors []ImageCacheMirror) string {
	host, path := splitRegistryRef(registry)
	if host == "" {
		return registry
	}
	for _, m := range mirrors {
		if m.TLS && m.Host == host && m.InternalAddr != "" {
			return fmt.Sprintf("%s%s", m.InternalAddr, path)
		}
	}
	return registry
}

// splitRegistryRef splits a bare "host[:port][/path]" registry reference into
// its host[:port] and the remaining path ("" when absent, "/..." otherwise).
// A scheme-prefixed reference (contains "://") is not a valid bare registry
// reference; it returns ("", "") so the caller skips the rewrite.
func splitRegistryRef(registry string) (host, path string) {
	if strings.Contains(registry, "://") {
		return "", ""
	}
	if i := strings.IndexByte(registry, '/'); i >= 0 {
		return registry[:i], registry[i:]
	}
	return registry, ""
}
