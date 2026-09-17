package integration

import (
	"context"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// TestProjectMappingIngestAssigns drives the real Raft-backed attribution
// pipeline with a config-driven project→group mapping and asserts the trace is
// attributed to the mapped group and the project row is persisted as assigned.
// No HTTP listener is needed: the mapping is applied inside AttributionService.
func TestProjectMappingIngestAssigns(t *testing.T) {
	logger := observ.NewTestLogger()
	store := newIntegrationStore(t)
	ctx := context.Background()

	groupRepo := repository.NewGroupRepo(store)
	projectRepo := repository.NewProjectRepo(store)
	traceMetaRepo := repository.NewTraceMetaRepo(store)

	groupsSvc := service.NewGroupService(groupRepo, repository.NewUserRepo(store), logger)
	projectsSvc := service.NewProjectService(projectRepo, groupRepo, logger)
	attributionSvc := service.NewAttributionService(projectsSvc, groupRepo, traceMetaRepo, logger)

	mapper, err := service.NewProjectMapper([]domain.ProjectMappingRule{
		{Pattern: `^github\.com/acme/.*`, Group: "acme"},
	})
	if err != nil {
		t.Fatalf("NewProjectMapper: %v", err)
	}
	attributionSvc.SetProjectMapper(mapper)

	g, err := groupsSvc.Create(ctx, service.GroupInput{Name: "acme", AgentAvailable: true})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}

	attributionSvc.Ingest(ctx, "trace-1", "user-1", "github.com/acme/api", "", "github", "v0.21.4", "success", 0, time.Now().UTC())

	meta, err := traceMetaRepo.Get(ctx, "trace-1")
	if err != nil {
		t.Fatalf("traceMeta.Get: %v", err)
	}
	if meta.GroupID != g.ID {
		t.Fatalf("trace group_id = %q, want %s (mapped)", meta.GroupID, g.ID)
	}
	proj, err := projectRepo.GetByName(ctx, "github.com/acme/api")
	if err != nil {
		t.Fatalf("project GetByName: %v", err)
	}
	if proj.GroupID != g.ID {
		t.Fatalf("project group_id = %q, want %s (persisted)", proj.GroupID, g.ID)
	}
}

// TestProjectMappingIngestMissingGroupSkipped verifies that a matched rule whose
// target group does not exist is authoritative: the project is skipped and does
// NOT fall through to a matching auto_assign_pattern group.
func TestProjectMappingIngestMissingGroupSkipped(t *testing.T) {
	logger := observ.NewTestLogger()
	store := newIntegrationStore(t)
	ctx := context.Background()

	groupRepo := repository.NewGroupRepo(store)
	projectRepo := repository.NewProjectRepo(store)
	traceMetaRepo := repository.NewTraceMetaRepo(store)

	groupsSvc := service.NewGroupService(groupRepo, repository.NewUserRepo(store), logger)
	projectsSvc := service.NewProjectService(projectRepo, groupRepo, logger)
	attributionSvc := service.NewAttributionService(projectsSvc, groupRepo, traceMetaRepo, logger)

	mapper, err := service.NewProjectMapper([]domain.ProjectMappingRule{
		{Pattern: `^github\.com/acme/.*`, Group: "missing"},
	})
	if err != nil {
		t.Fatalf("NewProjectMapper: %v", err)
	}
	attributionSvc.SetProjectMapper(mapper)

	if _, err := groupsSvc.Create(ctx, service.GroupInput{Name: "auto", AgentAvailable: true, AutoAssignPattern: `^github\.com/.*`}); err != nil {
		t.Fatalf("create auto group: %v", err)
	}

	attributionSvc.Ingest(ctx, "trace-2", "user-1", "github.com/acme/api", "", "github", "v0.21.4", "success", 0, time.Now().UTC())

	meta, err := traceMetaRepo.Get(ctx, "trace-2")
	if err != nil {
		t.Fatalf("traceMeta.Get: %v", err)
	}
	if meta.GroupID != "" {
		t.Fatalf("trace group_id = %q, want empty (missing target skipped, no fallthrough)", meta.GroupID)
	}
}
