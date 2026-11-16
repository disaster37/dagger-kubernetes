package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// S3ArtifactStore stores workflow artifacts with SHA-256 verification.
type S3ArtifactStore struct {
	client *minio.Client
	bucket string
	prefix string
}

var _ domain.ArtifactStore = (*S3ArtifactStore)(nil)

// NewS3ArtifactStore returns an S3-backed artifact store.
func NewS3ArtifactStore(client *minio.Client, bucket, prefix string) *S3ArtifactStore {
	return &S3ArtifactStore{client: client, bucket: bucket, prefix: strings.Trim(prefix, "/")}
}

func (s *S3ArtifactStore) key(orgID, projectID, runID, name string) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", s.prefix, orgID, projectID, runID, name)
}

// Put stores data and returns its key. The SHA-256 is stored as object
// metadata and verified on Get.
func (s *S3ArtifactStore) Put(ctx context.Context, orgID, projectID, runID, name string, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	key := s.key(orgID, projectID, runID, name)
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType:  "application/octet-stream",
		UserMetadata: map[string]string{"sha256": hex.EncodeToString(sum[:])},
	})
	if err != nil {
		return "", fmt.Errorf("put artifact: %w", err)
	}
	return key, nil
}

// Get reads and verifies an artifact.
func (s *S3ArtifactStore) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get artifact: %w", err)
	}
	defer func() { _ = obj.Close() }()
	info, err := obj.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat artifact: %w", err)
	}
	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, fmt.Errorf("read artifact: %w", err)
	}
	if want := info.Metadata.Get("X-Amz-Meta-Sha256"); want != "" {
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != want {
			return nil, fmt.Errorf("artifact checksum mismatch for %s", key)
		}
	}
	return data, nil
}

// List returns artifact keys for a run.
func (s *S3ArtifactStore) List(ctx context.Context, orgID, projectID, runID string) ([]string, error) {
	prefix := fmt.Sprintf("%s/%s/%s/%s/", s.prefix, orgID, projectID, runID)
	var keys []string
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list artifacts: %w", obj.Err)
		}
		keys = append(keys, obj.Key)
	}
	return keys, nil
}

// DeleteRun removes every artifact for a run.
func (s *S3ArtifactStore) DeleteRun(ctx context.Context, orgID, projectID, runID string) error {
	keys, err := s.List(ctx, orgID, projectID, runID)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
			return fmt.Errorf("delete artifact %s: %w", key, err)
		}
	}
	return nil
}
