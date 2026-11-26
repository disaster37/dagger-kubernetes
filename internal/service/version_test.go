package service

import (
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// TestResolverAllowRuleMatching covers the SemVer-aware allowlist semantics:
// a partial "MAJOR.MINOR" entry admits the whole minor series (incl.
// pre-releases), a full version with a dotted-or-empty pre-release is exact,
// and a full version with a dot-less pre-release admits that pre-release track.
func TestResolverAllowRuleMatching(t *testing.T) {
	cases := []struct {
		name      string
		floor     string
		allowlist []string
		version   string
		want      bool
	}{
		// Default allowlist (with 1.0 added) admits the whole 1.0 minor series.
		{name: "minor series admits stable", floor: "v0.19.0", allowlist: []string{"0.19", "0.20", "0.21", "1.0"}, version: "v0.21.8", want: true},
		{name: "minor series admits beta", floor: "v0.19.0", allowlist: []string{"0.19", "0.20", "0.21", "1.0"}, version: "v1.0.0-beta.28", want: true},
		{name: "minor series admits stable 1.0.0", floor: "v0.19.0", allowlist: []string{"0.19", "0.20", "0.21", "1.0"}, version: "v1.0.0", want: true},
		{name: "below floor rejected", floor: "v0.19.0", allowlist: []string{"0.19", "0.20", "0.21", "1.0"}, version: "v0.18.9", want: false},
		{name: "minor not in allowlist rejected", floor: "v0.19.0", allowlist: []string{"0.19", "0.20", "0.21", "1.0"}, version: "v1.1.0-beta.1", want: false},
		{name: "v-prefixed minor entry", floor: "v0.19.0", allowlist: []string{"v1.0"}, version: "v1.0.0-beta.5", want: true},

		// Pre-release track prefix.
		{name: "beta track admits beta.1", floor: "v0.19.0", allowlist: []string{"1.0.0-beta"}, version: "v1.0.0-beta.1", want: true},
		{name: "beta track admits beta.28", floor: "v0.19.0", allowlist: []string{"1.0.0-beta"}, version: "v1.0.0-beta.28", want: true},
		{name: "beta track rejects rc track", floor: "v0.19.0", allowlist: []string{"1.0.0-beta"}, version: "v1.0.0-rc.1", want: false},
		{name: "beta track rejects stable", floor: "v0.19.0", allowlist: []string{"1.0.0-beta"}, version: "v1.0.0", want: false},
		{name: "beta track rejects other patch", floor: "v0.19.0", allowlist: []string{"1.0.0-beta"}, version: "v1.0.1-beta.1", want: false},

		// Exact version.
		{name: "exact beta.28 admits itself", floor: "v0.19.0", allowlist: []string{"v1.0.0-beta.28"}, version: "v1.0.0-beta.28", want: true},
		{name: "exact beta.28 rejects beta.27", floor: "v0.19.0", allowlist: []string{"v1.0.0-beta.28"}, version: "v1.0.0-beta.27", want: false},
		{name: "exact stable admits itself", floor: "v0.19.0", allowlist: []string{"1.0.0"}, version: "v1.0.0", want: true},
		{name: "exact stable rejects beta", floor: "v0.19.0", allowlist: []string{"1.0.0"}, version: "v1.0.0-beta.28", want: false},

		// Floor compare preserved: a pre-release below the floor is rejected even
		// when its minor series is allowed (SemVer: beta < stable).
		{name: "floor equal to stable rejects beta of same version", floor: "v1.0.0", allowlist: []string{"1.0"}, version: "v1.0.0-beta.28", want: false},
		{name: "floor equal to stable admits stable", floor: "v1.0.0", allowlist: []string{"1.0"}, version: "v1.0.0", want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewResolver(tc.floor, tc.allowlist, nil)
			if err != nil {
				t.Fatalf("NewResolver: %v", err)
			}
			v, err := domain.Parse(tc.version)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.version, err)
			}
			if got := r.IsAllowed(v); got != tc.want {
				t.Fatalf("IsAllowed(%s) with floor %s allowlist %v = %v, want %v", tc.version, tc.floor, tc.allowlist, got, tc.want)
			}
		})
	}
}

// TestResolverRejectsMalformedPreReleaseEntry verifies that a "-"-containing
// non-full allowlist entry (e.g. the "1.0-beta" typo) is rejected instead of
// being classified as a minor rule that would admit the entire stable 1.0.x
// series.
func TestResolverRejectsMalformedPreReleaseEntry(t *testing.T) {
	for _, entry := range []string{"1.0-beta", "v1.0-rc", "1.0.0.0-beta"} {
		if _, err := NewResolver("v0.19.0", []string{entry}, nil); err == nil {
			t.Errorf("NewResolver allowlist %q = nil error, want rejection", entry)
		}
	}
	// The malformed entry must not admit a stable version: a resolver built
	// without it (empty allowlist = allow all >= floor) is the only way; prove
	// the entry itself is never accepted by contrast with a valid minor entry.
	if _, err := NewResolver("v0.19.0", []string{"1.0"}, nil); err != nil {
		t.Fatalf("valid minor entry rejected: %v", err)
	}
	// A well-formed pre-release track still parses.
	if _, err := NewResolver("v0.19.0", []string{"1.0.0-beta"}, nil); err != nil {
		t.Fatalf("valid pre-release track rejected: %v", err)
	}
}

func TestResolverFloor(t *testing.T) {
	r, err := NewResolver("v0.19.0", nil, nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	v019, _ := domain.Parse("v0.19.0")
	v016, _ := domain.Parse("v0.16.0")

	if !r.IsAllowed(v019) {
		t.Fatal("v0.19.0 should be allowed")
	}
	if r.IsAllowed(v016) {
		t.Fatal("v0.16.0 should not be allowed")
	}
}

func TestResolverAllowlist(t *testing.T) {
	r, err := NewResolver("v0.19.0", []string{"0.21"}, nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	v021, _ := domain.Parse("v0.21.4")
	v020, _ := domain.Parse("v0.20.0")

	if !r.IsAllowed(v021) {
		t.Fatal("v0.21.x should be allowed")
	}
	if r.IsAllowed(v020) {
		t.Fatal("v0.20.x should not be allowed")
	}
}

// TestResolverAdmitsChangelogRange proves every Dagger release in the
// changelog support range (v0.21.8 → v1.0.0-beta.20) passes the shipped
// version gate, so no entry in the compatibility matrix is blocked by the
// control plane itself (ADR-061 V1).
func TestResolverAdmitsChangelogRange(t *testing.T) {
	r, err := NewResolver("v0.19.0", nil, map[string][]string{
		"0.21": {"v0.21.0", "v0.21.8", "v0.21.9", "v0.21.10"},
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	cases := []struct {
		version string
		want    bool
	}{
		{version: "v0.21.8", want: true},
		{version: "v0.21.9", want: true},
		{version: "v0.21.10", want: true},
		{version: "v1.0.0-beta.20", want: true},
		{version: "v0.18.0", want: false},
	}
	for _, tc := range cases {
		v, err := domain.Parse(tc.version)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.version, err)
		}
		if got := r.IsAllowed(v); got != tc.want {
			t.Errorf("IsAllowed(%s) = %v, want %v", tc.version, got, tc.want)
		}
	}

	minimum, err := r.ResolveMinimal("0.21")
	if err != nil {
		t.Fatalf("ResolveMinimal: %v", err)
	}
	if minimum.Patch != 10 {
		t.Errorf("ResolveMinimal(0.21) patch = %d, want 10", minimum.Patch)
	}
}

func TestResolveMinimal(t *testing.T) {
	releases := map[string][]string{
		"0.21": {"v0.21.0", "v0.21.1", "v0.21.4"},
	}
	r, _ := NewResolver("v0.19.0", nil, releases)

	v, err := r.ResolveMinimal("0.21")
	if err != nil {
		t.Fatalf("ResolveMinimal: %v", err)
	}
	if v.Patch != 4 {
		t.Fatalf("expected patch 4, got %d", v.Patch)
	}
}
