package repository

import (
	"context"
	"fmt"
)

// OrgS3Target is the bucket (and optional endpoint override) selected for one
// organization's tenant objects.
type OrgS3Target struct {
	Bucket string
}

// OrgObjectStoreResolver is the extension point for hard-split object storage
// (bucket or credential per organization). The default implementation keeps
// every tenant in the shared bucket under the orgs/<org_id>/ prefix.
type OrgObjectStoreResolver interface {
	ResolveS3(ctx context.Context, orgID string) (*OrgS3Target, error)
}

// SharedBucketResolver is the default OrgObjectStoreResolver: every
// organization maps to the same shared bucket.
type SharedBucketResolver struct {
	Bucket string
}

var _ OrgObjectStoreResolver = (*SharedBucketResolver)(nil)

// NewSharedBucketResolver returns a resolver pinned to bucket.
func NewSharedBucketResolver(bucket string) *SharedBucketResolver {
	return &SharedBucketResolver{Bucket: bucket}
}

// ResolveS3 returns the shared bucket.
func (r *SharedBucketResolver) ResolveS3(_ context.Context, orgID string) (*OrgS3Target, error) {
	if r.Bucket == "" {
		return nil, fmt.Errorf("shared bucket resolver: empty bucket for org %q", orgID)
	}
	return &OrgS3Target{Bucket: r.Bucket}, nil
}
