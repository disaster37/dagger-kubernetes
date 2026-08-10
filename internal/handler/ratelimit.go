package handler

import (
	"sync"
	"time"
)

const (
	// maxFailedAttempts is the number of consecutive failures after which
	// lockouts start (CWE-307).
	maxFailedAttempts = 5

	// baseLockout is the first lockout duration once the failure threshold is
	// reached; it doubles with every further failure.
	baseLockout = 30 * time.Second

	// maxLockout caps the exponential lockout growth.
	maxLockout = 15 * time.Minute

	// limiterEntryTTL is how long an idle failure record is kept before it is
	// garbage-collected.
	limiterEntryTTL = 30 * time.Minute

	// maxLimiterEntries bounds the tracker map so a flood of distinct keys
	// cannot grow it without limit (CWE-770).
	maxLimiterEntries = 4096

	// maxLockoutShift caps the exponential shift so the duration computation
	// cannot overflow even with absurd failure counts.
	maxLockoutShift = 16
)

type attemptInfo struct {
	failures    int
	lockedUntil time.Time
	lastSeen    time.Time
}

// attemptLimiter tracks failed password-authentication attempts per key
// (username/user-id + client IP) and imposes exponentially growing lockouts
// after repeated failures (CWE-307). State is in-memory: it resets on restart
// and is not shared across supervisor replicas (single-node today, see
// ADR-010).
type attemptLimiter struct {
	mu       sync.Mutex
	attempts map[string]*attemptInfo
	now      func() time.Time
}

func newAttemptLimiter() *attemptLimiter {
	return &attemptLimiter{
		attempts: make(map[string]*attemptInfo),
		now:      func() time.Time { return time.Now() },
	}
}

// allow reports whether key may attempt authentication right now.
func (l *attemptLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	info, ok := l.attempts[key]
	if !ok {
		return true
	}
	return !now.Before(info.lockedUntil)
}

// recordFailure records a failed attempt and returns the lockout duration it
// triggered (0 when the failure did not yet cause a lockout).
func (l *attemptLimiter) recordFailure(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	info, ok := l.attempts[key]
	if !ok {
		if len(l.attempts) >= maxLimiterEntries {
			l.evictOldestLocked()
		}
		info = &attemptInfo{}
		l.attempts[key] = info
	}
	info.failures++
	info.lastSeen = now
	if info.failures < maxFailedAttempts {
		return 0
	}
	// Exponential backoff: 30s, 60s, 120s, ... capped at maxLockout.
	shift := info.failures - maxFailedAttempts
	if shift > maxLockoutShift {
		shift = maxLockoutShift
	}
	lock := baseLockout << shift
	if lock > maxLockout {
		lock = maxLockout
	}
	info.lockedUntil = now.Add(lock)
	return lock
}

// recordSuccess clears the failure history for key (a successful
// authentication proves possession of the credential).
func (l *attemptLimiter) recordSuccess(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}

// sweepLocked drops entries that are idle beyond limiterEntryTTL and no
// longer locked. Callers must hold mu.
func (l *attemptLimiter) sweepLocked(now time.Time) {
	for k, info := range l.attempts {
		if now.Sub(info.lastSeen) > limiterEntryTTL && !now.Before(info.lockedUntil) {
			delete(l.attempts, k)
		}
	}
}

// evictOldestLocked drops the entry with the oldest lastSeen. Callers must
// hold mu.
func (l *attemptLimiter) evictOldestLocked() {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, info := range l.attempts {
		if first || info.lastSeen.Before(oldestTime) {
			oldestKey = k
			oldestTime = info.lastSeen
			first = false
		}
	}
	if !first {
		delete(l.attempts, oldestKey)
	}
}
