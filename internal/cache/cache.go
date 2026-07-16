package cache

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/disaster/dagger-kubernetes/internal/version"
)

type Backend struct {
	Type       string
	Registry   string
	PublicHost string
	S3         S3Ref
}

type S3Ref struct {
	Bucket string
	Region string
}

type RegistryAuthEntry struct {
	Auth string `json:"auth"`
}

type EngineJSON struct {
	Registries map[string]RegistryAuthEntry `json:"registries"`
}

func (b *Backend) CacheRefForVersion(v *version.Version) string {
	return fmt.Sprintf("%s:%s", b.Registry, v.CacheRefTag())
}

func (b *Backend) BuildCacheConfig(v *version.Version, mode string) string {
	switch b.Type {
	case "registry":
		ref := b.CacheRefForVersion(v)
		if b.PublicHost != "" {
			parts := strings.SplitN(ref, "/", 2)
			if len(parts) == 2 {
				ref = fmt.Sprintf("%s/%s", b.PublicHost, parts[1])
			} else {
				ref = fmt.Sprintf("%s/%s", b.PublicHost, ref)
			}
		}
		return fmt.Sprintf("type=registry,ref=%s,mode=%s", ref, mode)
	case "s3":
		return fmt.Sprintf("type=s3,bucket=%s,region=%s,mode=%s", b.S3.Bucket, b.S3.Region, mode)
	default:
		return ""
	}
}

func (b *Backend) BuildEngineJSON(authToken string) ([]byte, error) {
	registryHost := b.Registry
	if b.PublicHost != "" {
		registryHost = b.PublicHost
	}

	engineJSON := EngineJSON{
		Registries: map[string]RegistryAuthEntry{
			registryHost: {Auth: authToken},
		},
	}
	return json.Marshal(engineJSON)
}
