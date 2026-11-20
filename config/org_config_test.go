package config

import (
	"strings"
	"testing"
)

func TestLoadOrgDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Org.Default.DisplayName != "Default" {
		t.Fatalf("display name = %q", cfg.Org.Default.DisplayName)
	}
	if cfg.Org.DefaultQuotas.MaxGroups.Hard != 0 || cfg.Org.DefaultQuotas.MaxPipelineRetention != 0 {
		t.Fatalf("default quotas = %+v, want unlimited", cfg.Org.DefaultQuotas)
	}
}

func TestLoadOrgEnvOverrides(t *testing.T) {
	t.Setenv("DAGGER_KUBERNETES_ORG_DEFAULT_DISPLAY_NAME", "Corp HQ")
	t.Setenv("DAGGER_KUBERNETES_ORG_DEFAULT_QUOTAS_MAX_GROUPS_SOFT", "2")
	t.Setenv("DAGGER_KUBERNETES_ORG_DEFAULT_QUOTAS_MAX_GROUPS_HARD", "5")
	t.Setenv("DAGGER_KUBERNETES_ORG_DEFAULT_QUOTAS_MAX_PIPELINE_RETENTION", "48h")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Org.Default.DisplayName != "Corp HQ" {
		t.Fatalf("display name = %q", cfg.Org.Default.DisplayName)
	}
	if cfg.Org.DefaultQuotas.MaxGroups.Soft != 2 || cfg.Org.DefaultQuotas.MaxGroups.Hard != 5 {
		t.Fatalf("max_groups = %+v", cfg.Org.DefaultQuotas.MaxGroups)
	}
	if cfg.Org.DefaultQuotas.MaxPipelineRetention.Hours() != 48 {
		t.Fatalf("retention = %v", cfg.Org.DefaultQuotas.MaxPipelineRetention)
	}
}

func TestLoadOrgValidation(t *testing.T) {
	t.Setenv("DAGGER_KUBERNETES_ORG_DEFAULT_DISPLAY_NAME", strings.Repeat("x", 129))
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "org.default.display_name") {
		t.Fatalf("long display name err = %v", err)
	}
}

func TestLoadOrgRejectsSoftAboveHard(t *testing.T) {
	t.Setenv("DAGGER_KUBERNETES_ORG_DEFAULT_QUOTAS_MAX_PROJECTS_SOFT", "9")
	t.Setenv("DAGGER_KUBERNETES_ORG_DEFAULT_QUOTAS_MAX_PROJECTS_HARD", "3")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "soft limit") {
		t.Fatalf("soft>hard err = %v", err)
	}
}

func TestOrgDefaultNameKeyAbsent(t *testing.T) {
	// org.default.name must never exist: slugs are immutable and only the
	// display name is configurable.
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Org.Default.DisplayName == "" {
		t.Fatal("display name must default")
	}
}
