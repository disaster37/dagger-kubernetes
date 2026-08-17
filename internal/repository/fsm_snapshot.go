package repository

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/hashicorp/raft"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// membershipEdge is one (groupID, userID) pair in a snapshot.
type membershipEdge struct {
	GroupID string `json:"group_id"`
	UserID  string `json:"user_id"`
}

// blobRouteEdge is one (digest, backendID, createdAt) row in a snapshot.
type blobRouteEdge struct {
	Digest    string `json:"digest"`
	BackendID string `json:"backend_id"`
	CreatedAt string `json:"created_at"`
}

// fsmSnapshotPayload is the JSON document persisted into a Raft snapshot.
type fsmSnapshotPayload struct {
	Users        []*cmdUser                   `json:"users"`
	Groups       []*domain.Group              `json:"groups"`
	Memberships  []membershipEdge             `json:"memberships"`
	Tokens       []*cmdToken                  `json:"tokens"`
	Projects     []*domain.Project            `json:"projects"`
	Traces       []*domain.TraceMeta          `json:"traces"`
	Meta         map[string]string            `json:"meta"`
	ObjectRoutes []*domain.CacheRoute         `json:"object_routes"`
	BlobRoutes   []blobRouteEdge              `json:"blob_routes"`
	Uploads      []*domain.CacheUploadSession `json:"uploads"`
}

// fsmSnapshot is a point-in-time snapshot of the FSM state.
type fsmSnapshot struct {
	payload fsmSnapshotPayload
}

var _ raft.FSMSnapshot = (*fsmSnapshot)(nil)

// Persist JSON-encodes the snapshot to sink and closes it.
func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	enc := json.NewEncoder(sink)
	if err := enc.Encode(&s.payload); err != nil {
		_ = sink.Cancel()
		return fmt.Errorf("encode snapshot: %w", err)
	}
	if err := sink.Close(); err != nil {
		return fmt.Errorf("close snapshot sink: %w", err)
	}
	return nil
}

// Release is a no-op (the payload has no external resources).
func (s *fsmSnapshot) Release() {}

// copySlice returns the pointer values of m as a slice of deep copies (safe to
// hand to the snapshot encoder without racing later Apply calls).
func copySlice[T any](m map[string]*T) []*T {
	out := make([]*T, 0, len(m))
	for _, v := range m {
		cp := *v
		out = append(out, &cp)
	}
	return out
}

// Snapshot deep-copies the state under the read lock and returns a snapshot.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()

	payload := fsmSnapshotPayload{
		Meta:         make(map[string]string, len(f.state.meta)),
		Users:        make([]*cmdUser, 0, len(f.state.users)),
		Tokens:       make([]*cmdToken, 0, len(f.state.tokens)),
		Groups:       copySlice(f.state.groups),
		Projects:     copySlice(f.state.projects),
		Traces:       copySlice(f.state.traces),
		Uploads:      copySlice(f.state.cacheUploadSessions),
		ObjectRoutes: copySlice(f.state.cacheObjectRoutes),
	}
	for k, v := range f.state.meta {
		payload.Meta[k] = v
	}
	for _, u := range f.state.users {
		payload.Users = append(payload.Users, cmdUserFrom(u))
	}
	for _, t := range f.state.tokens {
		payload.Tokens = append(payload.Tokens, cmdTokenFrom(t))
	}
	for gid, set := range f.state.memberships {
		for uid := range set {
			payload.Memberships = append(payload.Memberships, membershipEdge{GroupID: gid, UserID: uid})
		}
	}
	for digest, routes := range f.state.cacheBlobRoutes {
		for backendID, createdAt := range routes {
			payload.BlobRoutes = append(payload.BlobRoutes, blobRouteEdge{Digest: digest, BackendID: backendID, CreatedAt: createdAt})
		}
	}
	return &fsmSnapshot{payload: payload}, nil
}

// Restore JSON-decodes a snapshot and replaces the entire state under the
// write lock (never merges). Raft guarantees Restore is not concurrent with
// Apply.
func (f *FSM) Restore(rc io.ReadCloser) error {
	defer func() { _ = rc.Close() }()
	var payload fsmSnapshotPayload
	if err := json.NewDecoder(rc).Decode(&payload); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}

	s := newState()
	for _, cu := range payload.Users {
		if cu == nil {
			continue
		}
		s.storeUser(cu.toDomain())
	}
	for _, g := range payload.Groups {
		if g == nil {
			continue
		}
		s.storeGroup(g)
	}
	for _, ct := range payload.Tokens {
		if ct == nil {
			continue
		}
		s.storeToken(ct.toDomain())
	}
	for _, p := range payload.Projects {
		if p == nil {
			continue
		}
		s.storeProject(p)
	}
	for _, m := range payload.Traces {
		if m == nil {
			continue
		}
		cp := *m
		s.traces[cp.TraceID] = &cp
	}
	for k, v := range payload.Meta {
		s.meta[k] = v
	}
	for _, cr := range payload.ObjectRoutes {
		if cr == nil {
			continue
		}
		cp := *cr
		s.cacheObjectRoutes[manifestRouteKey(cp.Repo, cp.Tag)] = &cp
	}
	for _, edge := range payload.BlobRoutes {
		s.upsertBlobRoute(edge.Digest, edge.BackendID, edge.CreatedAt)
	}
	for _, sess := range payload.Uploads {
		if sess == nil {
			continue
		}
		s.recordUpload(sess)
	}
	for _, edge := range payload.Memberships {
		s.addMembership(edge.GroupID, edge.UserID)
	}

	// Swap the decoded maps into the live state under its write lock instead of
	// replacing the fsmState pointer. Read helpers dereference f.state without
	// synchronizing on the pointer itself, so reassigning it here would race
	// with concurrent reads (snapshot installs can happen on a live node).
	f.state.adopt(s)
	return nil
}

// adopt replaces this state's maps with other's under the write lock. It is
// the sole mechanism Restore uses to install a snapshot, keeping the fsmState
// pointer stable for lock-free read-helper access.
func (s *fsmState) adopt(other *fsmState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users = other.users
	s.usersByName = other.usersByName
	s.usersByOAuth = other.usersByOAuth
	s.groups = other.groups
	s.groupsByName = other.groupsByName
	s.memberships = other.memberships
	s.membershipsByUser = other.membershipsByUser
	s.tokens = other.tokens
	s.tokensByHash = other.tokensByHash
	s.tokensByUser = other.tokensByUser
	s.projects = other.projects
	s.projectsByName = other.projectsByName
	s.traces = other.traces
	s.meta = other.meta
	s.cacheObjectRoutes = other.cacheObjectRoutes
	s.cacheBlobRoutes = other.cacheBlobRoutes
	s.cacheUploadSessions = other.cacheUploadSessions
}
