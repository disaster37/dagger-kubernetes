package repository

import (
	"context"
	"errors"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// s3ObjectAPI is the slice of the minio-go client the S3 stores need. An
// interface (instead of the concrete *minio.Client) so tests can supply an
// in-memory object store; *minio.Client satisfies it via minioS3API.
type s3ObjectAPI interface {
	PutObject(ctx context.Context, bucketName, objectName string, reader io.Reader, objectSize int64, opts minio.PutObjectOptions) (minio.UploadInfo, error)
	GetObject(ctx context.Context, bucketName, objectName string, opts minio.GetObjectOptions) (io.ReadCloser, error)
	StatObject(ctx context.Context, bucketName, objectName string, opts minio.StatObjectOptions) (minio.ObjectInfo, error)
	ListObjects(ctx context.Context, bucketName string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo
}

// minioS3API adapts *minio.Client to s3ObjectAPI: GetObject's *minio.Object
// return narrows to io.ReadCloser, which the interface requires.
type minioS3API struct{ client *minio.Client }

func (m minioS3API) PutObject(ctx context.Context, bucketName, objectName string, reader io.Reader, objectSize int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) { //nolint:gocritic // hugeParam: signature must match the minio-go API
	return m.client.PutObject(ctx, bucketName, objectName, reader, objectSize, opts)
}

func (m minioS3API) GetObject(ctx context.Context, bucketName, objectName string, opts minio.GetObjectOptions) (io.ReadCloser, error) { //nolint:gocritic // hugeParam: signature must match the minio-go API
	return m.client.GetObject(ctx, bucketName, objectName, opts)
}

func (m minioS3API) StatObject(ctx context.Context, bucketName, objectName string, opts minio.StatObjectOptions) (minio.ObjectInfo, error) { //nolint:gocritic // hugeParam: signature must match the minio-go API
	return m.client.StatObject(ctx, bucketName, objectName, opts)
}

func (m minioS3API) ListObjects(ctx context.Context, bucketName string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo { //nolint:gocritic // hugeParam: signature must match the minio-go API
	return m.client.ListObjects(ctx, bucketName, opts)
}

// NewS3Client returns a minio client for an S3-compatible endpoint
// (self-hosted MinIO/SeaweedFS/Garage/Ceph RGW or AWS S3). Empty credentials
// fall back to the standard AWS env chain (AWS_ACCESS_KEY_ID /
// AWS_SECRET_ACCESS_KEY), matching minio-go's default credential lookup.
func NewS3Client(endpoint, region, accessKey, secretKey string, useSSL bool) (*minio.Client, error) {
	opts := &minio.Options{
		Secure: useSSL,
		Region: region,
	}
	if accessKey != "" || secretKey != "" {
		opts.Creds = credentials.NewStaticV4(accessKey, secretKey, "")
	} else {
		opts.Creds = credentials.NewEnvAWS()
	}
	return minio.New(endpoint, opts)
}

// isNoSuchKey reports whether err is an S3 NoSuchKey error (object missing).
func isNoSuchKey(err error) bool {
	var resp minio.ErrorResponse
	if !errors.As(err, &resp) {
		return false
	}
	return resp.Code == "NoSuchKey"
}
