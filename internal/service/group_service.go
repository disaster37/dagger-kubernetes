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

// ValidateGroupName reports whether name satisfies the supervisor group-name
// rule (^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$).
func ValidateGroupName(name string) bool { return groupNameRe.MatchString(name) }

// GroupInput is the validated payload for creating/updating a group.
type GroupInput struct {
	Name              string
	Description       string
	MaxRunnerSessions int
	AgentAvailable    bool
	AutoAssignPattern string
}

// GroupService implements org-scoped group CRUD and membership.
type GroupService struct {
	groups domain.GroupRepository
	users  domain.UserRepository
	quota  *QuotaService
	logger *logrus.Logger
}

// NewGroupService returns a GroupService.
func NewGroupService(groups domain.GroupRepository, users domain.UserRepository, logger *logrus.Logger) *GroupService {
	return &GroupService{groups: groups, users: users, logger: logger}
}

// WithQuotaService wires org-level quota enforcement into group creation and
// membership changes.
func (s *GroupService) WithQuotaService(q *QuotaService) *GroupService {
	s.quota = q
	return s
}

// Create creates a new group inside orgID.
func (s *GroupService) Create(ctx context.Context, orgID string, in GroupInput) (*domain.Group, error) {
	if err := validateGroupInput(in); err != nil {
		return nil, err
	}
	if warning, err := s.checkQuota(ctx, &domain.Identity{}, orgID, domain.QuotaGroups, 1); err != nil {
		return nil, err
	} else if warning != "" {
		s.logger.WithField("warning", warning).Warn("org quota soft limit exceeded")
	}
	g := &domain.Group{
		ID:                newID(),
		OrgID:             orgID,
		Name:              in.Name,
		Description:       in.Description,
		MaxRunnerSessions: in.MaxRunnerSessions,
		AgentAvailable:    in.AgentAvailable,
		AutoAssignPattern: in.AutoAssignPattern,
	}
	if err := s.groups.Create(ctx, g); err != nil {
		return nil, err
	}
	s.logger.WithFields(logrus.Fields{"org_id": orgID, "group_id": g.ID}).Info("group created")
	return g, nil
}

// GetInOrg returns a group by id, rejecting cross-org access.
func (s *GroupService) GetInOrg(ctx context.Context, orgID, id string) (*domain.Group, error) {
	g, err := s.groups.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if g.OrgID != orgID {
		return nil, fmt.Errorf("group %s: %w", id, domain.ErrNotFound)
	}
	return g, nil
}

// GetByName returns a group by name inside orgID.
func (s *GroupService) GetByName(ctx context.Context, orgID, name string) (*domain.Group, error) {
	return s.groups.GetByName(ctx, orgID, name)
}

// ListByOrg returns orgID's groups.
func (s *GroupService) ListByOrg(ctx context.Context, orgID string) ([]*domain.Group, error) {
	return s.groups.ListByOrg(ctx, orgID)
}

// List returns all groups (internal cross-org reads only).
func (s *GroupService) List(ctx context.Context) ([]*domain.Group, error) {
	return s.groups.List(ctx)
}

// Update modifies a group inside orgID.
func (s *GroupService) Update(ctx context.Context, orgID, id string, in GroupInput) (*domain.Group, error) {
	if err := validateGroupInput(in); err != nil {
		return nil, err
	}
	g, err := s.GetInOrg(ctx, orgID, id)
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

// Delete removes a group inside orgID.
func (s *GroupService) Delete(ctx context.Context, orgID, id string) error {
	if _, err := s.GetInOrg(ctx, orgID, id); err != nil {
		return err
	}
	return s.groups.Delete(ctx, id)
}

// SetMembers replaces the full membership of a group after verifying each user
// exists.
func (s *GroupService) SetMembers(ctx context.Context, orgID, groupID string, userIDs []string) error {
	if _, err := s.GetInOrg(ctx, orgID, groupID); err != nil {
		return err
	}
	ids, err := s.validateUsers(ctx, userIDs)
	if err != nil {
		return err
	}
	if warning, qErr := s.checkQuota(ctx, &domain.Identity{}, orgID, domain.QuotaMembers, int64(len(ids))); qErr != nil {
		return qErr
	} else if warning != "" {
		s.logger.WithField("warning", warning).Warn("org quota soft limit exceeded")
	}
	return s.groups.SetMembers(ctx, groupID, ids)
}

// SetUserGroupsInOrg replaces a user's group memberships inside orgID only.
func (s *GroupService) SetUserGroupsInOrg(ctx context.Context, orgID, userID string, groupIDs []string) error {
	if _, err := s.users.Get(ctx, userID); err != nil {
		return fmt.Errorf("user %s: %w", userID, err)
	}
	wanted := make(map[string]bool, len(groupIDs))
	for _, gid := range groupIDs {
		if gid == "" || wanted[gid] {
			continue
		}
		if _, err := s.GetInOrg(ctx, orgID, gid); err != nil {
			return fmt.Errorf("group %s: %w", gid, err)
		}
		wanted[gid] = true
	}
	current, err := s.groups.GroupsForUserInOrg(ctx, orgID, userID)
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

// EnsureMember adds userID to groupID if they are not already a member.
func (s *GroupService) EnsureMember(ctx context.Context, orgID, groupID, userID string) error {
	if _, err := s.GetInOrg(ctx, orgID, groupID); err != nil {
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
	if warning, qErr := s.checkQuota(ctx, &domain.Identity{}, orgID, domain.QuotaMembers, 1); qErr != nil {
		return qErr
	} else if warning != "" {
		s.logger.WithField("warning", warning).Warn("org quota soft limit exceeded")
	}
	return addGroupMember(ctx, s.groups, groupID, userID)
}

// Members returns the users in a group.
func (s *GroupService) Members(ctx context.Context, groupID string) ([]*domain.User, error) {
	return s.groups.Members(ctx, groupID)
}

// GroupsForUser returns the groups a user belongs to across all orgs.
func (s *GroupService) GroupsForUser(ctx context.Context, userID string) ([]*domain.Group, error) {
	return s.groups.GroupsForUser(ctx, userID)
}

// GroupsForUserInOrg returns a user's groups inside orgID.
func (s *GroupService) GroupsForUserInOrg(ctx context.Context, orgID, userID string) ([]*domain.Group, error) {
	return s.groups.GroupsForUserInOrg(ctx, orgID, userID)
}

func (s *GroupService) checkQuota(ctx context.Context, id *domain.Identity, orgID string, res domain.QuotaResource, delta int64) (string, error) {
	if s.quota == nil {
		return "", nil
	}
	return s.quota.CheckResource(ctx, id, orgID, res, delta)
}

func (s *GroupService) validateUsers(ctx context.Context, userIDs []string) ([]string, error) {
	seen := make(map[string]bool, len(userIDs))
	ids := make([]string, 0, len(userIDs))
	for _, uid := range userIDs {
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		if _, err := s.users.Get(ctx, uid); err != nil {
			return nil, fmt.Errorf("user %s: %w", uid, err)
		}
		ids = append(ids, uid)
	}
	return ids, nil
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
// membership list.
type membershipStore interface {
	Members(ctx context.Context, groupID string) ([]*domain.User, error)
	SetMembers(ctx context.Context, groupID string, userIDs []string) error
}

// addGroupMember appends userID to groupID's membership.
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
