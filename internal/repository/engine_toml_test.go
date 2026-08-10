package repository

import (
	"strings"
	"testing"
)

func TestEngineTOMLRender(t *testing.T) {
	tests := []struct {
		name string
		cfg  engineTOML
		want string
	}{
		{
			name: "empty config renders empty string",
			cfg:  engineTOML{},
			want: "",
		},
		{
			name: "debug only",
			cfg:  engineTOML{Debug: true},
			want: "debug = true\n",
		},
		{
			name: "log format only",
			cfg:  engineTOML{LogFormat: "json"},
			want: "[log]\n  format = \"json\"\n",
		},
		{
			name: "debug and format",
			cfg:  engineTOML{Debug: true, LogFormat: "json"},
			want: "debug = true\n\n[log]\n  format = \"json\"\n",
		},
		{
			name: "mirrors hosts sorted alphabetically",
			cfg: engineTOML{RegistryMirrors: map[string][]string{
				"docker.io":         {"mirror.gcr.io"},
				"gcr.io":            {"hm-registry.hm.dm.ad/gcr.io"},
				"docker.elastic.co": {"hm-registry.hm.dm.ad/docker-elastic"},
			}},
			want: "[registry.\"docker.elastic.co\"]\n  mirrors = [\"hm-registry.hm.dm.ad/docker-elastic\"]\n\n" +
				"[registry.\"docker.io\"]\n  mirrors = [\"mirror.gcr.io\"]\n\n" +
				"[registry.\"gcr.io\"]\n  mirrors = [\"hm-registry.hm.dm.ad/gcr.io\"]\n",
		},
		{
			name: "multiple mirrors per host",
			cfg: engineTOML{RegistryMirrors: map[string][]string{
				"docker.io": {"hm-registry.hm.dm.ad/docker-hub", "mirror.gcr.io"},
			}},
			want: "[registry.\"docker.io\"]\n  mirrors = [\"hm-registry.hm.dm.ad/docker-hub\", \"mirror.gcr.io\"]\n",
		},
		{
			name: "hosts with empty mirror lists skipped",
			cfg: engineTOML{RegistryMirrors: map[string][]string{
				"docker.io": {"mirror.gcr.io"},
				"gcr.io":    {},
			}},
			want: "[registry.\"docker.io\"]\n  mirrors = [\"mirror.gcr.io\"]\n",
		},
		{
			name: "full five-registry example from requirements",
			cfg: engineTOML{
				Debug:     true,
				LogFormat: "json",
				RegistryMirrors: map[string][]string{
					"docker.elastic.co": {"hm-registry.hm.dm.ad/docker-elastic"},
					"docker.io":         {"hm-registry.hm.dm.ad/docker-hub", "mirror.gcr.io"},
					"gcr.io":            {"hm-registry.hm.dm.ad/gcr.io"},
					"ghcr.io":           {"hm-registry.hm.dm.ad/docker-github"},
					"public.ecr.aws":    {"hm-registry.hm.dm.ad/docker-aws"},
				},
			},
			want: "debug = true\n\n" +
				"[log]\n  format = \"json\"\n\n" +
				"[registry.\"docker.elastic.co\"]\n  mirrors = [\"hm-registry.hm.dm.ad/docker-elastic\"]\n\n" +
				"[registry.\"docker.io\"]\n  mirrors = [\"hm-registry.hm.dm.ad/docker-hub\", \"mirror.gcr.io\"]\n\n" +
				"[registry.\"gcr.io\"]\n  mirrors = [\"hm-registry.hm.dm.ad/gcr.io\"]\n\n" +
				"[registry.\"ghcr.io\"]\n  mirrors = [\"hm-registry.hm.dm.ad/docker-github\"]\n\n" +
				"[registry.\"public.ecr.aws\"]\n  mirrors = [\"hm-registry.hm.dm.ad/docker-aws\"]\n",
		},
		{
			name: "escaping in log format",
			cfg:  engineTOML{LogFormat: "a\"b\\c\nd\te"},
			want: "[log]\n  format = \"a\\\"b\\\\c\\nd\\te\"\n",
		},
		{
			name: "escaping in registry host and mirror",
			cfg: engineTOML{RegistryMirrors: map[string][]string{
				"host\"name": {"mirror\\path"},
			}},
			want: "[registry.\"host\\\"name\"]\n  mirrors = [\"mirror\\\\path\"]\n",
		},
		{
			name: "control byte in log format",
			cfg:  engineTOML{LogFormat: "a\x01b"},
			want: "[log]\n  format = \"a\\u0001b\"\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.render()
			if got != tt.want {
				t.Errorf("render() mismatch:\nwant %q\ngot  %q", tt.want, got)
			}
		})
	}
}

func TestEngineTOMLEscape(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"plain", "plain"},
		{"back\\slash", "back\\\\slash"},
		{"double\"quote", "double\\\"quote"},
		{"bell\bform\fnew\nline\r\rtab\t", "bell\\bform\\fnew\\nline\\r\\rtab\\t"},
		{"control\x01byte", "control\\u0001byte"},
		{"del\x7f", "del\\u007f"},
		{"mixed\"\\\"\n\t\x02", "mixed\\\"\\\\\\\"\\n\\t\\u0002"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := tomlEscape(tt.in)
			if got != tt.want {
				t.Errorf("tomlEscape(%q) = %q, want %q", tt.in, got, tt.want)
			}
			// The escaped output must never contain a raw newline, tab, or
			// unescaped control byte (would break TOML basic strings).
			if strings.ContainsAny(got, "\n\r\t\b\f") {
				t.Errorf("tomlEscape(%q) produced raw control char: %q", tt.in, got)
			}
		})
	}
}
