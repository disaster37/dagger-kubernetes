package domain

import "testing"

func TestAssetFilename(t *testing.T) {
	tests := []struct {
		version string
		osName  string
		arch    string
		want    string
	}{
		{"v0.21.8", "linux", "amd64", "dagger_v0.21.8_linux_amd64.tar.gz"},
		{"v0.21.8", "darwin", "arm64", "dagger_v0.21.8_darwin_arm64.tar.gz"},
		{"v0.21.8", "linux", "armv7", "dagger_v0.21.8_linux_armv7.tar.gz"},
	}

	for _, tt := range tests {
		if got := AssetFilename(tt.version, tt.osName, tt.arch); got != tt.want {
			t.Fatalf("AssetFilename(%q, %q, %q) = %q, want %q", tt.version, tt.osName, tt.arch, got, tt.want)
		}
	}
}

func TestCLISentinelErrorsDistinct(t *testing.T) {
	sentinels := map[string]error{
		"not_found":            ErrCLINotFound,
		"not_allowed":          ErrCLIVersionNotAllowed,
		"checksum_mismatch":    ErrCLIChecksumMismatch,
		"upstream_unavailable": ErrCLIUpstreamUnavailable,
	}
	seen := map[error]string{}
	for name, err := range sentinels {
		if err == nil {
			t.Fatalf("%s sentinel is nil", name)
		}
		if prev, ok := seen[err]; ok {
			t.Fatalf("sentinel %s duplicates %s", name, prev)
		}
		seen[err] = name
	}
}
