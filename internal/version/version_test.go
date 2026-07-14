package version

import (
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		raw     string
		want    *Version
		wantErr bool
	}{
		{"v0.21.4", &Version{0, 21, 4, "v0.21.4"}, false},
		{"0.21.4", &Version{0, 21, 4, "v0.21.4"}, false},
		{"v1.0.0", &Version{1, 0, 0, "v1.0.0"}, false},
		{"0.19", &Version{0, 19, 0, "v0.19.0"}, false},
		{"invalid", nil, true},
	}

	for _, tt := range tests {
		v, err := Parse(tt.raw)
		if tt.wantErr {
			if err == nil {
				t.Errorf("Parse(%q) expected error", tt.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.raw, err)
			continue
		}
		if v.String() != tt.want.String() {
			t.Errorf("Parse(%q) = %s, want %s", tt.raw, v.String(), tt.want.String())
		}
	}
}

func TestVersionCompare(t *testing.T) {
	a, _ := Parse("v0.19.0")
	b, _ := Parse("v0.21.4")

	if a.Compare(b) >= 0 {
		t.Fatal("v0.19.0 should be less than v0.21.4")
	}
	if b.Compare(a) <= 0 {
		t.Fatal("v0.21.4 should be greater than v0.19.0")
	}
}

func TestVersionSlug(t *testing.T) {
	v, _ := Parse("v0.21.4")
	if slug := v.Slug(); slug != "v0-21-4" {
		t.Fatalf("expected v0-21-4, got %s", slug)
	}
}

func TestResolverFloor(t *testing.T) {
	r, err := NewResolver("v0.19.0", nil, nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	v019, _ := Parse("v0.19.0")
	v016, _ := Parse("v0.16.0")

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

	v021, _ := Parse("v0.21.4")
	v020, _ := Parse("v0.20.0")

	if !r.IsAllowed(v021) {
		t.Fatal("v0.21.x should be allowed")
	}
	if r.IsAllowed(v020) {
		t.Fatal("v0.20.x should not be allowed")
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
