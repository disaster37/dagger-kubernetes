package domain

import (
	"context"
	"errors"
)

// EngineCachePurgeResult is the response/status of a per-version local-cache
// purge. State is "running" | "completed" | "failed".
type EngineCachePurgeResult struct {
	Version    string                 `json:"version"`
	State      string                 `json:"state"`
	StartedAt  string                 `json:"started_at"`            // RFC3339 UTC
	FinishedAt string                 `json:"finished_at,omitempty"` // RFC3339 UTC
	Replicas   int                    `json:"replicas"`              // pods targeted
	Pods       []EnginePodPurgeResult `json:"pods"`
	Message    string                 `json:"message,omitempty"`
}

// EnginePodPurgeResult is the per-pod outcome of the local-cache prune.
type EnginePodPurgeResult struct {
	PodName string `json:"pod_name"`
	Ordinal int    `json:"ordinal"`
	Pruned  bool   `json:"pruned"` // engine.localCache.prune succeeded on this pod
	Error   string `json:"error,omitempty"`
}

// EnginePruner prunes one running engine pod's BuildKit local cache via the
// engine dagql API (engine.localCache.prune with useDefaultPolicy=false),
// spoken in-process over the engine's plaintext session HTTP port.
type EnginePruner interface {
	// PruneLocalCache prunes podIP's local cache (engine listens on 9999).
	// version is the pod's engine version, reported as the client_version so
	// the engine's version-compatibility check passes and its own schema view
	// is selected. It must leave the pod running and only remove releasable
	// cache.
	PruneLocalCache(ctx context.Context, podIP, version string) error
}

// EngineCachePurger purges the local cache of every engine pod in a version's
// StatefulSet (concurrently).
type EngineCachePurger interface {
	Purge(ctx context.Context, version string) (*EngineCachePurgeResult, error)
	Status(version string) (*EngineCachePurgeResult, bool)
}

// Sentinel errors mapped by the handler to HTTP responses.
var (
	// ErrEngineFleetNotFound: no StatefulSet exists for the version.
	ErrEngineFleetNotFound = errors.New("engine fleet not found")
	// ErrPurgeInProgress: a purge for the version is already running.
	ErrPurgeInProgress = errors.New("purge already in progress")
)
