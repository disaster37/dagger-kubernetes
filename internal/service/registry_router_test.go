package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
)

// probeRegistry emulates an OCI registry for HEAD manifest/blob probes.
type probeRegistry struct {
	manifests map[string]bool // "repo:ref" -> exists
	blobs     map[string]bool // digest -> exists
}

func newProbeServer(t *testing.T, manifests, blobs map[string]bool) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		trimmed := strings.TrimPrefix(r.URL.Path, "/v2/")
		parts := strings.Split(trimmed, "/")
		if len(parts) >= 2 && parts[len(parts)-2] == "manifests" {
			key := strings.Join(parts[:len(parts)-2], "/") + ":" + parts[len(parts)-1]
			if manifests[key] {
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		if len(parts) >= 2 && parts[len(parts)-2] == "blobs" {
			if blobs[parts[len(parts)-1]] {
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func digestRepeat(c string) string {
	return "sha256:" + strings.Repeat(c, 64)
}

func TestHealthyBackendsOrdering(t *testing.T) {
	r := NewRegistryRouter([]domain.RegistryBackend{
		{ID: "a"}, {ID: "b"}, {ID: "c"},
	}, nil, observ.NewTestLogger())
	r.SetCharges(map[string]int64{"a": 100, "b": 10, "c": 50})

	got := r.HealthyBackends()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	want := []string{"b", "c", "a"}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("order = %v, want %v", backendIDs(got), want)
		}
	}
}

func backendIDs(bs []domain.RegistryBackend) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.ID
	}
	return out
}

func TestRouteForPushLeastChargedAndNoBackend(t *testing.T) {
	r := NewRegistryRouter([]domain.RegistryBackend{
		{ID: "a"}, {ID: "b"},
	}, nil, observ.NewTestLogger())
	r.SetCharges(map[string]int64{"a": 90, "b": 5})

	b, err := r.RouteForPush("")
	if err != nil || b.ID != "b" {
		t.Fatalf("RouteForPush = %v err=%v, want b", b.ID, err)
	}

	r.MarkDown("b")
	b, err = r.RouteForPush("")
	if err != nil || b.ID != "a" {
		t.Fatalf("RouteForPush after down = %v err=%v, want a", b.ID, err)
	}

	r.MarkDown("a")
	if _, err := r.RouteForPush(""); !errors.Is(err, ErrNoBackend) {
		t.Fatalf("err = %v, want ErrNoBackend", err)
	}
}

func TestRouteForPullTableHit(t *testing.T) {
	ts := newProbeServer(t, nil, nil) // probes all 404
	r := newTestRouter(t, domain.RegistryBackend{ID: "b1", InternalAddr: ts.Listener.Addr().String()})
	if err := r.RecordManifest(context.Background(), "dagger-cache", "v0-21-4", digestRepeat("a"), "b1", 0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	b, err := r.RouteForPull(context.Background(), "dagger-cache", "v0-21-4")
	if err != nil || b.ID != "b1" {
		t.Fatalf("RouteForPull = %v err=%v, want b1", b.ID, err)
	}
}

func TestRouteForPullProbeSelfHeal(t *testing.T) {
	ts := newProbeServer(t, map[string]bool{"dagger-cache:v0-21-4": true}, nil)
	r := newTestRouter(t, domain.RegistryBackend{ID: "b1", InternalAddr: ts.Listener.Addr().String()})

	b, err := r.RouteForPull(context.Background(), "dagger-cache", "v0-21-4")
	if err != nil || b.ID != "b1" {
		t.Fatalf("RouteForPull = %v err=%v, want b1", b.ID, err)
	}
	// Self-heal must have upserted the route.
	route, ok, _ := r.routes.LookupManifest(context.Background(), "dagger-cache", "v0-21-4")
	if !ok || route.BackendID != "b1" {
		t.Fatalf("self-heal route = %+v ok=%v", route, ok)
	}
}

func TestRouteForPullProbeMiss(t *testing.T) {
	ts := newProbeServer(t, nil, nil)
	r := newTestRouter(t, domain.RegistryBackend{ID: "b1", InternalAddr: ts.Listener.Addr().String()})

	if _, err := r.RouteForPull(context.Background(), "dagger-cache", "missing"); !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("err = %v, want ErrRouteNotFound", err)
	}
}

func TestRouteForPullMarksDownOnTransportError(t *testing.T) {
	ts := newProbeServer(t, nil, nil)
	addr := ts.Listener.Addr().String()
	r := newTestRouter(t, domain.RegistryBackend{ID: "b1", InternalAddr: addr})
	ts.Close() // transport error on probe

	if _, err := r.RouteForPull(context.Background(), "dagger-cache", "v0-21-4"); !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("err = %v, want ErrRouteNotFound", err)
	}
	r.mu.RLock()
	down := r.down["b1"]
	r.mu.RUnlock()
	if !down {
		t.Fatal("backend should be marked down on transport error")
	}
}

func TestRouteForBlobPullTableHitAndProbe(t *testing.T) {
	dgst := digestRepeat("a")
	ts := newProbeServer(t, nil, map[string]bool{dgst: true})
	r := newTestRouter(t, domain.RegistryBackend{ID: "b1", InternalAddr: ts.Listener.Addr().String()})

	// Probe miss → self-heal.
	b, err := r.RouteForBlobPull(context.Background(), "dagger-cache", dgst)
	if err != nil || b.ID != "b1" {
		t.Fatalf("RouteForBlobPull = %v err=%v, want b1", b.ID, err)
	}
	if backendID, ok, _ := r.routes.LookupBlob(context.Background(), dgst); !ok || backendID != "b1" {
		t.Fatalf("blob self-heal = %q ok=%v", backendID, ok)
	}

	// Table hit (probe server still returns 200; route already recorded).
	b, err = r.RouteForBlobPull(context.Background(), "dagger-cache", dgst)
	if err != nil || b.ID != "b1" {
		t.Fatalf("RouteForBlobPull table hit = %v err=%v", b.ID, err)
	}
}

func TestRouteForBlobPullMiss(t *testing.T) {
	ts := newProbeServer(t, nil, nil)
	r := newTestRouter(t, domain.RegistryBackend{ID: "b1", InternalAddr: ts.Listener.Addr().String()})

	if _, err := r.RouteForBlobPull(context.Background(), "dagger-cache", digestRepeat("z")); !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("err = %v, want ErrRouteNotFound", err)
	}
}

func TestRouteForUploadResume(t *testing.T) {
	r := newTestRouter(t, domain.RegistryBackend{ID: "b1", InternalAddr: "127.0.0.1:1"})
	if err := r.RecordUploadSession(context.Background(), "uuid-1", "dagger-cache", "b1"); err != nil {
		t.Fatalf("RecordUploadSession: %v", err)
	}

	b, err := r.RouteForUploadResume(context.Background(), "uuid-1")
	if err != nil || b.ID != "b1" {
		t.Fatalf("RouteForUploadResume = %v err=%v", b.ID, err)
	}
	if _, err := r.RouteForUploadResume(context.Background(), "missing"); !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("err = %v, want ErrRouteNotFound", err)
	}
}

func TestCompleteUploadLifecycle(t *testing.T) {
	r := newTestRouter(t, domain.RegistryBackend{ID: "b1", InternalAddr: "127.0.0.1:1"})
	ctx := context.Background()
	if err := r.RecordUploadSession(ctx, "uuid-1", "dagger-cache", "b1"); err != nil {
		t.Fatalf("RecordUploadSession: %v", err)
	}
	dgst := digestRepeat("a")
	if err := r.CompleteUpload(ctx, "uuid-1", dgst, "b1"); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if _, ok, _ := r.routes.LookupUpload(ctx, "uuid-1"); ok {
		t.Fatal("upload session should be deleted on completion")
	}
	if backendID, ok, _ := r.routes.LookupBlob(ctx, dgst); !ok || backendID != "b1" {
		t.Fatalf("blob route = %q ok=%v", backendID, ok)
	}
}

func TestRecordManifestAndRefreshCharges(t *testing.T) {
	r := newTestRouter(t,
		domain.RegistryBackend{ID: "b1", InternalAddr: "127.0.0.1:1"},
		domain.RegistryBackend{ID: "b2", InternalAddr: "127.0.0.1:2"},
	)
	ctx := context.Background()
	if err := r.RecordManifest(ctx, "r", "a", "", "b1", 40); err != nil {
		t.Fatalf("RecordManifest: %v", err)
	}
	if err := r.RecordManifest(ctx, "r", "b", "", "b2", 60); err != nil {
		t.Fatalf("RecordManifest: %v", err)
	}
	if err := r.RefreshCharges(ctx); err != nil {
		t.Fatalf("RefreshCharges: %v", err)
	}
	r.mu.RLock()
	c1 := r.charges["b1"]
	c2 := r.charges["b2"]
	r.mu.RUnlock()
	if c1 != 40 || c2 != 60 {
		t.Fatalf("charges = b1:%d b2:%d, want 40/60", c1, c2)
	}
}

func TestMarkDownMarkUp(t *testing.T) {
	r := NewRegistryRouter([]domain.RegistryBackend{{ID: "a"}, {ID: "b"}}, nil, observ.NewTestLogger())

	if len(r.HealthyBackends()) != 2 {
		t.Fatalf("healthy = %d, want 2", len(r.HealthyBackends()))
	}
	r.MarkDown("a")
	if got := r.HealthyBackends(); len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("healthy after down = %v", backendIDs(got))
	}
	r.MarkUp("a")
	if len(r.HealthyBackends()) != 2 {
		t.Fatalf("healthy after up = %d, want 2", len(r.HealthyBackends()))
	}
}
