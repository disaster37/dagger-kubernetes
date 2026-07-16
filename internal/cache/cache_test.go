package cache

import (
	"encoding/json"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/version"
)

func mustVersion(t *testing.T) *version.Version {
	t.Helper()
	v, err := version.Parse("v0.21.4")
	if err != nil {
		t.Fatalf("parse v0.21.4: %v", err)
	}
	return v
}

func TestCacheRefForVersion(t *testing.T) {
	b := &Backend{Registry: "cache.reg/dagger-cache"}
	ref := b.CacheRefForVersion(mustVersion(t))
	if ref != "cache.reg/dagger-cache:v0-21-4" {
		t.Fatalf("ref = %q", ref)
	}
}

func TestBuildCacheConfigRegistry(t *testing.T) {
	b := &Backend{Type: "registry", Registry: "cache.reg/dagger-cache"}
	got := b.BuildCacheConfig(mustVersion(t), "max")
	want := "type=registry,ref=cache.reg/dagger-cache:v0-21-4,mode=max"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildCacheConfigRegistryPublicHost(t *testing.T) {
	b := &Backend{Type: "registry", Registry: "cache.reg/dagger-cache", PublicHost: "cache.example.com"}
	got := b.BuildCacheConfig(mustVersion(t), "max")
	want := "type=registry,ref=cache.example.com/dagger-cache:v0-21-4,mode=max"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildCacheConfigS3(t *testing.T) {
	b := &Backend{Type: "s3", S3: S3Ref{Bucket: "bkt", Region: "us-east-1"}}
	got := b.BuildCacheConfig(mustVersion(t), "max")
	want := "type=s3,bucket=bkt,region=us-east-1,mode=max"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildCacheConfigUnknown(t *testing.T) {
	b := &Backend{Type: "unknown"}
	if got := b.BuildCacheConfig(mustVersion(t), "max"); got != "" {
		t.Fatalf("expected empty for unknown backend, got %q", got)
	}
}

func TestBuildEngineJSON(t *testing.T) {
	b := &Backend{Registry: "cache.reg/dagger-cache"}
	data, err := b.BuildEngineJSON("tok")
	if err != nil {
		t.Fatalf("BuildEngineJSON: %v", err)
	}

	var ej EngineJSON
	if err := json.Unmarshal(data, &ej); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ej.Registries["cache.reg/dagger-cache"].Auth != "tok" {
		t.Fatalf("auth = %q", ej.Registries["cache.reg/dagger-cache"].Auth)
	}
}

func TestBuildEngineJSONPublicHost(t *testing.T) {
	b := &Backend{Registry: "cache.reg/dagger-cache", PublicHost: "cache.example.com"}
	data, err := b.BuildEngineJSON("tok")
	if err != nil {
		t.Fatalf("BuildEngineJSON: %v", err)
	}

	var ej EngineJSON
	if err := json.Unmarshal(data, &ej); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := ej.Registries["cache.example.com"]; !ok {
		t.Fatalf("expected registry key cache.example.com, got %v", ej.Registries)
	}
}
