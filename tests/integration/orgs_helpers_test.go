package integration

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// newIntegrationOrgService wires and bootstraps the organization service used
// by the black-box harnesses (single bootstrap org, unlimited quotas).
func newIntegrationOrgService(t *testing.T, store *repository.RaftStore, groups domain.GroupRepository, tokens *service.TokenService, logger *logrus.Logger) (orgSvc *service.OrgService, orgID string) {
	t.Helper()
	svc := service.NewOrgService(repository.NewOrgRepo(store), groups, tokens, logger).
		WithMetaStore(repository.NewMetaStore(store)).
		WithDefaultQuotas(domain.OrgQuotas{})
	boot, err := svc.EnsureBootstrap(context.Background(), domain.DefaultOrgDisplayName)
	if err != nil {
		t.Fatalf("bootstrap org: %v", err)
	}
	return svc, boot.ID
}
