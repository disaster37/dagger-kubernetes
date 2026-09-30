package domain

import (
	"fmt"
	"strings"
)

// EngineImageRegistryViaMirror rewrites registry (the engine image registry,
// e.g. "registry.dagger.io/engine") to route through an image-cache mirror when
// a mirror's Host matches the registry's host.
//
// Address selection order:
//  1. ExternalAddr — the node-reachable ingress host (TLS terminated at the
//     ingress), used when non-empty; this no longer requires tls: true.
//  2. TLS && InternalAddr — legacy ADR-043 in-cluster .svc path (operator must
//     pin DNS + trust the CA on each node).
//
// Returns registry unchanged otherwise.
func EngineImageRegistryViaMirror(registry string, mirrors []ImageCacheMirror) string {
	host, path := splitRegistryRef(registry)
	if host == "" {
		return registry
	}
	for _, m := range mirrors {
		if m.Host != host {
			continue
		}
		if m.ExternalAddr != "" {
			return fmt.Sprintf("%s%s", m.ExternalAddr, path)
		}
		if m.TLS && m.InternalAddr != "" {
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
