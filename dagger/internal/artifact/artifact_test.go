package artifact

import (
	"strings"
	"testing"
)

func TestValidateVersion(t *testing.T) {
	longVersion := strings.Repeat("a", 129)

	tests := []struct {
		name    string
		version string
		wantErr bool
	}{
		// valid
		{name: "semver with v prefix", version: "v0.1.0", wantErr: false},
		{name: "semver without v prefix", version: "0.1.0", wantErr: false},
		{name: "dev", version: "dev", wantErr: false},
		{name: "latest", version: "latest", wantErr: false},
		{name: "git sha", version: "0123456789abcdef0123456789abcdef01234567", wantErr: false},
		{name: "prerelease", version: "0.0.1-alpha56", wantErr: false},
		{name: "build metadata chars", version: "1.2.3-rc.1_x", wantErr: false},
		{name: "max length", version: strings.Repeat("a", 128), wantErr: false},

		// invalid
		{name: "empty", version: "", wantErr: true},
		{name: "path separator", version: "v1.0.0/build", wantErr: true},
		{name: "dot dot", version: "..", wantErr: true},
		{name: "single dot", version: ".", wantErr: true},
		{name: "leading hyphen", version: "-lead", wantErr: true},
		{name: "leading dot", version: ".dot", wantErr: true},
		{name: "whitespace", version: "has space", wantErr: true},
		{name: "control character", version: "ctrl\n", wantErr: true},
		{name: "too long", version: longVersion, wantErr: true},
		{name: "extra path segment", version: "v0.1.0/extra", wantErr: true},
		{name: "backslash", version: `v0.1.0\extra`, wantErr: true},
		{name: "embedded dot dot", version: "v0..1", wantErr: true},
		{name: "shell metacharacter", version: "v0.1.0;rm", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateVersion(tt.version)
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateVersion(%q) = nil, want error", tt.version)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateVersion(%q) = %v, want nil", tt.version, err)
			}
		})
	}
}

func TestFilename(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
		wantErr bool
	}{
		{name: "release tag", version: "v0.1.0", want: "jenkins-libs-v0.1.0.tar.gz", wantErr: false},
		{name: "dev", version: "dev", want: "jenkins-libs-dev.tar.gz", wantErr: false},
		{name: "untagged release", version: "0.0.1-alpha56", want: "jenkins-libs-0.0.1-alpha56.tar.gz", wantErr: false},
		{name: "invalid version returns validation error", version: "v0.1.0/extra", wantErr: true},
		{name: "empty version returns validation error", version: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Filename(tt.version)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Filename(%q) = %q, want error", tt.version, got)
				}
				if want := ValidateVersion(tt.version); want == nil || err.Error() != want.Error() {
					t.Fatalf("Filename(%q) error = %v, want validation error %v", tt.version, err, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("Filename(%q) error: %v", tt.version, err)
			}
			if got != tt.want {
				t.Fatalf("Filename(%q) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}
