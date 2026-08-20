package domain

import (
	"errors"
	"testing"
)

func TestPipelineViewURL(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		traceID string
		want    string
		wantErr bool
	}{
		{
			name:    "valid https",
			base:    "https://supv.example.com",
			traceID: "abc123",
			want:    "https://supv.example.com/pipelines/abc123",
		},
		{
			name:    "trailing slash trimmed",
			base:    "https://supv.example.com/",
			traceID: "abc123",
			want:    "https://supv.example.com/pipelines/abc123",
		},
		{
			name:    "http localhost with port",
			base:    "http://localhost:8080",
			traceID: "abc123",
			want:    "http://localhost:8080/pipelines/abc123",
		},
		{
			name:    "ipv6 host",
			base:    "https://[::1]",
			traceID: "abc123",
			want:    "https://[::1]/pipelines/abc123",
		},
		{
			name:    "path and query dropped",
			base:    "https://supv.example.com/foo/bar?x=1",
			traceID: "abc123",
			want:    "https://supv.example.com/pipelines/abc123",
		},
		{
			name:    "empty base",
			base:    "",
			traceID: "abc123",
			wantErr: true,
		},
		{
			name:    "missing scheme",
			base:    "supv.example.com",
			traceID: "abc123",
			wantErr: true,
		},
		{
			name:    "ftp scheme rejected",
			base:    "ftp://supv.example.com",
			traceID: "abc123",
			wantErr: true,
		},
		{
			name:    "no host",
			base:    "https:///path",
			traceID: "abc123",
			wantErr: true,
		},
		{
			name:    "empty trace id",
			base:    "https://supv.example.com",
			traceID: "",
			wantErr: true,
		},
		{
			name:    "unsafe trace id charset",
			base:    "https://supv.example.com",
			traceID: "../evil",
			wantErr: true,
		},
		{
			name:    "userinfo rejected",
			base:    "https://user:pass@supv.example.com",
			traceID: "abc123",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PipelineViewURL(tt.base, tt.traceID)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("PipelineViewURL(%q, %q) = %q, want error", tt.base, tt.traceID, got)
				}
				if !errors.Is(err, ErrValidation) {
					t.Fatalf("PipelineViewURL(%q, %q) err = %v, want ErrValidation", tt.base, tt.traceID, err)
				}
				if got != "" {
					t.Fatalf("PipelineViewURL(%q, %q) returned non-empty URL %q on error", tt.base, tt.traceID, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("PipelineViewURL(%q, %q) err = %v, want nil", tt.base, tt.traceID, err)
			}
			if got != tt.want {
				t.Fatalf("PipelineViewURL(%q, %q) = %q, want %q", tt.base, tt.traceID, got, tt.want)
			}
		})
	}
}
