package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// userCols is the canonical column list shared by all user queries.
const userCols = `id, username, role, password_hash, oauth_provider, oauth_id, created_at, updated_at`

// UserRepo is the SQLite implementation of domain.UserRepository.
type UserRepo struct {
	db *sql.DB
}

var _ domain.UserRepository = (*UserRepo)(nil)

// NewUserRepo returns a UserRepo backed by db.
func NewUserRepo(db *sql.DB) *UserRepo {
	return &UserRepo{db: db}
}

func (r *UserRepo) Create(ctx context.Context, u *domain.User) error {
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	if u.UpdatedAt.IsZero() {
		u.UpdatedAt = u.CreatedAt
	}
	_, err := r.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO users(%s) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, userCols),
		u.ID, u.Username, string(u.Role), u.PasswordHash, u.OAuthProvider, u.OAuthID, u.CreatedAt, u.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create user %s: %w", u.Username, err)
	}
	return nil
}

func (r *UserRepo) Get(ctx context.Context, id string) (*domain.User, error) {
	return r.queryUser(ctx, fmt.Sprintf("get user %s", id), fmt.Sprintf(`SELECT %s FROM users WHERE id = ?`, userCols), id)
}

func (r *UserRepo) GetByUsername(ctx context.Context, username string) (*domain.User, error) {
	return r.queryUser(ctx, fmt.Sprintf("get user by username %s", username), fmt.Sprintf(`SELECT %s FROM users WHERE username = ?`, userCols), username)
}

func (r *UserRepo) GetByOAuth(ctx context.Context, provider, oauthID string) (*domain.User, error) {
	return r.queryUser(ctx, fmt.Sprintf("get user by oauth %s/%s", provider, oauthID), fmt.Sprintf(`SELECT %s FROM users WHERE oauth_provider = ? AND oauth_id = ?`, userCols), provider, oauthID)
}

// queryUser runs a single-row user query and wraps scan errors with label.
func (r *UserRepo) queryUser(ctx context.Context, label, query string, args ...any) (*domain.User, error) {
	u, err := scanUser(r.db.QueryRowContext(ctx, query, args...))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	return u, nil
}

func (r *UserRepo) List(ctx context.Context) ([]*domain.User, error) {
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM users ORDER BY created_at ASC`, userCols))
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list users rows: %w", err)
	}
	return out, nil
}

func (r *UserRepo) Update(ctx context.Context, u *domain.User) error {
	u.UpdatedAt = time.Now().UTC()
	res, err := r.db.ExecContext(ctx, `UPDATE users SET role = ?, password_hash = ?, oauth_provider = ?, oauth_id = ?, updated_at = ? WHERE id = ?`,
		string(u.Role), u.PasswordHash, u.OAuthProvider, u.OAuthID, u.UpdatedAt, u.ID)
	if err != nil {
		return fmt.Errorf("update user %s: %w", u.ID, err)
	}
	return checkUpdated(res, "update user", u.ID)
}

func (r *UserRepo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete user %s: %w", id, err)
	}
	return checkUpdated(res, "delete user", id)
}

func (r *UserRepo) Count(ctx context.Context) (int, error) {
	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

func scanUser(row scanner) (*domain.User, error) {
	u := &domain.User{}
	var role string
	err := row.Scan(&u.ID, &u.Username, &role, &u.PasswordHash, &u.OAuthProvider, &u.OAuthID, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("user: %w", domain.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	u.Role = domain.Role(role)
	return u, nil
}
