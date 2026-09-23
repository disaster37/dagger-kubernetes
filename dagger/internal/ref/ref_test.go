package ref

import (
	"strings"
	"testing"
)

func TestValidateTag(t *testing.T) {
	tests := []struct {
		name    string
		tag     string
		wantErr bool
	}{
		{name: "dev", tag: "dev"},
		{name: "semver with v prefix", tag: "v0.1.0"},
		{name: "latest", tag: "latest"},
		{name: "git sha", tag: "0123456789abcdef0123456789abcdef01234567"},
		{name: "leading underscore", tag: "_private"},
		{name: "dots dashes underscores", tag: "v1.0.0-rc.1_x"},
		{name: "max length 128", tag: strings.Repeat("a", 128)},
		{name: "empty", tag: "", wantErr: true},
		{name: "too long 129", tag: strings.Repeat("a", 129), wantErr: true},
		{name: "space", tag: "bad tag", wantErr: true},
		{name: "slash", tag: "hello/world", wantErr: true},
		{name: "leading dash", tag: "-dash", wantErr: true},
		{name: "leading dot", tag: ".dot", wantErr: true},
		{name: "plus sign", tag: "v1.0.0+build", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTag(tt.tag)
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateTag(%q) = nil, want error", tt.tag)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateTag(%q) = %v, want nil", tt.tag, err)
			}
		})
	}
}

func TestValidateRegistry(t *testing.T) {
	tests := []struct {
		name     string
		registry string
		wantErr  bool
	}{
		{name: "ghcr.io", registry: "ghcr.io"},
		{name: "docker.io", registry: "docker.io"},
		{name: "ttl throwaway", registry: "ttl.sh"},
		{name: "host with port", registry: "localhost:5000"},
		{name: "max port digits", registry: "registry.example.com:99999"},
		{name: "empty", registry: "", wantErr: true},
		{name: "scheme", registry: "http://ghcr.io", wantErr: true},
		{name: "path", registry: "ghcr.io/path", wantErr: true},
		{name: "whitespace", registry: "ghcr io", wantErr: true},
		{name: "underscore", registry: "ghcr_io", wantErr: true},
		{name: "port too long", registry: "host:123456", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRegistry(tt.registry)
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateRegistry(%q) = nil, want error", tt.registry)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateRegistry(%q) = %v, want nil", tt.registry, err)
			}
		})
	}
}

func TestValidateImage(t *testing.T) {
	tests := []struct {
		name    string
		image   string
		wantErr bool
	}{
		{name: "owner name", image: "disaster37/dagger-kubernetes"},
		{name: "nested path", image: "owner/name/sub"},
		{name: "separators", image: "my-org/my_image.sub/app"},
		{name: "smoke throwaway", image: "smoke/dagger-kubernetes"},
		{name: "empty", image: "", wantErr: true},
		{name: "missing owner", image: "missingowner", wantErr: true},
		{name: "uppercase", image: "Owner/Name", wantErr: true},
		{name: "leading slash", image: "/leading/slash", wantErr: true},
		{name: "trailing slash", image: "trailing/slash/", wantErr: true},
		{name: "scheme", image: "http://owner/name", wantErr: true},
		{name: "leading separator", image: "_lead/name", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateImage(tt.image)
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateImage(%q) = nil, want error", tt.image)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateImage(%q) = %v, want nil", tt.image, err)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name     string
		registry string
		image    string
		tag      string
		wantErr  bool
	}{
		{
			name:     "ghcr dev",
			registry: "ghcr.io",
			image:    "disaster37/dagger-kubernetes",
			tag:      "dev",
		},
		{
			name:     "ttl.sh smoke",
			registry: "ttl.sh",
			image:    "smoke/dagger-kubernetes",
			tag:      "1h",
		},
		{
			name:     "semver tag",
			registry: "ghcr.io",
			image:    "disaster37/dagger-kubernetes",
			tag:      "v0.1.0",
		},
		{
			name:     "invalid registry first",
			registry: "http://ghcr.io",
			image:    "owner/name",
			tag:      "dev",
			wantErr:  true,
		},
		{
			name:     "invalid image second",
			registry: "ghcr.io",
			image:    "Owner/Name",
			tag:      "dev",
			wantErr:  true,
		},
		{
			name:     "invalid tag last",
			registry: "ghcr.io",
			image:    "owner/name",
			tag:      "bad tag",
			wantErr:  true,
		},
		{
			name:     "empty registry",
			registry: "",
			image:    "owner/name",
			tag:      "dev",
			wantErr:  true,
		},
		{
			name:     "empty image",
			registry: "ghcr.io",
			image:    "",
			tag:      "dev",
			wantErr:  true,
		},
		{
			name:     "empty tag",
			registry: "ghcr.io",
			image:    "owner/name",
			tag:      "",
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.registry, tt.image, tt.tag)
			if tt.wantErr && err == nil {
				t.Fatalf("Validate(%q, %q, %q) = nil, want error", tt.registry, tt.image, tt.tag)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate(%q, %q, %q) = %v, want nil", tt.registry, tt.image, tt.tag, err)
			}
		})
	}
}
