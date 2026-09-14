package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// cliTestContext returns a bare *cli.Context whose Context is context.Background
// (the actions only read c.Context).
func cliTestContext() *cli.Context {
	return cli.NewContext(&cli.App{}, flag.NewFlagSet("cache-sync", flag.ContinueOnError), nil)
}

func TestLoadCacheSyncEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    *cacheSyncEnv
		wantErr bool
	}{
		{
			name:    "missing endpoint",
			env:     map[string]string{"CACHE_SYNC_TAG": "worker-v2-v0-20-0", "CACHE_SYNC_S3_BUCKET": "bucket"},
			wantErr: true,
		},
		{
			name:    "missing bucket",
			env:     map[string]string{"CACHE_SYNC_TAG": "worker-v2-v0-20-0", "CACHE_SYNC_S3_ENDPOINT": "minio:9000"},
			wantErr: true,
		},
		{
			name:    "missing tag",
			env:     map[string]string{"CACHE_SYNC_S3_ENDPOINT": "minio:9000", "CACHE_SYNC_S3_BUCKET": "bucket"},
			wantErr: true,
		},
		{
			name: "bad interval",
			env: map[string]string{
				"CACHE_SYNC_S3_ENDPOINT": "minio:9000",
				"CACHE_SYNC_S3_BUCKET":   "bucket",
				"CACHE_SYNC_TAG":         "worker-v2-v0-20-0",
				"CACHE_SYNC_INTERVAL":    "10",
			},
			wantErr: true,
		},
		{
			name: "bad use_ssl",
			env: map[string]string{
				"CACHE_SYNC_S3_ENDPOINT": "minio:9000",
				"CACHE_SYNC_S3_BUCKET":   "bucket",
				"CACHE_SYNC_S3_USE_SSL":  "maybe",
				"CACHE_SYNC_TAG":         "worker-v2-v0-20-0",
			},
			wantErr: true,
		},
		{
			name: "defaults",
			env: map[string]string{
				"CACHE_SYNC_S3_ENDPOINT":   "minio:9000",
				"CACHE_SYNC_S3_BUCKET":     "bucket",
				"CACHE_SYNC_S3_ACCESS_KEY": "",
				"CACHE_SYNC_S3_SECRET_KEY": "",
				"CACHE_SYNC_TAG":           "worker-v2-v0-20-0",
			},
			want: &cacheSyncEnv{
				tag:          "worker-v2-v0-20-0",
				baseDir:      "/var/lib/dagger",
				workerSubdir: "worker",
				tmpDir:       "/tmp",
				interval:     10 * time.Minute,
				quiesceWait:  10 * time.Second,
				s3Endpoint:   "minio:9000",
				s3Bucket:     "bucket",
				s3Region:     "us-east-1",
				s3UseSSL:     true,
			},
		},
		{
			name: "overrides",
			env: map[string]string{
				"CACHE_SYNC_S3_ENDPOINT":   "s3.example.com:9000",
				"CACHE_SYNC_S3_BUCKET":     "b",
				"CACHE_SYNC_S3_REGION":     "eu-west-1",
				"CACHE_SYNC_S3_USE_SSL":    "false",
				"CACHE_SYNC_S3_ACCESS_KEY": "ak",
				"CACHE_SYNC_S3_SECRET_KEY": "sk",
				"CACHE_SYNC_TAG":           "worker-v2-v0-20-0",
				"CACHE_SYNC_BASE_DIR":      "/data",
				"CACHE_SYNC_WORKER_SUBDIR": "w",
				"CACHE_SYNC_TMP_DIR":       "/var/tmp",
				"CACHE_SYNC_INTERVAL":      "0",
				"CACHE_SYNC_QUIESCE_WAIT":  "2s",
			},
			want: &cacheSyncEnv{
				tag:          "worker-v2-v0-20-0",
				baseDir:      "/data",
				workerSubdir: "w",
				tmpDir:       "/var/tmp",
				interval:     0,
				quiesceWait:  2 * time.Second,
				s3Endpoint:   "s3.example.com:9000",
				s3Bucket:     "b",
				s3Region:     "eu-west-1",
				s3UseSSL:     false,
				s3AccessKey:  "ak",
				s3SecretKey:  "sk",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			got, err := loadCacheSyncEnv()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if *got != *tc.want {
				t.Fatalf("env = %+v, want %+v", *got, *tc.want)
			}
			if got.workerDir() != filepath.Join(got.baseDir, got.workerSubdir) {
				t.Fatalf("workerDir = %q", got.workerDir())
			}
		})
	}
}

func TestDirEmpty(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if got, err := dirEmpty(missing); err != nil || !got {
		t.Fatalf("missing dir: got %v/%v, want true/nil", got, err)
	}

	empty := t.TempDir()
	if got, err := dirEmpty(empty); err != nil || !got {
		t.Fatalf("empty dir: got %v/%v, want true/nil", got, err)
	}

	nonEmpty := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmpty, "x"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, err := dirEmpty(nonEmpty); err != nil || got {
		t.Fatalf("non-empty dir: got %v/%v, want false/nil", got, err)
	}
}

func TestCacheSyncConfig(t *testing.T) {
	logger := observ.NewTestLogger()

	tests := []struct {
		name string
		mut  func(*domain.Config)
		want repository.K8sCacheSyncConfig
	}{
		{
			name: "disabled",
			mut:  func(c *domain.Config) { c.Cache.Sync.Enabled = false },
			want: repository.K8sCacheSyncConfig{},
		},
		{
			name: "missing image disables",
			mut:  func(c *domain.Config) { c.Cache.Sync.Enabled = true },
			want: repository.K8sCacheSyncConfig{},
		},
		{
			name: "s3 config carried",
			mut: func(c *domain.Config) {
				c.Cache.Sync = domain.CacheSyncConfig{
					Enabled: true, OnStart: true, OnStop: true,
					Interval: 5 * time.Minute, QuiesceWait: 3 * time.Second,
					S3Endpoint: "minio.dagger-kubernetes.svc:9000",
					S3Bucket:   "snapshots-bucket",
					S3Region:   "eu-west-1",
					S3UseSSL:   false,
				}
				c.Fleet.EngineCacheSyncImage = "supervisor:dev"
			},
			want: repository.K8sCacheSyncConfig{
				Image:    "supervisor:dev",
				Interval: 5 * time.Minute, QuiesceWait: 3 * time.Second,
				Enabled: true, OnStart: true, OnStop: true,
				S3Endpoint: "minio.dagger-kubernetes.svc:9000",
				S3Bucket:   "snapshots-bucket",
				S3Region:   "eu-west-1",
				S3UseSSL:   false,
			},
		},
		{
			name: "s3 bucket falls back to cache.s3.bucket",
			mut: func(c *domain.Config) {
				c.Cache.S3.Bucket = "shared-bucket"
				c.Cache.Sync = domain.CacheSyncConfig{
					Enabled: true, OnStart: true, OnStop: true,
					S3Endpoint: "minio:9000",
				}
				c.Fleet.EngineCacheSyncImage = "supervisor:dev"
			},
			want: repository.K8sCacheSyncConfig{
				Image:      "supervisor:dev",
				Enabled:    true,
				OnStart:    true,
				OnStop:     true,
				S3Endpoint: "minio:9000",
				S3Bucket:   "shared-bucket",
				S3Region:   "",
				S3UseSSL:   false,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &domain.Config{}
			tc.mut(cfg)
			got := cacheSyncConfig(cfg, logger)
			if got != tc.want {
				t.Fatalf("cacheSyncConfig = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// fakeS3Server returns an httptest S3 endpoint implementing the subset the
// snapshot store uses (path-style addressing). meta is the metadata tarball
// body: nil means no snapshot exists (every object operation 404s with a
// NoSuchKey error body). PUTs always succeed and bucket listing returns an
// empty ListBucketResult.
func fakeS3Server(t *testing.T, bucket string, meta []byte) *httptest.Server {
	t.Helper()
	metaKey := fmt.Sprintf("/%s/%s%s", bucket,
		domain.WorkerSnapshotS3Prefix(domain.WorkerSnapshotVersionSlug("worker-v2-v0-20-0")),
		domain.MetaTarballName)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/"+bucket || r.URL.Path == "/"+bucket+"/":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>%s</Name><IsTruncated>false</IsTruncated></ListBucketResult>`, bucket)
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusOK)
		case meta != nil && r.URL.Path == metaKey && r.Method == http.MethodHead:
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(meta)))
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusOK)
		case meta != nil && r.URL.Path == metaKey && r.Method == http.MethodGet:
			_, _ = w.Write(meta)
		default:
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message><Key>%s</Key></Error>`, r.URL.Path)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// setCacheSyncS3Env points the helper at an httptest S3 endpoint.
func setCacheSyncS3Env(t *testing.T, ts *httptest.Server, bucket, baseDir string) {
	t.Helper()
	t.Setenv("CACHE_SYNC_S3_ENDPOINT", strings.TrimPrefix(ts.URL, "http://"))
	t.Setenv("CACHE_SYNC_S3_BUCKET", bucket)
	t.Setenv("CACHE_SYNC_S3_REGION", "us-east-1")
	t.Setenv("CACHE_SYNC_S3_USE_SSL", "false")
	t.Setenv("CACHE_SYNC_S3_ACCESS_KEY", "ak")
	t.Setenv("CACHE_SYNC_S3_SECRET_KEY", "sk")
	t.Setenv("CACHE_SYNC_TAG", "worker-v2-v0-20-0")
	t.Setenv("CACHE_SYNC_BASE_DIR", baseDir)
}

func TestRunCacheSyncRestoreSkipsExistingCache(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "worker"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "worker", "metadata.db"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Unreachable endpoint: the existing cache must short-circuit before any
	// dial.
	t.Setenv("CACHE_SYNC_S3_ENDPOINT", "127.0.0.1:1")
	t.Setenv("CACHE_SYNC_S3_BUCKET", "bucket")
	t.Setenv("CACHE_SYNC_TAG", "worker-v2-v0-20-0")
	t.Setenv("CACHE_SYNC_BASE_DIR", base)

	if err := runCacheSyncRestore(cliTestContext()); err != nil {
		t.Fatalf("restore: %v", err)
	}
}

func TestRunCacheSyncRestoreMissingSnapshot(t *testing.T) {
	ts := fakeS3Server(t, "snapshots", nil)
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "worker"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	setCacheSyncS3Env(t, ts, "snapshots", base)
	t.Setenv("CACHE_SYNC_TMP_DIR", t.TempDir())

	// Best-effort: a 404 snapshot must not fail the pod start.
	if err := runCacheSyncRestore(cliTestContext()); err != nil {
		t.Fatalf("restore: %v", err)
	}
}

// TestRunCacheSyncRestoreDiscardsPartialExtraction proves a failed untar
// (truncated meta tarball) leaves no leftovers: the restore discards the
// partial extraction so the next start retries a clean restore instead of
// skipping it because the dir is now non-empty.
func TestRunCacheSyncRestoreDiscardsPartialExtraction(t *testing.T) {
	// A valid worker tarball, then cut it in half: some entries extract, then
	// the untar hits the truncation and fails.
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "worker"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "worker", "a.bin"), []byte("0123456789"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var buf bytes.Buffer
	if _, _, err := repository.TarGzipDir(context.Background(), src, "worker", &buf); err != nil {
		t.Fatalf("TarGzipDir: %v", err)
	}
	truncated := buf.Bytes()[:buf.Len()/2]

	ts := fakeS3Server(t, "snapshots", truncated)
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "worker"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	setCacheSyncS3Env(t, ts, "snapshots", base)
	t.Setenv("CACHE_SYNC_TMP_DIR", t.TempDir())

	if err := runCacheSyncRestore(cliTestContext()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "worker")); !os.IsNotExist(err) {
		t.Fatalf("partial extraction not discarded: stat = %v", err)
	}
}

func TestRunCacheSyncServeFinalPushOnSIGTERM(t *testing.T) {
	ts := fakeS3Server(t, "snapshots", nil)
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "worker"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	setCacheSyncS3Env(t, ts, "snapshots", base)
	t.Setenv("CACHE_SYNC_TMP_DIR", t.TempDir())
	t.Setenv("CACHE_SYNC_INTERVAL", "0") // no periodic pushes
	t.Setenv("CACHE_SYNC_QUIESCE_WAIT", "1ms")

	done := make(chan error, 1)
	go func() { done <- runCacheSyncServe(cliTestContext()) }()

	time.Sleep(250 * time.Millisecond) // let the sidecar register its signal handler
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill: %v", err)
	}

	select {
	case err := <-done:
		// The final push is best-effort; the sidecar must still exit 0.
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not exit on SIGTERM")
	}
}
