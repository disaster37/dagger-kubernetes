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
// when set. It is exposed on the cache stats payload.
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
