package repository

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/hashicorp/raft"

	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// membershipEdge is one (groupID, userID) pair in a snapshot.
type membershipEdge struct {
	GroupID string `json:"group_id"`
	UserID  string `json:"user_id"`
}

// orgMembershipEdge is one (orgID, userID, role) direct membership in a
// snapshot.
type orgMembershipEdge struct {
	OrgID  string         `json:"org_id"`
	UserID string         `json:"user_id"`
	Role   domain.OrgRole `json:"role"`
}

// fsmSnapshotPayload is the JSON document persisted into a Raft snapshot.
type fsmSnapshotPayload struct {
	Users       []*cmdUser          `json:"users"`
	Groups      []*domain.Group     `json:"groups"`
	Memberships []membershipEdge    `json:"memberships"`
	Tokens      []*cmdToken         `json:"tokens"`
	Projects    []*domain.Project   `json:"projects"`
	Orgs        []*domain.Org       `json:"orgs"`
	OrgMembers  []orgMembershipEdge `json:"org_members"`
	// OrgMemberSince is optional: snapshots written before the membership
	// timestamp existed decode to an empty map and are backfilled on restore.
	OrgMemberSince   map[string]map[string]time.Time `json:"org_member_since,omitempty"`
	Secrets          []*cmdSecret                    `json:"secrets"`
	ForgeSources     []*cmdForgeSource               `json:"forge_sources"`
	Environments     []*domain.Environment           `json:"environments"`
	CheckRuns        []*domain.CheckRunRecord        `json:"check_runs"`
	CheckRunTokens   map[string]string               `json:"check_run_token_hashes,omitempty"`
	WorkflowDefs     []*domain.WorkflowDef           `json:"workflow_defs"`
	WorkflowRuns     []*cmdWorkflowRun               `json:"workflow_runs"`
	WorkflowCounters map[string]int64                `json:"workflow_counters"`
	Traces           []*domain.TraceMeta             `json:"traces"`
	Meta             map[string]string               `json:"meta"`
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

func copyEnvironments(m map[string]*domain.Environment) []*domain.Environment {
	out := make([]*domain.Environment, 0, len(m))
	for _, env := range m {
		cp := *env
		cp.ApproverGroupIDs = append([]string(nil), env.ApproverGroupIDs...)
		out = append(out, &cp)
	}
	return out
}

func copyCheckRuns(m map[string]*domain.CheckRunRecord) []*domain.CheckRunRecord {
	out := make([]*domain.CheckRunRecord, 0, len(m))
	for _, rec := range m {
		out = append(out, copyCheckRun(rec))
	}
	return out
}

func copyWorkflowDefs(m map[string]*domain.WorkflowDef) []*domain.WorkflowDef {
	out := make([]*domain.WorkflowDef, 0, len(m))
	for _, def := range m {
		out = append(out, copyWorkflowDef(def))
	}
	return out
}

// Snapshot deep-copies the state under the read lock and returns a snapshot.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()

	payload := fsmSnapshotPayload{
		Meta:             make(map[string]string, len(f.state.meta)),
		Users:            make([]*cmdUser, 0, len(f.state.users)),
		Tokens:           make([]*cmdToken, 0, len(f.state.tokens)),
		Groups:           copySlice(f.state.groups),
		Projects:         copySlice(f.state.projects),
		Orgs:             copySlice(f.state.orgs),
		Secrets:          make([]*cmdSecret, 0, len(f.state.secrets)),
		ForgeSources:     make([]*cmdForgeSource, 0, len(f.state.forgeSources)),
		Environments:     copyEnvironments(f.state.environments),
		CheckRuns:        copyCheckRuns(f.state.checkRuns),
		CheckRunTokens:   make(map[string]string, len(f.state.checkRuns)),
		WorkflowDefs:     copyWorkflowDefs(f.state.workflowDefs),
		WorkflowRuns:     make([]*cmdWorkflowRun, 0, len(f.state.workflowRuns)),
		WorkflowCounters: make(map[string]int64, len(f.state.workflowCounters)),
		Traces:           copySlice(f.state.traces),
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
	for _, sec := range f.state.secrets {
		payload.Secrets = append(payload.Secrets, cmdSecretFrom(sec))
	}
	for _, src := range f.state.forgeSources {
		payload.ForgeSources = append(payload.ForgeSources, cmdForgeSourceFrom(src))
	}
	for id, rec := range f.state.checkRuns {
		if rec.RunTokenHash != "" {
			payload.CheckRunTokens[id] = rec.RunTokenHash
		}
	}
	for _, run := range f.state.workflowRuns {
		payload.WorkflowRuns = append(payload.WorkflowRuns, cmdWorkflowRunFrom(run))
	}
	for k, v := range f.state.workflowCounters {
		payload.WorkflowCounters[k] = v
	}
	for gid, set := range f.state.memberships {
		for uid := range set {
			payload.Memberships = append(payload.Memberships, membershipEdge{GroupID: gid, UserID: uid})
		}
	}
	for oid, members := range f.state.orgMembers {
		for uid, role := range members {
			payload.OrgMembers = append(payload.OrgMembers, orgMembershipEdge{OrgID: oid, UserID: uid, Role: role})
		}
	}
	if len(f.state.orgMemberSince) > 0 {
		payload.OrgMemberSince = make(map[string]map[string]time.Time, len(f.state.orgMemberSince))
		for oid, members := range f.state.orgMemberSince {
			cp := make(map[string]time.Time, len(members))
			for uid, at := range members {
				cp[uid] = at
			}
			payload.OrgMemberSince[oid] = cp
		}
	}
	return &fsmSnapshot{payload: payload}, nil
}

// Restore JSON-decodes a snapshot and replaces the entire state under the
// write lock (never merges). Raft guarantees Restore is not concurrent with
// Apply.
//
//nolint:gocyclo // snapshot restore is a linear decode of every entity type
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
	for _, o := range payload.Orgs {
		if o == nil {
			continue
		}
		s.storeOrg(o)
	}
	for _, sec := range payload.Secrets {
		if sec == nil {
			continue
		}
		if d := sec.toDomain(); d != nil {
			s.secrets[secretKey(d.Scope, d.ScopeID, d.Name)] = d
		}
	}
	for _, src := range payload.ForgeSources {
		if src == nil {
			continue
		}
		if d := src.toDomain(); d != nil {
			s.forgeSources[d.ID] = d
		}
	}
	for _, env := range payload.Environments {
		if env == nil {
			continue
		}
		_ = s.upsertEnvironment(env)
	}
	for _, rec := range payload.CheckRuns {
		if rec == nil {
			continue
		}
		rec.RunTokenHash = payload.CheckRunTokens[rec.ID]
		_ = s.upsertCheckRun(rec)
	}
	for _, def := range payload.WorkflowDefs {
		if def == nil {
			continue
		}
		cp := *def
		cp.ContextFiles = copyBytesMap(def.ContextFiles)
		s.workflowDefs[workflowDefKey(def.OrgID, def.ProjectID, def.Workflow.Name)] = &cp
	}
	for _, run := range payload.WorkflowRuns {
		if run == nil {
			continue
		}
		if d := run.toDomain(); d != nil {
			s.workflowRuns[d.ID] = d
		}
	}
	for k, v := range payload.WorkflowCounters {
		s.workflowCounters[k] = v
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
	for _, edge := range payload.Memberships {
		s.addMembership(edge.GroupID, edge.UserID, time.Time{})
	}
	for _, edge := range payload.OrgMembers {
		if s.orgMembers[edge.OrgID] == nil {
			s.orgMembers[edge.OrgID] = make(map[string]domain.OrgRole)
		}
		s.orgMembers[edge.OrgID][edge.UserID] = edge.Role
		if s.orgMembersByUser[edge.UserID] == nil {
			s.orgMembersByUser[edge.UserID] = make(map[string]struct{})
		}
		s.orgMembersByUser[edge.UserID][edge.OrgID] = struct{}{}
	}
	for oid, members := range payload.OrgMemberSince {
		for uid, at := range members {
			// Never materialize a timestamp without its membership edge.
			if _, ok := s.orgMembers[oid][uid]; !ok {
				continue
			}
			s.setOrgMemberSinceLocked(oid, uid, at)
		}
	}
	// Pre-org_member_since snapshots (Raft revision <= 128) carry memberships
	// but no timestamps. Backfill them with the bootstrap org CreatedAt, the
	// best-known lower bound, so they never surface as year 1.
	s.backfillOrgMemberSinceLocked(time.Time{})
	if bootID := s.meta[domain.BootstrapOrgMetaKey]; bootID != "" {
		s.normalizeLegacyRowsLocked(bootID, "", time.Time{})
	}

	// Swap the decoded maps into the live state under its write lock instead of
	// replacing the fsmState pointer. Read helpers dereference f.state without
	// synchronizing on the pointer itself, so reassigning it here would race
	// with concurrent reads (snapshot installs can happen on a live node).
	f.state.adopt(s)
	if bootID := f.state.meta[domain.BootstrapOrgMetaKey]; bootID != "" && f.state.sessionSink != nil {
		f.state.sessionSink.ApplyBootstrapOrg(bootID)
	}

	// Notify the meta hook for every restored meta key so the Minting CA
	// rotator observes the replicated keyring after a snapshot install.
	if f.onMeta != nil {
		for k, v := range payload.Meta {
			f.onMeta(k, v)
		}
	}
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
	s.groupsByOrgName = other.groupsByOrgName
	s.memberships = other.memberships
	s.membershipsByUser = other.membershipsByUser
	s.tokens = other.tokens
	s.tokensByHash = other.tokensByHash
	s.tokensByUser = other.tokensByUser
	s.projects = other.projects
	s.projectsByOrgName = other.projectsByOrgName
	s.orgs = other.orgs
	s.orgsByName = other.orgsByName
	s.orgMembers = other.orgMembers
	s.orgMembersByUser = other.orgMembersByUser
	s.orgMemberSince = other.orgMemberSince
	s.secrets = other.secrets
	s.forgeSources = other.forgeSources
	s.environments = other.environments
	s.checkRuns = other.checkRuns
	s.workflowDefs = other.workflowDefs
	s.workflowRuns = other.workflowRuns
	s.workflowCounters = other.workflowCounters
	s.traces = other.traces
	s.meta = other.meta
}
