package service

import (
	"fmt"
	"strings"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const cacheTag = "cache"

type Cache struct {
	Type       string
	Registry   string
	PublicHost string
	S3         domain.S3Ref
}

var _ domain.CacheBackend = (*Cache)(nil)

func (b *Cache) BackendType() string {
	return b.Type
}

func (b *Cache) RegistryHost() string {
	return b.Registry
}

// CacheRef returns "<host>/<repo>:cache", rewriting the host to PublicHost
// when set (mirrors the old BuildCacheConfig host rewrite).
func (b *Cache) CacheRef() string {
	if b.PublicHost != "" {
		_, rest, ok := strings.Cut(b.Registry, "/")
		if ok {
			return fmt.Sprintf("%s/%s:%s", b.PublicHost, rest, cacheTag)
		}
		return fmt.Sprintf("%s:%s", b.PublicHost, cacheTag)
	}
	return fmt.Sprintf("%s:%s", b.Registry, cacheTag)
}

// BuildCacheConfig no longer takes a *domain.Version.
func (b *Cache) BuildCacheConfig(mode string) string {
	switch b.Type {
	case "registry":
		ref := b.CacheRef()
		return fmt.Sprintf("type=registry,ref=%s,mode=%s", ref, mode)
	case "s3":
		return fmt.Sprintf("type=s3,bucket=%s,region=%s,mode=%s", b.S3.Bucket, b.S3.Region, mode)
	default:
		return ""
	}
}
