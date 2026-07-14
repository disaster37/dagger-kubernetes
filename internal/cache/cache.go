package cache

import (
	"encoding/json"
	"fmt"

	"github.com/disaster/dagger-kubernetes/internal/version"
)

type Backend struct {
	Type     string
	Registry string
	S3       S3Ref
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
		return fmt.Sprintf("type=registry,ref=%s,mode=%s", b.CacheRefForVersion(v), mode)
	case "s3":
		return fmt.Sprintf("type=s3,bucket=%s,region=%s,mode=%s", b.S3.Bucket, b.S3.Region, mode)
	default:
		return ""
	}
}

func (b *Backend) BuildEngineJSON(authToken string) ([]byte, error) {
	engineJSON := EngineJSON{
		Registries: map[string]RegistryAuthEntry{
			b.Registry: {Auth: authToken},
		},
	}
	return json.Marshal(engineJSON)
}
