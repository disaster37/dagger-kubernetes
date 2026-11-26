package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// DeviceAuthService implements the RFC 8628 device authorization flow.
type DeviceAuthService struct {
	store    domain.DeviceAuthStore
	users    *UserService
	groups   domain.GroupRepository
	jwt      *JWTService
	codeTTL  time.Duration
	interval time.Duration
	logger   *logrus.Logger
}

// NewDeviceAuthService returns a DeviceAuthService.
func NewDeviceAuthService(store domain.DeviceAuthStore, users *UserService, groups domain.GroupRepository, jwt *JWTService, codeTTL, interval time.Duration, logger *logrus.Logger) *DeviceAuthService {
	if codeTTL <= 0 {
		codeTTL = 10 * time.Minute
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &DeviceAuthService{store: store, users: users, groups: groups, jwt: jwt, codeTTL: codeTTL, interval: interval, logger: logger}
}

// DeviceCodeResponse is the POST /oauth/device/code response.
type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// Start creates a new device authorization.
func (s *DeviceAuthService) Start(verificationURI string) (*DeviceCodeResponse, error) {
	deviceCode, err := deviceRandomHex(32)
	if err != nil {
		return nil, fmt.Errorf("generate device code: %w", err)
	}
	userCode, err := randomUserCode()
	if err != nil {
		return nil, fmt.Errorf("generate user code: %w", err)
	}
	a := &domain.DeviceAuthorization{
		DeviceCode: deviceCode,
		UserCode:   userCode,
		ExpiresAt:  time.Now().UTC().Add(s.codeTTL),
		Interval:   s.interval,
	}
	if err := s.store.Create(a); err != nil {
		return nil, err
	}
	return &DeviceCodeResponse{
		DeviceCode:      deviceCode,
		UserCode:        userCode,
		VerificationURI: verificationURI,
		ExpiresIn:       int(s.codeTTL.Seconds()),
		Interval:        int(s.interval.Seconds()),
	}, nil
}

// Approve binds a user to a pending user code.
func (s *DeviceAuthService) Approve(userCode, userID string) error {
	return s.store.Approve(strings.ToUpper(strings.TrimSpace(userCode)), userID)
}

// TokenResult is the OAuth token response.
type TokenResult struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

// ExchangeDeviceCode exchanges an approved device code for a token pair.
func (s *DeviceAuthService) ExchangeDeviceCode(ctx context.Context, deviceCode string) (*TokenResult, error) {
	a, err := s.store.GetByDeviceCode(deviceCode)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("%w: unknown device code", domain.ErrInvalidGrant)
		}
		return nil, err
	}
	// Check consumption before expiry: a code that was already redeemed is an
	// invalid grant regardless of how much time has passed since (RFC 6749 §5.2).
	if a.Consumed {
		return nil, fmt.Errorf("%w: device code already used", domain.ErrInvalidGrant)
	}
	if time.Now().UTC().After(a.ExpiresAt) {
		return nil, fmt.Errorf("%w: device code expired", domain.ErrExpiredDeviceToken)
	}
	if !a.Approved || a.UserID == "" {
		return nil, fmt.Errorf("%w: authorization pending", domain.ErrAuthorizationPending)
	}
	// Atomically consume before issuing tokens so a concurrent exchange cannot
	// mint a second token pair for the same device code (CWE-362).
	if err := s.store.Consume(deviceCode); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("%w: unknown device code", domain.ErrInvalidGrant)
		}
		return nil, err
	}
	u, err := s.users.Get(ctx, a.UserID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("%w: user not found", domain.ErrInvalidGrant)
		}
		return nil, err
	}
	if u.AccessBlocked() {
		return nil, fmt.Errorf("%w: user account disabled", domain.ErrInvalidGrant)
	}
	gids, err := s.groups.GroupsForUser(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	access, refresh, err := s.jwt.IssuePair(u, groupIDs(gids))
	if err != nil {
		return nil, err
	}
	return &TokenResult{AccessToken: access, RefreshToken: refresh, TokenType: "Bearer", ExpiresIn: int(s.jwt.AccessTTL().Seconds())}, nil
}

// randomHex returns n random bytes hex-encoded.
func deviceRandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// randomUserCode returns a human-friendly XXXX-XXXX code.
func randomUserCode() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, 0, 9)
	for i, c := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, alphabet[int(c)%len(alphabet)])
	}
	return string(out), nil
}

// MemoryDeviceAuthStore is an in-memory DeviceAuthStore. Device codes are
// short-lived and leader-pinned, so no Raft replication is required.
type MemoryDeviceAuthStore struct {
	mu     sync.Mutex
	byCode map[string]*domain.DeviceAuthorization
	byUser map[string]string
}

// NewMemoryDeviceAuthStore returns an empty in-memory store.
func NewMemoryDeviceAuthStore() *MemoryDeviceAuthStore {
	return &MemoryDeviceAuthStore{
		byCode: make(map[string]*domain.DeviceAuthorization),
		byUser: make(map[string]string),
	}
}

var _ domain.DeviceAuthStore = (*MemoryDeviceAuthStore)(nil)

func (m *MemoryDeviceAuthStore) Create(a *domain.DeviceAuthorization) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *a
	m.byCode[a.DeviceCode] = &cp
	m.byUser[a.UserCode] = a.DeviceCode
	return nil
}

func (m *MemoryDeviceAuthStore) GetByDeviceCode(code string) (*domain.DeviceAuthorization, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.byCode[code]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *a
	return &cp, nil
}

func (m *MemoryDeviceAuthStore) GetByUserCode(code string) (*domain.DeviceAuthorization, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	deviceCode, ok := m.byUser[code]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *m.byCode[deviceCode]
	return &cp, nil
}

func (m *MemoryDeviceAuthStore) Approve(userCode, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	deviceCode, ok := m.byUser[userCode]
	if !ok {
		return domain.ErrNotFound
	}
	a := m.byCode[deviceCode]
	if time.Now().UTC().After(a.ExpiresAt) {
		return fmt.Errorf("%w: device code expired", domain.ErrValidation)
	}
	a.Approved = true
	a.UserID = userID
	return nil
}

func (m *MemoryDeviceAuthStore) Consume(deviceCode string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.byCode[deviceCode]
	if !ok {
		return domain.ErrNotFound
	}
	if a.Consumed {
		return fmt.Errorf("%w: device code already used", domain.ErrInvalidGrant)
	}
	a.Consumed = true
	return nil
}
