package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

func TestMemoryDeviceAuthStore(t *testing.T) {
	store := NewMemoryDeviceAuthStore()
	a := &domain.DeviceAuthorization{DeviceCode: "dev", UserCode: "ABCD-EFGH", ExpiresAt: time.Now().Add(time.Minute)}
	if err := store.Create(a); err != nil {
		t.Fatal(err)
	}
	if err := store.Approve("ABCD-EFGH", "u1"); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetByDeviceCode("dev")
	if err != nil || !got.Approved || got.UserID != "u1" {
		t.Fatalf("got = %+v err=%v", got, err)
	}
	if err := store.Consume("dev"); err != nil {
		t.Fatal(err)
	}
	got, _ = store.GetByDeviceCode("dev")
	if !got.Consumed {
		t.Fatal("expected consumed")
	}
}

func TestMemoryDeviceAuthStoreUnknownCodes(t *testing.T) {
	cases := []struct {
		name string
		op   func(store *MemoryDeviceAuthStore) error
		want error
	}{
		{
			name: "get by unknown device code",
			op:   func(s *MemoryDeviceAuthStore) error { _, err := s.GetByDeviceCode("nope"); return err },
			want: domain.ErrNotFound,
		},
		{
			name: "get by unknown user code",
			op:   func(s *MemoryDeviceAuthStore) error { _, err := s.GetByUserCode("NOPE"); return err },
			want: domain.ErrNotFound,
		},
		{
			name: "approve unknown user code",
			op:   func(s *MemoryDeviceAuthStore) error { return s.Approve("NOPE", "u1") },
			want: domain.ErrNotFound,
		},
		{
			name: "consume unknown device code",
			op:   func(s *MemoryDeviceAuthStore) error { return s.Consume("nope") },
			want: domain.ErrNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.op(NewMemoryDeviceAuthStore()); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestMemoryDeviceAuthStoreApproveExpired(t *testing.T) {
	store := NewMemoryDeviceAuthStore()
	if err := store.Create(&domain.DeviceAuthorization{DeviceCode: "d", UserCode: "AB", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Approve("AB", "u1"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("approve expired err = %v, want ErrValidation", err)
	}
}

func TestMemoryDeviceAuthStoreConsumeTwice(t *testing.T) {
	store := NewMemoryDeviceAuthStore()
	if err := store.Create(&domain.DeviceAuthorization{DeviceCode: "d", UserCode: "AB", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Consume("d"); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if err := store.Consume("d"); !errors.Is(err, domain.ErrInvalidGrant) {
		t.Fatalf("second consume err = %v, want ErrInvalidGrant", err)
	}
}

// deviceAuthFixture wires a DeviceAuthService over an in-memory store with one
// pre-created admin user, mirroring the handler/integration fixtures.
type deviceAuthFixture struct {
	svc   *DeviceAuthService
	store *MemoryDeviceAuthStore
	users *UserService
}

func newDeviceAuthFixture(t *testing.T) *deviceAuthFixture {
	t.Helper()
	logger := observ.NewTestLogger()
	store, err := repository.NewInmemRaftStore("device-auth-test", logger, 5*time.Second)
	if err != nil {
		t.Fatalf("NewInmemRaftStore: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WaitForLeader(ctx); err != nil {
		t.Fatalf("WaitForLeader: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	groupRepo := repository.NewGroupRepo(store)
	usersSvc := NewUserService(repository.NewUserRepo(store), groupRepo, logger)
	jwtSvc := NewJWTService([]byte("device-auth-test-secret-32-bytes!!"), 15*time.Minute, 168*time.Hour)

	das := NewMemoryDeviceAuthStore()
	svc := NewDeviceAuthService(das, usersSvc, groupRepo, jwtSvc, 10*time.Minute, 5*time.Second, logger)
	return &deviceAuthFixture{svc: svc, store: das, users: usersSvc}
}

// seed creates a device authorization directly in the store.
func (f *deviceAuthFixture) seed(t *testing.T, deviceCode string, approved bool, userID string, expiresAt time.Time, consumed bool) {
	t.Helper()
	if err := f.store.Create(&domain.DeviceAuthorization{
		DeviceCode: deviceCode,
		UserCode:   "CODE-" + deviceCode,
		Approved:   approved,
		UserID:     userID,
		ExpiresAt:  expiresAt,
		Consumed:   consumed,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestDeviceAuthServiceExchangeDeviceCode(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	cases := []struct {
		name   string
		setup  func(f *deviceAuthFixture) (deviceCode string, wantErr error)
		result func(t *testing.T, res *TokenResult)
	}{
		{
			name: "unknown_device_code",
			setup: func(f *deviceAuthFixture) (string, error) {
				return "missing", domain.ErrInvalidGrant
			},
		},
		{
			name: "empty_device_code",
			setup: func(f *deviceAuthFixture) (string, error) {
				return "", domain.ErrInvalidGrant
			},
		},
		{
			name: "pending_unapproved",
			setup: func(f *deviceAuthFixture) (string, error) {
				f.seed(t, "dev1", false, "", now.Add(time.Minute), false)
				return "dev1", domain.ErrAuthorizationPending
			},
		},
		{
			name: "expired_unapproved",
			setup: func(f *deviceAuthFixture) (string, error) {
				f.seed(t, "dev2", false, "", now.Add(-time.Minute), false)
				return "dev2", domain.ErrExpiredDeviceToken
			},
		},
		{
			name: "expired_approved_unconsumed",
			setup: func(f *deviceAuthFixture) (string, error) {
				u, err := f.users.Create(ctx, "u-exp", "password123", domain.RoleUser)
				if err != nil {
					t.Fatal(err)
				}
				f.seed(t, "dev3", true, u.ID, now.Add(-time.Minute), false)
				return "dev3", domain.ErrExpiredDeviceToken
			},
		},
		{
			name: "approved_valid",
			setup: func(f *deviceAuthFixture) (string, error) {
				u, err := f.users.Create(ctx, "u-ok", "password123", domain.RoleUser)
				if err != nil {
					t.Fatal(err)
				}
				f.seed(t, "dev4", true, u.ID, now.Add(time.Minute), false)
				return "dev4", nil
			},
			result: func(t *testing.T, res *TokenResult) {
				if res.AccessToken == "" || res.RefreshToken == "" {
					t.Fatalf("tokens empty: %+v", res)
				}
				if res.TokenType != "Bearer" {
					t.Fatalf("token_type = %q, want Bearer", res.TokenType)
				}
				if res.ExpiresIn != 900 {
					t.Fatalf("expires_in = %d, want 900", res.ExpiresIn)
				}
			},
		},
		{
			name: "replay_already_consumed",
			setup: func(f *deviceAuthFixture) (string, error) {
				u, err := f.users.Create(ctx, "u-rep", "password123", domain.RoleUser)
				if err != nil {
					t.Fatal(err)
				}
				f.seed(t, "dev5", true, u.ID, now.Add(time.Minute), true)
				return "dev5", domain.ErrInvalidGrant
			},
		},
		{
			name: "replay_consumed_after_expiry",
			setup: func(f *deviceAuthFixture) (string, error) {
				f.seed(t, "dev6", true, "gone", now.Add(-time.Minute), true)
				return "dev6", domain.ErrInvalidGrant
			},
		},
		{
			name: "approved_user_deleted",
			setup: func(f *deviceAuthFixture) (string, error) {
				f.seed(t, "dev7", true, "does-not-exist", now.Add(time.Minute), false)
				return "dev7", domain.ErrInvalidGrant
			},
		},
		{
			name: "approved_user_disabled",
			setup: func(f *deviceAuthFixture) (string, error) {
				u, err := f.users.Create(ctx, "u-dis", "password123", domain.RoleUser)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.users.SetState(ctx, "", u.ID, domain.UserDisable); err != nil {
					t.Fatalf("disable user: %v", err)
				}
				f.seed(t, "dev8", true, u.ID, now.Add(time.Minute), false)
				return "dev8", domain.ErrInvalidGrant
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeviceAuthFixture(t)
			deviceCode, wantErr := tc.setup(f)
			res, err := f.svc.ExchangeDeviceCode(ctx, deviceCode)
			if wantErr != nil {
				if !errors.Is(err, wantErr) {
					t.Fatalf("err = %v, want %v", err, wantErr)
				}
				if res != nil {
					t.Fatalf("res = %+v, want nil on error", res)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if tc.result != nil {
				tc.result(t, res)
			}
		})
	}
}

func TestDeviceAuthServiceConcurrentExchange(t *testing.T) {
	ctx := context.Background()
	f := newDeviceAuthFixture(t)
	u, err := f.users.Create(ctx, "u-race", "password123", domain.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	f.seed(t, "race", true, u.ID, time.Now().UTC().Add(time.Minute), false)

	const n = 8
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = f.svc.ExchangeDeviceCode(ctx, "race")
		}(i)
	}
	wg.Wait()

	var success, invalid int
	for _, err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, domain.ErrInvalidGrant):
			invalid++
		default:
			t.Fatalf("unexpected err: %v", err)
		}
	}
	if success != 1 || invalid != n-1 {
		t.Fatalf("success = %d, invalid_grant = %d; want 1 and %d", success, invalid, n-1)
	}
}
