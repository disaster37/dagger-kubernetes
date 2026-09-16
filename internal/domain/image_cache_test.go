package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidDigest(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"valid", "sha256:" + strings.Repeat("a", 64), true},
		{"valid hex digits", "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", true},
		{"empty", "", false},
		{"missing prefix", strings.Repeat("a", 64), false},
		{"wrong algorithm", "sha512:" + strings.Repeat("a", 64), false},
		{"too short", "sha256:abc", false},
		{"too long", "sha256:" + strings.Repeat("a", 65), false},
		{"uppercase hex", "sha256:" + strings.Repeat("A", 64), false},
		{"non-hex", "sha256:" + strings.Repeat("g", 64), false},
		{"path traversal", "sha256:../../etc/passwd", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidDigest(tt.in); got != tt.want {
				t.Fatalf("ValidDigest(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestImageCacheSentinels(t *testing.T) {
	// The sentinels must be distinct so the handler's errors.Is switch can
	// discriminate them.
	sentinels := []error{
		ErrRegistryDeleteDisabled,
		ErrRegistryCatalogDisabled,
		ErrManifestNotFound,
		ErrImageCacheMirrorNotFound,
		ErrImageCacheInvalidRef,
		ErrImageCacheUnreachable,
	}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i != j && errors.Is(a, b) {
				t.Fatalf("sentinel %d unexpectedly matches sentinel %d", i, j)
			}
		}
	}
}

func TestImageCachePayloadJSON(t *testing.T) {
	info := ImageCacheInfo{
		Mirrors: []ImageCacheMirrorInfo{{
			ID:        "docker-io",
			Host:      "docker.io",
			Upstream:  "https://registry-1.docker.io",
			Backend:   "s3",
			Reachable: true,
			Repositories: []ImageCacheRepository{{
				Repository: "library/alpine",
				Tags: []ImageCacheTag{{
					Tag:        "3.20",
					Digest:     "sha256:" + strings.Repeat("b", 64),
					SizeBytes:  1234,
					LayerCount: 2,
				}},
			}},
		}},
		CollectedAt: "2026-01-02T03:04:05Z",
	}

	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Exact JSON tags are part of the API contract.
	for _, want := range []string{
		`"size_bytes":1234`,
		`"layer_count":2`,
		`"collected_at":"2026-01-02T03:04:05Z"`,
		`"reachable":true`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("marshalled info %s missing %s", raw, want)
		}
	}
	// Omitempty fields must be absent when empty.
	for _, unwanted := range []string{`"error":`, `"message":`, `"failed":`} {
		if strings.Contains(string(raw), unwanted) {
			t.Fatalf("marshalled info unexpectedly contains %s: %s", unwanted, raw)
		}
	}

	var back ImageCacheInfo
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.Mirrors) != 1 || len(back.Mirrors[0].Repositories[0].Tags) != 1 {
		t.Fatalf("round-trip lost data: %+v", back)
	}

	// Prune-all carries per-mirror failures inside the body as JSON arrays.
	all := ImageCachePruneAllResult{Mirrors: []ImageCachePruneAllMirror{{
		MirrorID:        "docker-io",
		ManifestsPruned: 3,
		Failed:          []ImageCachePruneAllItem{{Repository: "library/alpine", Error: "boom"}},
	}}}
	raw, err = json.Marshal(all)
	if err != nil {
		t.Fatalf("marshal prune-all: %v", err)
	}
	for _, want := range []string{`"manifests_pruned":3`, `"repositories_processed":0`, `"failed":[{"repository":"library/alpine","pruned":false,"error":"boom"}]`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("marshalled prune-all %s missing %s", raw, want)
		}
	}
}
