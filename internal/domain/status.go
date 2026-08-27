package domain

import "context"

// ServiceState is the rollup health of a single platform service.
type ServiceState string

const (
	ServiceOK       ServiceState = "ok"
	ServiceDegraded ServiceState = "degraded"
	ServiceDown     ServiceState = "down"
	ServiceUnknown  ServiceState = "unknown"
)

// ServiceStatus is one row in the services/status view.
type ServiceStatus struct {
	Name       string       `json:"name"`     // "supervisor" | "cache" | "collector" | "tempo" | "loki" | "victoria" | "fleet"
	Category   string       `json:"category"` // "control" | "cache" | "telemetry" | "fleet"
	State      ServiceState `json:"state"`
	Message    string       `json:"message,omitempty"`
	Configured bool         `json:"configured"` // false when the URL/feature is not configured (then state=unknown)
	CheckedAt  string       `json:"checked_at"` // RFC3339 UTC
}

// PlatformStatus is the aggregated response of GET /api/v1/status.
type PlatformStatus struct {
	State     ServiceState    `json:"state"` // rollup: down if any down; degraded if any degraded & none down; else ok
	Services  []ServiceStatus `json:"services"`
	CheckedAt string          `json:"checked_at"`
}

// StatusProvider aggregates platform service health.
type StatusProvider interface {
	Status(ctx context.Context) (*PlatformStatus, error)
}

// RaftCleanState reports whether the Raft consensus layer is in a clean state
// (all committed log entries applied, node is a cluster member, not shut down).
// When the Raft state is not clean the supervisor pod should not be considered
// ready.
type RaftCleanState interface {
	IsCleanState() bool
}

// StartupProvider reports whether the startup phase is complete.
// Used by the Kubernetes startupProbe (/startup endpoint).
type StartupProvider interface {
	IsStarted() bool
}
