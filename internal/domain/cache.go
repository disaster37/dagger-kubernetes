package domain

type S3Ref struct {
	Bucket string
	Region string
}

type CacheBackend interface {
	BackendType() string
	RegistryHost() string
}
