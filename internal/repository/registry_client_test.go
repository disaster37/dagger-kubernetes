package repository

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// digestRepeat builds a valid sha256:<64 hex> digest by repeating c, so test
// fakes use realistic digest shapes (the client validates digest shape before
// placing it in a request path).
func digestRepeat(c string) string {
	return "sha256:" + strings.Repeat(c, 64)
}

func testClient(t *testing.T, handler http.HandlerFunc) *DistributionClient {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return NewDistributionClient(ts.Listener.Addr().String())
}

func TestRegistryCatalog(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    []string
		wantErr error
	}{
		{
			name: "ok",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v2/_catalog" {
					t.Errorf("path = %q", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"repositories":["dagger-cache","other"]}`))
			},
			want: []string{"dagger-cache", "other"},
		},
		{
			name: "disabled-404",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			wantErr: ErrRegistryCatalogDisabled,
		},
		{
			name: "disabled-403",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			},
			wantErr: ErrRegistryCatalogDisabled,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, tc.handler)
			got, err := c.Catalog(context.Background())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestRegistryUnreachable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	c := NewDistributionClient(ts.Listener.Addr().String())
	ts.Close() // now unreachable

	_, err := c.Catalog(context.Background())
	if !errors.Is(err, ErrRegistryUnreachable) {
		t.Fatalf("err = %v, want ErrRegistryUnreachable", err)
	}
	if err := c.Ping(context.Background()); !errors.Is(err, ErrRegistryUnreachable) {
		t.Fatalf("ping err = %v, want ErrRegistryUnreachable", err)
	}
}

func TestRegistryTags(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/dagger-cache/tags/list" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"dagger-cache","tags":["v0-21-4","v0-20-0"]}`))
	})

	got, err := c.Tags(context.Background(), "dagger-cache")
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(got) != 2 || got[0] != "v0-21-4" || got[1] != "v0-20-0" {
		t.Fatalf("got %v", got)
	}
}

func TestRegistryManifestSize(t *testing.T) {
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantDigest string
		wantSize   int64
		wantLayers int64
		wantErr    error
	}{
		{
			name: "with-sizes",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("method = %q", r.Method)
				}
				w.Header().Set("Docker-Content-Digest", digestRepeat("a"))
				_, _ = w.Write([]byte(`{"config":{"digest":"sha256:cfg","size":10},"layers":[{"digest":"sha256:l1","size":20},{"digest":"sha256:l2","size":30}]}`))
			},
			wantDigest: digestRepeat("a"),
			wantSize:   60,
			wantLayers: 2,
		},
		{
			name: "no-sizes-unknown",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"config":{"digest":"sha256:cfg"},"layers":[{"digest":"sha256:l1"},{"digest":"sha256:l2"}]}`))
			},
			wantDigest: "", // computed from body hash
			wantSize:   -1,
			wantLayers: 2,
		},
		{
			name: "not-found",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			wantErr: ErrManifestNotFound,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, tc.handler)
			digest, size, layers, err := c.ManifestSize(context.Background(), "dagger-cache", "v0-21-4")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				return
			}
			if tc.wantDigest != "" && digest != tc.wantDigest {
				t.Errorf("digest = %q, want %q", digest, tc.wantDigest)
			}
			if tc.wantDigest == "" && digest == "" {
				t.Error("digest should be computed when header absent")
			}
			if size != tc.wantSize {
				t.Errorf("size = %d, want %d", size, tc.wantSize)
			}
			if layers != tc.wantLayers {
				t.Errorf("layers = %d, want %d", layers, tc.wantLayers)
			}
		})
	}
}

// indexChild renders one image-index child descriptor with a platform.
func indexChild(digest, os, arch string) string {
	return fmt.Sprintf(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":100,"platform":{"os":%q,"architecture":%q}}`, digest, os, arch)
}

// attestChild renders a BuildKit attestation child descriptor.
func attestChild(digest string) string {
	return fmt.Sprintf(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":50,"platform":{"os":"unknown","architecture":"unknown"},"annotations":{%q:%q}}`, digest, annotationReferenceType, referenceTypeAttestation)
}

// indexBody renders an OCI image index with the given child descriptors.
func indexBody(children ...string) string {
	return fmt.Sprintf(`{"mediaType":%q,"manifests":[%s]}`, ociIndexMediaType, strings.Join(children, ","))
}

// directBody renders a direct manifest whose config + layers sum to size with
// the given layer count (config and each layer = size/(layers+1)).
func directBody(size int64, layers int) string {
	per := size / int64(layers+1)
	descriptors := make([]string, 0, layers)
	for i := 0; i < layers; i++ {
		descriptors = append(descriptors, fmt.Sprintf(`{"digest":"sha256:%064x","size":%d}`, i, per))
	}
	return fmt.Sprintf(`{"config":{"digest":"sha256:cfg","size":%d},"layers":[%s]}`, per, strings.Join(descriptors, ","))
}

// indexTestHandler serves an index at the v0-21-4 tag and its children by
// digest; children absent from the map 404.
func indexTestHandler(index, indexDigest string, children map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const tagPath = "/v2/dagger-cache/manifests/v0-21-4"
		if r.URL.Path == tagPath {
			w.Header().Set("Docker-Content-Digest", indexDigest)
			_, _ = w.Write([]byte(index))
			return
		}
		digest := strings.TrimPrefix(r.URL.Path, "/v2/dagger-cache/manifests/")
		body, ok := children[digest]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Docker-Content-Digest", digest)
		_, _ = w.Write([]byte(body))
	}
}

// TestRegistryManifestSizeIndex covers index / manifest-list resolution: the
// returned digest must stay the top-level index digest (prune unlinks the tag)
// while size/layers come from a representative child.
func TestRegistryManifestSizeIndex(t *testing.T) {
	amd64 := digestRepeat("a")
	arm64 := digestRepeat("b")
	attest := digestRepeat("c")
	nested := digestRepeat("d")
	nested2 := digestRepeat("3")
	grand := digestRepeat("f")
	indexDig := digestRepeat("e")

	amd64Body := directBody(300, 2)
	arm64Body := directBody(330, 2)
	grandBody := directBody(500, 4)

	tests := []struct {
		name        string
		index       string
		indexDigest string
		children    map[string]string
		wantDigest  string
		wantSize    int64
		wantLayers  int64
	}{
		{
			name:        "amd64-preferred",
			index:       indexBody(indexChild(amd64, "linux", "amd64"), indexChild(arm64, "linux", "arm64")),
			indexDigest: indexDig,
			children:    map[string]string{amd64: amd64Body, arm64: arm64Body},
			wantDigest:  indexDig,
			wantSize:    300,
			wantLayers:  2,
		},
		{
			name:        "arm64-fallback",
			index:       indexBody(indexChild(arm64, "linux", "arm64")),
			indexDigest: indexDig,
			children:    map[string]string{arm64: arm64Body},
			wantDigest:  indexDig,
			wantSize:    330,
			wantLayers:  2,
		},
		{
			name:        "attestation-and-unknown-skipped",
			index:       indexBody(attestChild(attest), indexChild(arm64, "unknown", "unknown"), indexChild(amd64, "linux", "amd64")),
			indexDigest: indexDig,
			children:    map[string]string{amd64: amd64Body},
			wantDigest:  indexDig,
			wantSize:    300,
			wantLayers:  2,
		},
		{
			name:        "child-404-unknown",
			index:       indexBody(indexChild(amd64, "linux", "amd64")),
			indexDigest: indexDig,
			children:    map[string]string{},
			wantDigest:  indexDig,
			wantSize:    -1,
			wantLayers:  -1,
		},
		{
			name:        "empty-index-unknown",
			index:       indexBody(),
			indexDigest: indexDig,
			children:    map[string]string{},
			wantDigest:  indexDig,
			wantSize:    -1,
			wantLayers:  -1,
		},
		{
			name:        "all-attestation-unknown",
			index:       indexBody(attestChild(attest)),
			indexDigest: indexDig,
			children:    map[string]string{},
			wantDigest:  indexDig,
			wantSize:    -1,
			wantLayers:  -1,
		},
		{
			name:        "nested-index-resolves-grandchild",
			index:       indexBody(indexChild(nested, "linux", "amd64")),
			indexDigest: indexDig,
			children: map[string]string{
				nested: indexBody(indexChild(grand, "linux", "amd64")),
				grand:  grandBody,
			},
			wantDigest: indexDig,
			wantSize:   500,
			wantLayers: 4,
		},
		{
			name:        "nested-past-depth-cap-unknown",
			index:       indexBody(indexChild(nested, "linux", "amd64")),
			indexDigest: indexDig,
			children: map[string]string{
				nested:  indexBody(indexChild(nested2, "linux", "amd64")),
				nested2: indexBody(indexChild(grand, "linux", "amd64")),
			},
			wantDigest: indexDig,
			wantSize:   -1,
			wantLayers: -1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, indexTestHandler(tc.index, tc.indexDigest, tc.children))
			digest, size, layers, err := c.ManifestSize(context.Background(), "dagger-cache", "v0-21-4")
			if err != nil {
				t.Fatalf("ManifestSize: %v", err)
			}
			if digest != tc.wantDigest {
				t.Errorf("digest = %q, want %q", digest, tc.wantDigest)
			}
			if size != tc.wantSize {
				t.Errorf("size = %d, want %d", size, tc.wantSize)
			}
			if layers != tc.wantLayers {
				t.Errorf("layers = %d, want %d", layers, tc.wantLayers)
			}
		})
	}
}

// TestManifestSizeIndexRejectsMalformedChildDigest proves an index child with a
// hostile (non-sha256) digest is never interpolated into a request: only the
// top-level tag fetch happens, and the result is unknown (-1/-1) with the valid
// top-level digest (CWE-20/CWE-918).
func TestManifestSizeIndexRejectsMalformedChildDigest(t *testing.T) {
	var requests []string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		w.Header().Set("Docker-Content-Digest", digestRepeat("e"))
		_, _ = w.Write([]byte(`{"mediaType":"` + ociIndexMediaType + `","manifests":[{"digest":"../../v2/_catalog","platform":{"os":"linux","architecture":"amd64"}}]}`))
	})
	digest, size, layers, err := c.ManifestSize(context.Background(), "dagger-cache", "v0-21-4")
	if err != nil {
		t.Fatalf("ManifestSize: %v", err)
	}
	if size != -1 || layers != -1 {
		t.Fatalf("size/layers = %d/%d, want -1/-1", size, layers)
	}
	if !validDigest(digest) {
		t.Fatalf("digest = %q, want valid top-level digest", digest)
	}
	if len(requests) != 1 || requests[0] != "/v2/dagger-cache/manifests/v0-21-4" {
		t.Fatalf("requests = %v, want only the top-level tag fetch", requests)
	}
}

func TestRegistryDeleteManifest(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr error
	}{
		{"accepted", http.StatusAccepted, nil},
		{"ok", http.StatusOK, nil},
		{"no-content", http.StatusNoContent, nil},
		{"disabled-405", http.StatusMethodNotAllowed, domain.ErrRegistryDeleteDisabled},
		{"disabled-403", http.StatusForbidden, domain.ErrRegistryDeleteDisabled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dgst := digestRepeat("a")
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete {
					t.Errorf("method = %q", r.Method)
				}
				if r.URL.Path != "/v2/dagger-cache/manifests/"+dgst {
					t.Errorf("path = %q", r.URL.Path)
				}
				w.WriteHeader(tc.status)
			})
			err := c.DeleteManifest(context.Background(), "dagger-cache", dgst)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestRegistryPing(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	fail := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if err := fail.Ping(context.Background()); !errors.Is(err, ErrRegistryUnreachable) {
		t.Fatalf("ping err = %v, want ErrRegistryUnreachable", err)
	}
}

func TestRegistryHost(t *testing.T) {
	c := NewDistributionClient("localhost:5000")
	if c.Host() != "localhost:5000" {
		t.Fatalf("Host = %q", c.Host())
	}
	if got := c.baseURL(); got != "http://localhost:5000" {
		t.Fatalf("baseURL = %q", got)
	}
}

func TestDistributionClientWithAuthSendsBasic(t *testing.T) {
	tests := []struct {
		name     string
		username string
		password string
		wantAuth string
	}{
		{"both", "user", "pass", "Basic dXNlcjpwYXNz"},
		{"username-only", "user", "", "Basic dXNlcjo="},
		{"empty-creds", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				w.WriteHeader(http.StatusOK)
			}))
			defer ts.Close()

			c := NewDistributionClientWithAuth(ts.Listener.Addr().String(), tc.username, tc.password)
			if err := c.Ping(context.Background()); err != nil {
				t.Fatalf("Ping: %v", err)
			}
			if gotAuth != tc.wantAuth {
				t.Fatalf("Authorization = %q, want %q", gotAuth, tc.wantAuth)
			}
		})
	}
}

func TestDeleteManifestInvalidDigest(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %q", r.URL.Path)
	})
	if err := c.DeleteManifest(context.Background(), "dagger-cache", "../../v2/_catalog"); err == nil {
		t.Fatal("expected error for invalid digest")
	}
}

func TestManifestSizeRejectsMalformedDigestHeader(t *testing.T) {
	// A compromised registry returning a non-sha256 digest header must not
	// propagate it: the client falls back to computing the digest from the
	// body (CWE-20/CWE-918).
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Content-Digest", "../../v2/_catalog")
		_, _ = w.Write([]byte(`{"config":{"digest":"sha256:cfg","size":1},"layers":[]}`))
	})
	dgst, _, _, err := c.ManifestSize(context.Background(), "dagger-cache", "v0-21-4")
	if err != nil {
		t.Fatalf("ManifestSize: %v", err)
	}
	if !validDigest(dgst) {
		t.Fatalf("digest = %q, want sha256:<hex> computed from body", dgst)
	}
}

func TestCatalogRejectsOversizedBody(t *testing.T) {
	// A registry returning a body larger than maxRegistryBody must fail
	// instead of exhausting memory (CWE-400/CWE-770).
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"repositories":["`))
		_, _ = w.Write(make([]byte, maxRegistryBody+1))
		_, _ = w.Write([]byte(`"]}`))
	})
	if _, err := c.Catalog(context.Background()); err == nil {
		t.Fatal("expected error for oversized catalog body")
	}
}

func TestDistributionClientWithTimeout(t *testing.T) {
	// The 10s default truncates large transfers; the caller can raise the
	// total per-request timeout.
	c := NewDistributionClient("reg:5000").WithTimeout(5 * time.Minute)
	if c.httpClient.Timeout != 5*time.Minute {
		t.Fatalf("timeout = %v, want 5m", c.httpClient.Timeout)
	}
}

// TestDistributionClientPathEscaping verifies that repository/tag/digest
// values taken from the prune request body are validated and path-escaped
// before they are interpolated into the wire URL (CWE-22/CWE-918): they cannot
// traverse out of /v2/ via "..", steer the host via an absolute URL, or inject
// a query/fragment. Repository separators are preserved (each segment is
// escaped independently) so nested repos such as "library/alpine" reach the
// registry as a literal path instead of "library%2Falpine".
func TestDistributionClientPathEscaping(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	var gotPaths []string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.EscapedPath())
		if r.URL.RawQuery != "" {
			t.Errorf("rawQuery = %q, want empty (metacharacters must be escaped into the path)", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config":{"digest":"sha256:cfg","size":1},"layers":[]}`))
	})

	// Tags: nested repos keep their separators; hostile repos are rejected.
	tagsCases := []struct {
		name     string
		repo     string
		wantPath string
		wantErr  bool
	}{
		{"nested", "library/alpine", "/v2/library/alpine/tags/list", false},
		{"deeply-nested", "team/project/image", "/v2/team/project/image/tags/list", false},
		{"query-fragment", "a?b#c", "/v2/a%3Fb%23c/tags/list", false},
		{"traversal", "../../v2/_catalog", "", true},
		{"empty-segment", "a//b", "", true},
		{"dot-segment", "a/./b", "", true},
		{"absolute-url", "http://evil.com:5000/x", "", true},
		{"empty", "", "", true},
	}
	for _, tc := range tagsCases {
		before := len(gotPaths)
		_, err := c.Tags(context.Background(), tc.repo)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: Tags(%q) = nil error, want rejection", tc.name, tc.repo)
			}
			if len(gotPaths) != before {
				t.Errorf("%s: rejected repo must not issue a request, got %q", tc.name, gotPaths[before:])
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: Tags: %v", tc.name, err)
		}
		if got := gotPaths[len(gotPaths)-1]; got != tc.wantPath {
			t.Errorf("%s: wire path = %q, want %q", tc.name, got, tc.wantPath)
		}
	}

	// ManifestSize: nested repo + an escapable-but-hostile tag.
	if _, _, _, err := c.ManifestSize(context.Background(), "library/alpine", "v1?x#y"); err != nil {
		t.Fatalf("ManifestSize: %v", err)
	}
	if got := gotPaths[len(gotPaths)-1]; got != "/v2/library/alpine/manifests/v1%3Fx%23y" {
		t.Errorf("manifest wire path = %q", got)
	}
	// A tag containing "/" is not a single segment and must be rejected.
	if _, _, _, err := c.ManifestSize(context.Background(), "library/alpine", "v1/evil"); err == nil {
		t.Error("ManifestSize accepted a tag containing '/'")
	}

	// DeleteManifest: nested repo, valid digest (colon survives escaping).
	if err := c.DeleteManifest(context.Background(), "library/alpine", digest); err != nil {
		t.Fatalf("DeleteManifest: %v", err)
	}
	if got := gotPaths[len(gotPaths)-1]; got != "/v2/library/alpine/manifests/"+digest {
		t.Errorf("delete wire path = %q, want /v2/library/alpine/manifests/%s", got, digest)
	}
}

// TestDistributionClientNestedRepository serves only the literal nested path:
// the old whole-string escaping turned "library/alpine" into
// "library%2Falpine" and 404ed, so this is a regression test for the slash
// case against a server that does not unescape.
func TestDistributionClientNestedRepository(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/v2/library/alpine/tags/list" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"library/alpine","tags":["3.20","latest"]}`))
	})

	got, err := c.Tags(context.Background(), "library/alpine")
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(got) != 2 || got[0] != "3.20" || got[1] != "latest" {
		t.Fatalf("got %v", got)
	}
}
