package integration

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// s3FakeObject is one stored object in the in-process S3 fake.
type s3FakeObject struct {
	data         []byte
	lastModified time.Time
}

// s3FakeStore is a minimal S3-compatible backend covering exactly the
// operations the supervisor's S3 stores issue against minio-go: PUT/GET/HEAD/
// DELETE object (path-style) and ListObjectsV2 (prefix, not truncated). Auth
// headers are accepted and ignored, like a pre-signed dev bucket.
type s3FakeStore struct {
	mu      sync.Mutex
	bucket  string
	objects map[string]s3FakeObject
	nowFunc func() time.Time
}

func newS3FakeStore(bucket string) *s3FakeStore {
	return &s3FakeStore{
		bucket:  bucket,
		objects: map[string]s3FakeObject{},
		nowFunc: time.Now,
	}
}

func (s *s3FakeStore) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Path-style addressing: /<bucket>/<key...> or /<bucket>/?list-type=2
		// (minio-go adds a trailing slash for bucket-level operations).
		if strings.TrimSuffix(r.URL.Path, "/") == "/"+s.bucket {
			s.list(w, r)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/"+s.bucket+"/")
		if key == "" || key == r.URL.Path {
			http.NotFound(w, r)
			return
		}

		switch r.Method {
		case http.MethodPut:
			// minio-go signs uploads with the streaming AWS v4 payload
			// (Content-Encoding: aws-chunked); decode the chunk framing like
			// a real S3 endpoint would.
			body := io.Reader(r.Body)
			if isAWSChunked(r) {
				decoded, err := decodeAWSChunked(r.Body)
				if err != nil {
					http.Error(w, fmt.Sprintf("bad chunked body: %v", err), http.StatusBadRequest)
					return
				}
				body = decoded
			}
			plain, err := io.ReadAll(body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			s.mu.Lock()
			s.objects[key] = s3FakeObject{data: plain, lastModified: s.nowFunc()}
			s.mu.Unlock()
			w.Header().Set("ETag", `"fake-etag"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			s.mu.Lock()
			obj, ok := s.objects[key]
			s.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(obj.data)))
			w.Header().Set("Last-Modified", obj.lastModified.UTC().Format(http.TimeFormat))
			_, _ = w.Write(obj.data)
		case http.MethodHead:
			s.mu.Lock()
			obj, ok := s.objects[key]
			s.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(obj.data)))
			w.Header().Set("Last-Modified", obj.lastModified.UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			s.mu.Lock()
			delete(s.objects, key)
			s.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

// isAWSChunked reports whether the request body uses the streaming AWS v4
// chunked payload encoding.
func isAWSChunked(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") ||
		strings.HasPrefix(r.Header.Get("x-amz-content-sha256"), "STREAMING-")
}

// decodeAWSChunked decodes the "<hex-size>;chunk-signature=<sig>\r\n<data>\r\n"
// chunk framing into the plain payload.
func decodeAWSChunked(r io.Reader) (io.Reader, error) {
	var out []byte
	buf := make([]byte, 64*1024)
	for {
		line, err := readCRLFLine(r)
		if err != nil {
			return nil, fmt.Errorf("chunk header: %w", err)
		}
		sizeHex := strings.SplitN(line, ";", 2)[0]
		var size int64
		if _, err := fmt.Sscanf(sizeHex, "%x", &size); err != nil {
			return nil, fmt.Errorf("chunk size %q: %w", sizeHex, err)
		}
		if size == 0 {
			return strings.NewReader(string(out)), nil // final chunk; trailers ignored
		}
		for remaining := size; remaining > 0; {
			n, err := r.Read(buf[:min(int64(len(buf)), remaining)])
			if n > 0 {
				out = append(out, buf[:n]...)
				remaining -= int64(n)
			}
			if err == io.EOF && remaining > 0 {
				return nil, fmt.Errorf("unexpected end of body")
			}
			if err != nil && err != io.EOF {
				return nil, err
			}
		}
		// Consume the chunk-terminating CRLF.
		if _, err := readCRLFLine(r); err != nil {
			return nil, fmt.Errorf("chunk terminator: %w", err)
		}
	}
}

// readCRLFLine reads one CRLF-terminated line (without the CRLF).
func readCRLFLine(r io.Reader) (string, error) {
	var line []byte
	b := []byte{0}
	for {
		n, err := r.Read(b)
		if n > 0 {
			line = append(line, b[0])
			if len(line) >= 2 && line[len(line)-2] == '\r' && line[len(line)-1] == '\n' {
				return string(line[:len(line)-2]), nil
			}
			if len(line) > 1024 {
				return "", fmt.Errorf("chunk header line too long")
			}
		}
		if err != nil {
			return "", err
		}
	}
}

// s3ListBucketResult is the ListObjectsV2 response document (minio-go decodes
// Contents by field-name matching).
type s3ListBucketResult struct {
	XMLName  xml.Name         `xml:"ListBucketResult"`
	Name     string           `xml:"Name"`
	Prefix   string           `xml:"Prefix"`
	KeyCount int              `xml:"KeyCount"`
	MaxKeys  int              `xml:"MaxKeys"`
	Contents []s3ListContents `xml:"Contents"`
}

type s3ListContents struct {
	Key          string    `xml:"Key"`
	LastModified time.Time `xml:"LastModified"`
	Size         int64     `xml:"Size"`
}

func (s *s3FakeStore) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")

	s.mu.Lock()
	keys := make([]string, 0, len(s.objects))
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	s.mu.Unlock()
	sort.Strings(keys)

	result := s3ListBucketResult{
		Name:     s.bucket,
		Prefix:   prefix,
		KeyCount: len(keys),
		MaxKeys:  1000,
	}
	for _, k := range keys {
		obj := s.objects[k]
		result.Contents = append(result.Contents, s3ListContents{
			Key:          k,
			LastModified: obj.lastModified,
			Size:         int64(len(obj.data)),
		})
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(result)
}

// setLastModified overrides an object's LastModified (GC staleness testing).
func (s *s3FakeStore) setLastModified(key string, t time.Time) {
	s.mu.Lock()
	if obj, ok := s.objects[key]; ok {
		obj.lastModified = t
		s.objects[key] = obj
	}
	s.mu.Unlock()
}
