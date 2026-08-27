package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

var groupNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// GroupInput is the validated payload for creating/updating a group.
type GroupInput struct {
	Name              string
	Description       string
	MaxRunnerSessions int
	AgentAvailable    bool
	AutoAssignPattern string
}

// GroupService implements group CRUD and membership.
type GroupService struct {
	groups domain.GroupRepository
	users  domain.UserRepository
	logger *logrus.Logger
}

// NewGroupService returns a GroupService. users is used to validate membership
// IDs in SetMembers.
func NewGroupService(groups domain.GroupRepository, users domain.UserRepository, logger *logrus.Logger) *GroupService {
	return &GroupService{groups: groups, users: users, logger: logger}
}

// Create creates a new group.
func (s *GroupService) Create(ctx context.Context, in GroupInput) (*domain.Group, error) {
	if err := validateGroupInput(in); err != nil {
		return nil, err
	}
	g := &domain.Group{
		ID:                newID(),
		Name:              in.Name,
		Description:       in.Description,
		MaxRunnerSessions: in.MaxRunnerSessions,
		AgentAvailable:    in.AgentAvailable,
		AutoAssignPattern: in.AutoAssignPattern,
	}
	if err := s.groups.Create(ctx, g); err != nil {
		return nil, err
	}
	s.logger.WithField("group_id", g.ID).Info("group created")
	return g, nil
}

// Get returns a group by id.
func (s *GroupService) Get(ctx context.Context, id string) (*domain.Group, error) {
	return s.groups.Get(ctx, id)
}

// GetByName returns a group by name.
func (s *GroupService) GetByName(ctx context.Context, name string) (*domain.Group, error) {
	return s.groups.GetByName(ctx, name)
}

// List returns all groups.
func (s *GroupService) List(ctx context.Context) ([]*domain.Group, error) {
	return s.groups.List(ctx)
}

// Update modifies a group.
func (s *GroupService) Update(ctx context.Context, id string, in GroupInput) (*domain.Group, error) {
	if err := validateGroupInput(in); err != nil {
		return nil, err
	}
	g, err := s.groups.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	g.Name = in.Name
	g.Description = in.Description
	g.MaxRunnerSessions = in.MaxRunnerSessions
	g.AgentAvailable = in.AgentAvailable
	g.AutoAssignPattern = in.AutoAssignPattern
	if err := s.groups.Update(ctx, g); err != nil {
		return nil, err
	}
	return g, nil
}

// Delete removes a group (memberships cascade; projects/trace_meta group_id
// are set to NULL via ON DELETE SET NULL).
func (s *GroupService) Delete(ctx context.Context, id string) error {
	return s.groups.Delete(ctx, id)
}

// SetMembers replaces the full membership of a group after verifying each
// user exists.
func (s *GroupService) SetMembers(ctx context.Context, groupID string, userIDs []string) error {
	if _, err := s.groups.Get(ctx, groupID); err != nil {
		return err
	}
	seen := make(map[string]bool, len(userIDs))
	ids := make([]string, 0, len(userIDs))
	for _, uid := range userIDs {
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		if _, err := s.users.Get(ctx, uid); err != nil {
			return fmt.Errorf("user %s: %w", uid, err)
		}
		ids = append(ids, uid)
	}
	return s.groups.SetMembers(ctx, groupID, ids)
}

// SetUserGroups replaces a user's group memberships (full replace). Each
// group in groupIDs must exist; the user is added to those groups and removed
// from any groups they currently belong to but are not in the list.
func (s *GroupService) SetUserGroups(ctx context.Context, userID string, groupIDs []string) error {
	if _, err := s.users.Get(ctx, userID); err != nil {
		return fmt.Errorf("user %s: %w", userID, err)
	}
	wanted := make(map[string]bool, len(groupIDs))
	for _, gid := range groupIDs {
		if gid == "" || wanted[gid] {
			continue
		}
		if _, err := s.groups.Get(ctx, gid); err != nil {
			return fmt.Errorf("group %s: %w", gid, err)
		}
		wanted[gid] = true
	}

	current, err := s.groups.GroupsForUser(ctx, userID)
	if err != nil {
		return err
	}
	currentSet := make(map[string]bool, len(current))
	for _, g := range current {
		currentSet[g.ID] = true
	}

	for gid := range wanted {
		if !currentSet[gid] {
			if err := addGroupMember(ctx, s.groups, gid, userID); err != nil {
				return err
			}
		}
	}
	for gid := range currentSet {
		if !wanted[gid] {
			if err := removeGroupMember(ctx, s.groups, gid, userID); err != nil {
				return err
			}
		}
	}
	return nil
}

// EnsureMember adds userID to the group identified by groupID if they are not
// already a member. Idempotent — safe to call when the user is already present.
func (s *GroupService) EnsureMember(ctx context.Context, groupID, userID string) error {
	if _, err := s.groups.Get(ctx, groupID); err != nil {
		return fmt.Errorf("group %s: %w", groupID, err)
	}
	if _, err := s.users.Get(ctx, userID); err != nil {
		return fmt.Errorf("user %s: %w", userID, err)
	}
	members, err := s.groups.Members(ctx, groupID)
	if err != nil {
		return fmt.Errorf("list members of %s: %w", groupID, err)
	}
	for _, m := range members {
		if m.ID == userID {
			return nil // already a member
		}
	}
	return addGroupMember(ctx, s.groups, groupID, userID)
}

// Members returns the users in a group.
func (s *GroupService) Members(ctx context.Context, groupID string) ([]*domain.User, error) {
	return s.groups.Members(ctx, groupID)
}

// GroupsForUser returns the groups a user belongs to.
func (s *GroupService) GroupsForUser(ctx context.Context, userID string) ([]*domain.Group, error) {
	return s.groups.GroupsForUser(ctx, userID)
}

func validateGroupInput(in GroupInput) error {
	if !groupNameRe.MatchString(in.Name) {
		return fmt.Errorf("group name must match ^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$: %w", domain.ErrValidation)
	}
	if in.MaxRunnerSessions < 0 {
		return fmt.Errorf("max_runner_sessions must be >= 0: %w", domain.ErrValidation)
	}
	if in.AutoAssignPattern != "" {
		if _, err := regexp.Compile(in.AutoAssignPattern); err != nil {
			return fmt.Errorf("invalid auto_assign_pattern %q: %w", in.AutoAssignPattern, errors.Join(domain.ErrValidation, err))
		}
	}
	return nil
}

// membershipStore is the minimal interface needed to rewrite a group's
// membership list. Both domain.GroupRepository and *GroupService satisfy it.
type membershipStore interface {
	Members(ctx context.Context, groupID string) ([]*domain.User, error)
	SetMembers(ctx context.Context, groupID string, userIDs []string) error
}

// addGroupMember appends userID to groupID's membership, keeping existing
// members (idempotent when the user is already a member).
func addGroupMember(ctx context.Context, groups membershipStore, groupID, userID string) error {
	ids, err := memberIDsWithout(ctx, groups, groupID, userID)
	if err != nil {
		return err
	}
	return groups.SetMembers(ctx, groupID, append(ids, userID))
}

// removeGroupMember removes userID from groupID's membership.
func removeGroupMember(ctx context.Context, groups membershipStore, groupID, userID string) error {
	ids, err := memberIDsWithout(ctx, groups, groupID, userID)
	if err != nil {
		return err
	}
	return groups.SetMembers(ctx, groupID, ids)
}

// memberIDsWithout returns the member IDs of groupID excluding userID.
func memberIDsWithout(ctx context.Context, groups membershipStore, groupID, userID string) ([]string, error) {
	members, err := groups.Members(ctx, groupID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(members))
	for _, m := range members {
		if m.ID != userID {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}
