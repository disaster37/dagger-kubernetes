package domain

import (
	"fmt"
	"testing"
)

func TestEngineImageRegistryViaMirror(t *testing.T) {
	const internalAddr = "rel-registry-dagger-io-mirror.dagger.svc:5000"

	tlsMirror := ImageCacheMirror{
		Host:         "registry.dagger.io",
		InternalAddr: internalAddr,
		TLS:          true,
	}
	plainMirror := ImageCacheMirror{
		Host:         "registry.dagger.io",
		InternalAddr: "rel-registry-dagger-io-mirror.dagger.svc:5000",
		TLS:          false,
	}
	otherTLSMirror := ImageCacheMirror{
		Host:         "ghcr.io",
		InternalAddr: "rel-ghcr-io-mirror.dagger.svc:5000",
		TLS:          true,
	}
	noAddrTLSMirror := ImageCacheMirror{
		Host: "registry.dagger.io",
		TLS:  true,
	}

	tests := []struct {
		name     string
		registry string
		mirrors  []ImageCacheMirror
		want     string
	}{
		{
			name:     "no mirrors",
			registry: "registry.dagger.io/engine",
			mirrors:  nil,
			want:     "registry.dagger.io/engine",
		},
		{
			name:     "non-TLS matching mirror unchanged",
			registry: "registry.dagger.io/engine",
			mirrors:  []ImageCacheMirror{plainMirror},
			want:     "registry.dagger.io/engine",
		},
		{
			name:     "TLS mirror no host match unchanged",
			registry: "registry.dagger.io/engine",
			mirrors:  []ImageCacheMirror{otherTLSMirror},
			want:     "registry.dagger.io/engine",
		},
		{
			name:     "TLS mirror host match rewritten",
			registry: "registry.dagger.io/engine",
			mirrors:  []ImageCacheMirror{tlsMirror},
			want:     fmt.Sprintf("%s/engine", internalAddr),
		},
		{
			name:     "match after non-matching mirror",
			registry: "registry.dagger.io/engine",
			mirrors:  []ImageCacheMirror{otherTLSMirror, tlsMirror},
			want:     fmt.Sprintf("%s/engine", internalAddr),
		},
		{
			name:     "host with port and path preserved",
			registry: "my.reg:5000/foo/bar",
			mirrors: []ImageCacheMirror{{
				Host:         "my.reg:5000",
				InternalAddr: "rel-my-reg-mirror.dagger.svc:5000",
				TLS:          true,
			}},
			want: "rel-my-reg-mirror.dagger.svc:5000/foo/bar",
		},
		{
			name:     "TLS mirror without internal addr unchanged",
			registry: "registry.dagger.io/engine",
			mirrors:  []ImageCacheMirror{noAddrTLSMirror},
			want:     "registry.dagger.io/engine",
		},
		{
			name:     "scheme-prefixed registry unchanged",
			registry: "https://registry.dagger.io/engine",
			mirrors:  []ImageCacheMirror{tlsMirror},
			want:     "https://registry.dagger.io/engine",
		},
		{
			name:     "empty registry unchanged",
			registry: "",
			mirrors:  []ImageCacheMirror{tlsMirror},
			want:     "",
		},
		{
			name:     "registry without path rewritten to bare addr",
			registry: "registry.dagger.io",
			mirrors:  []ImageCacheMirror{tlsMirror},
			want:     internalAddr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EngineImageRegistryViaMirror(tt.registry, tt.mirrors); got != tt.want {
				t.Errorf("EngineImageRegistryViaMirror(%q) = %q, want %q", tt.registry, got, tt.want)
			}
		})
	}
}

func TestSplitRegistryRef(t *testing.T) {
	tests := []struct {
		name     string
		registry string
		wantHost string
		wantPath string
	}{
		{name: "host and path", registry: "registry.dagger.io/engine", wantHost: "registry.dagger.io", wantPath: "/engine"},
		{name: "host only", registry: "registry.dagger.io", wantHost: "registry.dagger.io", wantPath: ""},
		{name: "host with port and path", registry: "my.reg:5000/foo", wantHost: "my.reg:5000", wantPath: "/foo"},
		{name: "nested path", registry: "docker.io/library/alpine", wantHost: "docker.io", wantPath: "/library/alpine"},
		{name: "scheme-prefixed", registry: "https://registry.dagger.io/engine", wantHost: "", wantPath: ""},
		{name: "empty", registry: "", wantHost: "", wantPath: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, path := splitRegistryRef(tt.registry)
			if host != tt.wantHost || path != tt.wantPath {
				t.Errorf("splitRegistryRef(%q) = (%q, %q), want (%q, %q)", tt.registry, host, path, tt.wantHost, tt.wantPath)
			}
		})
	}
}
