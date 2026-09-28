package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCIConfig writes content to a config file in a fresh temp dir and
// returns its path.
func writeCIConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.app.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadForCIWrapperDefaults(t *testing.T) {
	cfg, err := LoadForCIWrapper(filepath.Join(t.TempDir(), "config.app.yaml"))
	if err != nil {
		t.Fatalf("LoadForCIWrapper with missing file: %v", err)
	}

	if cfg.CI.Jenkins.StepsPollInterval != 2*time.Second {
		t.Fatalf("ci.jenkins.steps_poll_interval default = %v, want 2s", cfg.CI.Jenkins.StepsPollInterval)
	}
	if cfg.CI.Jenkins.StepsMaxDepth != 8 {
		t.Fatalf("ci.jenkins.steps_max_depth default = %d, want 8", cfg.CI.Jenkins.StepsMaxDepth)
	}
	if cfg.Server.PublicURL != "https://supv.example.com" {
		t.Fatalf("server.public_url default = %q, want https://supv.example.com", cfg.Server.PublicURL)
	}
}

func TestLoadForCIWrapperSkipsSupervisorValidation(t *testing.T) {
	// Clear the TestMain-provided S3 prerequisites so the supervisor path
	// reaches the server/public_url validation first.
	t.Setenv("DAGGER_KUBERNETES_CACHE_S3_BUCKET", "")
	t.Setenv("DAGGER_KUBERNETES_CACHE_S3_ENDPOINT", "")
	path := writeCIConfig(t, "server:\n  public_url: \"\"\n")

	if _, err := LoadForCIWrapper(path); err != nil {
		t.Fatalf("LoadForCIWrapper = %v, want nil (supervisor validations skipped)", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load = nil, want supervisor validation error")
	}
	if !strings.Contains(err.Error(), "validate server config") {
		t.Fatalf("Load error = %q, want wrapped server validation message", err.Error())
	}
}

func TestLoadForCIWrapperRejectsInvalidCIConfig(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name:    "zero poll interval",
			content: "ci:\n  jenkins:\n    dynamic_stages: true\n    steps_poll_interval: \"0s\"\n",
			wantErr: "ci.jenkins.steps_poll_interval must be > 0",
		},
		{
			name:    "negative max depth",
			content: "ci:\n  jenkins:\n    steps_max_depth: -1\n",
			wantErr: "ci.jenkins.steps_max_depth must be >= 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadForCIWrapper(writeCIConfig(t, tt.content))
			if err == nil {
				t.Fatal("LoadForCIWrapper = nil, want validation error")
			}
			if !strings.Contains(err.Error(), "validate ci config") ||
				!strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("LoadForCIWrapper error = %q, want wrapped ci validation message containing %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestLoadRejectsMissingS3Bucket(t *testing.T) {
	t.Setenv("DAGGER_KUBERNETES_CACHE_S3_BUCKET", "")
	t.Setenv("DAGGER_KUBERNETES_CACHE_S3_ENDPOINT", "")
	path := writeCIConfig(t, "server:\n  public_url: \"https://supv.example.com\"\n")

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load = nil, want cli validation error")
	}
	if !strings.Contains(err.Error(), "validate cli config") ||
		!strings.Contains(err.Error(), "cli.s3_bucket (or cache.s3.bucket) is required when cli.enabled") {
		t.Fatalf("Load error = %q, want wrapped cli validation message", err.Error())
	}
}
