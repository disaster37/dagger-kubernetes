package domain

import (
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		raw     string
		want    *Version
		wantErr bool
	}{
		{"v0.21.4", &Version{Major: 0, Minor: 21, Patch: 4, Raw: "v0.21.4"}, false},
		{"0.21.4", &Version{Major: 0, Minor: 21, Patch: 4, Raw: "v0.21.4"}, false},
		{"v1.0.0", &Version{Major: 1, Minor: 0, Patch: 0, Raw: "v1.0.0"}, false},
		{"0.19", &Version{Major: 0, Minor: 19, Patch: 0, Raw: "v0.19.0"}, false},
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

func TestIsFullVersion(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{"v0.21.8", true},
		{"0.21.8", true},
		{"v0.21.0", true},
		{"v1.0.0", true},
		{"0.21", false},
		{"v0.21", false},
		{"v0.21.8-rc.1", true},
		{"v1.0.0-beta.27", true},
		{"notaversion", false},
		{"", false},
	}

	for _, tt := range tests {
		if got := IsFullVersion(tt.raw); got != tt.want {
			t.Errorf("IsFullVersion(%q) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}

func TestVersionPreReleaseOrdering(t *testing.T) {
	beta26, _ := Parse("v1.0.0-beta.26")
	beta27, _ := Parse("v1.0.0-beta.27")
	release, _ := Parse("v1.0.0")
	if beta26.Compare(beta27) >= 0 {
		t.Fatal("beta.26 must be lower than beta.27")
	}
	if beta27.Compare(release) >= 0 {
		t.Fatal("pre-release must be lower than the final release")
	}
	if release.Compare(beta27) <= 0 {
		t.Fatal("final release must be higher than a pre-release")
	}
}

func TestVersionSlugPreRelease(t *testing.T) {
	v, err := Parse("v1.0.0-beta.27")
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Slug(); got != "v1-0-0-beta-27" {
		t.Fatalf("Slug() = %q, want v1-0-0-beta-27", got)
	}
	if got := StsName(v.String()); got != "dagger-engine-v1-0-0-beta-27" {
		t.Fatalf("StsName() = %q", got)
	}
}
