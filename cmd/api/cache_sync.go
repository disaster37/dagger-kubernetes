package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
	"go.etcd.io/bbolt"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// Environment contract rendered by the K8sProvider on the cache-restore init
// container and the cache-sync sidecar (see docs/README.md, "Worker-cache
// sync"). S3 credentials are Secret references so they never appear as literal
// pod-spec values.
const (
	envCacheSyncTag          = "CACHE_SYNC_TAG"
	envCacheSyncBaseDir      = "CACHE_SYNC_BASE_DIR"
	envCacheSyncWorkerSubdir = "CACHE_SYNC_WORKER_SUBDIR"
	envCacheSyncTmpDir       = "CACHE_SYNC_TMP_DIR"
	envCacheSyncInterval     = "CACHE_SYNC_INTERVAL"
	envCacheSyncQuiesceWait  = "CACHE_SYNC_QUIESCE_WAIT"

	envCacheSyncS3Endpoint  = "CACHE_SYNC_S3_ENDPOINT"
	envCacheSyncS3Bucket    = "CACHE_SYNC_S3_BUCKET"
	envCacheSyncS3Region    = "CACHE_SYNC_S3_REGION"
	envCacheSyncS3UseSSL    = "CACHE_SYNC_S3_USE_SSL"
	envCacheSyncS3AccessKey = "CACHE_SYNC_S3_ACCESS_KEY" // #nosec G101 -- env var name, not a credential.
	envCacheSyncS3SecretKey = "CACHE_SYNC_S3_SECRET_KEY" // #nosec G101 -- env var name, not a credential.
)

const (
	defaultCacheSyncBaseDir  = "/var/lib/dagger"
	defaultCacheSyncWorker   = "worker"
	defaultCacheSyncTmpDir   = "/tmp"
	defaultCacheSyncInterval = 10 * time.Minute
	defaultCacheSyncQuiesce  = 10 * time.Second
	defaultCacheSyncS3Region = "us-east-1"
	defaultCacheSyncS3UseSSL = true

	// cacheSyncOpTimeout bounds the restore pull and the final on-stop push.
	// The pod's termination grace period is the real limit for the final push
	// (SIGKILL truncation is safe: the metadata tarball is only
	// updated after a successful upload, so the previous snapshot stays
	// intact); this is a backstop for non-K8s runs.
	cacheSyncOpTimeout = 5 * time.Minute
)

// cacheSyncEnv is the resolved helper configuration.
type cacheSyncEnv struct {
	tag          string
	baseDir      string
	workerSubdir string
	tmpDir       string
	interval     time.Duration
	quiesceWait  time.Duration

	s3Endpoint  string
	s3Bucket    string
	s3Region    string
	s3UseSSL    bool
	s3AccessKey string
	s3SecretKey string
}

func (e *cacheSyncEnv) workerDir() string {
	return filepath.Join(e.baseDir, e.workerSubdir)
}

// loadCacheSyncEnv reads the helper configuration from the environment.
// Missing optional keys fall back to the documented defaults.
func loadCacheSyncEnv() (*cacheSyncEnv, error) {
	interval, err := durationEnv(envCacheSyncInterval, defaultCacheSyncInterval)
	if err != nil {
		return nil, err
	}
	quiesceWait, err := durationEnv(envCacheSyncQuiesceWait, defaultCacheSyncQuiesce)
	if err != nil {
		return nil, err
	}

	env := &cacheSyncEnv{
		tag:          os.Getenv(envCacheSyncTag),
		baseDir:      stringEnv(envCacheSyncBaseDir, defaultCacheSyncBaseDir),
		workerSubdir: stringEnv(envCacheSyncWorkerSubdir, defaultCacheSyncWorker),
		tmpDir:       stringEnv(envCacheSyncTmpDir, defaultCacheSyncTmpDir),
		interval:     interval,
		quiesceWait:  quiesceWait,
	}
	if env.tag == "" {
		return nil, fmt.Errorf("%s must be set", envCacheSyncTag)
	}

	env.s3Endpoint = os.Getenv(envCacheSyncS3Endpoint)
	if env.s3Endpoint == "" {
		return nil, fmt.Errorf("%s must be set", envCacheSyncS3Endpoint)
	}
	env.s3Bucket = os.Getenv(envCacheSyncS3Bucket)
	if env.s3Bucket == "" {
		return nil, fmt.Errorf("%s must be set", envCacheSyncS3Bucket)
	}
	env.s3Region = stringEnv(envCacheSyncS3Region, defaultCacheSyncS3Region)
	useSSL, err := boolEnv(envCacheSyncS3UseSSL, defaultCacheSyncS3UseSSL)
	if err != nil {
		return nil, err
	}
	env.s3UseSSL = useSSL
	env.s3AccessKey = os.Getenv(envCacheSyncS3AccessKey)
	env.s3SecretKey = os.Getenv(envCacheSyncS3SecretKey)

	return env, nil
}

// stringEnv returns the env value, or def when unset/empty.
func stringEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// durationEnv parses key as a Go duration; def applies when unset.
func durationEnv(key string, def time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %s=%q: %w", key, raw, err)
	}
	return d, nil
}

// boolEnv parses key as a boolean; def applies when unset.
func boolEnv(key string, def bool) (bool, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("parse %s=%q: %w", key, raw, err)
	}
	return b, nil
}

// cacheSyncCommand returns the top-level "cache-sync" CLI command: the
// worker-cache helper running inside the engine pod (init container + sidecar).
func cacheSyncCommand() *cli.Command {
	return &cli.Command{
		Name:  "cache-sync",
		Usage: "restore/push the BuildKit worker-cache snapshot for engine pods",
		Description: "Runs inside the engine pod: 'restore' warms /var/lib/dagger/worker from the shared snapshot before the engine starts; " +
			"'serve' periodically pushes the worker dir and performs the final push on SIGTERM. " +
			"Both are best-effort: failures never block the pod. " +
			"Snapshots are stored on S3 at s3://<bucket>/worker-snapshots/<version-slug>/.",
		Subcommands: []*cli.Command{
			{
				Name:   "restore",
				Usage:  "restore the worker-dir snapshot (no-op when the worker dir already has content)",
				Action: runCacheSyncRestore,
			},
			{
				Name:   "serve",
				Usage:  "periodically push the worker-dir snapshot; final push on SIGTERM",
				Action: runCacheSyncServe,
			},
		},
	}
}

// newS3SnapshotStore builds the S3-backed snapshot store for this pod's
// engine version (the version slug is derived from the rendered snapshot
// tag; all pods of the same StatefulSet share one snapshot prefix).
func newS3SnapshotStore(env *cacheSyncEnv, logger *logrus.Logger) (*repository.S3SnapshotStore, error) {
	client, err := repository.NewS3Client(env.s3Endpoint, env.s3Region, env.s3AccessKey, env.s3SecretKey, env.s3UseSSL)
	if err != nil {
		return nil, fmt.Errorf("create s3 client: %w", err)
	}
	return repository.NewS3SnapshotStore(client, env.s3Bucket, domain.WorkerSnapshotVersionSlug(env.tag), env.workerSubdir, logger), nil
}

// runCacheSyncRestore restores the worker-dir snapshot. Best-effort: a
// missing/unreachable snapshot or a failed untar logs and exits 0, so the pod
// starts (with an empty or partial cache) either way.
func runCacheSyncRestore(c *cli.Context) error {
	env, err := loadCacheSyncEnv()
	if err != nil {
		return err
	}
	logger := observ.NewLogger("info", "text")

	// A retained PVC (scale-down keeps it) already holds a newer local cache:
	// only a truly empty/missing worker dir (fresh PVC, node migration) is
	// restored.
	empty, err := dirEmpty(env.workerDir())
	if err != nil {
		logger.WithError(err).Warn("worker dir check failed; skipping restore")
		return nil
	}
	if !empty {
		logger.Info("skipped: existing cache")
		return nil
	}

	ctx, cancel := context.WithTimeout(c.Context, cacheSyncOpTimeout)
	defer cancel()

	store, err := newS3SnapshotStore(env, logger)
	if err != nil {
		logger.WithError(err).Warn("worker snapshot restore failed; starting with an empty cache")
		return nil
	}
	restoreWorkerSnapshotS3(ctx, env, store, logger)
	return nil
}

// discardPartialRestore removes the worker directory after a failed snapshot
// restore so the next start sees an empty dir and retries a clean restore
// instead of freezing a torn cache (a truncated or missing metadata DB).
// The worker dir is a subdirectory of baseDir: both the registry and S3
// backends untar into baseDir with the workerSubdir prefix, so removing
// workerDir (= baseDir/workerSubdir) discards the partial extraction.
func discardPartialRestore(env *cacheSyncEnv, logger *logrus.Logger) {
	if rmErr := os.RemoveAll(env.workerDir()); rmErr != nil {
		logger.WithError(rmErr).Warn("cleanup after failed restore")
	}
}

// restoreWorkerSnapshotS3 restores the worker-dir snapshot from the S3
// backend, mirroring the registry path's best-effort semantics. After a
// successful pull the worker's metadata DB is verified (BoltDB open): a
// missing DB (neither metadata_v2.db nor metadata.db) or a corrupt DB
// discards the whole worker dir so the next start retries a clean restore
// instead of freezing a torn cache.
func restoreWorkerSnapshotS3(ctx context.Context, env *cacheSyncEnv, store *repository.S3SnapshotStore, logger *logrus.Logger) {
	ok, err := store.Pull(ctx, env.baseDir)
	if err != nil {
		logger.WithError(err).Warn("worker snapshot restore failed; starting with an empty cache")
		discardPartialRestore(env, logger)
		return
	}
	if !ok {
		logger.Info("no worker snapshot yet; starting with an empty cache")
		return
	}

	dbPath, found := metadataDBPath(env.workerDir())
	if !found {
		logger.Warn("restored snapshot has no metadata DB (metadata_v2.db or metadata.db); discarding the worker cache")
		discardPartialRestore(env, logger)
		return
	}
	if err := verifyMetadataDB(dbPath); err != nil {
		logger.WithError(err).Warn("restored metadata DB failed the integrity check; discarding the worker cache")
		discardPartialRestore(env, logger)
	}
}

// metadataDBPath returns the restored worker's metadata DB path, preferring the
// metadata_v2.db shipped by modern engines over the legacy metadata.db. found
// is false when neither exists, meaning the snapshot is not a usable worker
// cache.
func metadataDBPath(workerDir string) (path string, found bool) {
	for _, name := range []string{"metadata_v2.db", "metadata.db"} {
		candidate := filepath.Join(workerDir, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true
		}
	}
	return "", false
}

// verifyMetadataDB opens the restored BoltDB file to verify integrity (a
// corrupt metadata DB would prevent the engine from starting).
func verifyMetadataDB(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{ReadOnly: true, Timeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("open metadata db: %w", err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("close metadata db: %w", err)
	}
	return nil
}

// runCacheSyncServe periodically pushes the worker-dir snapshot and performs
// the final push after SIGTERM (the engine receives its own SIGTERM and needs
// a grace period to flush+exit). Every push is best-effort: failures are
// logged, never fatal.
func runCacheSyncServe(c *cli.Context) error {
	env, err := loadCacheSyncEnv()
	if err != nil {
		return err
	}
	logger := observ.NewLogger("info", "text")

	ctx, stop := signal.NotifyContext(c.Context, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	s3Store, err := newS3SnapshotStore(env, logger)
	if err != nil {
		return err
	}
	// Self-cleanup on startup (D5): delete this version's previous
	// snapshot objects before the first push so old blobs do not
	// accumulate across restarts. Best-effort: on failure the bucket's
	// lifecycle policy is the fallback.
	if err := s3Store.CleanupSelf(ctx); err != nil {
		logger.WithError(err).Warn("worker snapshot startup cleanup failed; relying on the bucket lifecycle policy")
	}

	// Serialize pushes so the final on-stop push never overlaps a periodic
	// one that SIGTERM interrupted mid-flight.
	var pushMu sync.Mutex
	push := func(ctx context.Context, reason string) {
		pushMu.Lock()
		defer pushMu.Unlock()
		if err := pushWorkerSnapshotS3(ctx, env, s3Store, logger); err != nil {
			logger.WithError(err).WithField("reason", reason).Warn("worker snapshot push failed")
		}
	}

	if env.interval > 0 {
		ticker := time.NewTicker(env.interval)
		defer ticker.Stop()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					// Best-effort dirty snapshot: the engine keeps running
					// (BoltDB is crash-safe, so a torn read recovers to the
					// last committed transaction — harmless for a cache).
					push(ctx, "interval")
				}
			}
		}()
	}
	logFields := logrus.Fields{
		"tag": env.tag, "interval": env.interval.String(),
		"bucket": env.s3Bucket,
		"prefix": domain.WorkerSnapshotS3Prefix(domain.WorkerSnapshotVersionSlug(env.tag)),
	}
	logger.WithFields(logFields).Info("cache-sync sidecar started")

	<-ctx.Done()

	finalCtx, cancel := context.WithTimeout(context.Background(), cacheSyncOpTimeout)
	defer cancel()
	time.Sleep(env.quiesceWait)
	push(finalCtx, "stop")
	return nil
}

// pushWorkerSnapshotS3 pushes the incremental snapshot to the S3 backend.
func pushWorkerSnapshotS3(ctx context.Context, env *cacheSyncEnv, s3Store *repository.S3SnapshotStore, logger *logrus.Logger) error {
	workerDir := env.workerDir()
	if _, err := os.Stat(workerDir); err != nil {
		if os.IsNotExist(err) {
			logger.Info("worker dir missing; skipping push")
			return nil
		}
		return fmt.Errorf("stat %s: %w", workerDir, err)
	}
	return s3Store.Push(ctx, workerDir, env.tmpDir)
}

// dirEmpty reports whether dir is missing or has no entries.
func dirEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}
