package domain

import (
	"strings"
	"testing"
)

func TestEngineOrgPodRegex(t *testing.T) {
	cases := []struct {
		name    string
		org     string
		version string
		want    string
	}{
		{
			name:    "per-org name",
			org:     "default",
			version: "v0.21.8",
			want:    `dagger-engine-default-v0-21-8`,
		},
		{
			name:    "org with dots and build separators slugged",
			org:     "my.org",
			version: "v1.0.0-beta.28",
			want:    `dagger-engine-my-org-v1-0-0-beta-28`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EngineOrgPodRegex(tc.org, tc.version); got != tc.want {
				t.Fatalf("EngineOrgPodRegex(%q,%q) = %q, want %q", tc.org, tc.version, got, tc.want)
			}
		})
	}
}

// TestEngineOrgPodRegexEscapesMeta verifies a hostile org slug cannot inject
// regex metacharacters through the pod matcher.
func TestEngineOrgPodRegexEscapesMeta(t *testing.T) {
	got := EngineOrgPodRegex(`a(b)`, "v0.21.8")
	if !strings.Contains(got, `\(`) || !strings.Contains(got, `\)`) {
		t.Fatalf("EngineOrgPodRegex did not escape metacharacters in %q", got)
	}
}
