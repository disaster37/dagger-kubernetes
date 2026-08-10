package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// groupCols is the canonical column list shared by all group queries.
const groupCols = `id, name, description, max_runner_sessions, agent_available, auto_assign_pattern, created_at, updated_at`

// GroupRepo is the SQLite implementation of domain.GroupRepository.
type GroupRepo struct {
	db *sql.DB
}

var _ domain.GroupRepository = (*GroupRepo)(nil)

// NewGroupRepo returns a GroupRepo backed by db.
func NewGroupRepo(db *sql.DB) *GroupRepo {
	return &GroupRepo{db: db}
}

func (r *GroupRepo) Create(ctx context.Context, g *domain.Group) error {
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now().UTC()
	}
	if g.UpdatedAt.IsZero() {
		g.UpdatedAt = g.CreatedAt
	}
	_, err := r.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO groups(%s) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, groupCols),
		g.ID, g.Name, g.Description, g.MaxRunnerSessions, boolToInt(g.AgentAvailable), g.AutoAssignPattern, g.CreatedAt, g.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create group %s: %w", g.Name, err)
	}
	return nil
}

func (r *GroupRepo) Get(ctx context.Context, id string) (*domain.Group, error) {
	return r.queryGroup(ctx, fmt.Sprintf("get group %s", id), fmt.Sprintf(`SELECT %s FROM groups WHERE id = ?`, groupCols), id)
}

func (r *GroupRepo) GetByName(ctx context.Context, name string) (*domain.Group, error) {
	return r.queryGroup(ctx, fmt.Sprintf("get group by name %s", name), fmt.Sprintf(`SELECT %s FROM groups WHERE name = ?`, groupCols), name)
}

// queryGroup runs a single-row group query and wraps scan errors with label.
func (r *GroupRepo) queryGroup(ctx context.Context, label, query string, args ...any) (*domain.Group, error) {
	g, err := scanGroup(r.db.QueryRowContext(ctx, query, args...))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	return g, nil
}

func (r *GroupRepo) List(ctx context.Context) ([]*domain.Group, error) {
	return r.queryGroups(ctx, "list groups", fmt.Sprintf(`SELECT %s FROM groups ORDER BY id ASC`, groupCols))
}

func (r *GroupRepo) Update(ctx context.Context, g *domain.Group) error {
	g.UpdatedAt = time.Now().UTC()
	res, err := r.db.ExecContext(ctx, `UPDATE groups SET name = ?, description = ?, max_runner_sessions = ?, agent_available = ?, auto_assign_pattern = ?, updated_at = ? WHERE id = ?`,
		g.Name, g.Description, g.MaxRunnerSessions, boolToInt(g.AgentAvailable), g.AutoAssignPattern, g.UpdatedAt, g.ID)
	if err != nil {
		return fmt.Errorf("update group %s: %w", g.ID, err)
	}
	return checkUpdated(res, "update group", g.ID)
}

func (r *GroupRepo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM groups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete group %s: %w", id, err)
	}
	return checkUpdated(res, "delete group", id)
}

// SetMembers replaces the full membership of groupID with userIDs in a single
// transaction.
func (r *GroupRepo) SetMembers(ctx context.Context, groupID string, userIDs []string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set members: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM user_groups WHERE group_id = ?`, groupID); err != nil {
		return fmt.Errorf("clear members for group %s: %w", groupID, err)
	}
	now := time.Now().UTC()
	for _, uid := range userIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_groups(user_id, group_id, created_at) VALUES(?, ?, ?)`, uid, groupID, now); err != nil {
			return fmt.Errorf("insert membership %s/%s: %w", groupID, uid, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit set members: %w", err)
	}
	return nil
}

func (r *GroupRepo) Members(ctx context.Context, groupID string) ([]*domain.User, error) {
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s
		FROM users u JOIN user_groups ug ON u.id = ug.user_id WHERE ug.group_id = ? ORDER BY u.username ASC`, prefixedColumns("u", userCols)), groupID)
	if err != nil {
		return nil, fmt.Errorf("members for group %s: %w", groupID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []*domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("members rows: %w", err)
	}
	return out, nil
}

func (r *GroupRepo) GroupsForUser(ctx context.Context, userID string) ([]*domain.Group, error) {
	label := fmt.Sprintf("groups for user %s", userID)
	return r.queryGroups(ctx, label, fmt.Sprintf(`SELECT %s
		FROM groups g JOIN user_groups ug ON g.id = ug.group_id WHERE ug.user_id = ? ORDER BY g.id ASC`, prefixedColumns("g", groupCols)), userID)
}

// queryGroups runs a multi-row group query, wrapping errors with label.
func (r *GroupRepo) queryGroups(ctx context.Context, label, query string, args ...any) ([]*domain.Group, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	defer func() { _ = rows.Close() }()

	var out []*domain.Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, fmt.Errorf("scan group: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s rows: %w", label, err)
	}
	return out, nil
}

// AllMemberships returns a map of groupID -> member userIDs.
func (r *GroupRepo) AllMemberships(ctx context.Context) (map[string][]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT group_id, user_id FROM user_groups`)
	if err != nil {
		return nil, fmt.Errorf("all memberships: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string][]string)
	for rows.Next() {
		var gid, uid string
		if err := rows.Scan(&gid, &uid); err != nil {
			return nil, fmt.Errorf("scan membership: %w", err)
		}
		out[gid] = append(out[gid], uid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memberships rows: %w", err)
	}
	// Stable ordering for deterministic quota accounting.
	for k := range out {
		sort.Strings(out[k])
	}
	return out, nil
}

func scanGroup(row scanner) (*domain.Group, error) {
	g := &domain.Group{}
	var agent int
	err := row.Scan(&g.ID, &g.Name, &g.Description, &g.MaxRunnerSessions, &agent, &g.AutoAssignPattern, &g.CreatedAt, &g.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("group: %w", domain.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	g.AgentAvailable = agent != 0
	return g, nil
}
