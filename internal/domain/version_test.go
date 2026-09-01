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
		{"v0.21.8-rc.1", false},
		{"notaversion", false},
		{"", false},
	}

	for _, tt := range tests {
		if got := IsFullVersion(tt.raw); got != tt.want {
			t.Errorf("IsFullVersion(%q) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}
