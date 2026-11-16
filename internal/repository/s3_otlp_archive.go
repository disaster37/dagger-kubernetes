package repository

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/minio/minio-go/v7"
	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// S3OTLPArchive stores raw OTLP chunks under
// <prefix>/<traceID>/<signal>/<seq>.bin.
type S3OTLPArchive struct {
	client *minio.Client
	bucket string
	prefix string
	logger *logrus.Logger
}

var _ domain.OTLPArchive = (*S3OTLPArchive)(nil)

// NewS3OTLPArchive returns an S3-backed OTLP archive.
func NewS3OTLPArchive(client *minio.Client, bucket, prefix string, logger *logrus.Logger) *S3OTLPArchive {
	return &S3OTLPArchive{client: client, bucket: bucket, prefix: strings.Trim(prefix, "/"), logger: logger}
}

func (a *S3OTLPArchive) key(traceID, signal string, seq uint64) string {
	return fmt.Sprintf("%s/%s/%s/%020d.bin", a.prefix, traceID, signal, seq)
}

func (a *S3OTLPArchive) Append(ctx context.Context, traceID, signal string, seq uint64, payload []byte) error {
	_, err := a.client.PutObject(ctx, a.bucket, a.key(traceID, signal, seq), bytes.NewReader(payload), int64(len(payload)), minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return fmt.Errorf("archive otlp chunk: %w", err)
	}
	return nil
}

func (a *S3OTLPArchive) ChunksAfter(ctx context.Context, traceID, signal string, fromSeq uint64) ([]domain.OTLPChunk, error) {
	prefix := fmt.Sprintf("%s/%s/%s/", a.prefix, traceID, signal)
	opts := minio.ListObjectsOptions{Prefix: prefix, Recursive: true}
	if fromSeq > 0 {
		opts.StartAfter = a.key(traceID, signal, fromSeq-1)
	}
	var keys []string
	for obj := range a.client.ListObjects(ctx, a.bucket, opts) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list otlp archive: %w", obj.Err)
		}
		keys = append(keys, obj.Key)
	}
	sort.Strings(keys)
	out := make([]domain.OTLPChunk, 0, len(keys))
	for _, key := range keys {
		seq, err := seqFromKey(key)
		if err != nil {
			continue
		}
		if seq < fromSeq {
			continue
		}
		obj, err := a.client.GetObject(ctx, a.bucket, key, minio.GetObjectOptions{})
		if err != nil {
			return nil, fmt.Errorf("get otlp chunk: %w", err)
		}
		data, err := io.ReadAll(obj)
		_ = obj.Close()
		if err != nil {
			return nil, fmt.Errorf("read otlp chunk: %w", err)
		}
		out = append(out, domain.OTLPChunk{Seq: seq, Payload: data})
	}
	return out, nil
}

// seqFromKey parses the %020d sequence number from an archive object key.
func seqFromKey(key string) (uint64, error) {
	base := key[strings.LastIndex(key, "/")+1:]
	base = strings.TrimSuffix(base, ".bin")
	return strconv.ParseUint(base, 10, 64)
}

func (a *S3OTLPArchive) DeleteTrace(ctx context.Context, traceID string) error {
	prefix := fmt.Sprintf("%s/%s/", a.prefix, traceID)
	var objects []minio.ObjectInfo
	for obj := range a.client.ListObjects(ctx, a.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return fmt.Errorf("list otlp archive for delete: %w", obj.Err)
		}
		objects = append(objects, obj)
	}
	if len(objects) == 0 {
		return nil
	}
	for i := range objects {
		obj := &objects[i]
		if err := a.client.RemoveObject(ctx, a.bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
			return fmt.Errorf("delete otlp chunk %s: %w", obj.Key, err)
		}
	}
	return nil
}

// MemoryOTLPArchive is an in-memory OTLPArchive for tests and single-node dev.
type MemoryOTLPArchive struct {
	mu     sync.RWMutex
	chunks map[string][]domain.OTLPChunk
}

// NewMemoryOTLPArchive returns an empty in-memory archive.
func NewMemoryOTLPArchive() *MemoryOTLPArchive {
	return &MemoryOTLPArchive{chunks: make(map[string][]domain.OTLPChunk)}
}

var _ domain.OTLPArchive = (*MemoryOTLPArchive)(nil)

func (a *MemoryOTLPArchive) Append(_ context.Context, traceID, signal string, seq uint64, payload []byte) error {
	key := fmt.Sprintf("%s/%s", traceID, signal)
	a.mu.Lock()
	defer a.mu.Unlock()
	chunks := a.chunks[key]
	idx := sort.Search(len(chunks), func(i int) bool { return chunks[i].Seq >= seq })
	if idx < len(chunks) && chunks[idx].Seq == seq {
		chunks[idx].Payload = append([]byte(nil), payload...)
	} else {
		chunks = append(chunks, domain.OTLPChunk{})
		copy(chunks[idx+1:], chunks[idx:])
		chunks[idx] = domain.OTLPChunk{Seq: seq, Payload: append([]byte(nil), payload...)}
	}
	a.chunks[key] = chunks
	return nil
}

func (a *MemoryOTLPArchive) ChunksAfter(_ context.Context, traceID, signal string, fromSeq uint64) ([]domain.OTLPChunk, error) {
	key := fmt.Sprintf("%s/%s", traceID, signal)
	a.mu.RLock()
	defer a.mu.RUnlock()
	chunks := a.chunks[key]
	idx := sort.Search(len(chunks), func(i int) bool { return chunks[i].Seq >= fromSeq })
	out := make([]domain.OTLPChunk, 0, len(chunks)-idx)
	for _, c := range chunks[idx:] {
		out = append(out, domain.OTLPChunk{Seq: c.Seq, Payload: append([]byte(nil), c.Payload...)})
	}
	return out, nil
}

func (a *MemoryOTLPArchive) DeleteTrace(_ context.Context, traceID string) error {
	prefix := fmt.Sprintf("%s/", traceID)
	a.mu.Lock()
	defer a.mu.Unlock()
	for key := range a.chunks {
		if strings.HasPrefix(key, prefix) {
			delete(a.chunks, key)
		}
	}
	return nil
}
