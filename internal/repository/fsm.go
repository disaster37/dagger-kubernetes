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
	kindSetMeta
	kindUpsertManifestRoute
	kindDeleteManifestRoute
	kindDeleteRoutesForBackend
	kindUpsertBlobRoute
	kindRecordUpload
	kindDeleteUpload
	kindReapUploads
	kindDeleteTrace
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
		ID            string      `json:"id"`
		Username      string      `json:"username"`
		Role          domain.Role `json:"role"`
		PasswordHash  string      `json:"password_hash"`
		OAuthProvider string      `json:"oauth_provider"`
		OAuthID       string      `json:"oauth_id"`
		CreatedAt     time.Time   `json:"created_at"`
		UpdatedAt     time.Time   `json:"updated_at"`
		Create        bool        `json:"create,omitempty"` // true = insert-only; false = update-only
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

	cmdSetMeta struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}

	cmdDeleteManifestRoute struct {
		Repo string `json:"repo"`
		Tag  string `json:"tag"`
	}

	cmdDeleteRoutesForBackend struct {
		BackendID string `json:"backend_id"`
	}

	cmdUpsertBlobRoute struct {
		Digest    string `json:"digest"`
		BackendID string `json:"backend_id"`
		CreatedAt string `json:"created_at"`
	}

	cmdDeleteUpload struct {
		UUID string `json:"uuid"`
	}

	cmdReapUploads struct {
		CutoffRFC3339 string `json:"cutoff"`
	}

	cmdDeleteTrace struct {
		TraceID string `json:"trace_id"`
	}
)

// toDomain converts a command user payload into a domain.User.
func (c *cmdUser) toDomain() *domain.User {
	return &domain.User{
		ID:            c.ID,
		Username:      c.Username,
		Role:          c.Role,
		PasswordHash:  c.PasswordHash,
		OAuthProvider: c.OAuthProvider,
		OAuthID:       c.OAuthID,
		CreatedAt:     c.CreatedAt,
		UpdatedAt:     c.UpdatedAt,
	}
}

// cmdUserFrom converts a domain.User into a command user payload (preserving
// the password hash that the domain JSON tag hides).
func cmdUserFrom(u *domain.User) *cmdUser {
	return &cmdUser{
		ID:            u.ID,
		Username:      u.Username,
		Role:          u.Role,
		PasswordHash:  u.PasswordHash,
		OAuthProvider: u.OAuthProvider,
		OAuthID:       u.OAuthID,
		CreatedAt:     u.CreatedAt,
		UpdatedAt:     u.UpdatedAt,
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

	cacheObjectRoutes   map[string]*domain.CacheRoute         // "repo\x00tag" -> route
	cacheBlobRoutes     map[string]map[string]string          // digest -> backendID -> createdAt
	cacheUploadSessions map[string]*domain.CacheUploadSession // uuid -> session
}

func newState() *fsmState {
	return &fsmState{
		users:               make(map[string]*domain.User),
		usersByName:         make(map[string]string),
		usersByOAuth:        make(map[string]string),
		groups:              make(map[string]*domain.Group),
		groupsByName:        make(map[string]string),
		memberships:         make(map[string]map[string]struct{}),
		membershipsByUser:   make(map[string]map[string]struct{}),
		tokens:              make(map[string]*domain.APIToken),
		tokensByHash:        make(map[string]string),
		tokensByUser:        make(map[string]string),
		projects:            make(map[string]*domain.Project),
		projectsByName:      make(map[string]string),
		traces:              make(map[string]*domain.TraceMeta),
		meta:                make(map[string]string),
		cacheObjectRoutes:   make(map[string]*domain.CacheRoute),
		cacheBlobRoutes:     make(map[string]map[string]string),
		cacheUploadSessions: make(map[string]*domain.CacheUploadSession),
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

	switch cmd.Kind {
	case kindUpsertUser:
		return nil, applyPayload(cmd, "user", func(p cmdUser) error {
			return s.upsertUser(p.toDomain(), p.Create)
		})
	case kindDeleteUser:
		return nil, applyPayload(cmd, "delete user", s.deleteUser)
	case kindUpsertGroup:
		return nil, applyPayload(cmd, "group", func(p cmdGroup) error {
			return s.upsertGroup(&p.Group, p.Create)
		})
	case kindDeleteGroup:
		return nil, applyPayload(cmd, "delete group", s.deleteGroup)
	case kindSetMembers:
		return nil, applyPayload(cmd, "set members", func(p cmdSetMembers) error {
			return s.setMembers(p.GroupID, p.UserIDs)
		})
	case kindUpsertProject:
		return nil, applyPayload(cmd, "project", func(p cmdProject) error {
			return s.upsertProject(&p.Project, p.Create)
		})
	case kindDeleteProject:
		return nil, applyPayload(cmd, "delete project", s.deleteProject)
	case kindUpsertToken:
		return nil, applyPayload(cmd, "api token", func(p cmdToken) error {
			return s.upsertToken(p.toDomain())
		})
	case kindDeleteToken:
		return nil, applyPayload(cmd, "delete token", func(p cmdDeleteToken) error {
			s.deleteToken(p.UserID)
			return nil
		})
	case kindTouchToken:
		return nil, applyPayload(cmd, "touch token", func(p cmdTouchToken) error {
			s.touchToken(p.ID, p.At)
			return nil
		})
	case kindUpsertTraceProvision:
		return nil, applyPayload(cmd, "trace provision", func(p cmdUpsertTraceProvision) error {
			s.upsertTraceProvision(p)
			return nil
		})
	case kindUpsertTraceIngest:
		return nil, applyPayload(cmd, "trace ingest", func(m domain.TraceMeta) error {
			s.upsertTraceIngest(&m)
			return nil
		})
	case kindSetMeta:
		return nil, applyPayload(cmd, "set meta", func(p cmdSetMeta) error {
			s.meta[p.Key] = p.Value
			return nil
		})
	case kindUpsertManifestRoute:
		return nil, applyPayload(cmd, "manifest route", func(cr domain.CacheRoute) error {
			s.upsertManifestRoute(&cr)
			return nil
		})
	case kindDeleteManifestRoute:
		return nil, applyPayload(cmd, "delete manifest route", func(p cmdDeleteManifestRoute) error {
			delete(s.cacheObjectRoutes, manifestRouteKey(p.Repo, p.Tag))
			return nil
		})
	case kindDeleteRoutesForBackend:
		return nil, applyPayload(cmd, "delete routes for backend", func(p cmdDeleteRoutesForBackend) error {
			s.deleteRoutesForBackend(p.BackendID)
			return nil
		})
	case kindUpsertBlobRoute:
		return nil, applyPayload(cmd, "upsert blob route", func(p cmdUpsertBlobRoute) error {
			s.upsertBlobRoute(p.Digest, p.BackendID, p.CreatedAt)
			return nil
		})
	case kindRecordUpload:
		return nil, applyPayload(cmd, "upload session", func(sess domain.CacheUploadSession) error {
			s.recordUpload(&sess)
			return nil
		})
	case kindDeleteUpload:
		return nil, applyPayload(cmd, "delete upload", func(p cmdDeleteUpload) error {
			delete(s.cacheUploadSessions, p.UUID)
			return nil
		})
	case kindReapUploads:
		p, err := decode[cmdReapUploads](cmd, "reap uploads")
		if err != nil {
			return nil, err
		}
		return s.reapUploads(p.CutoffRFC3339)
	case kindDeleteTrace:
		return nil, applyPayload(cmd, "delete trace", func(p cmdDeleteTrace) error {
			delete(s.traces, p.TraceID)
			return nil
		})
	default:
		return nil, fmt.Errorf("unknown raft command kind %d", cmd.Kind)
	}
}

// --- users ------------------------------------------------------------------

func (s *fsmState) upsertUser(u *domain.User, create bool) error {
	lower := strings.ToLower(u.Username)
	if existing, ok := s.users[u.ID]; ok {
		if create {
			return fmt.Errorf("user %s: %w", u.ID, domain.ErrConflict)
		}
		// Update path: reject a username that collides with another user.
		if other, ok := s.usersByName[lower]; ok && other != u.ID {
			return fmt.Errorf("user %s: %w", u.Username, domain.ErrConflict)
		}
		if u.OAuthID != "" {
			if other, ok := s.usersByOAuth[oauthKey(u.OAuthProvider, u.OAuthID)]; ok && other != u.ID {
				return fmt.Errorf("user oauth %s/%s: %w", u.OAuthProvider, u.OAuthID, domain.ErrConflict)
			}
		}
		u.CreatedAt = existing.CreatedAt
		if u.UpdatedAt.IsZero() {
			u.UpdatedAt = existing.UpdatedAt
		}
		// Refresh reverse indices (username/oauth may have changed).
		if existing.Username != "" {
			delete(s.usersByName, strings.ToLower(existing.Username))
		}
		if existing.OAuthID != "" {
			delete(s.usersByOAuth, oauthKey(existing.OAuthProvider, existing.OAuthID))
		}
		s.storeUser(u)
		return nil
	}

	if !create {
		return fmt.Errorf("update user %s: %w", u.ID, domain.ErrNotFound)
	}
	// Create path: reject duplicates.
	if _, ok := s.usersByName[lower]; ok {
		return fmt.Errorf("user %s: %w", u.Username, domain.ErrConflict)
	}
	if u.OAuthID != "" {
		if _, ok := s.usersByOAuth[oauthKey(u.OAuthProvider, u.OAuthID)]; ok {
			return fmt.Errorf("user oauth %s/%s: %w", u.OAuthProvider, u.OAuthID, domain.ErrConflict)
		}
	}
	if u.UpdatedAt.IsZero() {
		u.UpdatedAt = u.CreatedAt
	}
	s.storeUser(u)
	return nil
}

func (s *fsmState) storeUser(u *domain.User) {
	cp := *u
	s.users[u.ID] = &cp
	s.usersByName[strings.ToLower(u.Username)] = u.ID
	if u.OAuthID != "" {
		s.usersByOAuth[oauthKey(u.OAuthProvider, u.OAuthID)] = u.ID
	}
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
	// Cascade memberships.
	for uid := range s.memberships[id] {
		delete(s.membershipsByUser[uid], id)
		if len(s.membershipsByUser[uid]) == 0 {
			delete(s.membershipsByUser, uid)
		}
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
	// service, but the FSM must reject unknown users on any node).
	for _, uid := range userIDs {
		if _, ok := s.users[uid]; !ok {
			return fmt.Errorf("user %s: %w", uid, domain.ErrNotFound)
		}
	}
	// Remove old reverse edges.
	for uid := range s.memberships[groupID] {
		delete(s.membershipsByUser[uid], groupID)
		if len(s.membershipsByUser[uid]) == 0 {
			delete(s.membershipsByUser, uid)
		}
	}
	set := make(map[string]struct{}, len(userIDs))
	for _, uid := range userIDs {
		set[uid] = struct{}{}
	}
	if len(set) == 0 {
		delete(s.memberships, groupID)
		return nil
	}
	s.memberships[groupID] = set
	for uid := range set {
		s.addMembership(groupID, uid)
	}
	return nil
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
	if existing, ok := s.projects[p.ID]; ok {
		if create {
			return fmt.Errorf("project %s: %w", p.ID, domain.ErrConflict)
		}
		if other, ok := s.projectsByName[lower]; ok && other != p.ID {
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
	if !create {
		return fmt.Errorf("update project %s: %w", p.ID, domain.ErrNotFound)
	}
	if _, ok := s.projectsByName[lower]; ok {
		return fmt.Errorf("project %s: %w", p.Name, domain.ErrConflict)
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = p.CreatedAt
	}
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
	if m.Status != "" {
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

// --- cache routing ----------------------------------------------------------

func manifestRouteKey(repo, tag string) string {
	return fmt.Sprintf("%s\x00%s", repo, tag)
}

func (s *fsmState) upsertManifestRoute(cr *domain.CacheRoute) {
	key := manifestRouteKey(cr.Repo, cr.Tag)
	existing, ok := s.cacheObjectRoutes[key]
	cp := *cr
	if ok {
		// created_at is set-once; last_seen_at updates on every upsert.
		cp.CreatedAt = existing.CreatedAt
	}
	s.cacheObjectRoutes[key] = &cp
}

func (s *fsmState) upsertBlobRoute(digest, backendID, createdAt string) {
	routes, ok := s.cacheBlobRoutes[digest]
	if !ok {
		routes = make(map[string]string)
		s.cacheBlobRoutes[digest] = routes
	}
	// INSERT OR IGNORE: keep the first created_at.
	if _, exists := routes[backendID]; exists {
		return
	}
	routes[backendID] = createdAt
}

func (s *fsmState) deleteRoutesForBackend(backendID string) {
	for key, cr := range s.cacheObjectRoutes {
		if cr.BackendID == backendID {
			delete(s.cacheObjectRoutes, key)
		}
	}
	for digest, routes := range s.cacheBlobRoutes {
		delete(routes, backendID)
		if len(routes) == 0 {
			delete(s.cacheBlobRoutes, digest)
		}
	}
}

func (s *fsmState) recordUpload(sess *domain.CacheUploadSession) {
	cp := *sess
	s.cacheUploadSessions[sess.UploadUUID] = &cp
}

// reapUploads deletes upload sessions older than cutoff and returns the count.
func (s *fsmState) reapUploads(cutoffRFC3339 string) (int, error) {
	cutoff, err := time.Parse(time.RFC3339, cutoffRFC3339)
	if err != nil {
		return 0, fmt.Errorf("parse reap cutoff: %w", err)
	}
	n := 0
	for uuid, sess := range s.cacheUploadSessions {
		created, err := time.Parse(time.RFC3339, sess.CreatedAt)
		if err != nil {
			return 0, fmt.Errorf("parse upload created_at: %w", err)
		}
		if created.Before(cutoff) {
			delete(s.cacheUploadSessions, uuid)
			n++
		}
	}
	return n, nil
}

// --- read helpers (all take RLock internally and return deep copies) --------

func (f *FSM) readUserByID(id string) (*domain.User, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	u, ok := f.state.users[id]
	if !ok {
		return nil, fmt.Errorf("user %s: %w", id, domain.ErrNotFound)
	}
	cp := *u
	return &cp, nil
}

func (f *FSM) readUserByUsername(username string) (*domain.User, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	id, ok := f.state.usersByName[strings.ToLower(username)]
	if !ok {
		return nil, fmt.Errorf("user %s: %w", username, domain.ErrNotFound)
	}
	u := f.state.users[id]
	cp := *u
	return &cp, nil
}

func (f *FSM) readUserByOAuth(provider, oauthID string) (*domain.User, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	id, ok := f.state.usersByOAuth[oauthKey(provider, oauthID)]
	if !ok {
		return nil, fmt.Errorf("user oauth %s/%s: %w", provider, oauthID, domain.ErrNotFound)
	}
	u := f.state.users[id]
	cp := *u
	return &cp, nil
}

func (f *FSM) listUsers() []*domain.User {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	out := make([]*domain.User, 0, len(f.state.users))
	for _, u := range f.state.users {
		cp := *u
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
// COALESCE(started_at, updated_at) DESC, and clamps the limit.
func (f *FSM) listTraces(filter domain.TraceFilter) []*domain.TraceListResult {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()

	out := make([]*domain.TraceListResult, 0, len(f.state.traces))
	for _, m := range f.state.traces {
		if !traceMatches(filter, m) {
			continue
		}
		r := &domain.TraceListResult{TraceMeta: *m}
		if g, ok := f.state.groups[m.GroupID]; ok {
			r.GroupName = g.Name
		}
		if u, ok := f.state.users[m.UserID]; ok {
			r.Username = u.Username
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

func traceMatches(f domain.TraceFilter, m *domain.TraceMeta) bool {
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

func (f *FSM) lookupManifestRoute(repo, tag string) (domain.CacheRoute, bool) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	cr, ok := f.state.cacheObjectRoutes[manifestRouteKey(repo, tag)]
	if !ok {
		return domain.CacheRoute{}, false
	}
	return *cr, true
}

func (f *FSM) lookupBlobRoute(digest string) (string, bool) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	routes, ok := f.state.cacheBlobRoutes[digest]
	if !ok || len(routes) == 0 {
		return "", false
	}
	bestBackend := ""
	bestCreated := ""
	for backendID, createdAt := range routes {
		if createdAt > bestCreated || (createdAt == bestCreated && (bestBackend == "" || backendID < bestBackend)) {
			bestBackend = backendID
			bestCreated = createdAt
		}
	}
	return bestBackend, true
}

func (f *FSM) lookupUpload(uuid string) (domain.CacheUploadSession, bool) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	sess, ok := f.state.cacheUploadSessions[uuid]
	if !ok {
		return domain.CacheUploadSession{}, false
	}
	return *sess, true
}

func (f *FSM) backendCharge(backendID string) int64 {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	var sum int64
	for _, cr := range f.state.cacheObjectRoutes {
		if cr.BackendID == backendID {
			sum += cr.StoredBytes
		}
	}
	return sum
}

func (f *FSM) allCharges() map[string]int64 {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	out := make(map[string]int64)
	for _, cr := range f.state.cacheObjectRoutes {
		out[cr.BackendID] += cr.StoredBytes
	}
	return out
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
