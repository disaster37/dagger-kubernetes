package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

var (
	usernameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{1,63}$`)

	minPasswordLen = 8
	// maxPasswordLen is bcrypt's input limit: only the first 72 bytes are
	// hashed, so longer passwords would be silently truncated.
	maxPasswordLen = 72
)

// dummyPasswordHash is a valid bcrypt hash compared against when a login
// attempt targets an unknown user (or a user without a password), so the
// response timing does not reveal whether the username exists (CWE-208).
var dummyPasswordHash = func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("timing-equalization-dummy"), bcrypt.DefaultCost)
	if err != nil {
		panic(fmt.Sprintf("generate dummy hash: %v", err))
	}
	return h
}()

// UserService implements user CRUD and password logic.
type UserService struct {
	users  domain.UserRepository
	groups domain.GroupRepository
	logger *logrus.Logger
}

// NewUserService returns a UserService.
func NewUserService(users domain.UserRepository, groups domain.GroupRepository, logger *logrus.Logger) *UserService {
	return &UserService{users: users, groups: groups, logger: logger}
}

// Create creates a new user with the given credentials and role.
func (s *UserService) Create(ctx context.Context, username, password string, role domain.Role) (*domain.User, error) {
	if err := validateUsername(username); err != nil {
		return nil, err
	}
	if err := validatePassword(password); err != nil {
		return nil, err
	}
	if _, err := domain.ParseRole(string(role)); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrValidation, err)
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}

	u := &domain.User{
		ID:           newID(),
		Username:     username,
		Role:         role,
		PasswordHash: hash,
	}
	if err := s.users.Create(ctx, u); err != nil {
		return nil, err
	}
	s.logger.WithFields(logrus.Fields{
		"user_id":  u.ID,
		"username": u.Username,
	}).Info("user created")
	return u, nil
}

// Authenticate validates the username/password and returns the user. It never
// reveals which part failed, and performs a bcrypt comparison even for
// unknown users so timing does not leak username existence (CWE-208).
func (s *UserService) Authenticate(ctx context.Context, username, password string) (*domain.User, error) {
	u, err := s.users.GetByUsername(ctx, username)
	if err != nil || u.PasswordHash == "" {
		_ = bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte(password))
		return nil, domain.ErrInvalidCredential
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return nil, domain.ErrInvalidCredential
	}
	return u, nil
}

// Get returns a user by id.
func (s *UserService) Get(ctx context.Context, id string) (*domain.User, error) {
	return s.users.Get(ctx, id)
}

// GetByUsername returns a user by username.
func (s *UserService) GetByUsername(ctx context.Context, username string) (*domain.User, error) {
	return s.users.GetByUsername(ctx, username)
}

// List returns all users.
func (s *UserService) List(ctx context.Context) ([]*domain.User, error) {
	return s.users.List(ctx)
}

// UpdateRole changes a user's role.
func (s *UserService) UpdateRole(ctx context.Context, id string, role domain.Role) (*domain.User, error) {
	if _, err := domain.ParseRole(string(role)); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrValidation, err)
	}
	u, err := s.users.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	u.Role = role
	if err := s.users.Update(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// Delete removes a user (cascades tokens + memberships via FK).
func (s *UserService) Delete(ctx context.Context, id string) error {
	u, err := s.users.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := s.users.Delete(ctx, id); err != nil {
		return err
	}
	s.logger.WithFields(logrus.Fields{
		"user_id":  u.ID,
		"username": u.Username,
	}).Info("user deleted")
	return nil
}

// ResetPassword sets a new password for a user (admin-set; no current pw check).
func (s *UserService) ResetPassword(ctx context.Context, id, newPassword string) error {
	if err := validatePassword(newPassword); err != nil {
		return err
	}
	u, err := s.users.Get(ctx, id)
	if err != nil {
		return err
	}
	return s.setPassword(ctx, u, newPassword)
}

// ChangePassword verifies the current password before setting a new one.
func (s *UserService) ChangePassword(ctx context.Context, id, current, newPassword string) error {
	if err := validatePassword(newPassword); err != nil {
		return err
	}
	u, err := s.users.Get(ctx, id)
	if err != nil {
		return err
	}
	if u.PasswordHash == "" || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(current)) != nil {
		return domain.ErrInvalidCredential
	}
	return s.setPassword(ctx, u, newPassword)
}

// setPassword hashes newPassword and persists it on u.
func (s *UserService) setPassword(ctx context.Context, u *domain.User, newPassword string) error {
	hash, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	u.PasswordHash = hash
	return s.users.Update(ctx, u)
}

// EnsureOAuthUser returns the existing OAuth user, or creates a new one with a
// unique username (suffixing on collision). The bool is true when a new user
// was created.
func (s *UserService) EnsureOAuthUser(ctx context.Context, provider, oauthID, username string) (*domain.User, bool, error) {
	if existing, err := s.users.GetByOAuth(ctx, provider, oauthID); err == nil {
		return existing, false, nil
	} else if !isNotFound(err) {
		return nil, false, err
	}

	// Pick a unique username (suffix -2, -3, ... on collision).
	candidate := username
	for i := 2; ; i++ {
		_, err := s.users.GetByUsername(ctx, candidate)
		if isNotFound(err) {
			break
		}
		if err != nil {
			return nil, false, err
		}
		if i > 1000 {
			return nil, false, fmt.Errorf("could not find unique username for %s", username)
		}
		candidate = fmt.Sprintf("%s-%d", username, i)
	}

	u := &domain.User{
		ID:            newID(),
		Username:      candidate,
		Role:          domain.RoleUser,
		OAuthProvider: provider,
		OAuthID:       oauthID,
	}
	if err := s.users.Create(ctx, u); err != nil {
		return nil, false, err
	}
	s.logger.WithFields(logrus.Fields{
		"user_id":        u.ID,
		"username":       u.Username,
		"oauth_provider": provider,
	}).Info("oauth user created")
	return u, true, nil
}

// Count returns the total number of users.
func (s *UserService) Count(ctx context.Context) (int, error) {
	return s.users.Count(ctx)
}

func validateUsername(username string) error {
	if !usernameRe.MatchString(username) {
		return fmt.Errorf("username must match ^[a-zA-Z0-9][a-zA-Z0-9._-]{1,63}$: %w", domain.ErrValidation)
	}
	return nil
}

func validatePassword(password string) error {
	if len(password) < minPasswordLen {
		return fmt.Errorf("password must be at least %d characters: %w", minPasswordLen, domain.ErrValidation)
	}
	if len(password) > maxPasswordLen {
		return fmt.Errorf("password must be at most %d characters (bcrypt limit): %w", maxPasswordLen, domain.ErrValidation)
	}
	return nil
}

// hashPassword returns the bcrypt hash of password.
func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

func isNotFound(err error) bool {
	return errors.Is(err, domain.ErrNotFound)
}
