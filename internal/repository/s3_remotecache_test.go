package repository

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestPresignGetRangeServes206 stands up an in-process range-aware object
// server and verifies the presigned URL + Range header return the exact member
// slice with HTTP 206 Partial Content.
func TestPresignGetRangeServes206(t *testing.T) {
	payload := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHdr := r.Header.Get("Range")
		if rangeHdr == "" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload)
			return
		}
		spec := strings.TrimPrefix(rangeHdr, "bytes=")
		parts := strings.SplitN(spec, "-", 2)
		start, _ := strconv.Atoi(parts[0])
		end, _ := strconv.Atoi(parts[1])
		if start < 0 || end >= len(payload) || start > end {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	store, err := NewS3RemoteCacheStore(u.Host, "us-east-1", "key", "secret", false, "", "bucket", "prefix", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	packKey := store.packKey("acme", "packdigest")
	urlStr, rangeHdr, err := store.PresignGetRange(context.Background(), packKey, 10, 5)
	if err != nil {
		t.Fatal(err)
	}
	if rangeHdr != "bytes=10-14" {
		t.Fatalf("range header = %q", rangeHdr)
	}
	req, err := http.NewRequest(http.MethodGet, urlStr, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", rangeHdr)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "abcde" {
		t.Fatalf("body = %q", body)
	}
}

func TestPresignGetRangeRejectsInvalid(t *testing.T) {
	store := &S3RemoteCacheStore{}
	if _, _, err := store.PresignGetRange(context.Background(), "k", -1, 1); err == nil {
		t.Fatal("negative offset must fail")
	}
	if _, _, err := store.PresignGetRange(context.Background(), "k", 0, 0); err == nil {
		t.Fatal("zero length must fail")
	}
}

func TestPackKeyFormat(t *testing.T) {
	store := &S3RemoteCacheStore{prefix: "remote-cache"}
	got := store.packKey("acme", "abc")
	want := "remote-cache/acme/packs/abc.pack"
	if got != want {
		t.Fatalf("packKey = %q, want %q", got, want)
	}
}
