package repository

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The per-tag structs below mirror the keys each Dagger release actually
// declares in engine/config.Config. Decoding the rendered engine.json with
// DisallowUnknownFields against them is the crash-safety proof: the engine
// loads its config the same way and exits fatally on an unknown key.
type testRegistry struct {
	Mirrors []string `json:"mirrors,omitempty"`
	HTTP    *bool    `json:"http,omitempty"`
}

// engineConfigThroughBeta20 is the config shape at 0.21.12 and at the 1.0
// betas before RemoteCache existed (beta.2 through beta.15).
type engineConfigThroughBeta20 struct {
	LogLevel   string                  `json:"logLevel,omitempty"`
	Registries map[string]testRegistry `json:"registries,omitempty"`
}

// engineConfigBeta19 is the first shape that has a remoteCache field.
type engineConfigBeta19 struct {
	LogLevel    string                  `json:"logLevel,omitempty"`
	Registries  map[string]testRegistry `json:"registries,omitempty"`
	RemoteCache *struct {
		URL string `json:"url"`
	} `json:"remoteCache,omitempty"`
}

func decodeStrict(t *testing.T, raw []byte, out any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		t.Fatalf("engine.json %q rejected by the tag's config struct: %v", raw, err)
	}
}

// TestEngineConfigCrashSafety is the regression guard for the version gate: it
// renders a remote-cache-enabled config for every tag and proves the result
// decodes under that tag's real config shape. A major-only gate would emit
// remoteCache for beta.10/beta.15 and fail here (and crash the engine pod).
func TestEngineConfigCrashSafety(t *testing.T) {
	cfg := K8sProviderConfig{
		Debug:           true,
		RegistryMirrors: map[string][]string{"docker.io": {"mirror.example:5000"}},
		MirrorHTTP:      []string{"mirror.example:5000"},
		RemoteCacheURL:  "https://control.example.test/v1/engine",
	}

	tests := []struct {
		tag             string
		wantRemoteCache bool
		decode          func(t *testing.T, raw []byte)
	}{
		{
			tag: "v0.21.12",
			decode: func(t *testing.T, raw []byte) {
				var out engineConfigThroughBeta20
				decodeStrict(t, raw, &out)
				if out.LogLevel != "debug" || len(out.Registries) != 2 {
					t.Fatalf("decoded 0.21.12 config = %+v, want logLevel+registries", out)
				}
			},
		},
		{
			tag: "v1.0.0-beta.10",
			decode: func(t *testing.T, raw []byte) {
				var out engineConfigThroughBeta20
				decodeStrict(t, raw, &out)
			},
		},
		{
			tag: "v1.0.0-beta.15",
			decode: func(t *testing.T, raw []byte) {
				var out engineConfigThroughBeta20
				decodeStrict(t, raw, &out)
			},
		},
		{
			tag: "v1.0.0-beta.19",
			decode: func(t *testing.T, raw []byte) {
				var out engineConfigBeta19
				decodeStrict(t, raw, &out)
			},
		},
		{
			tag: "v1.0.0-beta.20",
			decode: func(t *testing.T, raw []byte) {
				var out engineConfigBeta19
				decodeStrict(t, raw, &out)
			},
		},
		{
			tag:             "v1.0.0-beta.28",
			wantRemoteCache: true,
			decode: func(t *testing.T, raw []byte) {
				var out engineConfigBeta19
				decodeStrict(t, raw, &out)
				if out.RemoteCache == nil || out.RemoteCache.URL != cfg.RemoteCacheURL {
					t.Fatalf("remoteCache = %+v, want url %q", out.RemoteCache, cfg.RemoteCacheURL)
				}
			},
		},
		{
			tag: "dev",
			decode: func(t *testing.T, raw []byte) {
				var out engineConfigThroughBeta20
				decodeStrict(t, raw, &out)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			raw, err := buildEngineConfig(&cfg, tt.tag).render()
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if raw == nil {
				t.Fatal("expected a rendered config")
			}
			hasRemoteCache := bytes.Contains(raw, []byte(`"remoteCache"`))
			if hasRemoteCache != tt.wantRemoteCache {
				t.Fatalf("remoteCache rendered = %v, want %v (%s)", hasRemoteCache, tt.wantRemoteCache, raw)
			}
			tt.decode(t, raw)
		})
	}
}

func TestEngineConfigRenderEmpty(t *testing.T) {
	for _, cfg := range []engineConfig{
		{},
		{Registries: map[string]engineRegistry{}},
	} {
		raw, err := cfg.render()
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if raw != nil {
			t.Errorf("render(%+v) = %q, want nil", cfg, raw)
		}
	}
}

func TestEngineConfigRenderShape(t *testing.T) {
	raw, err := engineConfig{
		LogLevel: "debug",
		Registries: map[string]engineRegistry{
			"docker.io": {Mirrors: []string{"mirror.example:5000"}},
			"plain.example:5000": {
				HTTP: boolPtr(true),
			},
		},
		RemoteCache: &engineRemoteCache{URL: "https://control.example.test/v1/engine"},
	}.render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := `{"logLevel":"debug","registries":{"docker.io":{"mirrors":["mirror.example:5000"]},` +
		`"plain.example:5000":{"http":true}},` +
		`"remoteCache":{"url":"https://control.example.test/v1/engine"}}`
	if string(raw) != want {
		t.Errorf("render = %s, want %s", raw, want)
	}
}

func TestBuildEngineRegistries(t *testing.T) {
	cfg := K8sProviderConfig{
		RegistryMirrors: map[string][]string{
			"docker.io": {"mirror.example:5000", ""},
			"gcr.io":    {},
		},
		MirrorHTTP: []string{"mirror.example:5000", "", "plain.example:5000"},
	}
	got := buildEngineRegistries(&cfg)
	if len(got) != 3 {
		t.Fatalf("registries = %+v, want 3 entries (docker.io + 2 plaintext hosts)", got)
	}
	if docker := got["docker.io"]; len(docker.Mirrors) != 1 || docker.Mirrors[0] != "mirror.example:5000" {
		t.Errorf("docker.io = %+v, want one mirror", docker)
	}
	if _, ok := got["gcr.io"]; ok {
		t.Error("empty mirror list should be omitted")
	}
	for _, host := range []string{"mirror.example:5000", "plain.example:5000"} {
		registry, ok := got[host]
		if !ok || registry.HTTP == nil || !*registry.HTTP {
			t.Errorf("%s = %+v, want http=true", host, registry)
		}
	}
}

func TestEngineSupportsRemoteCache(t *testing.T) {
	tests := map[string]bool{
		"v0.19.0":               false,
		"v0.21.12":              false,
		"v1.0.0-beta.2":         false,
		"v1.0.0-beta.27":        false,
		"v1.0.0-beta.28":        true,
		"v1.0.0-beta.29":        true,
		"v1.0.0-rc.1":           true,
		"v1.0.0":                true,
		"1.0.0-beta.28":         true,
		"":                      false,
		"dev":                   false,
		"latest":                false,
		"deadbeef":              false,
		"main-20260101":         false,
		strings.Repeat("a", 64): false,
	}
	for version, want := range tests {
		if got := engineSupportsRemoteCache(version); got != want {
			t.Errorf("engineSupportsRemoteCache(%q) = %v, want %v", version, got, want)
		}
	}
}
