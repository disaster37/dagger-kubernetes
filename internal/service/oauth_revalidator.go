package service

import (
	"context"
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
	tokens                       *TokenService
	logger                       *logrus.Logger
	cfg                          OAuthRevalidatorConfig
	clock                        func() time.Time

	mu    sync.Mutex
	cache map[string]*revalidationEntry
}

// NewOAuthRevalidator returns an OAuthRevalidator. Pass nil for any field that
// should be skipped (e.g. tokens when API-token revocation is not needed).
func NewOAuthRevalidator(
	provider OAuthProvider,
	mapper *GroupMapper,
	adminGroups []string,
	mappedGroupMaxRunnerSessions int,
	users *UserService,
	groups domain.GroupRepository,
	tokens *TokenService,
	logger *logrus.Logger,
	cfg OAuthRevalidatorConfig,
) *OAuthRevalidator {
	r := &OAuthRevalidator{
		provider: provider, mapper: mapper, adminGroups: adminGroups,
		mappedGroupMaxRunnerSessions: mappedGroupMaxRunnerSessions,
		users:                        users, groups: groups,
		tokens: tokens, logger: logger, cfg: cfg,
		clock: func() time.Time { return time.Now().UTC() },
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
// when access must be DENIED (revoked, or unavailable past grace with
// fail-closed). On a definitive revocation it applies side effects
// (deactivate user + revoke API token) best-effort.
func (r *OAuthRevalidator) Check(ctx context.Context, u *domain.User) ([]string, error) {
	for {
		r.mu.Lock()
		entry := r.cache[u.ID]

		if entry == nil {
			// Creator path: create the entry and perform the first check.
			entry = &revalidationEntry{inflight: make(chan struct{})}
			r.cache[u.ID] = entry
			r.mu.Unlock()
			r.refresh(ctx, u, entry)
			r.finish(entry)
			continue
		}

		if entry.inflight != nil {
			// Waiter path: another goroutine is checking; wait for its result.
			wait := entry.inflight
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-wait:
			}
			continue
		}

		// No check in flight: serve if fresh, otherwise become the refresher.
		now := r.clock()
		if r.fresh(entry, u, now) {
			gids, err := r.serve(entry, u, now)
			r.mu.Unlock()
			return gids, err
		}
		entry.inflight = make(chan struct{})
		r.mu.Unlock()

		r.refresh(ctx, u, entry)
		r.finish(entry)
		// Loop to serve the freshly computed result.
	}
}

// fresh reports whether entry can be served without a new IdP call at now.
// A revoked entry is only trusted while the user is still deactivated in the
// DB: a successful re-login clears DeactivatedAt, so a revoked cache entry
// must be re-checked to allow recovery (rather than denying forever on this
// pod). For a still-deactivated user the entry is re-trusted until expiresAt;
// refresh then short-circuits without an IdP call. An expired entry (unusable
// credential) is re-checked as soon as the user re-logs in: re-login writes a
// fresh credential ciphertext, so a changed credential forces recovery
// without waiting for revalidate_interval.
func (r *OAuthRevalidator) fresh(entry *revalidationEntry, u *domain.User, now time.Time) bool {
	if entry.state == stateRevoked && u.DeactivatedAt == nil {
		return false
	}
	if entry.state == stateExpired && entry.credential != u.OAuthTokenCiphertext {
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
	// Snapshot the credential this entry is built from so fresh() can force a
	// re-check after re-login (AES-GCM ciphertext always differs once rotated).
	entry.credential = u.OAuthTokenCiphertext

	// Already deactivated: no IdP call needed.
	if u.DeactivatedAt != nil {
		entry.state = stateRevoked
		entry.checkedAt = now
		entry.expiresAt = now.Add(jitteredTTL(stateRevoked, r.cfg.Interval, r.cfg.Grace))
		return
	}

	groups, err := r.provider.Revalidate(ctx, u)
	if err != nil {
		switch {
		case errors.Is(err, errOAuthNoCredential):
			// No credential: allow without deactivation (S8), bounded by session_max_age.
			r.logger.WithField("user_id", u.ID).Debug("oauth: no stored credential, allowing (bounded by session_max_age)")
			gids, _ := r.groups.GroupsForUser(ctx, u.ID)
			entry.state = stateOK
			entry.groupIDs = groupIDs(gids)
			entry.checkedAt = now
			entry.lastGood = now
			entry.expiresAt = now.Add(jitteredTTL(stateOK, r.cfg.Interval, r.cfg.Grace))
			return
		case errors.Is(err, errOAuthCredentialExpired):
			// Unusable credential (expired/rotated/revoked — ambiguous per
			// RFC 6749 §5.2): NOT evidence of revocation. Record stateExpired
			// WITHOUT revoke() (no deactivation, no API-token deletion) and
			// preserve any prior groupIDs/lastGood so the grace/fail policy
			// can serve them.
			r.logger.WithFields(logrus.Fields{
				"user_id": u.ID, "oauth_provider": u.OAuthProvider,
			}).Warn("oauth: credential expired, cannot verify membership (serving cached groups within grace / denying after grace)")
			entry.state = stateExpired
			entry.checkedAt = now
			entry.expiresAt = now.Add(jitteredTTL(stateExpired, r.cfg.Interval, r.cfg.Grace))
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

	// Success: reconcile OAuth-managed memberships (add new, remove stale).
	mapped := r.mapper.mapIfActive(groups)
	gids, err := reconcileMemberships(ctx, r.groups, r.logger, u, mapped, r.mappedGroupMaxRunnerSessions)
	if err != nil {
		r.logger.WithError(err).WithField("user_id", u.ID).Warn("oauth: membership reconciliation failed during revalidation")
	} else {
		u.OAuthGroupIDs = gids
	}
	u.OAuthGroups = normalizeGroupList(groups)
	if roleChanged := applyOAuthAdminRole(u, r.adminGroups, groups); roleChanged {
		r.logger.WithFields(logrus.Fields{
			"user_id":        u.ID,
			"oauth_provider": u.OAuthProvider,
			"role":           u.Role,
			"oauth_admin":    u.OAuthAdmin,
		}).Info("oauth: admin role changed via admin_groups (revalidation)")
	}
	if err := r.users.Update(ctx, u); err != nil {
		r.logger.WithError(err).WithField("user_id", u.ID).Warn("oauth: persist revalidated user failed")
	}
	// Re-snapshot the credential: Revalidate's refreshingSource may have rotated and
	// persisted u.OAuthTokenCiphertext mid-check (oauth_oidc.go refreshingSource.Token).
	// Keeping the top-of-refresh snapshot would make a later stateExpired entry appear
	// to have a "changed credential" and force one spurious re-check.
	entry.credential = u.OAuthTokenCiphertext
	entry.state = stateOK
	gs, _ := r.groups.GroupsForUser(ctx, u.ID)
	entry.groupIDs = groupIDs(gs)
	entry.checkedAt = now
	entry.lastGood = now
	entry.expiresAt = now.Add(jitteredTTL(stateOK, r.cfg.Interval, r.cfg.Grace))
}

// revoke marks the user as deactivated and revokes their API token. The write
// uses a detached, bounded context so a cancelled request cannot lose the
// revocation write (best-effort on followers, where the leader independently
// revalidates and persists deactivation).
func (r *OAuthRevalidator) revoke(_ context.Context, u *domain.User) {
	now := r.clock()
	u.DeactivatedAt = &now

	persistCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := r.users.Update(persistCtx, u); err != nil {
		r.logger.WithError(err).WithField("user_id", u.ID).Warn("oauth: deactivate user failed (not leader?)")
	}
	if r.tokens != nil {
		if err := r.tokens.Revoke(persistCtx, u.ID); err != nil && !errors.Is(err, domain.ErrNotFound) {
			r.logger.WithError(err).WithField("user_id", u.ID).Warn("oauth: revoke api token failed")
		}
	}
	r.logger.WithFields(logrus.Fields{
		"user_id":        u.ID,
		"username":       u.Username,
		"oauth_provider": u.OAuthProvider,
	}).Warn("oauth: membership revalidation revoked access")
}

// jitteredTTL returns the cache TTL for state, with ±10% jitter to prevent
// thundering herd on refresh. For the unavailable state the retry window is
// min(interval, grace) so we re-check sooner while degraded (but never faster
// than the interval when grace is zero, to avoid hammering the IdP).
func jitteredTTL(state revalidationState, interval, grace time.Duration) time.Duration {
	base := interval
	if state == stateUnavailable && grace > 0 && grace < base {
		base = grace
	}
	return time.Duration(float64(base) * (0.9 + rand.Float64()*0.2)) //nolint:gosec // G404: cache jitter needs no cryptographic randomness.
}
