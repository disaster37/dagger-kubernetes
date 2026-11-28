package repository

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
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

// orgPrefix is the canonical tenant prefix: orgs/<org_id>/<remote.prefix>/.
func (s *S3RemoteCacheStore) orgPrefix(namespace string) string {
	if s.prefix == "" {
		return fmt.Sprintf("orgs/%s", namespace)
	}
	return fmt.Sprintf("orgs/%s/%s", namespace, s.prefix)
}

func (s *S3RemoteCacheStore) bundleKey(namespace, digest string) string {
	return fmt.Sprintf("%s/bundles/%s.json", s.orgPrefix(namespace), digest)
}

func (s *S3RemoteCacheStore) blobKey(namespace, digest string) string {
	return fmt.Sprintf("%s/blobs/%s", s.orgPrefix(namespace), digest)
}

func (s *S3RemoteCacheStore) packKey(namespace, packDigest string) string {
	return fmt.Sprintf("%s/packs/%s.pack", s.orgPrefix(namespace), packDigest)
}

// PackKey returns the S3 key for a pack object.
func (s *S3RemoteCacheStore) PackKey(namespace, packDigest string) string {
	return s.packKey(namespace, packDigest)
}

func (s *S3RemoteCacheStore) checkpointKey(namespace string) string {
	return fmt.Sprintf("%s/index/checkpoint.json", s.orgPrefix(namespace))
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

// GetBundleByKey reads a bundle by its stored object key.
func (s *S3RemoteCacheStore) GetBundleByKey(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
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

// PresignPackPut returns a presigned PUT URL for a pack object.
func (s *S3RemoteCacheStore) PresignPackPut(ctx context.Context, namespace, packDigest string) (string, error) {
	u, err := s.signer.PresignedPutObject(ctx, s.bucket, s.packKey(namespace, packDigest), s.presignTTL)
	if err != nil {
		return "", fmt.Errorf("presign remote cache pack put: %w", err)
	}
	return u.String(), nil
}

// PutPack stores a pack object directly.
func (s *S3RemoteCacheStore) PutPack(ctx context.Context, namespace, packDigest string, data []byte) (string, error) {
	key := s.packKey(namespace, packDigest)
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return "", fmt.Errorf("put remote cache pack: %w", err)
	}
	return key, nil
}

// PresignGetRange generates a presigned GET URL cryptographically bound to
// HTTP Range: bytes=<offset>-<offset+length-1> so S3/MinIO serves 206.
func (s *S3RemoteCacheStore) PresignGetRange(ctx context.Context, packKey string, offset, length int64) (urlStr, rangeHeader string, err error) {
	if offset < 0 || length <= 0 {
		return "", "", fmt.Errorf("invalid pack byte range")
	}
	rangeHeader = fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	u, err := s.signer.PresignHeader(ctx, http.MethodGet, s.bucket, packKey, s.presignTTL, url.Values{}, http.Header{"Range": []string{rangeHeader}})
	if err != nil {
		return "", "", fmt.Errorf("presign remote cache range get: %w", err)
	}
	return u.String(), rangeHeader, nil
}

// PresignBundleGet returns a presigned GET URL for a bundle object.
func (s *S3RemoteCacheStore) PresignBundleGet(ctx context.Context, namespace, digest string) (string, error) {
	u, err := s.signer.PresignedGetObject(ctx, s.bucket, s.bundleKey(namespace, digest), s.presignTTL, url.Values{})
	if err != nil {
		return "", fmt.Errorf("presign remote cache bundle get: %w", err)
	}
	return u.String(), nil
}

// PutIndexCheckpoint stores the serialized bbolt index snapshot.
func (s *S3RemoteCacheStore) PutIndexCheckpoint(ctx context.Context, namespace string, data []byte) error {
	_, err := s.client.PutObject(ctx, s.bucket, s.checkpointKey(namespace), bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: "application/json"})
	if err != nil {
		return fmt.Errorf("put remote cache index checkpoint: %w", err)
	}
	return nil
}

// GetIndexCheckpoint reads the serialized bbolt index snapshot.
func (s *S3RemoteCacheStore) GetIndexCheckpoint(ctx context.Context, namespace string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.checkpointKey(namespace), minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get remote cache index checkpoint: %w", err)
	}
	defer func() { _ = obj.Close() }()
	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, fmt.Errorf("read remote cache index checkpoint: %w", err)
	}
	return data, nil
}

// ListOrgPrefixes returns the distinct organization IDs with objects under
// the canonical orgs/ prefix.
func (s *S3RemoteCacheStore) ListOrgPrefixes(ctx context.Context) ([]string, error) {
	seen := make(map[string]struct{})
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: "orgs/", Recursive: true}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list org prefixes: %w", obj.Err)
		}
		rest := strings.TrimPrefix(obj.Key, "orgs/")
		seg, _, _ := strings.Cut(rest, "/")
		if seg != "" {
			seen[seg] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// DeleteBundle removes a bundle object.
func (s *S3RemoteCacheStore) DeleteBundle(ctx context.Context, namespace, digest string) error {
	return s.client.RemoveObject(ctx, s.bucket, s.bundleKey(namespace, digest), minio.RemoveObjectOptions{})
}

// DeleteNamespace removes every object under a namespace prefix.
func (s *S3RemoteCacheStore) DeleteNamespace(ctx context.Context, namespace string) error {
	prefix := fmt.Sprintf("%s/", s.orgPrefix(namespace))
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
