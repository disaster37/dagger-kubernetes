package domain

import (
	"context"
	"errors"
)

// ErrRegistryDeleteDisabled indicates the OCI registry does not allow manifest
// deletion (REGISTRY_STORAGE_DELETE_ENABLED=false). It lives in domain so the
// HTTP handler can map it to a 409 without importing the repository layer.
var ErrRegistryDeleteDisabled = errors.New("registry delete not enabled")

// ErrRegistryCatalogDisabled indicates the OCI registry does not expose the
// /v2/_catalog endpoint, so tags cannot be enumerated (purge-all / GC can't
// list what exists). Lives in domain so the handler can map it to a 409.
var ErrRegistryCatalogDisabled = errors.New("registry catalog disabled")

// ErrManifestNotFound indicates the OCI registry does not have the requested
// repo:tag manifest (a definitive 404). Lives in domain so the service layer
// can branch on it without importing the repository layer.
var ErrManifestNotFound = errors.New("manifest not found")

type S3Ref struct {
	Bucket string
	Region string
}

type CacheBackend interface {
	BackendType() string
	RegistryHost() string
}

// CacheRoute is one persisted manifest→backend mapping.
type CacheRoute struct {
	Repo        string
	Tag         string
	Digest      string
	BackendID   string
	StoredBytes int64
	CreatedAt   string // RFC3339
	LastSeenAt  string // RFC3339
}

// CacheUploadSession is one in-flight OCI blob upload session.
type CacheUploadSession struct {
	UploadUUID string
	Repo       string
	BackendID  string
	CreatedAt  string // RFC3339
}

// CacheStats is the rich cache payload returned by GET /api/v1/cache.
type CacheStats struct {
	Backend     string    `json:"backend"`      // "registry" | "s3"
	Registry    string    `json:"registry"`     // registry host (or s3 bucket)
	Running     bool      `json:"running"`      // registry reachable / s3 configured
	Reachable   bool      `json:"reachable"`    // last probe succeeded
	TotalSize   int64     `json:"total_size"`   // bytes; -1 when unknown
	ObjectCount int64     `json:"object_count"` // layer/blob count; -1 when unknown
	Ref         *CacheRef `json:"ref"`          // single global cache ref (registry backend); nil for s3
	HitRate     *float64  `json:"hit_rate"`     // 0..1; nil when no data
	HitCount    int64     `json:"hit_count"`    // from VictoriaMetrics; 0 when no data
	MissCount   int64     `json:"miss_count"`
	CollectedAt string    `json:"collected_at"`      // RFC3339 UTC
	Message     string    `json:"message,omitempty"` // human note
	GC          GCRules   `json:"gc"`
}

// CacheRef describes the single global cache ref (registry backend).
type CacheRef struct {
	Ref        string `json:"ref"`                  // "<host>/<repo>:cache"
	Tag        string `json:"tag"`                  // "cache"
	Size       int64  `json:"size"`                 // layer+config bytes; -1 unknown
	LayerCount int64  `json:"layer_count"`          // number of layers; -1 unknown
	Digest     string `json:"digest"`               // sha256:...; "" unavailable
	LastUsedAt string `json:"last_used_at,omitempty"` // RFC3339; "" when unknown
}

// GCRules describes the auto-clean configuration and last/next run.
type GCRules struct {
	Enabled        bool          `json:"enabled"`
	MaxAge         string        `json:"max_age"`  // duration string e.g. "168h"
	Schedule       string        `json:"schedule"` // duration string e.g. "1h"
	LastRunAt      string        `json:"last_run_at,omitempty"` // RFC3339
	LastRunSummary *GCRunSummary `json:"last_run_summary,omitempty"`
	NextRunAt      string        `json:"next_run_at,omitempty"` // RFC3339 (estimated)
}

type GCRunSummary struct {
	StartedAt  string `json:"started_at"`  // RFC3339
	FinishedAt string `json:"finished_at"` // RFC3339
	PurgedTags int    `json:"purged_tags"`
	FreedBytes int64  `json:"freed_bytes"`
	Skipped    int    `json:"skipped"` // fresh, unknown-age, or missing backend
	Errors     int    `json:"errors"`
	Message    string `json:"message,omitempty"`
}

// PurgeResult is the response of purge endpoints.
type PurgeResult struct {
	Purged        int      `json:"purged"`
	FreedBytes    int64    `json:"freed_bytes"`
	AlreadyPurged int      `json:"already_purged"`
	Tags          []string `json:"tags,omitempty"`
	Message       string   `json:"message,omitempty"`
}

// CacheStatsProvider reports cache stats (size, objects, hit rate, GC rules).
type CacheStatsProvider interface {
	Stats(ctx context.Context) (*CacheStats, error)
	GCRules() GCRules
}

// CachePurger purges cache refs.
type CachePurger interface {
	Purge(ctx context.Context) (*PurgeResult, error)
}
