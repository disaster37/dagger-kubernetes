package repository

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3RemoteCacheStore stores ValueBundle JSON and blobs in S3 and issues
// presigned PUT/GET URLs. The signing client may use a different endpoint than
// the storage client so presigned URLs match the Host engine pods dial.
type S3RemoteCacheStore struct {
	client     *minio.Client
	signer     *minio.Client
	bucket     string
	prefix     string
	presignTTL time.Duration
}

// NewS3RemoteCacheStore builds the store. engineEndpoint, when non-empty, is
// used for presigning (e.g. an in-cluster MinIO service host).
func NewS3RemoteCacheStore(endpoint, region, accessKey, secretKey string, useSSL bool, engineEndpoint, bucket, prefix string, presignTTL time.Duration) (*S3RemoteCacheStore, error) {
	client, err := NewS3Client(endpoint, region, accessKey, secretKey, useSSL)
	if err != nil {
		return nil, err
	}
	signer := client
	if engineEndpoint != "" && engineEndpoint != endpoint {
		signer, err = minio.New(engineEndpoint, &minio.Options{
			Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
			Secure: useSSL,
			Region: region,
		})
		if err != nil {
			return nil, fmt.Errorf("create remote cache signing client: %w", err)
		}
	}
	if presignTTL <= 0 {
		presignTTL = time.Hour
	}
	return &S3RemoteCacheStore{client: client, signer: signer, bucket: bucket, prefix: strings.Trim(prefix, "/"), presignTTL: presignTTL}, nil
}

func (s *S3RemoteCacheStore) bundleKey(namespace, digest string) string {
	return fmt.Sprintf("%s/%s/bundles/%s.json", s.prefix, namespace, digest)
}

func (s *S3RemoteCacheStore) blobKey(namespace, digest string) string {
	return fmt.Sprintf("%s/%s/blobs/%s", s.prefix, namespace, digest)
}

// PutBundle stores a ValueBundle JSON document.
func (s *S3RemoteCacheStore) PutBundle(ctx context.Context, namespace, digest string, bundle []byte) (string, error) {
	key := s.bundleKey(namespace, digest)
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(bundle), int64(len(bundle)), minio.PutObjectOptions{ContentType: "application/json"})
	if err != nil {
		return "", fmt.Errorf("put remote cache bundle: %w", err)
	}
	return key, nil
}

// GetBundle reads a ValueBundle JSON document.
func (s *S3RemoteCacheStore) GetBundle(ctx context.Context, namespace, digest string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.bundleKey(namespace, digest), minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get remote cache bundle: %w", err)
	}
	defer func() { _ = obj.Close() }()
	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, fmt.Errorf("read remote cache bundle: %w", err)
	}
	return data, nil
}

// PresignPut returns a presigned PUT URL for a blob.
func (s *S3RemoteCacheStore) PresignPut(ctx context.Context, namespace, digest string) (string, error) {
	u, err := s.signer.PresignedPutObject(ctx, s.bucket, s.blobKey(namespace, digest), s.presignTTL)
	if err != nil {
		return "", fmt.Errorf("presign remote cache put: %w", err)
	}
	return u.String(), nil
}

// PresignGet returns a presigned GET URL for a blob.
func (s *S3RemoteCacheStore) PresignGet(ctx context.Context, namespace, digest string) (string, error) {
	u, err := s.signer.PresignedGetObject(ctx, s.bucket, s.blobKey(namespace, digest), s.presignTTL, url.Values{})
	if err != nil {
		return "", fmt.Errorf("presign remote cache get: %w", err)
	}
	return u.String(), nil
}

// DeleteBundle removes a bundle object.
func (s *S3RemoteCacheStore) DeleteBundle(ctx context.Context, namespace, digest string) error {
	return s.client.RemoveObject(ctx, s.bucket, s.bundleKey(namespace, digest), minio.RemoveObjectOptions{})
}

// DeleteNamespace removes every object under a namespace prefix.
func (s *S3RemoteCacheStore) DeleteNamespace(ctx context.Context, namespace string) error {
	prefix := fmt.Sprintf("%s/%s/", s.prefix, namespace)
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return fmt.Errorf("list remote cache namespace: %w", obj.Err)
		}
		if err := s.client.RemoveObject(ctx, s.bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
			return fmt.Errorf("delete remote cache object %s: %w", obj.Key, err)
		}
	}
	return nil
}
