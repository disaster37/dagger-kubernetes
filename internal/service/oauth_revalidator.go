package service

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// maxEntryAge is the maximum age of a cache entry before it is swept. Entries
// older than this have likely been abandoned (user deleted or no longer active)
// and keeping them wastes memory. The sweep runs every 10 minutes.
const maxEntryAge = 24 * time.Hour

// errOAuthNoCredential is returned by a provider's Revalidate when the user has
// no stored OAuth credential (pre-upgrade). The revalidator treats this as an
// allow (no deactivation), bounded by session_max_age.
var errOAuthNoCredential = errors.New("no stored oauth credential")

// OAuthRevalidatorConfig holds the tuning parameters for IdP group-membership
// revalidation (ADR-027).
type OAuthRevalidatorConfig struct {
	Interval      time.Duration // successful-check cache TTL
	Grace         time.Duration // offline grace window before fail-open/fail-closed applies
	FailOpen      bool          // after grace, true = allow, false = deny
	SessionMaxAge time.Duration // hard bound on OAuth session age; 0 = disabled
	// Clock is a test-only time source. It is nil in production (UTC now) and
	// is deliberately not a configurable config/Viper/Helm option.
	Clock func() time.Time
}

type revalidationState int

const (
	stateOK          revalidationState = iota // IdP check succeeded
	stateRevoked                              // positive revocation (allowlist miss, GitHub 401/404, user deactivated)
	stateUnavailable                          // IdP unreachable
	stateExpired                              // credential unusable; serve cache within grace then fail policy; non-destructive
)

// revalidationEntry is the per-user cache entry. All fields except inflight are
// written by the single goroutine performing the IdP call (the creator/refresher)
// while inflight is non-nil, and read by other goroutines only after inflight
// has been set to nil under mu (or after the inflight channel is closed, which
// synchronizes-with the write).
type revalidationEntry struct {
	state      revalidationState
	groupIDs   []string
	checkedAt  time.Time // when the last IdP check ran
	lastGood   time.Time // when the last successful (stateOK) check ran; zero if never
	expiresAt  time.Time // when this entry becomes stale (jittered TTL)
	inflight   chan struct{}
	credential string // snapshot of u.OAuthTokenCiphertext this entry was built from (re-login recovery)
	// loginVersion is the snapshot of u.OAuthLoginVersion this entry was built
	// from. It changes on every accepted login even when encryption is disabled
	// and the ciphertext is empty, so it is the authoritative login-generation
	// marker for cache invalidation.
	loginVersion uint64
}

// OAuthRevalidator enforces IdP group-membership revalidation behind a bounded,
// single-flight cache. It is wired into AuthService at startup when OAuth is
// enabled.
type OAuthRevalidator struct {
	provider                     OAuthProvider
	mapper                       *GroupMapper
	adminGroups                  []string
	mappedGroupMaxRunnerSessions int
	users                        *UserService
	groups                       domain.GroupRepository
	logger                       *logrus.Logger
	cfg                          OAuthRevalidatorConfig
	clock                        func() time.Time

	mu    sync.Mutex
	cache map[string]*revalidationEntry
}

// NewOAuthRevalidator returns an OAuthRevalidator. Pass nil for any field that
// should be skipped. API-token revocation is performed atomically by the
// OAuthRevocationCommit FSM command, so no TokenService dependency is needed.
func NewOAuthRevalidator(
	provider OAuthProvider,
	mapper *GroupMapper,
	adminGroups []string,
	mappedGroupMaxRunnerSessions int,
	users *UserService,
	groups domain.GroupRepository,
	logger *logrus.Logger,
	cfg OAuthRevalidatorConfig,
) *OAuthRevalidator {
	clock := cfg.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	r := &OAuthRevalidator{
		provider: provider, mapper: mapper, adminGroups: adminGroups,
		mappedGroupMaxRunnerSessions: mappedGroupMaxRunnerSessions,
		users:                        users, groups: groups,
		logger: logger, cfg: cfg,
		clock: clock,
		cache: make(map[string]*revalidationEntry),
	}
	go r.sweepLoop()
	return r
}

// sweepLoop periodically removes stale cache entries to bound memory growth.
func (r *OAuthRevalidator) sweepLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := r.clock()
		r.mu.Lock()
		for uid, entry := range r.cache {
			if entry.inflight == nil && now.Sub(entry.checkedAt) > maxEntryAge {
				delete(r.cache, uid)
			}
		}
		r.mu.Unlock()
	}
}

// SessionMaxAge returns the configured session max age.
func (r *OAuthRevalidator) SessionMaxAge() time.Duration {
	return r.cfg.SessionMaxAge
}

// Check returns the user's current effective supervisor group IDs, enforcing
// IdP revalidation behind a bounded cache. It returns a non-nil error only
// when access must be DENIED. After waiting for or performing a refresh it
// reloads the user so a concurrent login generation, ban or deletion is
// observed. Stale recovery is bounded to one retry per Check.
func (r *OAuthRevalidator) Check(ctx context.Context, u *domain.User) ([]string, error) {
	// cur is a per-goroutine local; the caller's u is never mutated, so
	// concurrent Checks can safely share the same input pointer.
	cur := u
	for attempt := 0; ; attempt++ {
		r.mu.Lock()
		entry := r.cache[cur.ID]
		switch {
		case entry == nil:
			entry = &revalidationEntry{inflight: make(chan struct{})}
			r.cache[cur.ID] = entry
			r.mu.Unlock()
		case entry.inflight != nil:
			wait := entry.inflight
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-wait:
			}
			next, err := r.loadCurrentUser(ctx, cur.ID)
			if err != nil {
				return nil, err
			}
			cur = next
			continue
		default:
			now := r.clock()
			if r.fresh(entry, cur, now) {
				gids, err := r.serve(entry, cur, now)
				r.mu.Unlock()
				return gids, err
			}
			entry.inflight = make(chan struct{})
			r.mu.Unlock()
		}

		// This goroutine owns the refresh.
		r.refresh(ctx, cur, entry)
		r.finish(entry)

		if attempt >= 1 {
			// Bounded recovery: a second conflict in a single Check denies
			// rather than looping over a provider indefinitely.
			r.mu.Lock()
			e := r.cache[cur.ID]
			if e != nil && r.fresh(e, cur, r.clock()) {
				gids, err := r.serve(e, cur, r.clock())
				r.mu.Unlock()
				return gids, err
			}
			r.mu.Unlock()
			return nil, domain.ErrUnauthenticated
		}
		next, err := r.loadCurrentUser(ctx, cur.ID)
		if err != nil {
			return nil, err
		}
		cur = next
	}
}

// loadCurrentUser re-reads a user row so account-state and revision changes
// applied during a refresh are observed. An administrator disable/deletion
// denies outright; IdP deactivation is returned so the cached stateRevoked
// result can surface ErrSessionRevoked rather than a generic denial.
func (r *OAuthRevalidator) loadCurrentUser(ctx context.Context, id string) (*domain.User, error) {
	cur, err := r.users.Get(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return nil, domain.ErrUnauthenticated
		}
		return nil, err
	}
	if cur.DeletedAt != nil || cur.DisabledAt != nil {
		return nil, domain.ErrUnauthenticated
	}
	return cur, nil
}

// fresh reports whether entry can be served without a new IdP call at now. The
// login-generation/credential check runs for EVERY state: an accepted login
// (which advances OAuthLoginVersion and usually the ciphertext) must force an
// immediate re-check even if the entry was a cached denial or a cached success.
// A revoked entry is only trusted while the user is still deactivated: a
// successful re-login clears DeactivatedAt, so a revoked cache entry must be
// re-checked to allow recovery.
func (r *OAuthRevalidator) fresh(entry *revalidationEntry, u *domain.User, now time.Time) bool {
	if entry.loginVersion != u.OAuthLoginVersion || entry.credential != u.OAuthTokenCiphertext {
		return false
	}
	if entry.state == stateRevoked && u.DeactivatedAt == nil {
		return false
	}
	if entry.expiresAt.IsZero() {
		return false
	}
	return now.Before(entry.expiresAt)
}

// serve returns the cached result for a non-inflight entry. Called under r.mu.
// stateExpired is handled like stateUnavailable: serve cached groups within
// revalidate_grace, then revalidate_fail_open / fail-closed.
func (r *OAuthRevalidator) serve(entry *revalidationEntry, u *domain.User, now time.Time) ([]string, error) {
	switch entry.state {
	case stateOK:
		return entry.groupIDs, nil
	case stateRevoked:
		return nil, domain.ErrSessionRevoked
	case stateUnavailable, stateExpired:
		reason := "IdP unreachable"
		if entry.state == stateExpired {
			reason = "credential expired, cannot verify membership"
		}
		withinGrace := !entry.lastGood.IsZero() && now.Sub(entry.lastGood) <= r.cfg.Grace
		if withinGrace {
			if len(entry.groupIDs) > 0 {
				r.logger.WithFields(logrus.Fields{
					"user_id": u.ID, "oauth_provider": u.OAuthProvider,
				}).Warn(fmt.Sprintf("oauth: %s within grace, serving last-known-good", reason))
				return entry.groupIDs, nil
			}
			// No cached groups: fall through to deny below.
		}
		if r.cfg.FailOpen && len(entry.groupIDs) > 0 {
			r.logger.WithFields(logrus.Fields{
				"user_id": u.ID, "oauth_provider": u.OAuthProvider,
			}).Error(fmt.Sprintf("oauth: %s past grace, serving last-known-good (fail-open)", reason))
			return entry.groupIDs, nil
		}
		r.logger.WithFields(logrus.Fields{
			"user_id": u.ID, "oauth_provider": u.OAuthProvider,
		}).Error(fmt.Sprintf("oauth: %s past grace, denying (fail-closed)", reason))
		return nil, domain.ErrUnauthenticated
	default:
		return nil, domain.ErrUnauthenticated
	}
}

// finish publishes the result of a refresh: it clears the inflight channel
// under the mutex (so new readers observe the final state) and then closes it
// (so waiting goroutines wake and re-read).
func (r *OAuthRevalidator) finish(entry *revalidationEntry) {
	r.mu.Lock()
	ch := entry.inflight
	entry.inflight = nil
	r.mu.Unlock()
	close(ch)
}

// refresh performs one IdP revalidation and records the outcome on entry. It is
// called by the single goroutine that owns entry.inflight; no other goroutine
// reads entry while inflight is non-nil.
func (r *OAuthRevalidator) refresh(ctx context.Context, u *domain.User, entry *revalidationEntry) {
	now := r.clock()
	// Work from a private snapshot of the latest persisted record so the CAS
	// precondition is current; the caller's object is never mutated. A store
	// failure denies; a missing/blocked account is recorded as revoked.
	cur, err := r.users.Get(ctx, u.ID)
	if err != nil {
		r.logger.WithError(err).WithField("user_id", u.ID).Warn("oauth: reload user failed; denying")
		r.recordDenial(entry, now)
		return
	}
	u = cur
	// Snapshot the login generation + credential this entry is built from so
	// fresh() can force a re-check after re-login. OAuthLoginVersion changes on
	// every accepted login even when encryption is disabled.
	entry.loginVersion = u.OAuthLoginVersion
	entry.credential = u.OAuthTokenCiphertext

	// Already blocked (deactivated/disabled/deleted): no IdP call needed.
	if u.AccessBlocked() {
		entry.state = stateRevoked
		entry.checkedAt = now
		entry.expiresAt = now.Add(jitteredTTL(stateRevoked, r.cfg.Interval, r.cfg.Grace))
		return
	}

	groups, err := r.provider.Revalidate(ctx, u)
	if err != nil {
		switch {
		case errors.Is(err, errOAuthNoCredential):
			// No credential: allow without deactivation (S8), bounded by
			// session_max_age. Never fabricate a successful entry from a store
			// failure.
			gids, gerr := r.groups.GroupsForUser(ctx, u.ID)
			if gerr != nil {
				r.logger.WithError(gerr).WithField("user_id", u.ID).Warn("oauth: load membership failed; denying")
				r.recordDenial(entry, now)
				return
			}
			r.logger.WithField("user_id", u.ID).Debug("oauth: no stored credential, allowing (bounded by session_max_age)")
			entry.state = stateOK
			entry.groupIDs = groupIDs(gids)
			entry.checkedAt = now
			entry.lastGood = now
			entry.expiresAt = now.Add(jitteredTTL(stateOK, r.cfg.Interval, r.cfg.Grace))
			return
		case errors.Is(err, errOAuthCredentialExpired):
			// Unusable credential: NOT evidence of revocation. Record
			// stateExpired WITHOUT revoke() and preserve prior groups/lastGood.
			r.logger.WithFields(logrus.Fields{
				"user_id": u.ID, "oauth_provider": u.OAuthProvider,
			}).Warn("oauth: credential expired, cannot verify membership (serving cached groups within grace / denying after grace)")
			entry.state = stateExpired
			entry.checkedAt = now
			entry.expiresAt = now.Add(jitteredTTL(stateExpired, r.cfg.Interval, r.cfg.Grace))
			return
		case errors.Is(err, errOAuthNotLeaderRefresh):
			// Follower: serve the leader's replicated membership. A store error
			// must deny rather than record authoritative success.
			r.logger.WithFields(logrus.Fields{
				"user_id": u.ID, "oauth_provider": u.OAuthProvider,
			}).Debug("oauth: refresh requires raft leadership, serving replicated membership")
			gs, gerr := r.groups.GroupsForUser(ctx, u.ID)
			if gerr != nil {
				r.logger.WithError(gerr).WithField("user_id", u.ID).Warn("oauth: load replicated membership failed; denying")
				r.recordDenial(entry, now)
				return
			}
			entry.state = stateUnavailable
			entry.groupIDs = groupIDs(gs)
			entry.lastGood = now
			entry.checkedAt = now
			entry.expiresAt = now.Add(jitteredTTL(stateUnavailable, r.cfg.Interval, r.cfg.Grace))
			return
		case errors.Is(err, domain.ErrForbidden), errors.Is(err, domain.ErrSessionRevoked):
			r.revoke(ctx, u)
			entry.state = stateRevoked
			entry.checkedAt = now
			entry.expiresAt = now.Add(jitteredTTL(stateRevoked, r.cfg.Interval, r.cfg.Grace))
			return
		default:
			// Transport error: record unavailable, preserving any prior
			// groupIDs/lastGood so the grace/fail-open policy can serve them.
			entry.state = stateUnavailable
			entry.checkedAt = now
			entry.expiresAt = now.Add(jitteredTTL(stateUnavailable, r.cfg.Interval, r.cfg.Grace))
			return
		}
	}

	// Success: prepare the desired OAuth-managed memberships, apply the role on
	// a private copy, then persist state+memberships in ONE revision-checked
	// commit. The credential source may already have advanced u.Revision during
	// Revalidate, so the final mutation uses the latest revision.
	mapped := r.mapper.mapIfActive(groups)
	managedIDs, gerr := resolveOAuthGroupIDs(ctx, r.groups, r.logger, mapped, r.mappedGroupMaxRunnerSessions)
	if gerr != nil {
		r.logger.WithError(gerr).WithField("user_id", u.ID).Warn("oauth: prepare membership failed during revalidation")
		entry.state = stateUnavailable
		entry.checkedAt = now
		entry.expiresAt = now.Add(jitteredTTL(stateUnavailable, r.cfg.Interval, r.cfg.Grace))
		return
	}
	if roleChanged := applyOAuthAdminRole(u, r.adminGroups, groups); roleChanged {
		r.logger.WithFields(logrus.Fields{
			"user_id":        u.ID,
			"oauth_provider": u.OAuthProvider,
			"role":           u.Role,
			"oauth_admin":    u.OAuthAdmin,
		}).Info("oauth: admin role changed via admin_groups (revalidation)")
	}
	res, cerr := r.users.CommitOAuth(ctx, domain.OAuthUserMutation{
		Kind: domain.OAuthMembershipCommit,
		User: &domain.User{
			ID:          u.ID,
			Role:        u.Role,
			OAuthAdmin:  u.OAuthAdmin,
			OAuthGroups: normalizeGroupList(groups),
		},
		ExpectedRevision: u.Revision,
		ManagedGroupIDs:  managedIDs,
	})
	if cerr != nil {
		if errors.Is(cerr, domain.ErrStaleUser) || errors.Is(cerr, domain.ErrAccountBlocked) || errors.Is(cerr, domain.ErrNotFound) {
			// A concurrent login/ban/deletion won: publish a short-lived denial
			// without reusing old groups or lastGood.
			r.logger.WithError(cerr).WithField("user_id", u.ID).Warn("oauth: revalidation commit conflicted; denying")
			r.recordDenial(entry, now)
			return
		}
		r.logger.WithError(cerr).WithField("user_id", u.ID).Warn("oauth: persist revalidated user failed")
		entry.state = stateUnavailable
		entry.checkedAt = now
		entry.expiresAt = now.Add(jitteredTTL(stateUnavailable, r.cfg.Interval, r.cfg.Grace))
		return
	}

	// Re-snapshot the committed generation. A committed membership mutation
	// advanced the revision but not the login version/credential, so the entry
	// remains fresh for the next caller without a spurious re-check.
	entry.loginVersion = res.User.OAuthLoginVersion
	entry.credential = res.User.OAuthTokenCiphertext
	entry.state = stateOK
	entry.groupIDs = append([]string(nil), res.GroupIDs...)
	entry.checkedAt = now
	entry.lastGood = now
	entry.expiresAt = now.Add(jitteredTTL(stateOK, r.cfg.Interval, r.cfg.Grace))
}

// recordDenial stores a short-lived denial with no group or lastGood reuse.
func (r *OAuthRevalidator) recordDenial(entry *revalidationEntry, now time.Time) {
	entry.state = stateUnavailable
	entry.groupIDs = nil
	entry.lastGood = time.Time{}
	entry.checkedAt = now
	entry.expiresAt = now.Add(jitteredTTL(stateUnavailable, r.cfg.Interval, r.cfg.Grace))
}

// revoke atomically records IdP revocation (deactivation + token deletion +
// token-version bump) as ONE revision-checked commit. There is no separate
// whole-user write or API-token deletion. The write uses a detached, bounded
// context so a cancelled request cannot lose it.
func (r *OAuthRevalidator) revoke(_ context.Context, u *domain.User) {
	persistCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := r.users.CommitOAuth(persistCtx, domain.OAuthUserMutation{
		Kind:             domain.OAuthRevocationCommit,
		User:             &domain.User{ID: u.ID},
		ExpectedRevision: u.Revision,
	}); err != nil {
		r.logger.WithError(err).WithField("user_id", u.ID).Warn("oauth: deactivate user failed (not leader or conflict)")
	}
	r.logger.WithFields(logrus.Fields{
		"user_id":        u.ID,
		"username":       u.Username,
		"oauth_provider": u.OAuthProvider,
	}).Warn("oauth: membership revalidation revoked access")
}

// jitterRng is the PRNG for cache-TTL jitter, seeded from crypto/rand so the
// jitter is unpredictable (CWE-330). math/rand is fine for jitter (no
// cryptographic property required), but the deterministic default seed is not.
var jitterRng = func() *rand.Rand {
	var seed int64
	_ = binary.Read(crand.Reader, binary.BigEndian, &seed)
	return rand.New(rand.NewSource(seed)) //nolint:gosec // G404: seeded from crypto/rand; jitter needs no cryptographic RNG.
}()

// jitterRngMu guards jitterRng: math/rand.Rand is not safe for concurrent use
// and jitteredTTL runs from concurrent per-entry refresh goroutines.
var jitterRngMu sync.Mutex

// jitteredTTL returns the cache TTL for state, with ±10% jitter to prevent
// thundering herd on refresh. For the unavailable state the retry window is
// min(interval, grace) so we re-check sooner while degraded (but never faster
// than the interval when grace is zero, to avoid hammering the IdP).
func jitteredTTL(state revalidationState, interval, grace time.Duration) time.Duration {
	base := interval
	if state == stateUnavailable && grace > 0 && grace < base {
		base = grace
	}
	jitterRngMu.Lock()
	f := jitterRng.Float64()
	jitterRngMu.Unlock()
	return time.Duration(float64(base) * (0.9 + f*0.2)) //nolint:gosec // G404: cache jitter needs no cryptographic randomness.
}
