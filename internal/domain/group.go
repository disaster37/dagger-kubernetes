package domain

import (
	"context"
	"time"
)

// Group is a collection of users sharing quota and project visibility.
type Group struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	Description       string    `json:"description"`
	MaxRunnerSessions int       `json:"max_runner_sessions"` // 0 = unlimited
	AgentAvailable    bool      `json:"agent_available"`
	AutoAssignPattern string    `json:"auto_assign_pattern,omitempty"` // regex vs project name; empty = off
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// GroupRepository is the persistence interface for groups and memberships.
type GroupRepository interface {
	Create(ctx context.Context, g *Group) error
	Get(ctx context.Context, id string) (*Group, error)
	GetByName(ctx context.Context, name string) (*Group, error)
	List(ctx context.Context) ([]*Group, error)
	Update(ctx context.Context, g *Group) error
	Delete(ctx context.Context, id string) error
	SetMembers(ctx context.Context, groupID string, userIDs []string) error
	Members(ctx context.Context, groupID string) ([]*User, error)
	GroupsForUser(ctx context.Context, userID string) ([]*Group, error)
	AllMemberships(ctx context.Context) (map[string][]string, error)
}
