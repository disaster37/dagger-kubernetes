package service

import (
	"fmt"
	"strings"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

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

func (b *Cache) CacheRefForVersion(v *domain.Version) string {
	return fmt.Sprintf("%s:%s", b.Registry, v.CacheRefTag())
}

func (b *Cache) BuildCacheConfig(v *domain.Version, mode string) string {
	switch b.Type {
	case "registry":
		ref := b.CacheRefForVersion(v)
		if b.PublicHost != "" {
			// Rewrite the registry host, keeping everything after the first "/".
			path := ref
			if _, rest, ok := strings.Cut(ref, "/"); ok {
				path = rest
			}
			ref = fmt.Sprintf("%s/%s", b.PublicHost, path)
		}
		return fmt.Sprintf("type=registry,ref=%s,mode=%s", ref, mode)
	case "s3":
		return fmt.Sprintf("type=s3,bucket=%s,region=%s,mode=%s", b.S3.Bucket, b.S3.Region, mode)
	default:
		return ""
	}
}
