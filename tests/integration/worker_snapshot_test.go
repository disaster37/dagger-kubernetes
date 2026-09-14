package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// ociSnapshotRegistry is a minimal in-memory OCI Distribution v2 registry:
// just enough of the API for the worker-snapshot flow (monolithic blob
// upload, manifest put/get, blob get). Concurrent writers are safe.
type ociSnapshotRegistry struct {
	mu        sync.Mutex
	blobs     map[string][]byte // digest -> bytes
	manifests map[string][]byte // "repo:tag" -> raw manifest JSON
}

func newOCISnapshotRegistry() *ociSnapshotRegistry {
	return &ociSnapshotRegistry{
		blobs:     map[string][]byte{},
		manifests: map[string][]byte{},
	}
}

func (o *ociSnapshotRegistry) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/"), "/")

		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/blobs/uploads"):
			// Initiate a monolithic blob upload: POST /v2/<repo>/blobs/uploads/
			w.Header().Set("Location", "/v2/"+path+"/1")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && strings.Contains(r.URL.RawQuery, "digest="):
			// Finish the monolithic blob upload: PUT /v2/<repo>/blobs/uploads/<id>?digest=<d>
			i := strings.LastIndex(path, "/blobs/uploads/")
			if i < 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			repo, digest := path[:i], r.URL.Query().Get("digest")
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			o.mu.Lock()
			o.blobs[repo+":"+digest] = body
			o.mu.Unlock()
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut:
			// Manifest put: /v2/<repo>/manifests/<tag> — stored verbatim, last writer wins.
			i := strings.LastIndex(path, "/manifests/")
			if i < 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			repo, tag := path[:i], path[i+len("/manifests/"):]
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			o.mu.Lock()
			o.manifests[repo+":"+tag] = body
			o.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && strings.Contains(path, "/manifests/"):
			i := strings.LastIndex(path, "/manifests/")
			repo, tag := path[:i], path[i+len("/manifests/"):]
			o.mu.Lock()
			defer o.mu.Unlock()
			body, ok := o.manifests[repo+":"+tag]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(body)
		case r.Method == http.MethodGet:
			// Blob get: /v2/<repo>/blobs/<digest>
			i := strings.LastIndex(path, "/blobs/")
			if i < 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			repo, digest := path[:i], path[i+len("/blobs/"):]
			o.mu.Lock()
			defer o.mu.Unlock()
			body, ok := o.blobs[repo+":"+digest]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// writeSyncTestFile creates a regular file (with parent dirs) in a worker dir.
func writeSyncTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestWorkerSnapshotSyncIntegration drives the full worker-snapshot flow
// against a real OCI Distribution v2 wire: pod 1 pushes its worker dir, pod 2
// (a fresh store, i.e. a fresh PVC) pulls it back, and concurrent pushes to
// the same tag leave a valid last-writer-wins manifest.
func TestWorkerSnapshotSyncIntegration(t *testing.T) {
	reg := newOCISnapshotRegistry()
	ts := httptest.NewServer(reg.handler())
	t.Cleanup(ts.Close)
	addr := ts.Listener.Addr().String()

	newStore := func() *repository.WorkerSnapshotStore {
		client := repository.NewRegistryStatsClientWithAuth(addr, "", "")
		return repository.NewWorkerSnapshotStore(client, domain.WorkerSnapshotsRepo, observ.NewTestLogger())
	}
	ctx := context.Background()
	tag := domain.WorkerSnapshotTag("v0.20.0")

	// --- pod 1: build a worker dir, push it ---
	src := t.TempDir()
	writeSyncTestFile(t, filepath.Join(src, "worker", "metadata.db"), "bolt-metadata")
	writeSyncTestFile(t, filepath.Join(src, "worker", "content", "abc"), "blob-bytes")
	var buf bytes.Buffer
	digest, size, err := repository.TarGzipDir(ctx, src, "worker", &buf)
	if err != nil {
		t.Fatalf("TarGzipDir: %v", err)
	}
	if err := newStore().Push(ctx, tag, &buf, digest, size); err != nil {
		t.Fatalf("pod1 push: %v", err)
	}

	// --- pod 2: fresh store (fresh PVC) pulls the same bytes back ---
	dst := t.TempDir()
	ok, err := newStore().Pull(ctx, tag, dst)
	if err != nil {
		t.Fatalf("pod2 pull: %v", err)
	}
	if !ok {
		t.Fatal("pod2 pull: ok=false, want true")
	}
	for _, rel := range []string{"worker/metadata.db", "worker/content/abc"} {
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if want := map[string]string{"worker/metadata.db": "bolt-metadata", "worker/content/abc": "blob-bytes"}[rel]; string(got) != want {
			t.Fatalf("%s = %q, want %q", rel, got, want)
		}
	}

	// --- concurrent pushes: any winner is a valid, self-consistent snapshot ---
	const pods = 4
	dirs := make([]string, pods)
	for i := range dirs {
		dirs[i] = t.TempDir()
		if err := os.MkdirAll(filepath.Join(dirs[i], "worker"), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	var wg sync.WaitGroup
	errCh := make(chan error, pods)
	for i := 0; i < pods; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dir := dirs[i]
			if err := os.WriteFile(filepath.Join(dir, "worker", fmt.Sprintf("pod-%d.txt", i)), []byte(fmt.Sprintf("content-%d", i)), 0o600); err != nil {
				errCh <- err
				return
			}
			var buf bytes.Buffer
			d, s, err := repository.TarGzipDir(ctx, dir, "worker", &buf)
			if err != nil {
				errCh <- fmt.Errorf("pod%d tar: %w", i, err)
				return
			}
			if err := newStore().Push(ctx, tag, &buf, d, s); err != nil {
				errCh <- fmt.Errorf("pod%d push: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent push: %v", err)
	}

	// The surviving manifest resolves to exactly one pod's file, intact.
	final := t.TempDir()
	if ok, err := newStore().Pull(ctx, tag, final); err != nil || !ok {
		t.Fatalf("final pull: ok=%v err=%v, want true/nil", ok, err)
	}
	entries, err := os.ReadDir(filepath.Join(final, "worker"))
	if err != nil {
		t.Fatalf("read final worker dir: %v", err)
	}
	found := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "pod-") || !strings.HasSuffix(name, ".txt") {
			continue
		}
		found++
		idx := strings.TrimSuffix(strings.TrimPrefix(name, "pod-"), ".txt")
		b, err := os.ReadFile(filepath.Join(final, "worker", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if want := "content-" + idx; string(b) != want {
			t.Fatalf("%s = %q, want %q (torn snapshot)", name, b, want)
		}
	}
	if found != 1 {
		t.Fatalf("found %d pod-*.txt files, want exactly 1 (one winning snapshot)", found)
	}
}
