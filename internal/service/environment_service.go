package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// EnvironmentService manages protected deployment environments.
type EnvironmentService struct {
	repo   domain.EnvironmentRepository
	logger *logrus.Logger
}

// NewEnvironmentService returns an EnvironmentService.
func NewEnvironmentService(repo domain.EnvironmentRepository, logger *logrus.Logger) *EnvironmentService {
	return &EnvironmentService{repo: repo, logger: logger}
}

// EnvironmentInput is the create/update payload.
type EnvironmentInput struct {
	ApproverGroupIDs    []string
	PreventSelfApproval bool
	ApprovalTimeout     time.Duration
}

// Upsert creates or updates a protected environment.
func (s *EnvironmentService) Upsert(ctx context.Context, orgID, name string, in EnvironmentInput) (*domain.Environment, error) {
	orgID = strings.TrimSpace(orgID)
	name = strings.ToLower(strings.TrimSpace(name))
	if orgID == "" || name == "" {
		return nil, fmt.Errorf("%w: org and environment name are required", domain.ErrValidation)
	}
	if strings.ContainsAny(name, "/|") {
		return nil, fmt.Errorf("%w: invalid environment name", domain.ErrValidation)
	}
	now := time.Now().UTC()
	env := &domain.Environment{
		ID:                  fmt.Sprintf("%s/%s", orgID, name),
		OrgID:               orgID,
		Name:                name,
		ApproverGroupIDs:    append([]string(nil), in.ApproverGroupIDs...),
		PreventSelfApproval: in.PreventSelfApproval,
		ApprovalTimeout:     in.ApprovalTimeout,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	if existing, err := s.repo.Get(ctx, orgID, name); err == nil {
		env.CreatedAt = existing.CreatedAt
	}
	if err := s.repo.Upsert(ctx, env); err != nil {
		return nil, err
	}
	return env, nil
}

// Get returns a protected environment.
func (s *EnvironmentService) Get(ctx context.Context, orgID, name string) (*domain.Environment, error) {
	return s.repo.Get(ctx, orgID, name)
}

// List returns an org's protected environments.
func (s *EnvironmentService) List(ctx context.Context, orgID string) ([]*domain.Environment, error) {
	return s.repo.ListByOrg(ctx, orgID)
}

// Delete removes a protected environment.
func (s *EnvironmentService) Delete(ctx context.Context, orgID, name string) error {
	return s.repo.Delete(ctx, orgID, name)
}
