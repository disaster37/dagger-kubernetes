package service

import (
	"strings"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestCacheRef(t *testing.T) {
	b := &Cache{Registry: "cache.reg/dagger-cache"}
	ref := b.CacheRef()
	if ref != "cache.reg/dagger-cache:cache" {
		t.Fatalf("ref = %q, want cache.reg/dagger-cache:cache", ref)
	}
}

func TestCacheRefPublicHost(t *testing.T) {
	b := &Cache{Registry: "cache.reg/dagger-cache", PublicHost: "cache.example.com"}
	ref := b.CacheRef()
	if ref != "cache.example.com/dagger-cache:cache" {
		t.Fatalf("ref = %q, want cache.example.com/dagger-cache:cache", ref)
	}
}

func TestBuildCacheConfigRegistry(t *testing.T) {
	b := &Cache{Type: "registry", Registry: "cache.reg/dagger-cache"}
	got := b.BuildCacheConfig("max")
	want := "type=registry,ref=cache.reg/dagger-cache:cache,mode=max"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildCacheConfigRegistryPublicHost(t *testing.T) {
	b := &Cache{Type: "registry", Registry: "cache.reg/dagger-cache", PublicHost: "cache.example.com"}
	got := b.BuildCacheConfig("max")
	want := "type=registry,ref=cache.example.com/dagger-cache:cache,mode=max"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestBuildCacheConfigRegistryRewritesByDefault pins the production path: when
// PublicHost is set (always true in production wiring), the emitted ref points
// at the Supervisor cache vhost, never the raw registry host.
func TestBuildCacheConfigRegistryRewritesByDefault(t *testing.T) {
	b := &Cache{Type: "registry", Registry: "cache.reg/dagger-cache", PublicHost: "cache.supv.example.com"}
	got := b.BuildCacheConfig("max")
	want := "type=registry,ref=cache.supv.example.com/dagger-cache:cache,mode=max"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "cache.reg") {
		t.Fatalf("emitted ref must not expose the raw registry host: %q", got)
	}
}

func TestBuildCacheConfigS3(t *testing.T) {
	b := &Cache{Type: "s3", S3: domain.S3Ref{Bucket: "bkt", Region: "us-east-1"}}
	got := b.BuildCacheConfig("max")
	want := "type=s3,bucket=bkt,region=us-east-1,mode=max"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildCacheConfigUnknown(t *testing.T) {
	b := &Cache{Type: "unknown"}
	if got := b.BuildCacheConfig("max"); got != "" {
		t.Fatalf("expected empty for unknown backend, got %q", got)
	}
}
