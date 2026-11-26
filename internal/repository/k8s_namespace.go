package repository

import (
	"os"
	"strings"
)

// podNamespaceFile is the in-cluster ServiceAccount namespace projection. It is
// a var so tests can point it at a temp file.
var podNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// podNamespaceEnv is the downward-API env var that carries the pod namespace.
const podNamespaceEnv = "POD_NAMESPACE"

// defaultRunnerNamespace is the fallback when no namespace can be resolved.
const defaultRunnerNamespace = "dagger-kubernetes"

// resolvePodNamespace resolves the namespace runner Jobs are created in. The
// configured value wins; otherwise the POD_NAMESPACE env var, then the
// ServiceAccount namespace file, then a fixed fallback.
func resolvePodNamespace(configured string) string {
	if ns := strings.TrimSpace(configured); ns != "" {
		return ns
	}
	if ns := strings.TrimSpace(os.Getenv(podNamespaceEnv)); ns != "" {
		return ns
	}
	if data, err := os.ReadFile(podNamespaceFile); err == nil {
		if ns := strings.TrimSpace(string(data)); ns != "" {
			return ns
		}
	}
	return defaultRunnerNamespace
}
