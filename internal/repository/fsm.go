package repository

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// commandKind identifies the deterministic mutation carried by a command.
type commandKind byte

const (
	kindUpsertUser commandKind = iota + 1
	kindDeleteUser
	kindUpsertGroup
	kindDeleteGroup
	kindSetMembers
	kindUpsertProject
	kindDeleteProject
	kindUpsertToken
	kindDeleteToken
	kindTouchToken
	kindUpsertTraceProvision
	kindUpsertTraceIngest
	kindMarkTraceFailed
	kindSetMeta
	// Slots 15, 16, 18, 19, 20, 21 and 25 were the registry-cache routing
	// kinds (manifest/blob routes and upload sessions). They are reserved:
	// their numeric IDs are persisted in the Raft log (command.Kind) and must
	// never be renumbered, or a node replaying a pre-upgrade log tail would
	// misapply commands. Their apply cases are decode-only no-ops below.
	kindUpsertManifestRoute
	kindDeleteManifestRoute
	// Slot 17 was kindDeleteRoutesForBackend (removed). It is reserved via a
	// blank identifier for the same reason.
	_
	kindUpsertBlobRoute
	kindRecordUpload
	kindDeleteUpload
	kindReapUploads
	kindDeleteTrace
	kindUpsertSession
	kindTouchSession
	kindTouchManifestRoute
	// Account-lifecycle commands (appended after every existing kind so the
	// persisted numeric IDs above never change on replay).
	kindUserState
	kindSoftDeleteUser
	kindOAuthUserMutation
)

// command is a single Raft log payload: a kind plus a JSON payload decoded by
// applyCommand.
type command struct {
	Kind commandKind     `json:"k"`
	Data json.RawMessage `json:"d"`
}

// newCommand marshals payload and wraps it in a command of the given kind.
func newCommand(kind commandKind, payload any) (*command, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal raft command payload: %w", err)
	}
	return &command{Kind: kind, Data: data}, nil
}

// decode unmarshals a command's payload into T, wrapping decode errors with the
// given label. It centralizes the per-command unmarshal boilerplate in
// applyCommand.
func decode[T any](cmd *command, label string) (T, error) {
	var p T
	if err := json.Unmarshal(cmd.Data, &p); err != nil {
		return p, fmt.Errorf("decode %s: %w", label, err)
	}
	return p, nil
}

// applyPayload decodes cmd's payload as T and runs fn, collapsing the
// decode-and-dispatch boilerplate shared by most command cases in applyCommand.
func applyPayload[T any](cmd *command, label string, fn func(T) error) error {
	p, err := decode[T](cmd, label)
	if err != nil {
		return err
	}
	return fn(p)
}

// command payloads (JSON-serialized into command.Data).
type (
	cmdUser struct {
		ID                   string      `json:"id"`
		Username             string      `json:"username"`
		Role                 domain.Role `json:"role"`
		PasswordHash         string      `json:"password_hash"`
		OAuthProvider        string      `json:"oauth_provider"`
		OAuthID              string      `json:"oauth_id"`
		OAuthTokenCiphertext string      `json:"oauth_token_ciphertext"`
		OAuthGroupIDs        []string    `json:"oauth_group_ids,omitempty"`
		OAuthGroups          []string    `json:"oauth_groups,omitempty"`
		OAuthAdmin           bool        `json:"oauth_admin,omitempty"`
		DeactivatedAt        *time.Time  `json:"deactivated_at,omitempty"`
		DisabledAt           *time.Time  `json:"disabled_at,omitempty"`
		DeletedAt            *time.Time  `json:"deleted_at,omitempty"`
		Revision             uint64      `json:"revision,omitempty"`
		OAuthLoginVersion    uint64      `json:"oauth_login_version,omitempty"`
		TokenVersion         int         `json:"token_version"`
		CreatedAt            time.Time   `json:"created_at"`
		UpdatedAt            time.Time   `json:"updated_at"`
		Create               bool        `json:"create,omitempty"` // true = insert-only; false = update-only
		// ExpectedRevision is a command-only optimistic-concurrency
		// precondition. It is never populated when building a snapshot (see
		// cmdUserFrom), so snapshots never serialize it. A nil pointer means a
		// legacy replay without a precondition; a pointer to zero is a valid
		// CAS update of a pre-upgrade row.
		ExpectedRevision *uint64 `json:"expected_revision,omitempty"`
	}

	cmdGroup struct {
		Group  domain.Group `json:"group"`
		Create bool         `json:"create"`
	}

	cmdProject struct {
		Project domain.Project `json:"project"`
		Create  bool           `json:"create"`
	}

	cmdToken struct {
		ID              string     `json:"id"`
		UserID          string     `json:"user_id"`
		TokenHash       string     `json:"token_hash"`
		TokenCiphertext string     `json:"token_ciphertext"`
		Prefix          string     `json:"prefix"`
		CreatedAt       time.Time  `json:"created_at"`
		LastUsedAt      *time.Time `json:"last_used_at,omitempty"`
	}

	cmdSetMembers struct {
		GroupID string   `json:"group_id"`
		UserIDs []string `json:"user_ids"`
	}

	cmdDeleteToken struct {
		UserID string `json:"user_id"`
	}

	cmdTouchToken struct {
		ID string    `json:"id"`
		At time.Time `json:"at"`
	}

	cmdUpsertTraceProvision struct {
		TraceID   string    `json:"trace_id"`
		UserID    string    `json:"user_id"`
		Version   string    `json:"version"`
		UpdatedAt time.Time `json:"updated_at"`
	}

	cmdMarkTraceFailed struct {
		TraceID   string    `json:"trace_id"`
		Reason    string    `json:"reason"`
		UpdatedAt time.Time `json:"updated_at"`
	}

	cmdSetMeta struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}

	cmdDeleteTrace struct {
		TraceID string `json:"trace_id"`
	}

	// cmdUpsertSession registers (or re-registers) a session lease. The lease
	// fields and the At timestamp come from the leader so replay on every pod
	// produces identical local state.
	cmdUpsertSession struct {
		Lease domain.Lease `json:"lease"`
		At    time.Time    `json:"at"`
	}

	// cmdTouchSession refreshes a lease's LastActivity from the leader clock.
	cmdTouchSession struct {
		CertFP string    `json:"cert_fp"`
		At     time.Time `json:"at"`
	}

	// cmdUserState applies an administrator lifecycle action. At is computed
	// by the leader; the FSM never reads the clock.
	cmdUserState struct {
		ActorID string                 `json:"actor_id"`
		ID      string                 `json:"id"`
		Action  domain.UserStateAction `json:"action"`
		At      time.Time              `json:"at"`
	}

	// cmdSoftDeleteUser tombstones a user account.
	cmdSoftDeleteUser struct {
		ActorID string    `json:"actor_id"`
		ID      string    `json:"id"`
		At      time.Time `json:"at"`
	}

	// cmdOAuthUserMutation is a revision-checked atomic OAuth account
	// mutation. ExpectedRevision is check-only and never stored.
	cmdOAuthUserMutation struct {
		Kind             domain.OAuthUserMutationKind `json:"kind"`
		User             *cmdUser                     `json:"user"`
		ExpectedRevision uint64                       `json:"expected_revision"`
		ManagedGroupIDs  []string                     `json:"managed_group_ids,omitempty"`
		FallbackGroupID  string                       `json:"fallback_group_id,omitempty"`
		At               time.Time                    `json:"at"`
	}
)

// toDomain converts a command user payload into a domain.User.
func (c *cmdUser) toDomain() *domain.User {
	return &domain.User{
		ID:                   c.ID,
		Username:             c.Username,
		Role:                 c.Role,
		PasswordHash:         c.PasswordHash,
		OAuthProvider:        c.OAuthProvider,
		OAuthID:              c.OAuthID,
		OAuthTokenCiphertext: c.OAuthTokenCiphertext,
		OAuthGroupIDs:        c.OAuthGroupIDs,
		OAuthGroups:          c.OAuthGroups,
		OAuthAdmin:           c.OAuthAdmin,
		DeactivatedAt:        c.DeactivatedAt,
		DisabledAt:           c.DisabledAt,
		DeletedAt:            c.DeletedAt,
		Revision:             c.Revision,
		OAuthLoginVersion:    c.OAuthLoginVersion,
		TokenVersion:         c.TokenVersion,
		CreatedAt:            c.CreatedAt,
		UpdatedAt:            c.UpdatedAt,
	}
}

// cmdUserFrom converts a domain.User into a command user payload (preserving
// the password hash that the domain JSON tag hides). ExpectedRevision is left
// nil so snapshots never serialize a precondition.
func cmdUserFrom(u *domain.User) *cmdUser {
	return &cmdUser{
		ID:                   u.ID,
		Username:             u.Username,
		Role:                 u.Role,
		PasswordHash:         u.PasswordHash,
		OAuthProvider:        u.OAuthProvider,
		OAuthID:              u.OAuthID,
		OAuthTokenCiphertext: u.OAuthTokenCiphertext,
		OAuthGroupIDs:        u.OAuthGroupIDs,
		OAuthGroups:          u.OAuthGroups,
		OAuthAdmin:           u.OAuthAdmin,
		DeactivatedAt:        u.DeactivatedAt,
		DisabledAt:           u.DisabledAt,
		DeletedAt:            u.DeletedAt,
		Revision:             u.Revision,
		OAuthLoginVersion:    u.OAuthLoginVersion,
		TokenVersion:         u.TokenVersion,
		CreatedAt:            u.CreatedAt,
		UpdatedAt:            u.UpdatedAt,
	}
}

// toDomain converts a command token payload into a domain.APIToken.
func (c *cmdToken) toDomain() *domain.APIToken {
	return &domain.APIToken{
		ID:              c.ID,
		UserID:          c.UserID,
		TokenHash:       c.TokenHash,
		TokenCiphertext: c.TokenCiphertext,
		Prefix:          c.Prefix,
		CreatedAt:       c.CreatedAt,
		LastUsedAt:      c.LastUsedAt,
	}
}

// cmdTokenFrom converts a domain.APIToken into a command token payload
// (preserving the hashes that the domain JSON tags hide).
func cmdTokenFrom(t *domain.APIToken) *cmdToken {
	return &cmdToken{
		ID:              t.ID,
		UserID:          t.UserID,
		TokenHash:       t.TokenHash,
		TokenCiphertext: t.TokenCiphertext,
		Prefix:          t.Prefix,
		CreatedAt:       t.CreatedAt,
		LastUsedAt:      t.LastUsedAt,
	}
}

// fsmState holds the in-memory replica of every row the legacy SQLite database
// held, protected by a sync.RWMutex. All writes go through applyCommand under
// the write lock; all reads take the read lock.
type fsmState struct {
	mu sync.RWMutex

	users        map[string]*domain.User
	usersByName  map[string]string
	usersByOAuth map[string]string

	groups            map[string]*domain.Group
	groupsByName      map[string]string
	memberships       map[string]map[string]struct{} // groupID -> set(userID)
	membershipsByUser map[string]map[string]struct{} // userID -> set(groupID)

	tokens       map[string]*domain.APIToken
	tokensByHash map[string]string
	tokensByUser map[string]string

	projects       map[string]*domain.Project
	projectsByName map[string]string

	traces map[string]*domain.TraceMeta

	meta map[string]string

	// sessionSink receives replicated session-lease state (domain.SessionStateSink,
	// usually the pod-local *service.Store). Nil until wired by NewRaftStore.
	sessionSink domain.SessionStateSink
}

func newState() *fsmState {
	return &fsmState{
		users:             make(map[string]*domain.User),
		usersByName:       make(map[string]string),
		usersByOAuth:      make(map[string]string),
		groups:            make(map[string]*domain.Group),
		groupsByName:      make(map[string]string),
		memberships:       make(map[string]map[string]struct{}),
		membershipsByUser: make(map[string]map[string]struct{}),
		tokens:            make(map[string]*domain.APIToken),
		tokensByHash:      make(map[string]string),
		tokensByUser:      make(map[string]string),
		projects:          make(map[string]*domain.Project),
		projectsByName:    make(map[string]string),
		traces:            make(map[string]*domain.TraceMeta),
		meta:              make(map[string]string),
	}
}

// FSM is the Raft finite state machine. All reads are served from the local
// state; all writes are deterministic applications of a command.
type FSM struct {
	state *fsmState
}

// NewFSM returns an empty FSM.
func NewFSM() *FSM {
	return &FSM{state: newState()}
}

var _ raft.FSM = (*FSM)(nil)

// Apply decodes a Raft log entry and applies it. Most commands return nil or an
// error; kindReapUploads is the documented exception and returns the number of
// reaped sessions as an int.
func (f *FSM) Apply(log *raft.Log) interface{} {
	cmd := &command{}
	if err := json.Unmarshal(log.Data, cmd); err != nil {
		return fmt.Errorf("decode raft command: %w", err)
	}
	resp, err := f.applyCommand(cmd)
	if err != nil {
		return err
	}
	return resp
}

// applyCommand is the deterministic core. It is called by Apply and directly
// by unit tests (no Raft instance). It never reads the clock; timestamps are
// computed by the leader before the command is enqueued.
func (f *FSM) applyCommand(cmd *command) (interface{}, error) {
	s := f.state
	s.mu.Lock()
	defer s.mu.Unlock()

	if result, err, handled := s.applyEntityCommand(cmd); handled {
		return result, err
	}
	if result, err, handled := s.applyTraceCacheCommand(cmd); handled {
		return result, err
	}
	if err, handled := s.applySessionCommand(cmd); handled {
		return nil, err
	}
	return nil, fmt.Errorf("unknown raft command kind %d", cmd.Kind)
}

// applyEntityCommand handles user/group/project/token commands. Handled
// commands may return a non-nil response object (the committed snapshot) which
// the repository layer type-checks.
func (s *fsmState) applyEntityCommand(cmd *command) (interface{}, error, bool) {
	switch cmd.Kind {
	case kindUpsertUser:
		var out *domain.User
		err := applyPayload(cmd, "user", func(p cmdUser) error {
			u, err := s.upsertUser(&p)
			out = u
			return err
		})
		return out, err, true
	case kindDeleteUser:
		// Legacy hard-delete replay semantics: unchanged.
		return nil, applyPayload(cmd, "delete user", s.deleteUser), true
	case kindUserState:
		var out *domain.User
		err := applyPayload(cmd, "user state", func(p cmdUserState) error {
			u, err := s.setUserState(p)
			out = u
			return err
		})
		return out, err, true
	case kindSoftDeleteUser:
		return nil, applyPayload(cmd, "soft delete user", s.softDeleteUser), true
	case kindOAuthUserMutation:
		var out *domain.OAuthUserResult
		err := applyPayload(cmd, "oauth user mutation", func(p cmdOAuthUserMutation) error {
			res, err := s.commitOAuthUser(&p)
			out = res
			return err
		})
		return out, err, true
	case kindUpsertGroup:
		return nil, applyPayload(cmd, "group", func(p cmdGroup) error {
			return s.upsertGroup(&p.Group, p.Create)
		}), true
	case kindDeleteGroup:
		return nil, applyPayload(cmd, "delete group", s.deleteGroup), true
	case kindSetMembers:
		return nil, applyPayload(cmd, "set members", func(p cmdSetMembers) error {
			return s.setMembers(p.GroupID, p.UserIDs)
		}), true
	case kindUpsertProject:
		return nil, applyPayload(cmd, "project", func(p cmdProject) error {
			return s.upsertProject(&p.Project, p.Create)
		}), true
	case kindDeleteProject:
		return nil, applyPayload(cmd, "delete project", s.deleteProject), true
	case kindUpsertToken:
		return nil, applyPayload(cmd, "api token", func(p cmdToken) error {
			return s.upsertToken(p.toDomain())
		}), true
	case kindDeleteToken:
		return nil, applyPayload(cmd, "delete token", func(p cmdDeleteToken) error {
			s.deleteToken(p.UserID)
			return nil
		}), true
	case kindTouchToken:
		return nil, applyPayload(cmd, "touch token", func(p cmdTouchToken) error {
			s.touchToken(p.ID, p.At)
			return nil
		}), true
	default:
		return nil, nil, false
	}
}

// applyTraceCacheCommand handles trace metadata, meta KV, and cache routing
// commands.
func (s *fsmState) applyTraceCacheCommand(cmd *command) (interface{}, error, bool) {
	switch cmd.Kind {
	case kindUpsertTraceProvision:
		return nil, applyPayload(cmd, "trace provision", func(p cmdUpsertTraceProvision) error {
			s.upsertTraceProvision(p)
			return nil
		}), true
	case kindUpsertTraceIngest:
		return nil, applyPayload(cmd, "trace ingest", func(m domain.TraceMeta) error {
			s.upsertTraceIngest(&m)
			return nil
		}), true
	case kindMarkTraceFailed:
		p, err := decode[cmdMarkTraceFailed](cmd, "mark trace failed")
		if err != nil {
			return nil, err, true
		}
		return s.markTraceFailed(p), nil, true
	case kindSetMeta:
		return nil, applyPayload(cmd, "set meta", func(p cmdSetMeta) error {
			s.meta[p.Key] = p.Value
			return nil
		}), true
	case kindUpsertManifestRoute, kindDeleteManifestRoute, kindUpsertBlobRoute,
		kindRecordUpload, kindDeleteUpload, kindReapUploads:
		// Reserved registry-cache routing kinds. Decode-only no-op: the
		// payload is discarded without unmarshalling so a node replaying a
		// pre-upgrade Raft log tail does not error (the routing state was
		// removed along with the remote cache).
		return nil, nil, true
	case kindDeleteTrace:
		return nil, applyPayload(cmd, "delete trace", func(p cmdDeleteTrace) error {
			delete(s.traces, p.TraceID)
			return nil
		}), true
	default:
		return nil, nil, false
	}
}

// applySessionCommand handles the session-lease commands. The session state
// itself is NOT stored in the FSM maps: it is forwarded to the pod-local
// sessionSink so the local domain.SessionStore (used by the data plane,
// quota, and the fleet sweeper) mirrors the replicated state on every pod.
func (s *fsmState) applySessionCommand(cmd *command) (error, bool) {
	switch cmd.Kind {
	case kindUpsertSession:
		return applyPayload(cmd, "session register", func(p cmdUpsertSession) error {
			lease := p.Lease
			lease.LastActivity = p.At
			if s.sessionSink != nil {
				s.sessionSink.ApplySessionRegistered(&lease)
			}
			return nil
		}), true
	case kindTouchSession:
		return applyPayload(cmd, "session touch", func(p cmdTouchSession) error {
			if s.sessionSink != nil {
				s.sessionSink.ApplySessionTouched(p.CertFP, p.At)
			}
			return nil
		}), true
	case kindTouchManifestRoute:
		// Reserved registry-cache routing kind. Decode-only no-op (see
		// applyTraceCacheCommand).
		return nil, true
	default:
		return nil, false
	}
}

// --- users ------------------------------------------------------------------

// upsertUser applies a create or CAS-tagged update and returns the committed
// snapshot. Immutable identity, administrator timestamps and OAuthLoginVersion
// may only change through their dedicated commands.
func (s *fsmState) upsertUser(c *cmdUser) (*domain.User, error) {
	if c.Create {
		return s.createUser(c)
	}
	return s.updateUser(c)
}

// createUser inserts a new record, starting its revision at 1.
func (s *fsmState) createUser(c *cmdUser) (*domain.User, error) {
	if _, ok := s.users[c.ID]; ok {
		return nil, fmt.Errorf("user %s: %w", c.ID, domain.ErrConflict)
	}
	if _, ok := s.usersByName[strings.ToLower(c.Username)]; ok {
		return nil, fmt.Errorf("user %s: %w", c.Username, domain.ErrConflict)
	}
	if c.OAuthID != "" {
		if _, ok := s.usersByOAuth[oauthKey(c.OAuthProvider, c.OAuthID)]; ok {
			return nil, fmt.Errorf("user oauth %s/%s: %w", c.OAuthProvider, c.OAuthID, domain.ErrConflict)
		}
	}
	u := c.toDomain()
	u.Revision = 1
	if u.UpdatedAt.IsZero() {
		u.UpdatedAt = u.CreatedAt
	}
	s.storeUser(u)
	return copyUser(u), nil
}

// updateUser applies a CAS-tagged update. Immutable identity, administrator
// timestamps and OAuthLoginVersion are preserved; the revision is advanced.
func (s *fsmState) updateUser(c *cmdUser) (*domain.User, error) {
	existing, ok := s.users[c.ID]
	if !ok {
		return nil, fmt.Errorf("update user %s: %w", c.ID, domain.ErrNotFound)
	}
	if existing.DeletedAt != nil {
		// A general update may not resurrect (or touch) a tombstone.
		return nil, fmt.Errorf("user %s: %w", c.ID, domain.ErrAccountBlocked)
	}
	if c.ExpectedRevision != nil && existing.Revision != *c.ExpectedRevision {
		return nil, fmt.Errorf("user %s: %w", c.ID, domain.ErrStaleUser)
	}
	if err := s.checkUserUniqueness(c, existing.ID); err != nil {
		return nil, err
	}
	// An explicit role demotion must not remove the last active administrator.
	if existing.Role == domain.RoleAdmin && c.Role != domain.RoleAdmin && isActiveAdmin(existing) && s.activeAdminCount() <= 1 {
		return nil, fmt.Errorf("user %s: %w", c.ID, domain.ErrLastAdmin)
	}

	u := c.toDomain()
	// Preserve identity and dedicated-command-only fields.
	u.OAuthProvider = existing.OAuthProvider
	u.OAuthID = existing.OAuthID
	u.DisabledAt = existing.DisabledAt
	u.DeletedAt = existing.DeletedAt
	u.OAuthLoginVersion = existing.OAuthLoginVersion
	u.CreatedAt = existing.CreatedAt
	if u.UpdatedAt.IsZero() {
		u.UpdatedAt = existing.UpdatedAt
	}
	u.Revision = existing.Revision + 1

	// Refresh reverse indices (username may have changed).
	if existing.Username != "" {
		delete(s.usersByName, strings.ToLower(existing.Username))
	}
	if existing.OAuthID != "" {
		delete(s.usersByOAuth, oauthKey(existing.OAuthProvider, existing.OAuthID))
	}
	s.storeUser(u)
	return copyUser(u), nil
}

// checkUserUniqueness rejects a username/identity that collides with a
// different user.
func (s *fsmState) checkUserUniqueness(c *cmdUser, selfID string) error {
	if other, ok := s.usersByName[strings.ToLower(c.Username)]; ok && other != selfID {
		return fmt.Errorf("user %s: %w", c.Username, domain.ErrConflict)
	}
	if c.OAuthID != "" {
		if other, ok := s.usersByOAuth[oauthKey(c.OAuthProvider, c.OAuthID)]; ok && other != selfID {
			return fmt.Errorf("user oauth %s/%s: %w", c.OAuthProvider, c.OAuthID, domain.ErrConflict)
		}
	}
	return nil
}

func (s *fsmState) storeUser(u *domain.User) {
	s.users[u.ID] = copyUser(u)
	s.usersByName[strings.ToLower(u.Username)] = u.ID
	if u.OAuthID != "" {
		s.usersByOAuth[oauthKey(u.OAuthProvider, u.OAuthID)] = u.ID
	}
}

// copyUser returns a deep copy of u so callers can never mutate FSM-owned
// slices or timestamp pointers outside the write mutex. A nil user yields nil.
func copyUser(u *domain.User) *domain.User {
	if u == nil {
		return nil
	}
	cp := *u
	if u.OAuthGroupIDs != nil {
		cp.OAuthGroupIDs = append([]string(nil), u.OAuthGroupIDs...)
	}
	if u.OAuthGroups != nil {
		cp.OAuthGroups = append([]string(nil), u.OAuthGroups...)
	}
	cp.DeactivatedAt = copyTime(u.DeactivatedAt)
	cp.DisabledAt = copyTime(u.DisabledAt)
	cp.DeletedAt = copyTime(u.DeletedAt)
	return &cp
}

// copyTime returns a pointer to a copy of t, or nil.
func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	cp := *t
	return &cp
}

// activeAdminCount returns the number of administrators that are not blocked
// by disable, deletion or IdP revocation.
func (s *fsmState) activeAdminCount() int {
	n := 0
	for _, u := range s.users {
		if u.Role == domain.RoleAdmin && !u.AccessBlocked() {
			n++
		}
	}
	return n
}

// isActiveAdmin reports whether u counts toward the last-admin guard.
func isActiveAdmin(u *domain.User) bool {
	return u != nil && u.Role == domain.RoleAdmin && !u.AccessBlocked()
}

func (s *fsmState) storeGroup(g *domain.Group) {
	cp := *g
	s.groups[g.ID] = &cp
	s.groupsByName[strings.ToLower(g.Name)] = g.ID
}

func (s *fsmState) storeProject(p *domain.Project) {
	cp := *p
	s.projects[p.ID] = &cp
	s.projectsByName[strings.ToLower(p.Name)] = p.ID
}

func (s *fsmState) deleteUser(id string) error {
	u, ok := s.users[id]
	if !ok {
		return fmt.Errorf("delete user %s: %w", id, domain.ErrNotFound)
	}
	// Cascade token.
	if tokenID, ok := s.tokensByUser[id]; ok {
		s.removeToken(tokenID)
	}
	// Cascade memberships.
	for gid := range s.membershipsByUser[id] {
		delete(s.memberships[gid], id)
		if len(s.memberships[gid]) == 0 {
			delete(s.memberships, gid)
		}
	}
	delete(s.membershipsByUser, id)
	// Null trace_meta.user_id (ON DELETE SET NULL).
	for traceID, m := range s.traces {
		if m.UserID == id {
			m.UserID = ""
			s.traces[traceID] = m
		}
	}
	delete(s.users, id)
	delete(s.usersByName, strings.ToLower(u.Username))
	if u.OAuthID != "" {
		delete(s.usersByOAuth, oauthKey(u.OAuthProvider, u.OAuthID))
	}
	return nil
}

// removeUserMemberships deletes every membership edge for userID in both
// directions.
func (s *fsmState) removeUserMemberships(userID string) {
	for gid := range s.membershipsByUser[userID] {
		delete(s.memberships[gid], userID)
		if len(s.memberships[gid]) == 0 {
			delete(s.memberships, gid)
		}
	}
	delete(s.membershipsByUser, userID)
}

// setUserState applies an administrator disable/enable/restore action
// atomically, enforcing self and last-active-admin guards in the FSM itself.
func (s *fsmState) setUserState(p cmdUserState) (*domain.User, error) {
	u, ok := s.users[p.ID]
	if !ok {
		return nil, fmt.Errorf("user state %s: %w", p.ID, domain.ErrNotFound)
	}
	switch p.Action {
	case domain.UserDisable:
		if p.ActorID != "" && p.ActorID == p.ID {
			return nil, fmt.Errorf("disable user %s: %w", p.ID, domain.ErrForbidden)
		}
		if u.DisabledAt != nil {
			return copyUser(u), nil // idempotent: no counter bump
		}
		if isActiveAdmin(u) && s.activeAdminCount() <= 1 {
			return nil, fmt.Errorf("disable user %s: %w", p.ID, domain.ErrLastAdmin)
		}
		u.DisabledAt = copyTime(&p.At)
		u.TokenVersion++
		s.deleteToken(p.ID)
		u.Revision++
		u.UpdatedAt = p.At
		s.storeUser(u)
		return copyUser(s.users[p.ID]), nil
	case domain.UserEnable:
		if u.DeletedAt != nil {
			return nil, fmt.Errorf("enable user %s: %w", p.ID, domain.ErrAccountBlocked)
		}
		if u.DisabledAt == nil {
			return copyUser(u), nil // idempotent
		}
		u.DisabledAt = nil
		// Never restore an API token; the user must generate a new one.
		u.Revision++
		u.UpdatedAt = p.At
		s.storeUser(u)
		return copyUser(s.users[p.ID]), nil
	case domain.UserRestore:
		if u.DeletedAt == nil {
			return copyUser(u), nil // idempotent
		}
		u.DeletedAt = nil
		if u.DisabledAt == nil {
			// A restored account stays disabled until explicitly enabled.
			u.DisabledAt = copyTime(&p.At)
		}
		u.Revision++
		u.UpdatedAt = p.At
		s.storeUser(u)
		return copyUser(s.users[p.ID]), nil
	default:
		return nil, fmt.Errorf("user state %s: invalid action %q: %w", p.ID, p.Action, domain.ErrValidation)
	}
}

// softDeleteUser tombstones an account: credentials, token and memberships are
// removed while the row, username and identity reservation are preserved.
func (s *fsmState) softDeleteUser(p cmdSoftDeleteUser) error {
	u, ok := s.users[p.ID]
	if !ok {
		return fmt.Errorf("delete user %s: %w", p.ID, domain.ErrNotFound)
	}
	if u.DeletedAt != nil {
		return nil // idempotent
	}
	if p.ActorID != "" && p.ActorID == p.ID {
		return fmt.Errorf("delete user %s: %w", p.ID, domain.ErrForbidden)
	}
	if isActiveAdmin(u) && s.activeAdminCount() <= 1 {
		return fmt.Errorf("delete user %s: %w", p.ID, domain.ErrLastAdmin)
	}
	s.deleteToken(p.ID)
	s.removeUserMemberships(p.ID)
	u.DeletedAt = copyTime(&p.At)
	u.DisabledAt = copyTime(&p.At)
	u.PasswordHash = ""
	u.OAuthTokenCiphertext = ""
	u.OAuthGroups = nil
	u.OAuthGroupIDs = nil
	u.OAuthAdmin = false
	u.TokenVersion++
	u.Revision++
	u.UpdatedAt = p.At
	s.storeUser(u)
	return nil
}

// commitOAuthUser atomically applies an OAuth account mutation. The revision
// precondition is checked before any account, membership or token effect, so a
// stale mutation has no effect at all.
func (s *fsmState) commitOAuthUser(p *cmdOAuthUserMutation) (*domain.OAuthUserResult, error) {
	existing, err := s.oauthTarget(p)
	if err != nil {
		return nil, err
	}
	switch p.Kind {
	case domain.OAuthLoginCommit:
		return s.oauthLogin(existing, p)
	case domain.OAuthMembershipCommit:
		return s.oauthMembership(existing, p)
	case domain.OAuthCredentialCommit:
		return s.oauthCredential(existing, p)
	case domain.OAuthRevocationCommit:
		return s.oauthRevoke(existing, p)
	default:
		return nil, fmt.Errorf("oauth mutation: invalid kind %q: %w", p.Kind, domain.ErrValidation)
	}
}

// oauthTarget validates the command shape and the revision precondition before
// any effect. It returns the current record on success.
func (s *fsmState) oauthTarget(p *cmdOAuthUserMutation) (*domain.User, error) {
	if p.User == nil {
		return nil, fmt.Errorf("oauth mutation: %w", domain.ErrValidation)
	}
	if p.User.ID == "" {
		return nil, fmt.Errorf("oauth mutation: missing user id: %w", domain.ErrValidation)
	}
	existing, ok := s.users[p.User.ID]
	if !ok {
		return nil, fmt.Errorf("oauth user %s: %w", p.User.ID, domain.ErrNotFound)
	}
	// Revision precondition first: no effect whatsoever on mismatch.
	if existing.Revision != p.ExpectedRevision {
		return nil, fmt.Errorf("oauth user %s: %w", p.User.ID, domain.ErrStaleUser)
	}
	if existing.DeletedAt != nil {
		return nil, fmt.Errorf("oauth user %s: %w", p.User.ID, domain.ErrAccountBlocked)
	}
	if p.Kind != domain.OAuthLoginCommit && p.Kind != domain.OAuthMembershipCommit {
		return existing, nil
	}
	// Login/membership only apply to OAuth-backed targets.
	if existing.OAuthProvider == "" {
		return nil, fmt.Errorf("oauth user %s: not an oauth account: %w", p.User.ID, domain.ErrValidation)
	}
	// Validate the desired managed memberships before any effect.
	if err := s.validateGroupIDs(p.ManagedGroupIDs); err != nil {
		return nil, err
	}
	if p.FallbackGroupID != "" {
		if _, ok := s.groups[p.FallbackGroupID]; !ok {
			return nil, fmt.Errorf("oauth fallback group %s: %w", p.FallbackGroupID, domain.ErrNotFound)
		}
	}
	return existing, nil
}

func (s *fsmState) oauthLogin(existing *domain.User, p *cmdOAuthUserMutation) (*domain.OAuthUserResult, error) {
	if existing.DisabledAt != nil {
		return nil, fmt.Errorf("oauth user %s: %w", p.User.ID, domain.ErrAccountBlocked)
	}
	if p.User.OAuthProvider == "" || p.User.OAuthID == "" {
		return nil, fmt.Errorf("oauth login: missing identity: %w", domain.ErrValidation)
	}
	u := copyUser(existing)
	u.Role = p.User.Role
	u.OAuthAdmin = p.User.OAuthAdmin
	u.OAuthTokenCiphertext = p.User.OAuthTokenCiphertext
	u.OAuthGroups = normalizeIDs(p.User.OAuthGroups)
	u.DeactivatedAt = nil
	u.OAuthLoginVersion++
	u.Revision++
	u.UpdatedAt = p.At
	s.applyManagedMemberships(u, p.ManagedGroupIDs)
	s.storeUser(u)
	return s.oauthResult(u, p.FallbackGroupID), nil
}

func (s *fsmState) oauthMembership(existing *domain.User, p *cmdOAuthUserMutation) (*domain.OAuthUserResult, error) {
	if existing.DisabledAt != nil || existing.DeactivatedAt != nil {
		return nil, fmt.Errorf("oauth user %s: %w", p.User.ID, domain.ErrAccountBlocked)
	}
	u := copyUser(existing)
	u.Role = p.User.Role
	u.OAuthAdmin = p.User.OAuthAdmin
	u.OAuthGroups = normalizeIDs(p.User.OAuthGroups)
	u.Revision++
	u.UpdatedAt = p.At
	s.applyManagedMemberships(u, p.ManagedGroupIDs)
	s.storeUser(u)
	return s.oauthResult(u, ""), nil
}

func (s *fsmState) oauthCredential(existing *domain.User, p *cmdOAuthUserMutation) (*domain.OAuthUserResult, error) {
	if existing.DisabledAt != nil || existing.DeactivatedAt != nil {
		return nil, fmt.Errorf("oauth user %s: %w", p.User.ID, domain.ErrAccountBlocked)
	}
	u := copyUser(existing)
	u.OAuthTokenCiphertext = p.User.OAuthTokenCiphertext
	u.Revision++
	u.UpdatedAt = p.At
	s.storeUser(u)
	return s.oauthResult(u, ""), nil
}

func (s *fsmState) oauthRevoke(existing *domain.User, p *cmdOAuthUserMutation) (*domain.OAuthUserResult, error) {
	if existing.DisabledAt != nil {
		return nil, fmt.Errorf("oauth user %s: %w", p.User.ID, domain.ErrAccountBlocked)
	}
	u := copyUser(existing)
	if u.DeactivatedAt == nil {
		u.DeactivatedAt = copyTime(&p.At)
		u.TokenVersion++
		u.Revision++
		u.UpdatedAt = p.At
	}
	s.deleteToken(p.User.ID)
	s.storeUser(u)
	return s.oauthResult(u, ""), nil
}

// validateGroupIDs rejects a managed-membership set referencing unknown groups.
func (s *fsmState) validateGroupIDs(ids []string) error {
	for _, gid := range ids {
		if gid == "" {
			return fmt.Errorf("oauth group id: %w", domain.ErrValidation)
		}
		if _, ok := s.groups[gid]; !ok {
			return fmt.Errorf("oauth group %s: %w", gid, domain.ErrNotFound)
		}
	}
	return nil
}

// applyManagedMemberships replaces only the OAuth-managed membership edges on
// u.OAuthGroupIDs, leaving manual memberships untouched. A desired group that
// is already a member (manual or managed) is retained; it is simply claimed as
// OAuth-managed.
func (s *fsmState) applyManagedMemberships(u *domain.User, desired []string) {
	desiredSet := make(map[string]struct{}, len(desired))
	for _, gid := range desired {
		desiredSet[gid] = struct{}{}
	}
	previous := u.OAuthGroupIDs
	for _, gid := range previous {
		if _, keep := desiredSet[gid]; keep {
			continue
		}
		// Drop only the OAuth-managed edge for this user.
		if set, ok := s.memberships[gid]; ok {
			delete(set, u.ID)
			if len(set) == 0 {
				delete(s.memberships, gid)
			}
		}
		if set, ok := s.membershipsByUser[u.ID]; ok {
			delete(set, gid)
			if len(set) == 0 {
				delete(s.membershipsByUser, u.ID)
			}
		}
	}
	for _, gid := range desired {
		s.addMembership(gid, u.ID)
	}
	u.OAuthGroupIDs = normalizeIDs(desired)
}

// oauthResult builds the committed response for an OAuth mutation, optionally
// applying the fallback group when the effective membership is still empty.
func (s *fsmState) oauthResult(u *domain.User, fallbackGroupID string) *domain.OAuthUserResult {
	if fallbackGroupID != "" && len(s.membershipsByUser[u.ID]) == 0 {
		s.addMembership(fallbackGroupID, u.ID)
	}
	groups := s.effectiveUserGroupIDs(u.ID)
	return &domain.OAuthUserResult{User: copyUser(s.users[u.ID]), GroupIDs: groups}
}

// effectiveUserGroupIDs returns the sorted group IDs the user is a member of.
func (s *fsmState) effectiveUserGroupIDs(userID string) []string {
	set := s.membershipsByUser[userID]
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for gid := range set {
		out = append(out, gid)
	}
	sort.Strings(out)
	return out
}

// normalizeIDs de-duplicates and sorts a group-ID list, returning nil for an
// empty set.
func normalizeIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// --- groups -----------------------------------------------------------------

func (s *fsmState) upsertGroup(g *domain.Group, create bool) error {
	lower := strings.ToLower(g.Name)
	if existing, ok := s.groups[g.ID]; ok {
		if create {
			return fmt.Errorf("group %s: %w", g.ID, domain.ErrConflict)
		}
		if other, ok := s.groupsByName[lower]; ok && other != g.ID {
			return fmt.Errorf("group %s: %w", g.Name, domain.ErrConflict)
		}
		g.CreatedAt = existing.CreatedAt
		if g.UpdatedAt.IsZero() {
			g.UpdatedAt = existing.UpdatedAt
		}
		delete(s.groupsByName, strings.ToLower(existing.Name))
		s.storeGroup(g)
		return nil
	}
	if !create {
		return fmt.Errorf("update group %s: %w", g.ID, domain.ErrNotFound)
	}
	if _, ok := s.groupsByName[lower]; ok {
		return fmt.Errorf("group %s: %w", g.Name, domain.ErrConflict)
	}
	if g.UpdatedAt.IsZero() {
		g.UpdatedAt = g.CreatedAt
	}
	s.storeGroup(g)
	return nil
}

func (s *fsmState) deleteGroup(id string) error {
	g, ok := s.groups[id]
	if !ok {
		return fmt.Errorf("delete group %s: %w", id, domain.ErrNotFound)
	}
	// Cascade memberships and advance each affected user's revision so an
	// in-flight OAuth mutation conflicts with the group removal.
	for uid := range s.memberships[id] {
		delete(s.membershipsByUser[uid], id)
		if len(s.membershipsByUser[uid]) == 0 {
			delete(s.membershipsByUser, uid)
		}
		s.bumpRevision(uid)
	}
	delete(s.memberships, id)
	// Null projects.group_id (ON DELETE SET NULL).
	for pid, p := range s.projects {
		if p.GroupID == id {
			p.GroupID = ""
			s.projects[pid] = p
		}
	}
	// Null trace_meta.group_id (ON DELETE SET NULL).
	for tid, m := range s.traces {
		if m.GroupID == id {
			m.GroupID = ""
			s.traces[tid] = m
		}
	}
	delete(s.groups, id)
	delete(s.groupsByName, strings.ToLower(g.Name))
	return nil
}

func (s *fsmState) setMembers(groupID string, userIDs []string) error {
	if _, ok := s.groups[groupID]; !ok {
		return fmt.Errorf("group %s: %w", groupID, domain.ErrNotFound)
	}
	// Re-check every user deterministically (validation already done by the
	// service, but the FSM must reject unknown users on any node). Soft-deleted
	// targets are rejected; disabled-but-not-deleted targets are permitted.
	for _, uid := range userIDs {
		u, ok := s.users[uid]
		if !ok {
			return fmt.Errorf("user %s: %w", uid, domain.ErrNotFound)
		}
		if u.DeletedAt != nil {
			return fmt.Errorf("user %s: %w", uid, domain.ErrAccountBlocked)
		}
	}
	// Track every affected user (old and new membership) so each advances its
	// revision exactly once for this command.
	affected := make(map[string]struct{}, len(userIDs))
	for uid := range s.memberships[groupID] {
		affected[uid] = struct{}{}
		delete(s.membershipsByUser[uid], groupID)
		if len(s.membershipsByUser[uid]) == 0 {
			delete(s.membershipsByUser, uid)
		}
	}
	set := make(map[string]struct{}, len(userIDs))
	for _, uid := range userIDs {
		set[uid] = struct{}{}
		affected[uid] = struct{}{}
	}
	if len(set) == 0 {
		delete(s.memberships, groupID)
	} else {
		s.memberships[groupID] = set
		for uid := range set {
			s.addMembership(groupID, uid)
		}
	}
	for uid := range affected {
		s.bumpRevision(uid)
	}
	return nil
}

// bumpRevision advances a user's revision by one, ignoring unknown users.
func (s *fsmState) bumpRevision(userID string) {
	if u, ok := s.users[userID]; ok {
		u.Revision++
	}
}

// addMembership records one (groupID, userID) edge in both directions.
func (s *fsmState) addMembership(groupID, userID string) {
	if s.memberships[groupID] == nil {
		s.memberships[groupID] = make(map[string]struct{})
	}
	s.memberships[groupID][userID] = struct{}{}
	if s.membershipsByUser[userID] == nil {
		s.membershipsByUser[userID] = make(map[string]struct{})
	}
	s.membershipsByUser[userID][groupID] = struct{}{}
}

// --- projects ---------------------------------------------------------------

func (s *fsmState) upsertProject(p *domain.Project, create bool) error {
	lower := strings.ToLower(p.Name)
	existing, ok := s.projects[p.ID]
	if !ok {
		if !create {
			return fmt.Errorf("update project %s: %w", p.ID, domain.ErrNotFound)
		}
		if _, dup := s.projectsByName[lower]; dup {
			return fmt.Errorf("project %s: %w", p.Name, domain.ErrConflict)
		}
		if p.UpdatedAt.IsZero() {
			p.UpdatedAt = p.CreatedAt
		}
		s.storeProject(p)
		return nil
	}
	if create {
		return fmt.Errorf("project %s: %w", p.ID, domain.ErrConflict)
	}
	if other, dup := s.projectsByName[lower]; dup && other != p.ID {
		return fmt.Errorf("project %s: %w", p.Name, domain.ErrConflict)
	}
	p.CreatedAt = existing.CreatedAt
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = existing.UpdatedAt
	}
	delete(s.projectsByName, strings.ToLower(existing.Name))
	s.storeProject(p)
	return nil
}

func (s *fsmState) deleteProject(id string) error {
	p, ok := s.projects[id]
	if !ok {
		return fmt.Errorf("delete project %s: %w", id, domain.ErrNotFound)
	}
	delete(s.projects, id)
	delete(s.projectsByName, strings.ToLower(p.Name))
	return nil
}

// --- tokens -----------------------------------------------------------------

func (s *fsmState) upsertToken(t *domain.APIToken) error {
	// The owning user must exist and not be blocked before any old token is
	// removed or replaced. A blocked owner yields no token side effect.
	owner, ok := s.users[t.UserID]
	if !ok {
		return fmt.Errorf("api token owner %s: %w", t.UserID, domain.ErrNotFound)
	}
	if owner.DeletedAt != nil || owner.DisabledAt != nil {
		return fmt.Errorf("api token owner %s: %w", t.UserID, domain.ErrAccountBlocked)
	}
	if existingID, ok := s.tokensByUser[t.UserID]; ok {
		// One token per user: replace the existing one.
		old := s.tokens[existingID]
		if t.TokenHash != old.TokenHash {
			if other, ok := s.tokensByHash[t.TokenHash]; ok && other != existingID {
				return fmt.Errorf("api token hash: %w", domain.ErrConflict)
			}
		}
		s.removeToken(existingID)
	} else if _, ok := s.tokensByHash[t.TokenHash]; ok {
		return fmt.Errorf("api token hash: %w", domain.ErrConflict)
	}
	s.storeToken(t)
	return nil
}

// storeToken indexes a token by id, hash, and owning user (deep-copying the
// LastUsedAt pointer so callers can't mutate state).
func (s *fsmState) storeToken(t *domain.APIToken) {
	s.tokens[t.ID] = copyToken(t)
	s.tokensByHash[t.TokenHash] = t.ID
	s.tokensByUser[t.UserID] = t.ID
}

func (s *fsmState) removeToken(id string) {
	t, ok := s.tokens[id]
	if !ok {
		return
	}
	delete(s.tokens, id)
	delete(s.tokensByHash, t.TokenHash)
	delete(s.tokensByUser, t.UserID)
}

func (s *fsmState) deleteToken(userID string) {
	if id, ok := s.tokensByUser[userID]; ok {
		s.removeToken(id)
	}
}

func (s *fsmState) touchToken(id string, at time.Time) {
	t, ok := s.tokens[id]
	if !ok {
		return
	}
	t.LastUsedAt = &at
}

// --- trace metadata ---------------------------------------------------------

func (s *fsmState) upsertTraceProvision(p cmdUpsertTraceProvision) {
	existing, ok := s.traces[p.TraceID]
	if !ok {
		s.traces[p.TraceID] = &domain.TraceMeta{
			TraceID:   p.TraceID,
			UserID:    p.UserID,
			Version:   p.Version,
			UpdatedAt: p.UpdatedAt,
		}
		return
	}
	// user_id: first non-empty wins; version: new non-empty wins; updated_at: now.
	if existing.UserID == "" {
		existing.UserID = p.UserID
	}
	if p.Version != "" {
		existing.Version = p.Version
	}
	existing.UpdatedAt = p.UpdatedAt
	s.traces[p.TraceID] = existing
}

func (s *fsmState) upsertTraceIngest(m *domain.TraceMeta) {
	existing, ok := s.traces[m.TraceID]
	if !ok {
		cp := *m
		s.traces[m.TraceID] = &cp
		return
	}

	// user_id is set on insert only (not in the ON CONFLICT update list).
	// group_id: first non-empty wins; the rest: new non-empty wins.
	if existing.GroupID == "" {
		existing.GroupID = m.GroupID
	}
	if m.ProjectName != "" {
		existing.ProjectName = m.ProjectName
	}
	// A terminal OTLP finish (success/failed) is authoritative: it supersedes
	// any disconnect-failed status and clears the disconnect reason. A
	// non-terminal OTLP status ("running", the default for an in-flight root
	// span) must NOT overwrite a terminal status or clear its failure_reason
	// — otherwise a late-arriving "running" span (e.g. a buffered/retried
	// OTLP batch from before the disconnect, or an engine-emitted root span
	// without a status code) would resurrect a disconnect-failed trace and
	// defeat the disconnect detection (CWE-346 / data integrity).
	if m.Status == "success" || m.Status == "failed" {
		existing.Status = m.Status
		existing.FailureReason = ""
	} else if m.Status != "" && existing.Status != "success" && existing.Status != "failed" {
		existing.Status = m.Status
	}
	if m.Version != "" {
		existing.Version = m.Version
	}
	if m.CIProvider != "" {
		existing.CIProvider = m.CIProvider
	}
	if m.CIRepo != "" {
		existing.CIRepo = m.CIRepo
	}
	if m.DurationMS != 0 {
		existing.DurationMS = m.DurationMS
	}
	if !m.StartedAt.IsZero() {
		existing.StartedAt = m.StartedAt
	}
	existing.UpdatedAt = m.UpdatedAt
	s.traces[m.TraceID] = existing
}

// markTraceFailed transitions a non-terminal trace to "failed". Returns
// true iff the status changed. Deterministic; caller supplies UpdatedAt.
func (s *fsmState) markTraceFailed(p cmdMarkTraceFailed) bool {
	m, ok := s.traces[p.TraceID]
	if !ok {
		return false
	}
	if m.Status == "success" || m.Status == "failed" {
		return false
	}
	m.Status = "failed"
	m.FailureReason = p.Reason
	m.UpdatedAt = p.UpdatedAt
	s.traces[p.TraceID] = m
	return true
}

// --- read helpers (all take RLock internally and return deep copies) --------

func (f *FSM) readUserByID(id string) (*domain.User, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	u, ok := f.state.users[id]
	if !ok {
		return nil, fmt.Errorf("user %s: %w", id, domain.ErrNotFound)
	}
	return copyUser(u), nil
}

func (f *FSM) readUserByUsername(username string) (*domain.User, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	id, ok := f.state.usersByName[strings.ToLower(username)]
	if !ok {
		return nil, fmt.Errorf("user %s: %w", username, domain.ErrNotFound)
	}
	return copyUser(f.state.users[id]), nil
}

func (f *FSM) readUserByOAuth(provider, oauthID string) (*domain.User, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	id, ok := f.state.usersByOAuth[oauthKey(provider, oauthID)]
	if !ok {
		return nil, fmt.Errorf("user oauth %s/%s: %w", provider, oauthID, domain.ErrNotFound)
	}
	return copyUser(f.state.users[id]), nil
}

func (f *FSM) listUsers() []*domain.User {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	out := make([]*domain.User, 0, len(f.state.users))
	for _, u := range f.state.users {
		out = append(out, copyUser(u))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func (f *FSM) countUsers() int {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	return len(f.state.users)
}

func (f *FSM) readGroupByID(id string) (*domain.Group, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	g, ok := f.state.groups[id]
	if !ok {
		return nil, fmt.Errorf("group %s: %w", id, domain.ErrNotFound)
	}
	cp := *g
	return &cp, nil
}

func (f *FSM) readGroupByName(name string) (*domain.Group, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	id, ok := f.state.groupsByName[strings.ToLower(name)]
	if !ok {
		return nil, fmt.Errorf("group %s: %w", name, domain.ErrNotFound)
	}
	g := f.state.groups[id]
	cp := *g
	return &cp, nil
}

func (f *FSM) listGroups() []*domain.Group {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	out := make([]*domain.Group, 0, len(f.state.groups))
	for _, g := range f.state.groups {
		cp := *g
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (f *FSM) members(groupID string) []*domain.User {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	out := make([]*domain.User, 0, len(f.state.memberships[groupID]))
	for uid := range f.state.memberships[groupID] {
		if u, ok := f.state.users[uid]; ok {
			cp := *u
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

func (f *FSM) groupsForUser(userID string) []*domain.Group {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	out := make([]*domain.Group, 0, len(f.state.membershipsByUser[userID]))
	for gid := range f.state.membershipsByUser[userID] {
		if g, ok := f.state.groups[gid]; ok {
			cp := *g
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (f *FSM) allMemberships() map[string][]string {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	out := make(map[string][]string, len(f.state.memberships))
	for gid, set := range f.state.memberships {
		ids := make([]string, 0, len(set))
		for uid := range set {
			ids = append(ids, uid)
		}
		sort.Strings(ids)
		out[gid] = ids
	}
	return out
}

func (f *FSM) readProjectByID(id string) (*domain.Project, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	p, ok := f.state.projects[id]
	if !ok {
		return nil, fmt.Errorf("project %s: %w", id, domain.ErrNotFound)
	}
	cp := *p
	return &cp, nil
}

func (f *FSM) readProjectByName(name string) (*domain.Project, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	id, ok := f.state.projectsByName[strings.ToLower(name)]
	if !ok {
		return nil, fmt.Errorf("project %s: %w", name, domain.ErrNotFound)
	}
	p := f.state.projects[id]
	cp := *p
	return &cp, nil
}

func (f *FSM) listProjects() []*domain.Project {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	out := make([]*domain.Project, 0, len(f.state.projects))
	for _, p := range f.state.projects {
		cp := *p
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func (f *FSM) readTokenByHash(hash string) (*domain.APIToken, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	id, ok := f.state.tokensByHash[hash]
	if !ok {
		return nil, fmt.Errorf("api token: %w", domain.ErrNotFound)
	}
	return copyToken(f.state.tokens[id]), nil
}

func (f *FSM) readTokenByUser(userID string) (*domain.APIToken, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	id, ok := f.state.tokensByUser[userID]
	if !ok {
		return nil, fmt.Errorf("api token for user %s: %w", userID, domain.ErrNotFound)
	}
	return copyToken(f.state.tokens[id]), nil
}

func copyToken(t *domain.APIToken) *domain.APIToken {
	if t == nil {
		return nil
	}
	cp := *t
	if t.LastUsedAt != nil {
		at := *t.LastUsedAt
		cp.LastUsedAt = &at
	}
	return &cp
}

func (f *FSM) readTrace(traceID string) (*domain.TraceMeta, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	m, ok := f.state.traces[traceID]
	if !ok {
		return nil, fmt.Errorf("trace_meta %s: %w", traceID, domain.ErrNotFound)
	}
	cp := *m
	return &cp, nil
}

// listTracesBefore returns trace_meta rows older than cutoff (by
// COALESCE(started_at, updated_at)), excluding running traces when
// protectRunning is true. Sorted oldest-first. No limit (the sweeper must
// see every candidate). Rows with a zero-time sort key (unknown age) are
// always skipped — never purge what we cannot age.
func (f *FSM) listTracesBefore(cutoff time.Time, protectRunning bool) []*domain.TraceMeta {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	var out []*domain.TraceMeta
	for _, m := range f.state.traces {
		key := traceSortKey(m)
		if key.IsZero() {
			continue
		}
		if key.After(cutoff) {
			continue
		}
		if protectRunning && (m.Status == "" || m.Status == "running") {
			continue
		}
		cp := *m
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return traceSortKey(out[i]).Before(traceSortKey(out[j]))
	})
	return out
}

// listRunningTraces returns deep copies of non-terminal traces with a non-empty
// version, sorted by COALESCE(started_at, updated_at) then trace_id.
func (f *FSM) listRunningTraces() []*domain.TraceMeta {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	var out []*domain.TraceMeta
	for _, m := range f.state.traces {
		if m.Status != "" && m.Status != "running" {
			continue
		}
		if m.Version == "" {
			continue
		}
		cp := *m
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		ki, kj := traceSortKey(out[i]), traceSortKey(out[j])
		if ki.Equal(kj) {
			return out[i].TraceID < out[j].TraceID
		}
		return ki.Before(kj)
	})
	return out
}

// traceStats returns the total trace count and the oldest trace sort key
// (zero time when no trace has a known age).
func (f *FSM) traceStats() (int, time.Time) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	var oldest time.Time
	for _, m := range f.state.traces {
		k := traceSortKey(m)
		if k.IsZero() {
			continue // unknown age never counts as the oldest
		}
		if oldest.IsZero() || k.Before(oldest) {
			oldest = k
		}
	}
	return len(f.state.traces), oldest
}

// listTraces applies the filter, joins group/user names, sorts by
// COALESCE(started_at, updated_at) DESC, and clamps the limit. The filter is
// taken by pointer: TraceFilter exceeds gocritic's hugeParam threshold.
func (f *FSM) listTraces(filter *domain.TraceFilter) []*domain.TraceListResult {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()

	out := make([]*domain.TraceListResult, 0, len(f.state.traces))
	for _, m := range f.state.traces {
		if !traceMatches(filter, m) {
			continue
		}
		r := &domain.TraceListResult{TraceMeta: *m}
		// Normalise in-flight/unknown statuses so the UI never renders a bare
		// "unset" (or empty) badge: a trace without a final status is running.
		if r.Status == "" || r.Status == "unset" {
			r.Status = "running"
		}
		if g, ok := f.state.groups[m.GroupID]; ok {
			r.GroupName = g.Name
		}
		if u, ok := f.state.users[m.UserID]; ok {
			r.Username = u.Username
		}
		// Text filters run after the group/user joins (Username) and before the
		// limit clamp, so they see the same rows the UI would render.
		if filter.CIRepo != "" && !containsFold(r.CIRepo, filter.CIRepo) && !containsFold(r.ProjectName, filter.CIRepo) {
			continue
		}
		if filter.Username != "" && !containsFold(r.Username, filter.Username) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		ki, kj := traceSortKey(&out[i].TraceMeta), traceSortKey(&out[j].TraceMeta)
		if ki.Equal(kj) {
			return out[i].TraceID < out[j].TraceID
		}
		return ki.After(kj)
	})
	limit := clampLimit(filter.Limit)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func traceSortKey(m *domain.TraceMeta) time.Time {
	if !m.StartedAt.IsZero() {
		return m.StartedAt
	}
	return m.UpdatedAt
}

// containsFold reports whether s contains substr (ASCII case-insensitive).
func containsFold(s, substr string) bool {
	if substr == "" {
		return true
	}
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

func traceMatches(f *domain.TraceFilter, m *domain.TraceMeta) bool {
	if f.UnassignedOnly {
		return m.GroupID == ""
	}
	if len(f.GroupIDs) > 0 {
		for _, gid := range f.GroupIDs {
			if m.GroupID == gid {
				return true
			}
		}
		if f.UserID != "" {
			return m.GroupID == "" && m.UserID == f.UserID
		}
		return false
	}
	if !f.IncludeUnassigned && f.UserID != "" {
		return m.UserID == f.UserID
	}
	return true
}

func (f *FSM) readMeta(key string) (string, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	v, ok := f.state.meta[key]
	if !ok {
		return "", fmt.Errorf("meta %s: %w", key, domain.ErrNotFound)
	}
	return v, nil
}

func oauthKey(provider, oauthID string) string {
	return fmt.Sprintf("%s\x00%s", provider, oauthID)
}

// clampLimit bounds n to the [1, MaxTraceLimit] range, defaulting to
// DefaultTraceLimit for non-positive values.
func clampLimit(n int) int {
	if n <= 0 {
		return domain.DefaultTraceLimit
	}
	if n > domain.MaxTraceLimit {
		return domain.MaxTraceLimit
	}
	return n
}
