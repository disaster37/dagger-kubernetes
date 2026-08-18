package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/common/ut"
)

func newClockLimiter(start time.Time) (*attemptLimiter, *time.Time) {
	now := start
	l := newAttemptLimiter()
	l.now = func() time.Time { return now }
	return l, &now
}

func TestAttemptLimiterAllowsUntilThreshold(t *testing.T) {
	l, _ := newClockLimiter(time.Now())
	key := "login|alice|1.2.3.4"

	for i := 0; i < maxFailedAttempts-1; i++ {
		if !l.allow(key) {
			t.Fatalf("attempt %d should be allowed", i)
		}
		if lock := l.recordFailure(key); lock != 0 {
			t.Fatalf("attempt %d should not lock, got %v", i, lock)
		}
	}
	if !l.allow(key) {
		t.Fatal("attempt at threshold-1 should be allowed")
	}
}

func TestAttemptLimiterLocksAtThresholdWithBackoff(t *testing.T) {
	start := time.Now()
	l, now := newClockLimiter(start)
	key := "login|alice|1.2.3.4"

	for i := 0; i < maxFailedAttempts-1; i++ {
		l.recordFailure(key)
	}
	lock := l.recordFailure(key)
	if lock != baseLockout {
		t.Fatalf("first lockout = %v, want %v", lock, baseLockout)
	}
	if l.allow(key) {
		t.Fatal("locked key must be rejected")
	}

	// Still locked just before expiry.
	*now = start.Add(baseLockout - time.Second)
	if l.allow(key) {
		t.Fatal("locked key must be rejected before expiry")
	}

	// After expiry the attempt is allowed again, and the next failure
	// re-locks with a doubled duration.
	*now = start.Add(baseLockout + time.Second)
	if !l.allow(key) {
		t.Fatal("lock should have expired")
	}
	lock = l.recordFailure(key)
	if lock != 2*baseLockout {
		t.Fatalf("second lockout = %v, want %v", lock, 2*baseLockout)
	}
}

func TestAttemptLimiterLockoutCapped(t *testing.T) {
	l, _ := newClockLimiter(time.Now())
	key := "login|alice|1.2.3.4"

	var lock time.Duration
	for i := 0; i < maxFailedAttempts+maxLockoutShift+5; i++ {
		lock = l.recordFailure(key)
	}
	if lock != maxLockout {
		t.Fatalf("lockout = %v, want cap %v", lock, maxLockout)
	}
}

func TestAttemptLimiterSuccessResets(t *testing.T) {
	l, _ := newClockLimiter(time.Now())
	key := "login|alice|1.2.3.4"

	for i := 0; i < maxFailedAttempts-1; i++ {
		l.recordFailure(key)
	}
	l.recordSuccess(key)
	// After a success the full failure budget is available again.
	for i := 0; i < maxFailedAttempts-1; i++ {
		if lock := l.recordFailure(key); lock != 0 {
			t.Fatalf("failure %d after reset should not lock, got %v", i, lock)
		}
	}
}

func TestAttemptLimiterKeysAreIndependent(t *testing.T) {
	l, _ := newClockLimiter(time.Now())
	for i := 0; i < maxFailedAttempts; i++ {
		l.recordFailure("login|alice|1.2.3.4")
	}
	if l.allow("login|alice|1.2.3.4") {
		t.Fatal("locked key must be rejected")
	}
	if !l.allow("login|alice|5.6.7.8") {
		t.Fatal("same user from another IP is a separate key")
	}
	if !l.allow("login|bob|1.2.3.4") {
		t.Fatal("another user from the same IP is a separate key")
	}
}

func TestAttemptLimiterSweepsStaleEntries(t *testing.T) {
	start := time.Now()
	l, now := newClockLimiter(start)

	l.recordFailure("login|alice|1.2.3.4")
	*now = start.Add(limiterEntryTTL + time.Minute)
	// Any operation triggers the sweep; afterwards the key is unknown again.
	if !l.allow("login|bob|1.2.3.4") {
		t.Fatal("unrelated key must be allowed")
	}
	l.mu.Lock()
	_, stillThere := l.attempts["login|alice|1.2.3.4"]
	l.mu.Unlock()
	if stillThere {
		t.Fatal("stale entry should have been swept")
	}
}

func TestAttemptLimiterEvictsWhenFull(t *testing.T) {
	l, _ := newClockLimiter(time.Now())
	for i := 0; i < maxLimiterEntries; i++ {
		l.recordFailure(fmt.Sprintf("key-%d", i))
	}
	// One more distinct key must not grow the map beyond the cap.
	l.recordFailure("overflow-key")
	l.mu.Lock()
	n := len(l.attempts)
	l.mu.Unlock()
	if n > maxLimiterEntries {
		t.Fatalf("map size = %d, want <= %d", n, maxLimiterEntries)
	}
}

// TestLoginRateLimited verifies the login endpoint locks out a username+IP
// after repeated failures (CWE-307).
func TestLoginRateLimited(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	body := `{"username":"admin","password":"wrong"}`
	for i := 0; i < maxFailedAttempts; i++ {
		resp := ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
			ut.Header{Key: "Content-Type", Value: "application/json"})
		if resp.Result().StatusCode() != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d, want 401", i, resp.Result().StatusCode())
		}
	}

	// Next attempt is locked out even with the CORRECT password.
	body = `{"username":"admin","password":"password123"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("locked login: %d, want 429", resp.Result().StatusCode())
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Result().Body(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["message"] == nil {
		t.Fatal("missing message")
	}
}

// TestLoginRateLimitCaseInsensitive verifies username casing cannot bypass a
// lockout (usernames are COLLATE NOCASE).
func TestLoginRateLimitCaseInsensitive(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	for i := 0; i < maxFailedAttempts; i++ {
		body := `{"username":"ADMIN","password":"wrong"}`
		resp := ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
			ut.Header{Key: "Content-Type", Value: "application/json"})
		if resp.Result().StatusCode() != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d, want 401", i, resp.Result().StatusCode())
		}
	}

	body := `{"username":"admin","password":"password123"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("case-variant lockout: %d, want 429", resp.Result().StatusCode())
	}
}

// TestLoginSuccessClearsFailures verifies a successful login resets the
// failure counter for the key.
func TestLoginSuccessClearsFailures(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	// Fewer than threshold failures, then a success.
	for i := 0; i < maxFailedAttempts-1; i++ {
		body := `{"username":"admin","password":"wrong"}`
		ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
			ut.Header{Key: "Content-Type", Value: "application/json"})
	}
	body := `{"username":"admin","password":"password123"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("success login: %d, want 200", resp.Result().StatusCode())
	}

	// The full failure budget is available again.
	for i := 0; i < maxFailedAttempts-1; i++ {
		bad := `{"username":"admin","password":"wrong"}`
		resp = ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(bad), Len: len(bad)},
			ut.Header{Key: "Content-Type", Value: "application/json"})
		if resp.Result().StatusCode() != http.StatusUnauthorized {
			t.Fatalf("post-reset attempt %d: %d, want 401", i, resp.Result().StatusCode())
		}
	}
}

// TestChangePasswordRateLimited verifies the change-password endpoint (which
// also verifies a password) is rate-limited.
func TestChangePasswordRateLimited(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	bearer := env.loginAsAdmin(t)

	body := `{"current_password":"wrong","new_password":"newpassword123"}`
	for i := 0; i < maxFailedAttempts; i++ {
		resp := ut.PerformRequest(e, "PUT", "/api/v1/auth/password", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
			ut.Header{Key: "Authorization", Value: bearer},
			ut.Header{Key: "Content-Type", Value: "application/json"})
		if resp.Result().StatusCode() != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d, want 401", i, resp.Result().StatusCode())
		}
	}
	resp := ut.PerformRequest(e, "PUT", "/api/v1/auth/password", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("locked change-password: %d, want 429", resp.Result().StatusCode())
	}
}
