package repository

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestGitHubCLIUpstreamListSinglePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("page") != "1" {
			t.Errorf("unexpected query %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"tag_name":"v0.21.8"},{"tag_name":"v0.21.7"}]`)
	}))
	defer srv.Close()

	u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{ReleasesURL: srv.URL + "/releases"})
	tags, err := u.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tags) != 2 || tags[0] != "v0.21.8" || tags[1] != "v0.21.7" {
		t.Fatalf("tags = %v", tags)
	}
}

func TestGitHubCLIUpstreamListPaginated(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("Link", "<"+srv.URL+"/releases?per_page=100&page=2>; rel=\"next\"")
			_, _ = io.WriteString(w, `[{"tag_name":"v0.21.8"}]`)
		case "2":
			_, _ = io.WriteString(w, `[{"tag_name":"v0.21.7"},{"tag_name":""}]`)
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	}))
	defer srv.Close()

	u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{ReleasesURL: srv.URL + "/releases"})
	tags, err := u.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tags) != 2 || tags[0] != "v0.21.8" || tags[1] != "v0.21.7" {
		t.Fatalf("tags = %v", tags)
	}
}

func TestGitHubCLIUpstreamListSendsTokenToAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sekrit" {
			t.Errorf("Authorization = %q, want Bearer sekrit", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	}))
	defer srv.Close()

	u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{ReleasesURL: srv.URL + "/releases", GitHubToken: "sekrit"})
	if _, err := u.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
}

func TestGitHubCLIUpstreamListPageCap(t *testing.T) {
	var srv *httptest.Server
	requests := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Link", "<"+srv.URL+"/releases?per_page=100&page=2>; rel=\"next\"")
		_, _ = io.WriteString(w, `[{"tag_name":"v0.21.8"}]`)
	}))
	defer srv.Close()

	u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{ReleasesURL: srv.URL + "/releases"})
	_, err := u.List(context.Background())
	if !errors.Is(err, domain.ErrCLIUpstreamUnavailable) {
		t.Fatalf("err = %v, want ErrCLIUpstreamUnavailable", err)
	}
	if requests != maxReleasePages {
		t.Fatalf("requests = %d, want %d (bounded)", requests, maxReleasePages)
	}
}

func TestGitHubCLIUpstreamNextLinkHostValidation(t *testing.T) {
	u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{ReleasesURL: "https://api.github.com/repos/dagger/dagger/releases"})

	tests := []struct {
		name string
		link string
		want string
	}{
		{
			name: "same host",
			link: `<https://api.github.com/repos/dagger/dagger/releases?page=2>; rel="next"`,
			want: "https://api.github.com/repos/dagger/dagger/releases?page=2",
		},
		{
			name: "different host rejected",
			link: `<https://evil.example.com/releases?page=2>; rel="next"`,
			want: "",
		},
		{
			name: "non-http scheme rejected",
			link: `<ftp://api.github.com/releases?page=2>; rel="next"`,
			want: "",
		},
		{
			name: "relative rejected",
			link: `</releases?page=2>; rel="next"`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := u.nextLink(tt.link); got != tt.want {
				t.Fatalf("nextLink(%q) = %q, want %q", tt.link, got, tt.want)
			}
		})
	}
}

func TestGitHubCLIUpstreamListErrors(t *testing.T) {
	t.Run("non-200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "rate limited", http.StatusForbidden)
		}))
		defer srv.Close()

		u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{ReleasesURL: srv.URL + "/releases"})
		_, err := u.List(context.Background())
		if !errors.Is(err, domain.ErrCLIUpstreamUnavailable) {
			t.Fatalf("err = %v, want ErrCLIUpstreamUnavailable", err)
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `not json`)
		}))
		defer srv.Close()

		u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{ReleasesURL: srv.URL + "/releases"})
		_, err := u.List(context.Background())
		if !errors.Is(err, domain.ErrCLIUpstreamUnavailable) {
			t.Fatalf("err = %v, want ErrCLIUpstreamUnavailable", err)
		}
	})

	t.Run("network error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		url := srv.URL
		srv.Close()

		u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{ReleasesURL: url + "/releases"})
		_, err := u.List(context.Background())
		if !errors.Is(err, domain.ErrCLIUpstreamUnavailable) {
			t.Fatalf("err = %v, want ErrCLIUpstreamUnavailable", err)
		}
	})
}

func TestGitHubCLIUpstreamFetchChecksums(t *testing.T) {
	const body = "53e226c7 dagger_v0.21.8_linux_amd64.tar.gz\n00aa11bb *dagger_v0.21.8_darwin_arm64.tar.gz\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/download/v0.21.8/checksums.txt" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{DownloadBase: srv.URL + "/download"})
	sums, err := u.FetchChecksums(context.Background(), "v0.21.8")
	if err != nil {
		t.Fatalf("FetchChecksums: %v", err)
	}
	if len(sums) != 2 {
		t.Fatalf("sums = %v, want 2 entries", sums)
	}
	if sums["dagger_v0.21.8_linux_amd64.tar.gz"] != "53e226c7" {
		t.Fatalf("linux sum = %q", sums["dagger_v0.21.8_linux_amd64.tar.gz"])
	}
	if sums["dagger_v0.21.8_darwin_arm64.tar.gz"] != "00aa11bb" {
		t.Fatalf("darwin sum = %q", sums["dagger_v0.21.8_darwin_arm64.tar.gz"])
	}
}

func TestGitHubCLIUpstreamFetchChecksumsErrors(t *testing.T) {
	t.Run("404", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.NotFound(w, nil)
		}))
		defer srv.Close()

		u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{DownloadBase: srv.URL + "/download"})
		_, err := u.FetchChecksums(context.Background(), "v9.9.9")
		if !errors.Is(err, domain.ErrCLINotFound) {
			t.Fatalf("err = %v, want ErrCLINotFound", err)
		}
	})

	t.Run("non-200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()

		u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{DownloadBase: srv.URL + "/download"})
		_, err := u.FetchChecksums(context.Background(), "v0.21.8")
		if !errors.Is(err, domain.ErrCLIUpstreamUnavailable) {
			t.Fatalf("err = %v, want ErrCLIUpstreamUnavailable", err)
		}
	})

	t.Run("network error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		url := srv.URL
		srv.Close()

		u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{DownloadBase: url + "/download"})
		_, err := u.FetchChecksums(context.Background(), "v0.21.8")
		if !errors.Is(err, domain.ErrCLIUpstreamUnavailable) {
			t.Fatalf("err = %v, want ErrCLIUpstreamUnavailable", err)
		}
	})
}

func TestGitHubCLIUpstreamFetchTarball(t *testing.T) {
	t.Run("token never sent to download host", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/download/v0.21.8/dagger_v0.21.8_linux_amd64.tar.gz" {
				t.Errorf("unexpected path %q", r.URL.Path)
			}
			// The token is for the releases API rate limit only; it must not
			// leak to the (possibly mirrored) download host.
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("Authorization = %q, want empty (token must not reach download_base)", got)
			}
			if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
				t.Errorf("Accept = %q", got)
			}
			_, _ = io.WriteString(w, "tarball-bytes")
		}))
		defer srv.Close()

		u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{
			DownloadBase: srv.URL + "/download",
			GitHubToken:  "sekrit",
		})
		rc, size, err := u.FetchTarball(context.Background(), "v0.21.8", "linux", "amd64")
		if err != nil {
			t.Fatalf("FetchTarball: %v", err)
		}
		defer func() { _ = rc.Close() }()
		body, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(body) != "tarball-bytes" {
			t.Fatalf("body = %q", body)
		}
		if size != int64(len("tarball-bytes")) {
			t.Fatalf("size = %d, want %d", size, len("tarball-bytes"))
		}
	})

	t.Run("no token omits authorization", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("Authorization = %q, want empty", got)
			}
			_, _ = io.WriteString(w, "data")
		}))
		defer srv.Close()

		u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{DownloadBase: srv.URL + "/download"})
		rc, _, err := u.FetchTarball(context.Background(), "v0.21.8", "linux", "amd64")
		if err != nil {
			t.Fatalf("FetchTarball: %v", err)
		}
		_ = rc.Close()
	})
}

func TestGitHubCLIUpstreamFetchTarballErrors(t *testing.T) {
	t.Run("404", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.NotFound(w, nil)
		}))
		defer srv.Close()

		u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{DownloadBase: srv.URL + "/download"})
		_, _, err := u.FetchTarball(context.Background(), "v9.9.9", "linux", "amd64")
		if !errors.Is(err, domain.ErrCLINotFound) {
			t.Fatalf("err = %v, want ErrCLINotFound", err)
		}
	})

	t.Run("non-200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()

		u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{DownloadBase: srv.URL + "/download"})
		_, _, err := u.FetchTarball(context.Background(), "v0.21.8", "linux", "amd64")
		if !errors.Is(err, domain.ErrCLIUpstreamUnavailable) {
			t.Fatalf("err = %v, want ErrCLIUpstreamUnavailable", err)
		}
	})

	t.Run("network error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		url := srv.URL
		srv.Close()

		u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{DownloadBase: url + "/download"})
		_, _, err := u.FetchTarball(context.Background(), "v0.21.8", "linux", "amd64")
		if !errors.Is(err, domain.ErrCLIUpstreamUnavailable) {
			t.Fatalf("err = %v, want ErrCLIUpstreamUnavailable", err)
		}
	})
}

func TestGitHubCLIUpstreamInvalidURLs(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{ReleasesURL: "://bad"})
		_, err := u.List(context.Background())
		if !errors.Is(err, domain.ErrCLIUpstreamUnavailable) {
			t.Fatalf("err = %v, want ErrCLIUpstreamUnavailable", err)
		}
	})
	t.Run("checksums", func(t *testing.T) {
		u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{DownloadBase: "://bad"})
		_, err := u.FetchChecksums(context.Background(), "v0.21.8")
		if !errors.Is(err, domain.ErrCLIUpstreamUnavailable) {
			t.Fatalf("err = %v, want ErrCLIUpstreamUnavailable", err)
		}
	})
	t.Run("tarball", func(t *testing.T) {
		u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{DownloadBase: "://bad"})
		_, _, err := u.FetchTarball(context.Background(), "v0.21.8", "linux", "amd64")
		if !errors.Is(err, domain.ErrCLIUpstreamUnavailable) {
			t.Fatalf("err = %v, want ErrCLIUpstreamUnavailable", err)
		}
	})
}

func TestNewGitHubCLIUpstreamTimeoutFallback(t *testing.T) {
	u := NewGitHubCLIUpstream(GitHubCLIUpstreamConfig{})
	if u.client == nil {
		t.Fatal("nil client")
	}
	if u.client.Timeout == 0 {
		t.Fatal("expected a non-zero default timeout")
	}
}

func TestParseNextLink(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "next present",
			in:   `<https://api.github.com/releases?page=2>; rel="next", <https://api.github.com/releases?page=3>; rel="last"`,
			want: "https://api.github.com/releases?page=2",
		},
		{
			name: "next absent",
			in:   `<https://api.github.com/releases?page=2>; rel="last"`,
			want: "",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "malformed angle brackets",
			in:   `https://api.github.com/releases?page=2; rel="next"`,
			want: "",
		},
		{
			name: "single segment",
			in:   `<https://api.github.com/releases?page=2>`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseNextLink(tt.in); got != tt.want {
				t.Fatalf("parseNextLink(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseChecksums(t *testing.T) {
	in := "53e226c7 dagger_v0.21.8_linux_amd64.tar.gz\n" +
		"00aa11bb *dagger_v0.21.8_darwin_arm64.tar.gz\n" +
		"abc *\n" +
		"malformed-line\n" +
		"\n" +
		"   \n" +
		"onlydigest\n" +
		"  \n"
	got := parseChecksums(in)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %v", len(got), got)
	}
	if got["dagger_v0.21.8_linux_amd64.tar.gz"] != "53e226c7" {
		t.Fatalf("linux = %q", got["dagger_v0.21.8_linux_amd64.tar.gz"])
	}
	if got["dagger_v0.21.8_darwin_arm64.tar.gz"] != "00aa11bb" {
		t.Fatalf("darwin = %q", got["dagger_v0.21.8_darwin_arm64.tar.gz"])
	}
}
