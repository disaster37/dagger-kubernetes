package repository

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
)

// mockS3ObjectStore is an in-memory S3 object store for tests, implementing
// the s3ObjectAPI slice of the minio client. Not-found errors carry the
// NoSuchKey code, matching a real S3 endpoint's behavior.
type mockS3ObjectStore struct {
	mu      sync.Mutex
	objects map[string]mockS3Object
	nowFunc func() time.Time

	putErr error // injected PutObject failure
}

type mockS3Object struct {
	data         []byte
	lastModified time.Time
}

func newMockS3ObjectStore() *mockS3ObjectStore {
	return &mockS3ObjectStore{
		objects: map[string]mockS3Object{},
		nowFunc: time.Now,
	}
}

func (m *mockS3ObjectStore) PutObject(_ context.Context, _, objectName string, reader io.Reader, objectSize int64, _ minio.PutObjectOptions) (minio.UploadInfo, error) { //nolint:gocritic // hugeParam: signature must match the minio-go API
	if m.putErr != nil {
		return minio.UploadInfo{}, m.putErr
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return minio.UploadInfo{}, err
	}
	if objectSize >= 0 && int64(len(data)) != objectSize {
		return minio.UploadInfo{}, errMockSizeMismatch{got: int64(len(data)), want: objectSize}
	}
	m.mu.Lock()
	m.objects[objectName] = mockS3Object{data: data, lastModified: m.nowFunc()}
	m.mu.Unlock()
	return minio.UploadInfo{Key: objectName, Size: int64(len(data))}, nil
}

func (m *mockS3ObjectStore) GetObject(_ context.Context, _, objectName string, _ minio.GetObjectOptions) (io.ReadCloser, error) { //nolint:gocritic // hugeParam: signature must match the minio-go API
	m.mu.Lock()
	obj, ok := m.objects[objectName]
	m.mu.Unlock()
	if !ok {
		return nil, errMockNoSuchKey(objectName)
	}
	return io.NopCloser(bytes.NewReader(obj.data)), nil
}

func (m *mockS3ObjectStore) StatObject(_ context.Context, _, objectName string, _ minio.StatObjectOptions) (minio.ObjectInfo, error) { //nolint:gocritic // hugeParam: signature must match the minio-go API
	m.mu.Lock()
	obj, ok := m.objects[objectName]
	m.mu.Unlock()
	if !ok {
		return minio.ObjectInfo{}, errMockNoSuchKey(objectName)
	}
	return minio.ObjectInfo{Key: objectName, Size: int64(len(obj.data)), LastModified: obj.lastModified}, nil
}

func (m *mockS3ObjectStore) ListObjects(_ context.Context, _ string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo { //nolint:gocritic // hugeParam: signature must match the minio-go API
	ch := make(chan minio.ObjectInfo)
	go func() {
		defer close(ch)
		m.mu.Lock()
		keys := make([]string, 0, len(m.objects))
		for k := range m.objects {
			if strings.HasPrefix(k, opts.Prefix) {
				keys = append(keys, k)
			}
		}
		m.mu.Unlock()
		sort.Strings(keys)
		for _, k := range keys {
			m.mu.Lock()
			obj := m.objects[k]
			m.mu.Unlock()
			ch <- minio.ObjectInfo{Key: k, Size: int64(len(obj.data)), LastModified: obj.lastModified}
		}
	}()
	return ch
}

// keys returns the sorted list of all object keys.
func (m *mockS3ObjectStore) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// setObject plants an object with an explicit LastModified.
func (m *mockS3ObjectStore) setObject(key string, data []byte, lastModified time.Time) {
	m.mu.Lock()
	m.objects[key] = mockS3Object{data: data, lastModified: lastModified}
	m.mu.Unlock()
}

type errMockSizeMismatch struct {
	got  int64
	want int64
}

func (e errMockSizeMismatch) Error() string {
	return "size mismatch"
}

// errMockNoSuchKey builds the error a real endpoint produces for a missing
// object (minio.ErrorResponse with Code NoSuchKey).
func errMockNoSuchKey(key string) error {
	return minio.ErrorResponse{
		Code:       "NoSuchKey",
		Message:    "The specified key does not exist.",
		Key:        key,
		StatusCode: 404,
	}
}
