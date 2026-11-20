package repository

import (
	"context"
	"testing"
	"time"
)

// newTestArtifactStore builds an S3ArtifactStore backed by the in-memory mock.
func newTestArtifactStore(mock *mockS3ObjectStore, prefix string) *S3ArtifactStore {
	return &S3ArtifactStore{
		client: mock,
		bucket: "test-bucket",
		prefix: prefix,
	}
}

// TestS3ArtifactStoreListPrefixes covers List's prefix construction: every
// combination of populated org/project/run segments must produce a prefix
// without an empty path component (MinIO rejects object names containing "//"),
// and the listing must return exactly the keys under that prefix. The
// empty-project/run case is the regression that made GET /api/v1/artifacts
// return 500 when called with only ?org=.
func TestS3ArtifactStoreListPrefixes(t *testing.T) {
	cases := []struct {
		name     string
		org      string
		project  string
		run      string
		wantKeys []string
	}{
		{
			name:     "org only matches every artifact in the org",
			org:      "default",
			project:  "",
			run:      "",
			wantKeys: []string{"artifacts/orgs/default/artifacts/p1/r1/a.txt", "artifacts/orgs/default/artifacts/p1/r2/c.txt", "artifacts/orgs/default/artifacts/p2/r2/b.txt"},
		},
		{
			name:     "org and project",
			org:      "default",
			project:  "p1",
			run:      "",
			wantKeys: []string{"artifacts/orgs/default/artifacts/p1/r1/a.txt", "artifacts/orgs/default/artifacts/p1/r2/c.txt"},
		},
		{
			name:     "org, project and run",
			org:      "default",
			project:  "p1",
			run:      "r1",
			wantKeys: []string{"artifacts/orgs/default/artifacts/p1/r1/a.txt"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newMockS3ObjectStore()
			now := time.Now()
			for _, k := range []string{
				"artifacts/orgs/default/artifacts/p1/r1/a.txt",
				"artifacts/orgs/default/artifacts/p2/r2/b.txt",
				"artifacts/orgs/default/artifacts/p1/r2/c.txt",
				"artifacts/orgs/other/artifacts/p1/r1/a.txt",
			} {
				mock.setObject(k, []byte("x"), now)
			}
			store := newTestArtifactStore(mock, "artifacts")

			got, err := store.List(context.Background(), tc.org, tc.project, tc.run)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(got) != len(tc.wantKeys) {
				t.Fatalf("List returned %v, want %v", got, tc.wantKeys)
			}
			for i, k := range tc.wantKeys {
				if got[i] != k {
					t.Fatalf("List[%d] = %q, want %q", i, got[i], k)
				}
			}
		})
	}
}

// TestS3ArtifactStoreListNoPrefixCollision ensures an org-prefix listing does
// not leak a sibling org whose slug shares a string prefix (e.g. "default" vs
// "default2"): the trailing slash on the built prefix is what prevents it.
func TestS3ArtifactStoreListNoPrefixCollision(t *testing.T) {
	mock := newMockS3ObjectStore()
	now := time.Now()
	mock.setObject("artifacts/orgs/default/artifacts/p1/r1/a.txt", []byte("x"), now)
	mock.setObject("artifacts/orgs/default2/artifacts/p1/r1/b.txt", []byte("x"), now)
	store := newTestArtifactStore(mock, "artifacts")

	got, err := store.List(context.Background(), "default", "", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0] != "artifacts/orgs/default/artifacts/p1/r1/a.txt" {
		t.Fatalf("List = %v, want only artifacts/default/p1/r1/a.txt", got)
	}
}

// TestS3ArtifactStoreDeleteRun covers DeleteRun removing every object for a run.
func TestS3ArtifactStoreDeleteRun(t *testing.T) {
	mock := newMockS3ObjectStore()
	now := time.Now()
	mock.setObject("artifacts/orgs/default/artifacts/p1/r1/a.txt", []byte("x"), now)
	mock.setObject("artifacts/orgs/default/artifacts/p1/r1/b.txt", []byte("x"), now)
	mock.setObject("artifacts/orgs/default/artifacts/p1/r2/c.txt", []byte("x"), now)
	store := newTestArtifactStore(mock, "artifacts")

	if err := store.DeleteRun(context.Background(), "default", "p1", "r1"); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	got := mock.keys()
	if len(got) != 1 || got[0] != "artifacts/orgs/default/artifacts/p1/r2/c.txt" {
		t.Fatalf("remaining keys = %v, want [artifacts/default/p1/r2/c.txt]", got)
	}
}
