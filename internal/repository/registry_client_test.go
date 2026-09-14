package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

func testClient(t *testing.T, handler http.HandlerFunc) *RegistryStatsClient {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return NewRegistryStatsClient(ts.Listener.Addr().String())
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
	c := NewRegistryStatsClient(ts.Listener.Addr().String())
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
			name: "no-sizes-fallback-head",
			handler: func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					_, _ = fmt.Fprintf(w, `{"config":{"digest":"sha256:cfg"},"layers":[{"digest":"%s"},{"digest":"%s"}]}`, digestRepeat("1"), digestRepeat("2"))
				case http.MethodHead:
					if r.URL.Path == "/v2/dagger-cache/blobs/"+digestRepeat("1") {
						w.Header().Set("Content-Length", "40")
					} else if r.URL.Path == "/v2/dagger-cache/blobs/"+digestRepeat("2") {
						w.Header().Set("Content-Length", "50")
					}
					w.WriteHeader(http.StatusOK)
				}
			},
			wantDigest: "", // computed from body hash
			wantSize:   90,
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

func TestRegistryProbeManifest(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantOK  bool
		wantErr error
	}{
		{"ok-200", http.StatusOK, true, nil},
		{"not-found-404", http.StatusNotFound, false, nil},
		{"method-not-allowed-405", http.StatusMethodNotAllowed, false, nil},
		{"unauthorized-401", http.StatusUnauthorized, false, ErrRegistryUnreachable},
		{"forbidden-403", http.StatusForbidden, false, ErrRegistryUnreachable},
		{"server-error-500", http.StatusInternalServerError, false, ErrRegistryUnreachable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead {
					t.Errorf("method = %q", r.Method)
				}
				w.WriteHeader(tc.status)
			})
			got, err := c.ProbeManifest(context.Background(), "dagger-cache", "v0-21-4")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got != tc.wantOK {
				t.Fatalf("ok = %v, want %v", got, tc.wantOK)
			}
		})
	}
}

func TestRegistryProbeBlob(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantOK  bool
		wantErr error
	}{
		{"ok-200", http.StatusOK, true, nil},
		{"not-found-404", http.StatusNotFound, false, nil},
		{"method-not-allowed-405", http.StatusMethodNotAllowed, false, nil},
		{"unauthorized-401", http.StatusUnauthorized, false, ErrRegistryUnreachable},
		{"forbidden-403", http.StatusForbidden, false, ErrRegistryUnreachable},
		{"server-error-500", http.StatusInternalServerError, false, ErrRegistryUnreachable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead {
					t.Errorf("method = %q", r.Method)
				}
				w.WriteHeader(tc.status)
			})
			got, err := c.ProbeBlob(context.Background(), "dagger-cache", digestRepeat("a"))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got != tc.wantOK {
				t.Fatalf("ok = %v, want %v", got, tc.wantOK)
			}
		})
	}
}

func TestRegistryHost(t *testing.T) {
	c := NewRegistryStatsClient("localhost:5000")
	if c.Host() != "localhost:5000" {
		t.Fatalf("Host = %q", c.Host())
	}
	if got := c.baseURL(); got != "http://localhost:5000" {
		t.Fatalf("baseURL = %q", got)
	}
}

func TestRegistryStatsClientWithAuthSendsBasic(t *testing.T) {
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

			c := NewRegistryStatsClientWithAuth(ts.Listener.Addr().String(), tc.username, tc.password)
			if err := c.Ping(context.Background()); err != nil {
				t.Fatalf("Ping: %v", err)
			}
			if gotAuth != tc.wantAuth {
				t.Fatalf("Authorization = %q, want %q", gotAuth, tc.wantAuth)
			}
		})
	}
}

func TestBlobSizeMissingLength(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	_, err := c.BlobSize(context.Background(), "dagger-cache", digestRepeat("a"))
	if err == nil {
		t.Fatal("expected error when content-length missing")
	}
}

func TestBlobSizeInvalidDigest(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %q", r.URL.Path)
	})
	if _, err := c.BlobSize(context.Background(), "dagger-cache", "sha256:not-hex"); err == nil {
		t.Fatal("expected error for invalid digest")
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

func TestGetManifestRejectsMalformedDigestHeader(t *testing.T) {
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

func TestUploadBlobStream(t *testing.T) {
	payload := []byte("streamed-blob-bytes-0123456789")
	wantDigest := "sha256:" + sha256HexBytes(payload)

	tests := []struct {
		name       string
		digest     string
		size       int64
		wantErr    error
		wantPut    bool
		respDigest string
		putStatus  int
		initStatus int
	}{
		{
			name:      "ok",
			digest:    wantDigest,
			size:      int64(len(payload)),
			wantPut:   true,
			putStatus: http.StatusCreated,
		},
		{
			name:      "ok-200",
			digest:    wantDigest,
			size:      int64(len(payload)),
			wantPut:   true,
			putStatus: http.StatusOK,
		},
		{
			name:       "digest-mismatch-header",
			digest:     wantDigest,
			size:       int64(len(payload)),
			wantPut:    true,
			putStatus:  http.StatusCreated,
			respDigest: "sha256:" + strings.Repeat("f", 64),
			wantErr:    errUploadDigestMismatch,
		},
		{
			name:       "initiate-500",
			digest:     wantDigest,
			size:       int64(len(payload)),
			initStatus: http.StatusInternalServerError,
			wantErr:    ErrRegistryUnreachable,
		},
		{
			name:      "put-500",
			digest:    wantDigest,
			size:      int64(len(payload)),
			wantPut:   true,
			putStatus: http.StatusInternalServerError,
			wantErr:   ErrRegistryUnreachable,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody []byte
			var gotLength int64
			var gotPutDigest string
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPost:
					if r.URL.Path != "/v2/dagger-cache/blobs/uploads/" {
						t.Errorf("post path = %q", r.URL.Path)
					}
					if tc.initStatus != 0 {
						w.WriteHeader(tc.initStatus)
						return
					}
					w.Header().Set("Location", "http://"+r.Host+"/v2/dagger-cache/blobs/uploads/1")
					w.WriteHeader(http.StatusAccepted)
				case http.MethodPut:
					gotLength = r.ContentLength
					gotPutDigest = r.URL.Query().Get("digest")
					if tc.respDigest != "" {
						w.Header().Set("Docker-Content-Digest", tc.respDigest)
					}
					b, _ := io.ReadAll(r.Body)
					gotBody = b
					w.WriteHeader(tc.putStatus)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
			})

			err := c.UploadBlobStream(context.Background(), "dagger-cache", tc.digest, tc.size, bytes.NewReader(payload))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if !tc.wantPut {
				return
			}
			if !bytes.Equal(gotBody, payload) {
				t.Fatalf("uploaded body = %q, want %q", gotBody, payload)
			}
			if gotLength != tc.size {
				t.Fatalf("Content-Length = %d, want %d", gotLength, tc.size)
			}
			if gotPutDigest != tc.digest {
				t.Fatalf("digest param = %q, want %q", gotPutDigest, tc.digest)
			}
		})
	}
}

func TestUploadBlobStreamInvalidDigest(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %q", r.URL.Path)
	})
	if err := c.UploadBlobStream(context.Background(), "dagger-cache", "sha256:not-hex", 1, strings.NewReader("x")); err == nil {
		t.Fatal("expected error for invalid digest")
	}
}

func TestUploadBlobStreamMissingLocation(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted) // no Location header
	})
	if err := c.UploadBlobStream(context.Background(), "dagger-cache", digestRepeat("a"), 1, strings.NewReader("x")); err == nil {
		t.Fatal("expected error for missing Location header")
	}
}

func TestUploadBlobDelegatesToStream(t *testing.T) {
	// UploadBlob must keep working (CLI cache path) and return the computed
	// digest of the buffered body.
	var gotBody []byte
	var gotDigestParam string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Location", "http://"+r.Host+"/v2/dagger-cache/blobs/uploads/1")
			w.WriteHeader(http.StatusAccepted)
		case http.MethodPut:
			gotDigestParam = r.URL.Query().Get("digest")
			b, _ := io.ReadAll(r.Body)
			gotBody = b
			w.Header().Set("Docker-Content-Digest", gotDigestParam)
			w.WriteHeader(http.StatusCreated)
		}
	})

	body := []byte("buffered-then-streamed")
	digest, size, err := c.UploadBlob(context.Background(), "dagger-cache", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("UploadBlob: %v", err)
	}
	if size != int64(len(body)) {
		t.Fatalf("size = %d, want %d", size, len(body))
	}
	if digest != "sha256:"+sha256HexBytes(body) {
		t.Fatalf("digest = %s, want sha256 of the body", digest)
	}
	if !bytes.Equal(gotBody, body) || gotDigestParam != digest {
		t.Fatalf("uploaded = %q (digest %q), want %q (digest %s)", gotBody, gotDigestParam, body, digest)
	}
}

func TestRegistryStatsClientWithTimeout(t *testing.T) {
	// The 10s default truncates multi-GB transfers (worker snapshots); the
	// builder must raise the total per-request timeout.
	c := NewRegistryStatsClient("reg:5000").WithTimeout(5 * time.Minute)
	if c.httpClient.Timeout != 5*time.Minute {
		t.Fatalf("timeout = %v, want 5m", c.httpClient.Timeout)
	}
}
