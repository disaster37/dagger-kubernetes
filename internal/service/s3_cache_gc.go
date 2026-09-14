package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
)

// s3SweeperAPI is the slice of the minio-go client the S3 cache GC needs. An
// interface (instead of the concrete *minio.Client) so tests can supply an
// in-memory object store; *minio.Client satisfies it via minioSweeperAPI.
type s3SweeperAPI interface {
	RemoveObject(ctx context.Context, bucketName, objectName string, opts minio.RemoveObjectOptions) error
	ListObjects(ctx context.Context, bucketName string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo
}

// minioSweeperAPI adapts *minio.Client to s3SweeperAPI (plain delegation).
type minioSweeperAPI struct{ client *minio.Client }

func (m minioSweeperAPI) RemoveObject(ctx context.Context, bucketName, objectName string, opts minio.RemoveObjectOptions) error { //nolint:gocritic // hugeParam: signature must match the minio-go API
	return m.client.RemoveObject(ctx, bucketName, objectName, opts)
}

func (m minioSweeperAPI) ListObjects(ctx context.Context, bucketName string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo { //nolint:gocritic // hugeParam: signature must match the minio-go API
	return m.client.ListObjects(ctx, bucketName, opts)
}

// S3CacheGC sweeps stale BuildKit cache refs from an S3-compatible bucket
// (backend cache.backend: "s3"). It lists the objects under the BuildKit
// remote-cache prefix, checks each object's own LastModified timestamp (no
// VictoriaMetrics hit counters needed), and deletes the objects older than
// MaxAge. The worker-snapshot and CLI-cache prefixes live outside the cache
// prefix and are never touched — they have their own cleanup mechanisms
// (startup self-cleanup + bucket lifecycle policies).
type S3CacheGC struct {
	client     s3SweeperAPI
	bucket     string
	prefix     string // BuildKit remote-cache prefix (domain.S3CachePrefix)
	gcCfg      domain.GCConfig
	logger     *logrus.Logger
	metricsObs *observ.Metrics // may be nil

	purgeMu  sync.Mutex // serializes purge / GC
	gcMu     sync.Mutex // guards lastGC / lastGCAt / nextGCAt (short critical section)
	lastGC   *domain.GCRunSummary
	lastGCAt time.Time
	nextGCAt time.Time
}

// NewS3CacheGC returns an S3-backed cache GC for the given bucket + cache
// prefix, backed by the supplied S3 client.
func NewS3CacheGC(client *minio.Client, bucket, prefix string, gcCfg domain.GCConfig, logger *logrus.Logger, obs *observ.Metrics) *S3CacheGC {
	return &S3CacheGC{
		client:     minioSweeperAPI{client},
		bucket:     bucket,
		prefix:     prefix,
		gcCfg:      gcCfg,
		logger:     logger,
		metricsObs: obs,
	}
}

// RunGC lists the objects under the cache prefix and deletes those whose
// LastModified is older than MaxAge. Non-fatal per-object failures are counted
// in the summary (the failed objects are retried on the next tick).
func (g *S3CacheGC) RunGC(ctx context.Context) (*domain.GCRunSummary, error) {
	g.purgeMu.Lock()
	defer g.purgeMu.Unlock()

	summary := &domain.GCRunSummary{StartedAt: rfc3339(time.Now())}
	finish := func(msg string, err error) (*domain.GCRunSummary, error) {
		summary.FinishedAt = rfc3339(time.Now())
		summary.Message = msg
		g.recordGC(summary, err)
		return summary, err
	}

	for info := range g.client.ListObjects(ctx, g.bucket, minio.ListObjectsOptions{
		Prefix:    g.prefix,
		Recursive: true,
	}) {
		if err := ctx.Err(); err != nil {
			return finish("cancelled", err)
		}
		if info.Err != nil {
			summary.Errors++
			continue
		}
		if info.LastModified.IsZero() || time.Since(info.LastModified) < g.gcCfg.MaxAge {
			summary.Skipped++
			continue
		}
		if err := g.client.RemoveObject(ctx, g.bucket, info.Key, minio.RemoveObjectOptions{}); err != nil {
			g.logger.WithError(err).WithField("key", info.Key).Warn("cache gc: delete failed")
			summary.Errors++
			continue
		}
		summary.PurgedTags++
		summary.FreedBytes += info.Size
		if g.metricsObs != nil {
			g.metricsObs.CachePurgeTotal.Inc()
		}
	}
	return finish("", nil)
}

// Purge deletes ALL objects under the cache prefix, capped at maxPurgeAllTags
// objects (run it repeatedly to clear a large cache). The AlreadyPurged field
// in PurgeResult is always 0 here (the S3 path starts with an empty result;
// the field exists for compatibility with the registry-based purge where
// deleted manifests are tracked separately).
func (g *S3CacheGC) Purge(ctx context.Context) (*domain.PurgeResult, error) {
	g.purgeMu.Lock()
	defer g.purgeMu.Unlock()

	result := &domain.PurgeResult{}
	for info := range g.client.ListObjects(ctx, g.bucket, minio.ListObjectsOptions{
		Prefix:    g.prefix,
		Recursive: true,
	}) {
		if info.Err != nil {
			return nil, fmt.Errorf("list objects: %w", info.Err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if result.Purged+result.AlreadyPurged >= maxPurgeAllTags {
			result.Message = fmt.Sprintf("truncated at %d objects", maxPurgeAllTags)
			return result, nil
		}
		if err := g.client.RemoveObject(ctx, g.bucket, info.Key, minio.RemoveObjectOptions{}); err != nil {
			return nil, fmt.Errorf("delete %s: %w", info.Key, err)
		}
		result.Purged++
		result.FreedBytes += info.Size
		result.Tags = append(result.Tags, info.Key)
		if g.metricsObs != nil {
			g.metricsObs.CachePurgeTotal.Inc()
		}
	}
	return result, nil
}

// GCRules returns the current GC configuration and the last-run summary.
func (g *S3CacheGC) GCRules() domain.GCRules {
	rules := domain.GCRules{
		Enabled:  g.gcCfg.Enabled,
		MaxAge:   g.gcCfg.MaxAge.String(),
		Schedule: g.gcCfg.Schedule.String(),
	}

	g.gcMu.Lock()
	defer g.gcMu.Unlock()
	if !g.lastGCAt.IsZero() {
		rules.LastRunAt = rfc3339(g.lastGCAt)
		rules.LastRunSummary = g.lastGC
	}
	if g.gcCfg.Enabled && !g.nextGCAt.IsZero() {
		rules.NextRunAt = rfc3339(g.nextGCAt)
	}
	return rules
}

// recordGC stores the last run summary and bumps the GC run counter.
func (g *S3CacheGC) recordGC(summary *domain.GCRunSummary, runErr error) {
	g.gcMu.Lock()
	g.lastGC = summary
	g.lastGCAt = time.Now()
	g.nextGCAt = g.lastGCAt.Add(g.gcCfg.Schedule)
	g.gcMu.Unlock()

	if g.metricsObs != nil {
		status := "success"
		if runErr != nil {
			status = "error"
		}
		g.metricsObs.GCRunTotal.WithLabelValues(status).Inc()
	}
}

// StartGCSweeper launches the background ticker goroutine and returns a stop
// func. No-op when gcCfg.Enabled is false (operators rely on S3 lifecycle
// policies for cleanup).
func (g *S3CacheGC) StartGCSweeper(ctx context.Context) (stop func()) {
	if !g.gcCfg.Enabled {
		return func() {}
	}
	if g.gcCfg.Schedule <= 0 {
		g.logger.Warn("cache gc schedule is non-positive; sweeper disabled")
		return func() {}
	}
	ticker := time.NewTicker(g.gcCfg.Schedule)
	done := make(chan struct{})
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				if _, err := g.RunGC(ctx); err != nil {
					g.logger.WithError(err).Error("cache gc run failed")
				}
			}
		}
	}()
	return func() { close(done) }
}
