package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// ProjectService implements org-scoped project CRUD and assignment.
type ProjectService struct {
	projects domain.ProjectRepository
	groups   domain.GroupRepository
	quota    *QuotaService
	logger   *logrus.Logger
}

// NewProjectService returns a ProjectService.
func NewProjectService(projects domain.ProjectRepository, groups domain.GroupRepository, logger *logrus.Logger) *ProjectService {
	return &ProjectService{projects: projects, groups: groups, logger: logger}
}

// WithQuotaService wires org-level quota enforcement into project creation.
func (s *ProjectService) WithQuotaService(q *QuotaService) *ProjectService {
	s.quota = q
	return s
}

// maxProjectNameLen bounds project names (repo slugs in practice).
const maxProjectNameLen = 256

func validateProjectName(name string) error {
	if name == "" {
		return fmt.Errorf("project name is required: %w", domain.ErrValidation)
	}
	if len(name) > maxProjectNameLen {
		return fmt.Errorf("project name must be at most %d characters: %w", maxProjectNameLen, domain.ErrValidation)
	}
	return nil
}

// Create creates a new project inside orgID.
func (s *ProjectService) Create(ctx context.Context, orgID, name, groupID string) (*domain.Project, error) {
	if err := validateProjectName(name); err != nil {
		return nil, err
	}
	if err := s.requireGroup(ctx, orgID, groupID); err != nil {
		return nil, err
	}
	if warning, err := s.checkQuota(ctx, orgID, 1); err != nil {
		return nil, err
	} else if warning != "" {
		s.logger.WithField("warning", warning).Warn("org quota soft limit exceeded")
	}
	p := &domain.Project{
		ID:      newID(),
		OrgID:   orgID,
		Name:    name,
		GroupID: groupID,
	}
	if err := s.projects.Create(ctx, p); err != nil {
		return nil, err
	}
	s.logger.WithFields(logrus.Fields{
		"org_id":     orgID,
		"project_id": p.ID,
		"name":       p.Name,
	}).Info("project created")
	return p, nil
}

// GetInOrg returns a project by id, rejecting cross-org access.
func (s *ProjectService) GetInOrg(ctx context.Context, orgID, id string) (*domain.Project, error) {
	p, err := s.projects.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.OrgID != orgID {
		return nil, fmt.Errorf("project %s: %w", id, domain.ErrNotFound)
	}
	return p, nil
}

// ListByOrg returns orgID's projects.
func (s *ProjectService) ListByOrg(ctx context.Context, orgID string) ([]*domain.Project, error) {
	return s.projects.ListByOrg(ctx, orgID)
}

// List returns all projects (internal cross-org reads only).
func (s *ProjectService) List(ctx context.Context) ([]*domain.Project, error) {
	return s.projects.List(ctx)
}

// ListByNameAcrossOrgs returns every project with the given name across orgs.
func (s *ProjectService) ListByNameAcrossOrgs(ctx context.Context, name string) ([]*domain.Project, error) {
	return s.projects.ListByNameAcrossOrgs(ctx, name)
}

// Delete removes a project inside orgID.
func (s *ProjectService) Delete(ctx context.Context, orgID, id string) error {
	if _, err := s.GetInOrg(ctx, orgID, id); err != nil {
		return err
	}
	return s.projects.Delete(ctx, id)
}

// Assign sets (or clears, when groupID is empty) a project's group. The group
// must belong to the same organization.
func (s *ProjectService) Assign(ctx context.Context, orgID, id, groupID string) (*domain.Project, error) {
	p, err := s.GetInOrg(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	if err := s.requireGroup(ctx, orgID, groupID); err != nil {
		return nil, err
	}
	p.GroupID = groupID
	if err := s.projects.Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// GetOrCreateByName returns the existing project for name in orgID, or
// creates one.
func (s *ProjectService) GetOrCreateByName(ctx context.Context, orgID, name string) (*domain.Project, error) {
	if err := validateProjectName(name); err != nil {
		return nil, err
	}
	if p, err := s.projects.GetByName(ctx, orgID, name); err == nil {
		return p, nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}
	p := &domain.Project{ID: newID(), OrgID: orgID, Name: name}
	if err := s.projects.Create(ctx, p); err != nil {
		if again, gerr := s.projects.GetByName(ctx, orgID, name); gerr == nil {
			return again, nil
		}
		return nil, err
	}
	return p, nil
}

func (s *ProjectService) checkQuota(ctx context.Context, orgID string, delta int64) (string, error) {
	if s.quota == nil {
		return "", nil
	}
	return s.quota.CheckResource(ctx, &domain.Identity{}, orgID, domain.QuotaProjects, delta)
}

// requireGroup verifies that a non-empty groupID refers to a group in orgID.
func (s *ProjectService) requireGroup(ctx context.Context, orgID, groupID string) error {
	if groupID == "" {
		return nil
	}
	g, err := s.groups.Get(ctx, groupID)
	if err != nil {
		return fmt.Errorf("group: %w", err)
	}
	if g.OrgID != orgID {
		return fmt.Errorf("%w: group belongs to another organization", domain.ErrValidation)
	}
	return nil
}
