package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// ProjectService implements project CRUD and assignment.
type ProjectService struct {
	projects domain.ProjectRepository
	groups   domain.GroupRepository
	logger   *logrus.Logger
}

// NewProjectService returns a ProjectService.
func NewProjectService(projects domain.ProjectRepository, groups domain.GroupRepository, logger *logrus.Logger) *ProjectService {
	return &ProjectService{projects: projects, groups: groups, logger: logger}
}

// maxProjectNameLen bounds project names (repo slugs in practice) so neither
// admin input nor OTLP auto-creation can store unbounded values (CWE-770).
const maxProjectNameLen = 256

// Create creates a new project. When groupID is non-empty, the group must
// exist.
func (s *ProjectService) Create(ctx context.Context, name, groupID string) (*domain.Project, error) {
	if name == "" {
		return nil, fmt.Errorf("project name is required: %w", domain.ErrValidation)
	}
	if len(name) > maxProjectNameLen {
		return nil, fmt.Errorf("project name must be at most %d characters: %w", maxProjectNameLen, domain.ErrValidation)
	}
	if err := s.requireGroup(ctx, groupID); err != nil {
		return nil, err
	}
	p := &domain.Project{
		ID:      newID(),
		Name:    name,
		GroupID: groupID,
	}
	if err := s.projects.Create(ctx, p); err != nil {
		return nil, err
	}
	s.logger.WithFields(logrus.Fields{
		"project_id": p.ID,
		"name":       p.Name,
	}).Info("project created")
	return p, nil
}

// Get returns a project by id.
func (s *ProjectService) Get(ctx context.Context, id string) (*domain.Project, error) {
	return s.projects.Get(ctx, id)
}

// List returns all projects.
func (s *ProjectService) List(ctx context.Context) ([]*domain.Project, error) {
	return s.projects.List(ctx)
}

// Delete removes a project.
func (s *ProjectService) Delete(ctx context.Context, id string) error {
	return s.projects.Delete(ctx, id)
}

// Assign sets (or clears, when groupID is empty) a project's group.
func (s *ProjectService) Assign(ctx context.Context, id, groupID string) (*domain.Project, error) {
	p, err := s.projects.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.requireGroup(ctx, groupID); err != nil {
		return nil, err
	}
	p.GroupID = groupID
	if err := s.projects.Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// GetOrCreateByName returns the existing project for name, or creates one.
// Race-safe: on a unique-constraint violation it retries GetByName once.
func (s *ProjectService) GetOrCreateByName(ctx context.Context, name string) (*domain.Project, error) {
	if name == "" {
		return nil, fmt.Errorf("project name is required: %w", domain.ErrValidation)
	}
	if len(name) > maxProjectNameLen {
		return nil, fmt.Errorf("project name must be at most %d characters: %w", maxProjectNameLen, domain.ErrValidation)
	}
	if p, err := s.projects.GetByName(ctx, name); err == nil {
		return p, nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}

	p := &domain.Project{ID: newID(), Name: name}
	if err := s.projects.Create(ctx, p); err != nil {
		// Collision: another writer created it; fetch.
		if again, gerr := s.projects.GetByName(ctx, name); gerr == nil {
			return again, nil
		}
		return nil, err
	}
	return p, nil
}

// requireGroup verifies that a non-empty groupID refers to an existing group.
func (s *ProjectService) requireGroup(ctx context.Context, groupID string) error {
	if groupID == "" {
		return nil
	}
	if _, err := s.groups.Get(ctx, groupID); err != nil {
		return fmt.Errorf("group: %w", err)
	}
	return nil
}
