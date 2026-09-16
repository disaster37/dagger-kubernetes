package repository

import (
	"errors"
	"fmt"
	"testing"

	"github.com/minio/minio-go/v7"
)

// TestNewS3Client covers the constructor's credential-selection branches:
// static credentials when either key is set, and the standard AWS env
// credential chain when both are empty (the documented fallback for
// secret-less deployments). Construction never dials the endpoint, so these
// are pure unit tests; the wire behavior is exercised by the S3CLICache tests
// through the s3ObjectAPI mock.
func TestNewS3Client(t *testing.T) {
	tests := []struct {
		name      string
		endpoint  string
		region    string
		accessKey string
		secretKey string
		useSSL    bool
	}{
		{name: "static credentials, plaintext (MinIO)", endpoint: "minio:9000", region: "us-east-1", accessKey: "ak", secretKey: "sk", useSSL: false},
		{name: "static credentials, https (AWS S3)", endpoint: "s3.amazonaws.com", region: "us-west-2", accessKey: "ak", secretKey: "sk", useSSL: true},
		{name: "empty credentials fall back to the AWS env chain", endpoint: "minio:9000", region: "us-east-1"},
		{name: "only access key set still uses static credentials", endpoint: "minio:9000", region: "us-east-1", accessKey: "ak"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewS3Client(tt.endpoint, tt.region, tt.accessKey, tt.secretKey, tt.useSSL)
			if err != nil {
				t.Fatalf("NewS3Client(%q, %q, useSSL=%v): %v", tt.endpoint, tt.region, tt.useSSL, err)
			}
			if client == nil {
				t.Fatal("NewS3Client returned a nil client")
			}
		})
	}
}

// TestIsNoSuchKey covers the S3-NoSuchKey detector: it must match the error
// both bare and wrapped (callers wrap with %w), and must not match other S3
// error codes or plain errors.
func TestIsNoSuchKey(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "bare NoSuchKey", err: minio.ErrorResponse{Code: "NoSuchKey"}, want: true},
		{name: "wrapped NoSuchKey", err: fmt.Errorf("stat object: %w", minio.ErrorResponse{Code: "NoSuchKey"}), want: true},
		{name: "other S3 error code", err: minio.ErrorResponse{Code: "AccessDenied"}, want: false},
		{name: "wrapped other S3 error code", err: fmt.Errorf("get: %w", minio.ErrorResponse{Code: "BucketNotFound"}), want: false},
		{name: "plain error", err: errors.New("connection refused"), want: false},
		{name: "nil error", err: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNoSuchKey(tt.err); got != tt.want {
				t.Errorf("isNoSuchKey(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
