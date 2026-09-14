package service

import (
	"testing"
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
