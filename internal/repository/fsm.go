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
	// Organization tenancy commands (appended after every existing kind).
	kindUpsertOrg
	kindDeleteOrg
	kindSetOrgMember
	kindRemoveOrgMember
	kindUpsertSecret
	kindDeleteSecret
	kindUpsertForgeSource
	kindDeleteForgeSource
	// Cloud parity replicated state (appended after every existing kind).
	kindUpsertEnvironment
	kindDeleteEnvironment
	kindUpsertCheckRun
	kindDeleteCheckRun
	kindSyncProjectWorkflowDefs
	kindCreateWorkflowRun
	kindUpsertWorkflowRun
	kindDeleteWorkflowRun
	// Organization root-namespace commands (appended after every existing kind).
	kindBootstrapOrg     // 45
	kindDeleteOrgCascade // 46

	// GitHub CLI integration, account sources, OAuth states, and repo claims.
	kindUpsertGitHubConnection    // 47
	kindDeleteGitHubConnection    // 48
	kindUpsertGitHubAccountSource // 49
	kindDeleteGitHubAccountSource // 50
	kindBatchUpsertRepoSettings   // 51
	kindDeleteRepoSetting         // 52
	kindCreateGitHubOAuthState    // 53
	kindConsumeGitHubOAuthState   // 54
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
		GroupID string    `json:"group_id"`
		UserIDs []string  `json:"user_ids"`
		At      time.Time `json:"at"`
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
		OrgID     string    `json:"org_id,omitempty"`
		UserID    string    `json:"user_id"`
		Version   string    `json:"version"`
		UpdatedAt time.Time `json:"updated_at"`
	}

	// cmdBootstrapOrg atomically commits the cluster's bootstrap organization
	// UID and normalizes legacy rows into it. It is idempotent.
	cmdBootstrapOrg struct {
		Org             domain.Org `json:"org"`
		DefaultGroupID  string     `json:"default_group_id"`
		DisplayNameMeta string     `json:"display_name_meta"`
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

	cmdOrg struct {
		Org    domain.Org `json:"org"`
		Create bool       `json:"create"`
	}

	cmdOrgMember struct {
		OrgID  string         `json:"org_id"`
		UserID string         `json:"user_id"`
		Role   domain.OrgRole `json:"role"`
		At     time.Time      `json:"at"`
	}

	// cmdSecret is the snapshot-safe secret payload. domain.Secret hides
	// Ciphertext with json:"-", so the command and snapshot payloads carry it
	// explicitly to avoid silently dropping sealed values on Raft replay or
	// snapshot restore (CWE-311).
	cmdSecret struct {
		ID         string             `json:"id"`
		Scope      domain.SecretScope `json:"scope"`
		ScopeID    string             `json:"scope_id"`
		Name       string             `json:"name"`
		Ciphertext string             `json:"ciphertext"`
		CreatedAt  time.Time          `json:"created_at"`
		UpdatedAt  time.Time          `json:"updated_at"`
	}

	cmdDeleteSecret struct {
		Scope   domain.SecretScope `json:"scope"`
		ScopeID string             `json:"scope_id"`
		Name    string             `json:"name"`
	}

	// cmdForgeSource is the snapshot-safe forge-source payload. It preserves
	// the sealed credential fields that domain.ForgeSource hides with json:"-".
	cmdForgeSource struct {
		ID                      string           `json:"id"`
		OrgID                   string           `json:"org_id"`
		ProjectID               string           `json:"project_id,omitempty"`
		Provider                domain.ForgeKind `json:"provider"`
		BaseURL                 string           `json:"base_url"`
		Repo                    string           `json:"repo"`
		AccountSourceID         string           `json:"account_source_id,omitempty"`
		TokenCiphertext         string           `json:"token_ciphertext"`
		WebhookSecretCiphertext string           `json:"webhook_secret_ciphertext"`
		AppID                   string           `json:"app_id,omitempty"`
		InstallationID          string           `json:"installation_id,omitempty"`
		PrivateKeyCiphertext    string           `json:"private_key_ciphertext"`
		CreatedAt               time.Time        `json:"created_at"`
		UpdatedAt               time.Time        `json:"updated_at"`
	}

	cmdEnvironment struct {
		Environment domain.Environment `json:"environment"`
	}

	cmdDeleteEnvironment struct {
		OrgID string `json:"org_id"`
		Name  string `json:"name"`
	}

	// cmdCheckRun is the snapshot-safe check-run payload. It preserves
	// RunTokenHash, which domain.CheckRunRecord hides with json:"-".
	cmdCheckRun struct {
		Record       domain.CheckRunRecord `json:"record"`
		RunTokenHash string                `json:"run_token_hash,omitempty"`
	}

	cmdDeleteCheckRun struct {
		ID string `json:"id"`
	}

	cmdSyncProjectWorkflowDefs struct {
		OrgID     string                `json:"org_id"`
		ProjectID string                `json:"project_id"`
		Defs      []*domain.WorkflowDef `json:"defs"`
	}

	// cmdWorkflowRun is the snapshot-safe workflow-run payload. It preserves
	// ContextFiles, RunTokenHash, and PreviousRunTokenHash, which
	// domain.WorkflowRun hides with json:"-".
	cmdWorkflowRun struct {
		Run                  domain.WorkflowRun `json:"run"`
		ContextFiles         map[string][]byte  `json:"context_files,omitempty"`
		RunTokenHash         string             `json:"run_token_hash,omitempty"`
		PreviousRunTokenHash string             `json:"previous_run_token_hash,omitempty"`
	}

	cmdDeleteWorkflowRun struct {
		ID string `json:"id"`
	}

	// cmdGitHubConnection is the snapshot-safe user GitHub connection payload.
	// domain.UserGitHubConnection hides TokenCiphertext with json:"-".
	cmdGitHubConnection struct {
		UserID          string                        `json:"user_id"`
		GitHubUserID    int64                         `json:"github_user_id"`
		GitHubLogin     string                        `json:"github_login"`
		AvatarURL       string                        `json:"avatar_url,omitempty"`
		BaseURL         string                        `json:"base_url"`
		Scopes          []string                      `json:"scopes,omitempty"`
		TokenCiphertext string                        `json:"token_ciphertext"`
		Status          domain.GitHubConnectionStatus `json:"status"`
		ConnectedAt     time.Time                     `json:"connected_at"`
		UpdatedAt       time.Time                     `json:"updated_at"`
	}

	cmdDeleteGitHubConnection struct {
		UserID string `json:"user_id"`
	}

	// cmdGitHubAccountSource is the snapshot-safe org account payload.
	cmdGitHubAccountSource struct {
		ID                string                        `json:"id"`
		OrgID             string                        `json:"org_id"`
		Owner             string                        `json:"owner"`
		OwnerType         string                        `json:"owner_type"`
		BaseURL           string                        `json:"base_url"`
		ConnectedByUserID string                        `json:"connected_by_user_id,omitempty"`
		Scopes            []string                      `json:"scopes,omitempty"`
		TokenCiphertext   string                        `json:"token_ciphertext"`
		AppID             string                        `json:"app_id,omitempty"`
		InstallationID    string                        `json:"installation_id,omitempty"`
		Status            domain.GitHubConnectionStatus `json:"status"`
		CreatedAt         time.Time                     `json:"created_at"`
		UpdatedAt         time.Time                     `json:"updated_at"`
	}

	cmdDeleteGitHubAccountSource struct {
		ID string `json:"id"`
	}

	cmdRepoSettingWrite struct {
		Setting     domain.RepoSetting `json:"setting"`
		ForgeSource *cmdForgeSource    `json:"forge_source,omitempty"`
	}

	cmdBatchUpsertRepoSettings struct {
		OrgID  string                `json:"org_id"`
		Writes []cmdRepoSettingWrite `json:"writes"`
	}

	cmdDeleteRepoSetting struct {
		OrgID               string `json:"org_id"`
		Repo                string `json:"repo"`
		DeleteForgeSourceID string `json:"delete_forge_source_id,omitempty"`
	}

	// cmdGitHubOAuthState is the snapshot-safe OAuth state payload.
	// domain.GitHubOAuthState hides PKCEVerifier with json:"-".
	cmdGitHubOAuthState struct {
		StateID      string    `json:"state_id"`
		UserID       string    `json:"user_id"`
		OrgID        string    `json:"org_id,omitempty"`
		RedirectURI  string    `json:"redirect_uri"`
		PKCEVerifier string    `json:"pkce_verifier"`
		CreatedAt    time.Time `json:"created_at"`
		ExpiresAt    time.Time `json:"expires_at"`
		Consumed     bool      `json:"consumed"`
	}

	cmdConsumeGitHubOAuthState struct {
		StateID string    `json:"state_id"`
		At      time.Time `json:"at"`
	}
)

// cmdCheckRunFrom converts a domain.CheckRunRecord into its snapshot-safe
// command form.
func cmdCheckRunFrom(rec *domain.CheckRunRecord) *cmdCheckRun {
	if rec == nil {
		return nil
	}
	cp := *rec
	cp.RunTokenHash = ""
	return &cmdCheckRun{Record: cp, RunTokenHash: rec.RunTokenHash}
}

// toDomain converts a command check-run payload back into a
// domain.CheckRunRecord.
func (c *cmdCheckRun) toDomain() *domain.CheckRunRecord {
	if c == nil {
		return nil
	}
	cp := c.Record
	cp.RunTokenHash = c.RunTokenHash
	return &cp
}

// cmdWorkflowRunFrom converts a domain.WorkflowRun into its snapshot-safe
// command form.
func cmdWorkflowRunFrom(run *domain.WorkflowRun) *cmdWorkflowRun {
	if run == nil {
		return nil
	}
	cp := *run
	cp.ContextFiles = nil
	cp.RunTokenHash = ""
	cp.PreviousRunTokenHash = ""
	return &cmdWorkflowRun{Run: cp, ContextFiles: run.ContextFiles, RunTokenHash: run.RunTokenHash, PreviousRunTokenHash: run.PreviousRunTokenHash}
}

// toDomain converts a command workflow-run payload back into a
// domain.WorkflowRun.
func (c *cmdWorkflowRun) toDomain() *domain.WorkflowRun {
	if c == nil {
		return nil
	}
	cp := c.Run
	cp.ContextFiles = c.ContextFiles
	cp.RunTokenHash = c.RunTokenHash
	cp.PreviousRunTokenHash = c.PreviousRunTokenHash
	return &cp
}

// cmdSecretFrom converts a domain.Secret into its snapshot-safe command form.
func cmdSecretFrom(s *domain.Secret) *cmdSecret {
	if s == nil {
		return nil
	}
	return &cmdSecret{
		ID:         s.ID,
		Scope:      s.Scope,
		ScopeID:    s.ScopeID,
		Name:       s.Name,
		Ciphertext: s.Ciphertext,
		CreatedAt:  s.CreatedAt,
		UpdatedAt:  s.UpdatedAt,
	}
}

// toDomain converts a command secret payload back into a domain.Secret.
func (c *cmdSecret) toDomain() *domain.Secret {
	if c == nil {
		return nil
	}
	return &domain.Secret{
		ID:         c.ID,
		Scope:      c.Scope,
		ScopeID:    c.ScopeID,
		Name:       c.Name,
		Ciphertext: c.Ciphertext,
		CreatedAt:  c.CreatedAt,
		UpdatedAt:  c.UpdatedAt,
	}
}

// cmdForgeSourceFrom converts a domain.ForgeSource into its snapshot-safe
// command form.
func cmdForgeSourceFrom(s *domain.ForgeSource) *cmdForgeSource {
	if s == nil {
		return nil
	}
	return &cmdForgeSource{
		ID:                      s.ID,
		OrgID:                   s.OrgID,
		ProjectID:               s.ProjectID,
		Provider:                s.Provider,
		BaseURL:                 s.BaseURL,
		Repo:                    s.Repo,
		AccountSourceID:         s.AccountSourceID,
		TokenCiphertext:         s.TokenCiphertext,
		WebhookSecretCiphertext: s.WebhookSecretCiphertext,
		AppID:                   s.AppID,
		InstallationID:          s.InstallationID,
		PrivateKeyCiphertext:    s.PrivateKeyCiphertext,
		CreatedAt:               s.CreatedAt,
		UpdatedAt:               s.UpdatedAt,
	}
}

// toDomain converts a command forge-source payload back into a
// domain.ForgeSource.
func (c *cmdForgeSource) toDomain() *domain.ForgeSource {
	if c == nil {
		return nil
	}
	return &domain.ForgeSource{
		ID:                      c.ID,
		OrgID:                   c.OrgID,
		ProjectID:               c.ProjectID,
		Provider:                c.Provider,
		BaseURL:                 c.BaseURL,
		Repo:                    c.Repo,
		AccountSourceID:         c.AccountSourceID,
		TokenCiphertext:         c.TokenCiphertext,
		WebhookSecretCiphertext: c.WebhookSecretCiphertext,
		AppID:                   c.AppID,
		InstallationID:          c.InstallationID,
		PrivateKeyCiphertext:    c.PrivateKeyCiphertext,
		CreatedAt:               c.CreatedAt,
		UpdatedAt:               c.UpdatedAt,
	}
}

// toDomain converts a command GitHub connection payload into a
// domain.UserGitHubConnection.
func (c *cmdGitHubConnection) toDomain() *domain.UserGitHubConnection {
	if c == nil {
		return nil
	}
	return &domain.UserGitHubConnection{
		UserID:          c.UserID,
		GitHubUserID:    c.GitHubUserID,
		GitHubLogin:     c.GitHubLogin,
		AvatarURL:       c.AvatarURL,
		BaseURL:         c.BaseURL,
		Scopes:          append([]string(nil), c.Scopes...),
		TokenCiphertext: c.TokenCiphertext,
		Status:          c.Status,
		ConnectedAt:     c.ConnectedAt,
		UpdatedAt:       c.UpdatedAt,
	}
}

// cmdGitHubConnectionFrom converts a domain.UserGitHubConnection into its
// snapshot-safe command form.
func cmdGitHubConnectionFrom(conn *domain.UserGitHubConnection) *cmdGitHubConnection {
	if conn == nil {
		return nil
	}
	return &cmdGitHubConnection{
		UserID:          conn.UserID,
		GitHubUserID:    conn.GitHubUserID,
		GitHubLogin:     conn.GitHubLogin,
		AvatarURL:       conn.AvatarURL,
		BaseURL:         conn.BaseURL,
		Scopes:          append([]string(nil), conn.Scopes...),
		TokenCiphertext: conn.TokenCiphertext,
		Status:          conn.Status,
		ConnectedAt:     conn.ConnectedAt,
		UpdatedAt:       conn.UpdatedAt,
	}
}

// toDomain converts a command GitHub account source payload into a
// domain.GitHubAccountSource.
func (c *cmdGitHubAccountSource) toDomain() *domain.GitHubAccountSource {
	if c == nil {
		return nil
	}
	return &domain.GitHubAccountSource{
		ID:                c.ID,
		OrgID:             c.OrgID,
		Owner:             c.Owner,
		OwnerType:         c.OwnerType,
		BaseURL:           c.BaseURL,
		ConnectedByUserID: c.ConnectedByUserID,
		Scopes:            append([]string(nil), c.Scopes...),
		TokenCiphertext:   c.TokenCiphertext,
		AppID:             c.AppID,
		InstallationID:    c.InstallationID,
		Status:            c.Status,
		CreatedAt:         c.CreatedAt,
		UpdatedAt:         c.UpdatedAt,
	}
}

// cmdGitHubAccountSourceFrom converts a domain.GitHubAccountSource into its
// snapshot-safe command form.
func cmdGitHubAccountSourceFrom(src *domain.GitHubAccountSource) *cmdGitHubAccountSource {
	if src == nil {
		return nil
	}
	return &cmdGitHubAccountSource{
		ID:                src.ID,
		OrgID:             src.OrgID,
		Owner:             src.Owner,
		OwnerType:         src.OwnerType,
		BaseURL:           src.BaseURL,
		ConnectedByUserID: src.ConnectedByUserID,
		Scopes:            append([]string(nil), src.Scopes...),
		TokenCiphertext:   src.TokenCiphertext,
		AppID:             src.AppID,
		InstallationID:    src.InstallationID,
		Status:            src.Status,
		CreatedAt:         src.CreatedAt,
		UpdatedAt:         src.UpdatedAt,
	}
}

// toDomain converts a command OAuth state payload into a
// domain.GitHubOAuthState.
func (c *cmdGitHubOAuthState) toDomain() *domain.GitHubOAuthState {
	if c == nil {
		return nil
	}
	return &domain.GitHubOAuthState{
		StateID:      c.StateID,
		UserID:       c.UserID,
		OrgID:        c.OrgID,
		RedirectURI:  c.RedirectURI,
		PKCEVerifier: c.PKCEVerifier,
		CreatedAt:    c.CreatedAt,
		ExpiresAt:    c.ExpiresAt,
		Consumed:     c.Consumed,
	}
}

// cmdGitHubOAuthStateFrom converts a domain.GitHubOAuthState into its
// snapshot-safe command form.
func cmdGitHubOAuthStateFrom(st *domain.GitHubOAuthState) *cmdGitHubOAuthState {
	if st == nil {
		return nil
	}
	return &cmdGitHubOAuthState{
		StateID:      st.StateID,
		UserID:       st.UserID,
		OrgID:        st.OrgID,
		RedirectURI:  st.RedirectURI,
		PKCEVerifier: st.PKCEVerifier,
		CreatedAt:    st.CreatedAt,
		ExpiresAt:    st.ExpiresAt,
		Consumed:     st.Consumed,
	}
}

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
	groupsByOrgName   map[string]string
	memberships       map[string]map[string]struct{} // groupID -> set(userID)
	membershipsByUser map[string]map[string]struct{} // userID -> set(groupID)

	tokens       map[string]*domain.APIToken
	tokensByHash map[string]string
	tokensByUser map[string]string

	projects          map[string]*domain.Project
	projectsByOrgName map[string]string

	orgs             map[string]*domain.Org
	orgsByName       map[string]string
	orgMembers       map[string]map[string]domain.OrgRole // orgID -> userID -> role
	orgMembersByUser map[string]map[string]struct{}       // userID -> set(orgID)
	// orgMemberSince records each direct membership's creation time. It is
	// kept parallel to orgMembers so snapshots stay backward compatible (the
	// org_members role strings never change shape).
	orgMemberSince map[string]map[string]time.Time // orgID -> userID -> created_at

	secrets      map[string]*domain.Secret // key = scope|scopeID|name
	forgeSources map[string]*domain.ForgeSource

	githubConnections        map[string]*domain.UserGitHubConnection // key = userID
	githubAccountSources     map[string]*domain.GitHubAccountSource  // key = accountSourceID
	githubAccountsByOrgOwner map[string]string                       // orgID\x00lower(owner) -> accountSourceID
	repoSettings             map[string]*domain.RepoSetting          // orgID\x00lower(canonicalRepo) -> setting
	repoClaimsByRepo         map[string]string                       // lower(canonicalRepo) -> orgID
	githubOAuthStates        map[string]*domain.GitHubOAuthState     // key = stateID

	environments     map[string]*domain.Environment // key = orgID/name
	checkRuns        map[string]*domain.CheckRunRecord
	workflowDefs     map[string]*domain.WorkflowDef // key = orgID/projectID/wfName
	workflowRuns     map[string]*domain.WorkflowRun
	workflowCounters map[string]int64 // key = orgID/projectID/wfName

	traces map[string]*domain.TraceMeta

	meta map[string]string

	// sessionSink receives replicated session-lease state (domain.SessionStateSink,
	// usually the pod-local *service.Store). Nil until wired by NewRaftStore.
	sessionSink domain.SessionStateSink
	// quotaDefaults carries the cluster-wide org quota defaults so FSM-side
	// guards are deterministic on every peer.
	quotaDefaults domain.OrgQuotas
	// metaHook observes replicated meta writes (bootstrap org UID, CA keyring).
	metaHook func(key, value string)
}

func newState() *fsmState {
	st := &fsmState{
		users:                    make(map[string]*domain.User),
		usersByName:              make(map[string]string),
		usersByOAuth:             make(map[string]string),
		groups:                   make(map[string]*domain.Group),
		groupsByOrgName:          make(map[string]string),
		memberships:              make(map[string]map[string]struct{}),
		membershipsByUser:        make(map[string]map[string]struct{}),
		tokens:                   make(map[string]*domain.APIToken),
		tokensByHash:             make(map[string]string),
		tokensByUser:             make(map[string]string),
		projects:                 make(map[string]*domain.Project),
		projectsByOrgName:        make(map[string]string),
		orgs:                     make(map[string]*domain.Org),
		orgsByName:               make(map[string]string),
		orgMembers:               make(map[string]map[string]domain.OrgRole),
		orgMembersByUser:         make(map[string]map[string]struct{}),
		orgMemberSince:           make(map[string]map[string]time.Time),
		secrets:                  make(map[string]*domain.Secret),
		forgeSources:             make(map[string]*domain.ForgeSource),
		githubConnections:        make(map[string]*domain.UserGitHubConnection),
		githubAccountSources:     make(map[string]*domain.GitHubAccountSource),
		githubAccountsByOrgOwner: make(map[string]string),
		repoSettings:             make(map[string]*domain.RepoSetting),
		repoClaimsByRepo:         make(map[string]string),
		githubOAuthStates:        make(map[string]*domain.GitHubOAuthState),
		environments:             make(map[string]*domain.Environment),
		checkRuns:                make(map[string]*domain.CheckRunRecord),
		workflowDefs:             make(map[string]*domain.WorkflowDef),
		workflowRuns:             make(map[string]*domain.WorkflowRun),
		workflowCounters:         make(map[string]int64),
		traces:                   make(map[string]*domain.TraceMeta),
		meta:                     make(map[string]string),
	}
	return st
}

// FSM is the Raft finite state machine. All reads are served from the local
// state; all writes are deterministic applications of a command.
type FSM struct {
	state *fsmState
	// onMeta, when set, is invoked for every replicated meta key/value on both
	// Apply (kindSetMeta) and Restore. It lets the Minting CA rotator observe
	// meta["minting_tls_keyring"] on every pod in real time.
	onMeta func(key, value string)
}

// NewFSM returns an empty FSM.
func NewFSM() *FSM {
	return &FSM{state: newState()}
}

// SetMetaHook installs the meta-change callback. It must be called before the
// FSM starts applying (i.e. before NewRaftStore returns).
func (f *FSM) SetMetaHook(hook func(key, value string)) {
	f.onMeta = hook
	f.state.metaHook = hook
}

// SetOrgQuotaDefaults installs the cluster-wide default quotas so FSM-side
// hard-cap guards match the service layer on every peer.
//
//nolint:gocritic // quota structs are value-semantic
func (f *FSM) SetOrgQuotaDefaults(q domain.OrgQuotas) {
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	f.state.quotaDefaults = q
}

// BootstrapOrgID returns the committed bootstrap organization UID.
func (f *FSM) BootstrapOrgID() (string, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	id := f.state.meta[domain.BootstrapOrgMetaKey]
	if id == "" {
		return "", domain.ErrBootstrapNotReady
	}
	return id, nil
}

// bootstrapOrg returns the committed bootstrap organization.
func (f *FSM) bootstrapOrg() (*domain.Org, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	id := f.state.meta[domain.BootstrapOrgMetaKey]
	if id == "" {
		return nil, domain.ErrBootstrapNotReady
	}
	o, ok := f.state.orgs[id]
	if !ok {
		return nil, domain.ErrBootstrapNotReady
	}
	return copyOrg(o), nil
}

// memberRole returns (orgID,userID)'s direct role.
func (f *FSM) memberRole(orgID, userID string) (domain.OrgRole, bool) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	role, ok := f.state.orgMembers[orgID][userID]
	return role, ok
}

// distinctMemberCountLocked counts distinct members in orgID. Caller holds mu.
func (s *fsmState) distinctMemberCountLocked(orgID string) int {
	seen := make(map[string]struct{}, len(s.orgMembers[orgID]))
	for uid := range s.orgMembers[orgID] {
		seen[uid] = struct{}{}
	}
	for gid, set := range s.memberships {
		g, ok := s.groups[gid]
		if !ok || g.OrgID != orgID {
			continue
		}
		for uid := range set {
			seen[uid] = struct{}{}
		}
	}
	return len(seen)
}

// distinctMemberCount is the locked read variant used by the org repository.
func (f *FSM) distinctMemberCount(orgID string) int {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	return f.state.distinctMemberCountLocked(orgID)
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

	if cmd.Kind == kindSetMeta {
		return nil, f.applySetMetaLocked(cmd)
	}
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

// applySetMetaLocked applies a kindSetMeta command and invokes the meta hook.
// Callers must hold the state write lock.
func (f *FSM) applySetMetaLocked(cmd *command) error {
	return applyPayload(cmd, "set meta", func(p cmdSetMeta) error {
		f.state.meta[p.Key] = p.Value
		if f.onMeta != nil {
			f.onMeta(p.Key, p.Value)
		}
		return nil
	})
}

// applyEntityCommand handles user/group/project/token commands. Handled
// commands may return a non-nil response object (the committed snapshot) which
// the repository layer type-checks.
//
//nolint:gocyclo // command dispatch table
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
	case kindUpsertOrg:
		return nil, applyPayload(cmd, "org", func(p cmdOrg) error {
			return s.upsertOrg(&p.Org, p.Create)
		}), true
	case kindDeleteOrg:
		return nil, applyPayload(cmd, "delete org", s.deleteOrg), true
	case kindSetOrgMember:
		return nil, applyPayload(cmd, "set org member", func(p cmdOrgMember) error {
			return s.setOrgMember(p)
		}), true
	case kindRemoveOrgMember:
		return nil, applyPayload(cmd, "remove org member", func(p cmdOrgMember) error {
			return s.removeOrgMember(p.OrgID, p.UserID)
		}), true
	case kindBootstrapOrg:
		var out *domain.Org
		err := applyPayload(cmd, "bootstrap org", func(p cmdBootstrapOrg) error {
			o, err := s.applyBootstrapOrg(&p)
			out = o
			return err
		})
		return out, err, true
	case kindDeleteOrgCascade:
		return nil, applyPayload(cmd, "delete org cascade", s.deleteOrgCascade), true
	case kindUpsertSecret:
		return nil, applyPayload(cmd, "secret", func(p cmdSecret) error {
			return s.upsertSecret(p.toDomain())
		}), true
	case kindDeleteSecret:
		return nil, applyPayload(cmd, "delete secret", func(p cmdDeleteSecret) error {
			delete(s.secrets, secretKey(p.Scope, p.ScopeID, p.Name))
			return nil
		}), true
	case kindUpsertForgeSource:
		return nil, applyPayload(cmd, "forge source", func(p cmdForgeSource) error {
			return s.upsertForgeSource(p.toDomain())
		}), true
	case kindDeleteForgeSource:
		return nil, applyPayload(cmd, "delete forge source", func(p cmdForgeSource) error {
			s.deleteForgeSource(p.ID)
			return nil
		}), true
	case kindUpsertEnvironment:
		return nil, applyPayload(cmd, "environment", func(p cmdEnvironment) error {
			return s.upsertEnvironment(&p.Environment)
		}), true
	case kindDeleteEnvironment:
		return nil, applyPayload(cmd, "delete environment", func(p cmdDeleteEnvironment) error {
			delete(s.environments, environmentKey(p.OrgID, p.Name))
			return nil
		}), true
	case kindUpsertCheckRun:
		return nil, applyPayload(cmd, "check run", func(p cmdCheckRun) error {
			return s.upsertCheckRun(p.toDomain())
		}), true
	case kindDeleteCheckRun:
		return nil, applyPayload(cmd, "delete check run", func(p cmdDeleteCheckRun) error {
			delete(s.checkRuns, p.ID)
			return nil
		}), true
	case kindSyncProjectWorkflowDefs:
		return nil, applyPayload(cmd, "sync workflow defs", func(p cmdSyncProjectWorkflowDefs) error {
			return s.syncProjectWorkflowDefs(p.OrgID, p.ProjectID, p.Defs)
		}), true
	case kindCreateWorkflowRun:
		var out *domain.WorkflowRun
		err := applyPayload(cmd, "create workflow run", func(p cmdWorkflowRun) error {
			run, err := s.createWorkflowRun(p.toDomain())
			out = run
			return err
		})
		return out, err, true
	case kindUpsertWorkflowRun:
		return nil, applyPayload(cmd, "workflow run", func(p cmdWorkflowRun) error {
			return s.upsertWorkflowRun(p.toDomain())
		}), true
	case kindDeleteWorkflowRun:
		return nil, applyPayload(cmd, "delete workflow run", func(p cmdDeleteWorkflowRun) error {
			delete(s.workflowRuns, p.ID)
			return nil
		}), true
	case kindUpsertGroup:
		return nil, applyPayload(cmd, "group", func(p cmdGroup) error {
			return s.upsertGroup(&p.Group, p.Create)
		}), true
	case kindDeleteGroup:
		return nil, applyPayload(cmd, "delete group", s.deleteGroup), true
	case kindSetMembers:
		return nil, applyPayload(cmd, "set members", func(p cmdSetMembers) error {
			return s.setMembers(p.GroupID, p.UserIDs, p.At)
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
	case kindUpsertGitHubConnection:
		return nil, applyPayload(cmd, "github connection", func(p cmdGitHubConnection) error {
			return s.upsertGitHubConnection(p.toDomain())
		}), true
	case kindDeleteGitHubConnection:
		return nil, applyPayload(cmd, "delete github connection", func(p cmdDeleteGitHubConnection) error {
			s.deleteGitHubConnection(p.UserID)
			return nil
		}), true
	case kindUpsertGitHubAccountSource:
		return nil, applyPayload(cmd, "github account source", func(p cmdGitHubAccountSource) error {
			return s.upsertGitHubAccountSource(p.toDomain())
		}), true
	case kindDeleteGitHubAccountSource:
		return nil, applyPayload(cmd, "delete github account source", func(p cmdDeleteGitHubAccountSource) error {
			s.deleteGitHubAccountSource(p.ID)
			return nil
		}), true
	case kindBatchUpsertRepoSettings:
		return nil, applyPayload(cmd, "batch upsert repo settings", func(p cmdBatchUpsertRepoSettings) error {
			return s.batchUpsertRepoSettings(&p)
		}), true
	case kindDeleteRepoSetting:
		return nil, applyPayload(cmd, "delete repo setting", func(p cmdDeleteRepoSetting) error {
			s.deleteRepoSetting(p)
			return nil
		}), true
	case kindCreateGitHubOAuthState:
		return nil, applyPayload(cmd, "create github oauth state", func(p cmdGitHubOAuthState) error {
			return s.createGitHubOAuthState(p.toDomain())
		}), true
	case kindConsumeGitHubOAuthState:
		var out *domain.GitHubOAuthState
		err := applyPayload(cmd, "consume github oauth state", func(p cmdConsumeGitHubOAuthState) error {
			st, err := s.consumeGitHubOAuthState(p.StateID, p.At)
			out = st
			return err
		})
		return out, err, true
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
	s.groupsByOrgName[orgEntityNameKey(g.OrgID, g.Name)] = g.ID
}

func (s *fsmState) storeProject(p *domain.Project) {
	cp := *p
	s.projects[p.ID] = &cp
	s.projectsByOrgName[orgEntityNameKey(p.OrgID, p.Name)] = p.ID
}

// orgEntityNameKey builds the composite (orgID, lowerName) uniqueness key.
func orgEntityNameKey(orgID, name string) string {
	return fmt.Sprintf("%s\x00%s", orgID, strings.ToLower(name))
}

// maxPendingGitHubOAuthStates bounds replicated OAuth state memory.
const maxPendingGitHubOAuthStates = 4096

func githubAccountOwnerKey(orgID, owner string) string {
	return fmt.Sprintf("%s\x00%s", orgID, strings.ToLower(strings.TrimSpace(owner)))
}

func repoSettingKey(orgID, repo string) string {
	return fmt.Sprintf("%s\x00%s", orgID, strings.ToLower(strings.TrimSpace(repo)))
}

func repoClaimKey(repo string) string {
	return strings.ToLower(strings.TrimSpace(repo))
}

// upsertGitHubConnection stores a user-scoped connection.
func (s *fsmState) upsertGitHubConnection(conn *domain.UserGitHubConnection) error {
	if conn == nil || conn.UserID == "" {
		return fmt.Errorf("github connection: %w", domain.ErrValidation)
	}
	cp := *conn
	cp.Scopes = append([]string(nil), conn.Scopes...)
	s.githubConnections[conn.UserID] = &cp
	return nil
}

// deleteGitHubConnection erases a user's connection and any pending OAuth
// states bound to that user.
func (s *fsmState) deleteGitHubConnection(userID string) {
	delete(s.githubConnections, userID)
	for id, st := range s.githubOAuthStates {
		if st.UserID == userID {
			delete(s.githubOAuthStates, id)
		}
	}
}

// upsertGitHubAccountSource stores an org-scoped account source and refreshes
// the (org, owner) index.
func (s *fsmState) upsertGitHubAccountSource(src *domain.GitHubAccountSource) error {
	if src == nil || src.ID == "" || src.OrgID == "" || src.Owner == "" {
		return fmt.Errorf("github account source: %w", domain.ErrValidation)
	}
	if existing, ok := s.githubAccountSources[src.ID]; ok {
		delete(s.githubAccountsByOrgOwner, githubAccountOwnerKey(existing.OrgID, existing.Owner))
	}
	cp := *src
	cp.Scopes = append([]string(nil), src.Scopes...)
	s.githubAccountSources[src.ID] = &cp
	s.githubAccountsByOrgOwner[githubAccountOwnerKey(src.OrgID, src.Owner)] = src.ID
	return nil
}

// deleteGitHubAccountSource removes an account source and its owner index.
func (s *fsmState) deleteGitHubAccountSource(id string) {
	src, ok := s.githubAccountSources[id]
	if !ok {
		return
	}
	delete(s.githubAccountsByOrgOwner, githubAccountOwnerKey(src.OrgID, src.Owner))
	delete(s.githubAccountSources, id)
}

// batchUpsertRepoSettings atomically validates and applies repo claims plus
// their optional materialized ForgeSources.
func (s *fsmState) batchUpsertRepoSettings(p *cmdBatchUpsertRepoSettings) error {
	if p == nil || p.OrgID == "" {
		return fmt.Errorf("repo settings: %w", domain.ErrValidation)
	}
	for i := range p.Writes {
		w := &p.Writes[i]
		if w.Setting.OrgID == "" || w.Setting.OrgID != p.OrgID || w.Setting.Repo == "" {
			return fmt.Errorf("repo setting: %w", domain.ErrValidation)
		}
		if w.ForgeSource != nil && w.ForgeSource.OrgID != p.OrgID {
			return fmt.Errorf("repo setting forge source: %w", domain.ErrValidation)
		}
		if ownerOrg, taken := s.repoClaimsByRepo[repoClaimKey(w.Setting.Repo)]; taken && ownerOrg != p.OrgID {
			return fmt.Errorf("repo %s: %w", w.Setting.Repo, domain.ErrRepoClaimedByOtherOrg)
		}
	}
	for i := range p.Writes {
		w := &p.Writes[i]
		if w.ForgeSource != nil {
			if err := s.upsertForgeSource(w.ForgeSource.toDomain()); err != nil {
				return err
			}
		}
		cp := w.Setting
		cp.OrgID = p.OrgID
		s.repoSettings[repoSettingKey(p.OrgID, cp.Repo)] = &cp
		s.repoClaimsByRepo[repoClaimKey(cp.Repo)] = p.OrgID
	}
	return nil
}

// deleteRepoSetting removes one claim and (optionally) its materialized forge
// source. It is idempotent when already absent.
func (s *fsmState) deleteRepoSetting(p cmdDeleteRepoSetting) {
	key := repoSettingKey(p.OrgID, p.Repo)
	delete(s.repoSettings, key)
	if s.repoClaimsByRepo[repoClaimKey(p.Repo)] == p.OrgID {
		delete(s.repoClaimsByRepo, repoClaimKey(p.Repo))
	}
	if p.DeleteForgeSourceID != "" {
		if src, ok := s.forgeSources[p.DeleteForgeSourceID]; ok && src.OrgID == p.OrgID {
			delete(s.forgeSources, p.DeleteForgeSourceID)
		}
	}
}

// deleteForgeSource removes a forge source and any repo claim it backed.
func (s *fsmState) deleteForgeSource(id string) {
	src, ok := s.forgeSources[id]
	if !ok {
		return
	}
	for key, rs := range s.repoSettings {
		if rs.OrgID != src.OrgID || rs.ForgeSourceID != id {
			continue
		}
		delete(s.repoSettings, key)
		if s.repoClaimsByRepo[repoClaimKey(rs.Repo)] == src.OrgID {
			delete(s.repoClaimsByRepo, repoClaimKey(rs.Repo))
		}
	}
	delete(s.forgeSources, id)
}

// createGitHubOAuthState stores a single-use OAuth state, sweeping expired or
// consumed entries first and evicting one bounded entry when full.
func (s *fsmState) createGitHubOAuthState(st *domain.GitHubOAuthState) error {
	if st == nil || st.StateID == "" || st.UserID == "" {
		return fmt.Errorf("github oauth state: %w", domain.ErrValidation)
	}
	for id, existing := range s.githubOAuthStates {
		if existing.Consumed || (!existing.ExpiresAt.IsZero() && !existing.ExpiresAt.After(st.CreatedAt)) {
			delete(s.githubOAuthStates, id)
		}
	}
	if len(s.githubOAuthStates) >= maxPendingGitHubOAuthStates {
		s.evictOldestGitHubOAuthState()
	}
	cp := *st
	cp.Consumed = false
	s.githubOAuthStates[st.StateID] = &cp
	return nil
}

// evictOldestGitHubOAuthState drops the entry with the earliest ExpiresAt,
// breaking ties by the lexicographically smallest StateID (deterministic on
// every peer).
func (s *fsmState) evictOldestGitHubOAuthState() {
	var victimID string
	var victimExpiry time.Time
	for id, st := range s.githubOAuthStates {
		if victimID == "" || st.ExpiresAt.Before(victimExpiry) || (st.ExpiresAt.Equal(victimExpiry) && id < victimID) {
			victimID = id
			victimExpiry = st.ExpiresAt
		}
	}
	if victimID != "" {
		delete(s.githubOAuthStates, victimID)
	}
}

// consumeGitHubOAuthState atomically validates and deletes a state, returning
// a deep copy for the caller.
func (s *fsmState) consumeGitHubOAuthState(stateID string, at time.Time) (*domain.GitHubOAuthState, error) {
	st, ok := s.githubOAuthStates[stateID]
	if !ok || st.Consumed || !at.Before(st.ExpiresAt) {
		return nil, domain.ErrOAuthStateInvalid
	}
	cp := *st
	cp.Consumed = true
	delete(s.githubOAuthStates, stateID)
	return &cp, nil
}

// removeGitHubOrgScopedLocked removes every GitHub account source, repo claim,
// and pending OAuth state owned by orgID.
func (s *fsmState) removeGitHubOrgScopedLocked(orgID string) {
	for id, src := range s.githubAccountSources {
		if src.OrgID == orgID {
			delete(s.githubAccountsByOrgOwner, githubAccountOwnerKey(src.OrgID, src.Owner))
			delete(s.githubAccountSources, id)
		}
	}
	for key, rs := range s.repoSettings {
		if rs.OrgID == orgID {
			if s.repoClaimsByRepo[repoClaimKey(rs.Repo)] == orgID {
				delete(s.repoClaimsByRepo, repoClaimKey(rs.Repo))
			}
			delete(s.repoSettings, key)
		}
	}
	for id, st := range s.githubOAuthStates {
		if st.OrgID == orgID {
			delete(s.githubOAuthStates, id)
		}
	}
}

// secretKey builds the FSM key for a scoped secret.
func secretKey(scope domain.SecretScope, scopeID, name string) string {
	return fmt.Sprintf("%s|%s|%s", scope, scopeID, name)
}

// upsertSecret stores a sealed secret.
func (s *fsmState) upsertSecret(sec *domain.Secret) error {
	if sec == nil || sec.Name == "" || sec.ScopeID == "" {
		return fmt.Errorf("secret: %w", domain.ErrValidation)
	}
	cp := *sec
	s.secrets[secretKey(sec.Scope, sec.ScopeID, sec.Name)] = &cp
	return nil
}

// upsertForgeSource stores a forge integration.
func (s *fsmState) upsertForgeSource(src *domain.ForgeSource) error {
	if src == nil || src.ID == "" || src.OrgID == "" {
		return fmt.Errorf("forge source: %w", domain.ErrValidation)
	}
	cp := *src
	s.forgeSources[src.ID] = &cp
	return nil
}

func environmentKey(orgID, name string) string {
	return fmt.Sprintf("%s/%s", orgID, name)
}

// workflowSlug normalizes a workflow name for use in a run ID.
func workflowSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

func workflowDefKey(orgID, projectID, name string) string {
	return fmt.Sprintf("%s/%s/%s", orgID, projectID, name)
}

func workflowCounterKey(orgID, projectID, name string) string {
	return fmt.Sprintf("%s/%s/%s", orgID, projectID, name)
}

// upsertEnvironment stores a protected environment.
func (s *fsmState) upsertEnvironment(env *domain.Environment) error {
	if env == nil || env.OrgID == "" || env.Name == "" {
		return fmt.Errorf("environment: %w", domain.ErrValidation)
	}
	cp := *env
	cp.ApproverGroupIDs = append([]string(nil), env.ApproverGroupIDs...)
	s.environments[environmentKey(env.OrgID, env.Name)] = &cp
	return nil
}

// upsertCheckRun stores a Cloud Checks run record.
func (s *fsmState) upsertCheckRun(rec *domain.CheckRunRecord) error {
	if rec == nil || rec.ID == "" {
		return fmt.Errorf("check run: %w", domain.ErrValidation)
	}
	cp := *rec
	cp.Checks = append([]domain.CheckItem(nil), rec.Checks...)
	cp.WorkflowFiles = copyStringMap(rec.WorkflowFiles)
	cp.ContextFiles = copyBytesMap(rec.ContextFiles)
	cp.ApprovedAt = copyTime(rec.ApprovedAt)
	s.checkRuns[rec.ID] = &cp
	return nil
}

// syncProjectWorkflowDefs replaces the indexed catalog for one project.
func (s *fsmState) syncProjectWorkflowDefs(orgID, projectID string, defs []*domain.WorkflowDef) error {
	prefix := fmt.Sprintf("%s/%s/", orgID, projectID)
	for key := range s.workflowDefs {
		if strings.HasPrefix(key, prefix) {
			delete(s.workflowDefs, key)
		}
	}
	for _, def := range defs {
		if def == nil || def.Workflow.Name == "" {
			continue
		}
		cp := *def
		cp.ContextFiles = copyBytesMap(def.ContextFiles)
		s.workflowDefs[workflowDefKey(orgID, projectID, def.Workflow.Name)] = &cp
	}
	return nil
}

// createWorkflowRun atomically assigns a monotonic run number and ID.
func (s *fsmState) createWorkflowRun(run *domain.WorkflowRun) (*domain.WorkflowRun, error) {
	if run == nil || run.OrgID == "" || run.ProjectID == "" || run.Workflow == "" {
		return nil, fmt.Errorf("workflow run: %w", domain.ErrValidation)
	}
	key := workflowCounterKey(run.OrgID, run.ProjectID, run.Workflow)
	s.workflowCounters[key]++
	n := s.workflowCounters[key]
	run.Number = n
	if run.ID == "" {
		run.ID = fmt.Sprintf("%s-%s-%d", run.ProjectID, workflowSlug(run.Workflow), n)
	}
	cp := *run
	cp.ContextFiles = copyBytesMap(run.ContextFiles)
	s.workflowRuns[run.ID] = &cp
	out := cp
	out.ContextFiles = copyBytesMap(run.ContextFiles)
	return &out, nil
}

// upsertWorkflowRun stores a workflow run without touching the counter.
func (s *fsmState) upsertWorkflowRun(run *domain.WorkflowRun) error {
	if run == nil || run.ID == "" {
		return fmt.Errorf("workflow run: %w", domain.ErrValidation)
	}
	cp := *run
	cp.ContextFiles = copyBytesMap(run.ContextFiles)
	s.workflowRuns[run.ID] = &cp
	return nil
}

func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyBytesMap(in map[string][]byte) map[string][]byte {
	if in == nil {
		return nil
	}
	out := make(map[string][]byte, len(in))
	for k, v := range in {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// storeOrg stores a deep copy of o and refreshes the name index. Orgs without
// an explicit feature list are backfilled with the platform defaults so a
// snapshot restore (or a pre-feature record) always reports CLOUD_CHECKS.
func (s *fsmState) storeOrg(o *domain.Org) {
	cp := *o
	cp.Features = append([]string(nil), o.Features...)
	if len(cp.Features) == 0 {
		cp.Features = append([]string(nil), domain.DefaultOrgFeatures...)
	}
	s.orgs[o.ID] = &cp
	s.orgsByName[strings.ToLower(o.Name)] = o.ID
}

// upsertOrg applies a create or update command.
func (s *fsmState) upsertOrg(o *domain.Org, create bool) error {
	if o == nil || o.ID == "" || o.Name == "" {
		return fmt.Errorf("org: %w", domain.ErrValidation)
	}
	if create {
		if _, ok := s.orgs[o.ID]; ok {
			return fmt.Errorf("org %s: %w", o.ID, domain.ErrConflict)
		}
		if _, ok := s.orgsByName[strings.ToLower(o.Name)]; ok {
			return fmt.Errorf("org %s: %w", o.Name, domain.ErrConflict)
		}
	} else {
		existing, ok := s.orgs[o.ID]
		if !ok {
			return fmt.Errorf("org %s: %w", o.ID, domain.ErrNotFound)
		}
		if existing.Name != o.Name {
			return fmt.Errorf("org %s: organization name cannot be changed: %w", o.ID, domain.ErrValidation)
		}
		if other, ok := s.orgsByName[strings.ToLower(o.Name)]; ok && other != o.ID {
			return fmt.Errorf("org %s: %w", o.Name, domain.ErrConflict)
		}
	}
	if len(o.Features) == 0 {
		o.Features = append([]string(nil), domain.DefaultOrgFeatures...)
	}
	s.storeOrg(o)
	return nil
}

// deleteOrg removes an empty organization and its direct memberships. Groups
// and projects are left intact (they retain their OrgID for audit).
func (s *fsmState) deleteOrg(id string) error {
	o, ok := s.orgs[id]
	if !ok {
		return fmt.Errorf("org %s: %w", id, domain.ErrNotFound)
	}
	if bootID := s.meta[domain.BootstrapOrgMetaKey]; bootID != "" && id == bootID {
		return fmt.Errorf("org %s: cannot delete bootstrap organization: %w", id, domain.ErrForbidden)
	}
	for _, p := range s.projects {
		if p.OrgID == id {
			return fmt.Errorf("org %s: %w", id, domain.ErrOrgNotEmpty)
		}
	}
	for _, g := range s.groups {
		if g.OrgID == id && !strings.EqualFold(g.Name, domain.DefaultOrgName) {
			return fmt.Errorf("org %s: %w", id, domain.ErrOrgNotEmpty)
		}
	}
	if defGroup, ok := s.defaultGroupInOrgLocked(id); ok {
		if err := s.deleteGroup(defGroup.ID); err != nil {
			return err
		}
	}
	s.removeOrgScopedStateLocked(id)
	delete(s.orgs, id)
	delete(s.orgsByName, strings.ToLower(o.Name))
	if s.sessionSink != nil {
		s.sessionSink.ApplyOrgDeleted(id)
	}
	return nil
}

// deleteOrgCascade atomically removes an organization and every child entity.
//
//nolint:gocyclo // cascade enumerates every org-scoped entity type
func (s *fsmState) deleteOrgCascade(id string) error {
	o, ok := s.orgs[id]
	if !ok {
		return fmt.Errorf("org %s: %w", id, domain.ErrNotFound)
	}
	if bootID := s.meta[domain.BootstrapOrgMetaKey]; bootID != "" && id == bootID {
		return fmt.Errorf("org %s: cannot delete bootstrap organization: %w", id, domain.ErrForbidden)
	}
	for pid, p := range s.projects {
		if p.OrgID != id {
			continue
		}
		delete(s.projects, pid)
		delete(s.projectsByOrgName, orgEntityNameKey(p.OrgID, p.Name))
	}
	for gid, g := range s.groups {
		if g.OrgID != id {
			continue
		}
		if err := s.deleteGroup(gid); err != nil {
			return err
		}
	}
	for tid, m := range s.traces {
		if m.OrgID == id {
			delete(s.traces, tid)
		}
	}
	for key, run := range s.workflowRuns {
		if run.OrgID == id {
			delete(s.workflowRuns, key)
		}
	}
	for key, def := range s.workflowDefs {
		if def.OrgID == id {
			delete(s.workflowDefs, key)
		}
	}
	for key := range s.workflowCounters {
		if strings.HasPrefix(key, fmt.Sprintf("%s/", id)) {
			delete(s.workflowCounters, key)
		}
	}
	for key, rec := range s.checkRuns {
		if rec.OrgID == id {
			delete(s.checkRuns, key)
		}
	}
	for key, env := range s.environments {
		if env.OrgID == id {
			delete(s.environments, key)
		}
	}
	for key, src := range s.forgeSources {
		if src.OrgID == id {
			delete(s.forgeSources, key)
		}
	}
	s.removeOrgScopedStateLocked(id)
	delete(s.orgs, id)
	delete(s.orgsByName, strings.ToLower(o.Name))
	if s.sessionSink != nil {
		s.sessionSink.ApplyOrgDeleted(id)
	}
	return nil
}

// removeOrgScopedStateLocked removes memberships, secrets, and the org token.
func (s *fsmState) removeOrgScopedStateLocked(id string) {
	for uid := range s.orgMembers[id] {
		delete(s.orgMembersByUser[uid], id)
		if len(s.orgMembersByUser[uid]) == 0 {
			delete(s.orgMembersByUser, uid)
		}
	}
	delete(s.orgMembers, id)
	delete(s.orgMemberSince, id)
	for key, sec := range s.secrets {
		if sec.Scope == domain.SecretScopeOrg && sec.ScopeID == id {
			delete(s.secrets, key)
		}
	}
	s.removeGitHubOrgScopedLocked(id)
	s.deleteToken(fmt.Sprintf("org:%s", id))
}

// defaultGroupInOrgLocked returns the org's "default" group.
func (s *fsmState) defaultGroupInOrgLocked(orgID string) (*domain.Group, bool) {
	gid, ok := s.groupsByOrgName[orgEntityNameKey(orgID, domain.DefaultOrgName)]
	if !ok {
		return nil, false
	}
	g, ok := s.groups[gid]
	return g, ok
}

// applyBootstrapOrg atomically commits the bootstrap organization UID and
// normalizes legacy rows. It is idempotent across duplicate proposals.
func (s *fsmState) applyBootstrapOrg(p *cmdBootstrapOrg) (*domain.Org, error) {
	if existingID := s.meta[domain.BootstrapOrgMetaKey]; existingID != "" {
		if o, ok := s.orgs[existingID]; ok {
			return copyOrg(o), nil
		}
		return nil, fmt.Errorf("bootstrap org %s: %w", existingID, domain.ErrNotFound)
	}
	if p == nil || p.Org.ID == "" || p.Org.Name != domain.DefaultOrgName {
		return nil, fmt.Errorf("bootstrap org: %w", domain.ErrValidation)
	}
	s.meta[domain.BootstrapOrgMetaKey] = p.Org.ID
	if p.DisplayNameMeta != "" {
		s.meta[domain.DefaultOrgDisplayNameMetaKey] = p.DisplayNameMeta
	}
	now := p.Org.CreatedAt
	if now.IsZero() {
		now = p.Org.UpdatedAt
	}
	o := p.Org
	o.Name = domain.DefaultOrgName
	if o.DisplayName == "" {
		o.DisplayName = domain.DefaultOrgDisplayName
	}
	if len(o.Features) == 0 {
		o.Features = append([]string(nil), domain.DefaultOrgFeatures...)
	}
	s.storeOrg(&o)
	s.normalizeLegacyRowsLocked(p.Org.ID, p.DefaultGroupID, now)
	if s.metaHook != nil {
		s.metaHook(domain.BootstrapOrgMetaKey, p.Org.ID)
		if p.DisplayNameMeta != "" {
			s.metaHook(domain.DefaultOrgDisplayNameMetaKey, p.DisplayNameMeta)
		}
	}
	return copyOrg(s.orgs[p.Org.ID]), nil
}

// normalizeLegacyRowsLocked migrates every pre-bootstrap row (empty OrgID) into
// bootstrapID, rebuilds composite indices, ensures the bootstrap "default"
// group, and backfills org membership.
//
//nolint:gocyclo // linear migration of every legacy entity type
func (s *fsmState) normalizeLegacyRowsLocked(bootstrapID, defaultGroupID string, now time.Time) {
	if bootstrapID == "" {
		return
	}
	s.groupsByOrgName = make(map[string]string, len(s.groups))
	for id, g := range s.groups {
		if g.OrgID == "" {
			g.OrgID = bootstrapID
		}
		_ = id
		s.groupsByOrgName[orgEntityNameKey(g.OrgID, g.Name)] = g.ID
	}
	s.projectsByOrgName = make(map[string]string, len(s.projects))
	for id, p := range s.projects {
		if p.OrgID == "" {
			p.OrgID = bootstrapID
		}
		_ = id
		s.projectsByOrgName[orgEntityNameKey(p.OrgID, p.Name)] = p.ID
	}
	for _, t := range s.traces {
		if t.OrgID == "" {
			t.OrgID = bootstrapID
		}
	}
	for _, src := range s.forgeSources {
		if src.OrgID == "" {
			src.OrgID = bootstrapID
		}
	}
	for _, rec := range s.checkRuns {
		if rec.OrgID == "" {
			rec.OrgID = bootstrapID
		}
	}
	for _, env := range s.environments {
		if env.OrgID == "" {
			env.OrgID = bootstrapID
		}
	}
	for key, def := range s.workflowDefs {
		if def.OrgID == "" {
			def.OrgID = bootstrapID
			delete(s.workflowDefs, key)
			s.workflowDefs[workflowDefKey(def.OrgID, def.ProjectID, def.Workflow.Name)] = def
		}
	}
	for _, run := range s.workflowRuns {
		if run.OrgID == "" {
			run.OrgID = bootstrapID
		}
	}
	for key, val := range s.workflowCounters {
		if strings.HasPrefix(key, "/") {
			delete(s.workflowCounters, key)
			s.workflowCounters[fmt.Sprintf("%s%s", bootstrapID, key)] = val
		}
	}
	for key, sec := range s.secrets {
		if sec.Scope == domain.SecretScopeOrg && sec.ScopeID == "" {
			delete(s.secrets, key)
			sec.ScopeID = bootstrapID
			s.secrets[secretKey(sec.Scope, sec.ScopeID, sec.Name)] = sec
		}
	}
	if _, ok := s.defaultGroupInOrgLocked(bootstrapID); !ok && defaultGroupID != "" {
		g := &domain.Group{
			ID:             defaultGroupID,
			OrgID:          bootstrapID,
			Name:           domain.DefaultOrgName,
			AgentAvailable: true,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		s.storeGroup(g)
	}
	// Every group member is an org member.
	for gid, set := range s.memberships {
		g, ok := s.groups[gid]
		if !ok || g.OrgID == "" {
			continue
		}
		if s.orgMembers[g.OrgID] == nil {
			s.orgMembers[g.OrgID] = make(map[string]domain.OrgRole)
		}
		for uid := range set {
			if _, exists := s.orgMembers[g.OrgID][uid]; !exists {
				s.orgMembers[g.OrgID][uid] = domain.OrgMember
			}
			if s.orgMembersByUser[uid] == nil {
				s.orgMembersByUser[uid] = make(map[string]struct{})
			}
			s.orgMembersByUser[uid][g.OrgID] = struct{}{}
		}
	}
	// Active admins with no org membership join the bootstrap org as admins.
	for uid, u := range s.users {
		if u.Role != domain.RoleAdmin || u.AccessBlocked() {
			continue
		}
		if len(s.orgMembersByUser[uid]) > 0 {
			continue
		}
		if s.orgMembers[bootstrapID] == nil {
			s.orgMembers[bootstrapID] = make(map[string]domain.OrgRole)
		}
		s.orgMembers[bootstrapID][uid] = domain.OrgAdmin
		if s.orgMembersByUser[uid] == nil {
			s.orgMembersByUser[uid] = make(map[string]struct{})
		}
		s.orgMembersByUser[uid][bootstrapID] = struct{}{}
	}
	if s.sessionSink != nil {
		s.sessionSink.ApplyBootstrapOrg(bootstrapID)
	}
	s.backfillOrgMemberSinceLocked(now)
}

// orgUsageLocked counts live usage of one quota resource.
func (s *fsmState) orgUsageLocked(orgID string, res domain.QuotaResource) int64 {
	switch res {
	case domain.QuotaGroups:
		var n int64
		for _, g := range s.groups {
			if g.OrgID == orgID {
				n++
			}
		}
		return n
	case domain.QuotaProjects:
		var n int64
		for _, p := range s.projects {
			if p.OrgID == orgID {
				n++
			}
		}
		return n
	case domain.QuotaMembers:
		return int64(s.distinctMemberCountLocked(orgID))
	}
	return 0
}

// checkOrgQuotaLocked enforces an org hard cap deterministically. 0 = unlimited.
func (s *fsmState) checkOrgQuotaLocked(orgID string, res domain.QuotaResource, delta int64) error {
	o, ok := s.orgs[orgID]
	if !ok {
		return nil
	}
	eff := o.Quotas.Effective(s.quotaDefaults)
	var hard int64
	switch res {
	case domain.QuotaGroups:
		hard = eff.MaxGroups.Hard
	case domain.QuotaProjects:
		hard = eff.MaxProjects.Hard
	case domain.QuotaMembers:
		hard = eff.MaxMembers.Hard
	default:
		return nil
	}
	if hard <= 0 {
		return nil
	}
	if s.orgUsageLocked(orgID, res)+delta > hard {
		return fmt.Errorf("%w: %s limit (%d) reached", domain.ErrOrgQuotaExceeded, res, hard)
	}
	return nil
}

// setOrgMember adds or updates a direct org membership. A brand-new membership
// records p.At as its creation time; a role change preserves the original one.
func (s *fsmState) setOrgMember(p cmdOrgMember) error {
	if _, ok := s.orgs[p.OrgID]; !ok {
		return fmt.Errorf("org %s: %w", p.OrgID, domain.ErrNotFound)
	}
	if _, ok := s.users[p.UserID]; !ok {
		return fmt.Errorf("user %s: %w", p.UserID, domain.ErrNotFound)
	}
	if p.Role != domain.OrgAdmin && p.Role != domain.OrgMember {
		return fmt.Errorf("org role %q: %w", p.Role, domain.ErrValidation)
	}
	if _, exists := s.orgMembers[p.OrgID][p.UserID]; !exists {
		if err := s.checkOrgQuotaLocked(p.OrgID, domain.QuotaMembers, 1); err != nil {
			return err
		}
		s.setOrgMemberSinceLocked(p.OrgID, p.UserID, p.At)
	}
	if s.orgMembers[p.OrgID] == nil {
		s.orgMembers[p.OrgID] = make(map[string]domain.OrgRole)
	}
	s.orgMembers[p.OrgID][p.UserID] = p.Role
	if s.orgMembersByUser[p.UserID] == nil {
		s.orgMembersByUser[p.UserID] = make(map[string]struct{})
	}
	s.orgMembersByUser[p.UserID][p.OrgID] = struct{}{}
	return nil
}

// setOrgMemberSinceLocked records a membership creation time, ignoring an
// unset timestamp (a legacy command replayed without one).
func (s *fsmState) setOrgMemberSinceLocked(orgID, userID string, at time.Time) {
	if at.IsZero() {
		return
	}
	if s.orgMemberSince[orgID] == nil {
		s.orgMemberSince[orgID] = make(map[string]time.Time)
	}
	s.orgMemberSince[orgID][userID] = at
}

// backfillOrgMemberSinceLocked assigns the best-known lower bound (the
// bootstrap organization's CreatedAt) to every direct membership with no
// recorded timestamp. Called on bootstrap normalization and snapshot restore,
// i.e. exactly when pre-org_member_since state is decoded.
func (s *fsmState) backfillOrgMemberSinceLocked(fallback time.Time) {
	if fallback.IsZero() {
		if boot, ok := s.orgs[s.meta[domain.BootstrapOrgMetaKey]]; ok {
			fallback = boot.CreatedAt
		}
	}
	if fallback.IsZero() {
		return
	}
	for orgID, members := range s.orgMembers {
		for userID := range members {
			if _, known := s.orgMemberSince[orgID][userID]; known {
				continue
			}
			s.setOrgMemberSinceLocked(orgID, userID, fallback)
		}
	}
}

// removeOrgMember deletes a direct org membership (idempotent).
func (s *fsmState) removeOrgMember(orgID, userID string) error {
	delete(s.orgMembers[orgID], userID)
	if len(s.orgMembers[orgID]) == 0 {
		delete(s.orgMembers, orgID)
	}
	delete(s.orgMemberSince[orgID], userID)
	if len(s.orgMemberSince[orgID]) == 0 {
		delete(s.orgMemberSince, orgID)
	}
	delete(s.orgMembersByUser[userID], orgID)
	if len(s.orgMembersByUser[userID]) == 0 {
		delete(s.orgMembersByUser, userID)
	}
	// Cascade ONLY this org's group memberships.
	for gid := range s.membershipsByUser[userID] {
		g, ok := s.groups[gid]
		if !ok || g.OrgID != orgID {
			continue
		}
		delete(s.memberships[gid], userID)
		if len(s.memberships[gid]) == 0 {
			delete(s.memberships, gid)
		}
		delete(s.membershipsByUser[userID], gid)
		s.bumpRevision(userID)
	}
	if len(s.membershipsByUser[userID]) == 0 {
		delete(s.membershipsByUser, userID)
	}
	if s.sessionSink != nil {
		s.sessionSink.ApplyUserOrgRevoked(orgID, userID)
	}
	return nil
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
	s.deleteGitHubConnection(id)
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
	s.removeUserOrgMembershipsLocked(id)
	delete(s.users, id)
	delete(s.usersByName, strings.ToLower(u.Username))
	if u.OAuthID != "" {
		delete(s.usersByOAuth, oauthKey(u.OAuthProvider, u.OAuthID))
	}
	return nil
}

// removeUserOrgMembershipsLocked deletes every org membership for userID and
// evicts that user's leases in each affected org on every pod.
func (s *fsmState) removeUserOrgMembershipsLocked(userID string) {
	for oid := range s.orgMembersByUser[userID] {
		delete(s.orgMembers[oid], userID)
		if len(s.orgMembers[oid]) == 0 {
			delete(s.orgMembers, oid)
		}
		delete(s.orgMemberSince[oid], userID)
		if len(s.orgMemberSince[oid]) == 0 {
			delete(s.orgMemberSince, oid)
		}
		if s.sessionSink != nil {
			s.sessionSink.ApplyUserOrgRevoked(oid, userID)
		}
	}
	delete(s.orgMembersByUser, userID)
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
	s.removeUserOrgMembershipsLocked(p.ID)
	s.deleteGitHubConnection(p.ID)
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
	if err := s.enforceManagedMembershipQuota(u.ID, p.ManagedGroupIDs); err != nil {
		return nil, err
	}
	s.applyManagedMemberships(u, p.ManagedGroupIDs, p.At)
	s.storeUser(u)
	return s.oauthResult(u, p.FallbackGroupID, p.At), nil
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
	if err := s.enforceManagedMembershipQuota(u.ID, p.ManagedGroupIDs); err != nil {
		return nil, err
	}
	s.applyManagedMemberships(u, p.ManagedGroupIDs, p.At)
	s.storeUser(u)
	return s.oauthResult(u, "", p.At), nil
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
	return s.oauthResult(u, "", p.At), nil
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
	return s.oauthResult(u, "", p.At), nil
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

// enforceManagedMembershipQuota rejects an OAuth membership mutation that would
// add the user as a new distinct member of an org already at its max_members
// hard cap. Membership in other orgs and role changes (delta == 0) are never
// blocked.
func (s *fsmState) enforceManagedMembershipQuota(userID string, desired []string) error {
	newByOrg := make(map[string]struct{})
	for _, gid := range desired {
		g, ok := s.groups[gid]
		if !ok || g.OrgID == "" {
			continue
		}
		if s.isOrgMemberLocked(g.OrgID, userID) {
			continue
		}
		newByOrg[g.OrgID] = struct{}{}
	}
	for orgID := range newByOrg {
		if err := s.checkOrgQuotaLocked(orgID, domain.QuotaMembers, 1); err != nil {
			return err
		}
	}
	return nil
}

// isOrgMemberLocked reports whether userID is already a distinct member of
// orgID (direct org membership or any group membership in orgID).
func (s *fsmState) isOrgMemberLocked(orgID, userID string) bool {
	if _, ok := s.orgMembers[orgID][userID]; ok {
		return true
	}
	for gid, set := range s.memberships {
		g, ok := s.groups[gid]
		if !ok || g.OrgID != orgID {
			continue
		}
		if _, ok := set[userID]; ok {
			return true
		}
	}
	return false
}

// applyManagedMemberships replaces only the OAuth-managed membership edges on
// u.OAuthGroupIDs, leaving manual memberships untouched. A desired group that
// is already a member (manual or managed) is retained; it is simply claimed as
// OAuth-managed.
func (s *fsmState) applyManagedMemberships(u *domain.User, desired []string, at time.Time) {
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
		s.addMembership(gid, u.ID, at)
	}
	u.OAuthGroupIDs = normalizeIDs(desired)
}

// oauthResult builds the committed response for an OAuth mutation, optionally
// applying the fallback group when the effective membership is still empty.
func (s *fsmState) oauthResult(u *domain.User, fallbackGroupID string, at time.Time) *domain.OAuthUserResult {
	if fallbackGroupID != "" && len(s.membershipsByUser[u.ID]) == 0 {
		s.addMembership(fallbackGroupID, u.ID, at)
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
	if g == nil {
		return fmt.Errorf("group: %w", domain.ErrValidation)
	}
	bootstrapped := s.meta[domain.BootstrapOrgMetaKey] != ""
	if bootstrapped {
		if g.OrgID == "" {
			return fmt.Errorf("org_id is required: %w", domain.ErrValidation)
		}
		if _, ok := s.orgs[g.OrgID]; !ok {
			return fmt.Errorf("org %s: %w", g.OrgID, domain.ErrNotFound)
		}
	}
	lower := orgEntityNameKey(g.OrgID, g.Name)
	if existing, ok := s.groups[g.ID]; ok {
		if create {
			return fmt.Errorf("group %s: %w", g.ID, domain.ErrConflict)
		}
		if existing.OrgID != g.OrgID {
			return fmt.Errorf("group %s: org_id cannot be changed: %w", g.ID, domain.ErrValidation)
		}
		if other, ok := s.groupsByOrgName[lower]; ok && other != g.ID {
			return fmt.Errorf("group %s: %w", g.Name, domain.ErrConflict)
		}
		g.CreatedAt = existing.CreatedAt
		if g.UpdatedAt.IsZero() {
			g.UpdatedAt = existing.UpdatedAt
		}
		delete(s.groupsByOrgName, orgEntityNameKey(existing.OrgID, existing.Name))
		s.storeGroup(g)
		return nil
	}
	if !create {
		return fmt.Errorf("update group %s: %w", g.ID, domain.ErrNotFound)
	}
	if _, ok := s.groupsByOrgName[lower]; ok {
		return fmt.Errorf("group %s: %w", g.Name, domain.ErrConflict)
	}
	if bootstrapped {
		if err := s.checkOrgQuotaLocked(g.OrgID, domain.QuotaGroups, 1); err != nil {
			return err
		}
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
	delete(s.groupsByOrgName, orgEntityNameKey(g.OrgID, g.Name))
	return nil
}

func (s *fsmState) setMembers(groupID string, userIDs []string, at time.Time) error {
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
	newMembers := int64(0)
	orgID := ""
	if g, ok := s.groups[groupID]; ok {
		orgID = g.OrgID
	}
	for _, uid := range userIDs {
		set[uid] = struct{}{}
		affected[uid] = struct{}{}
		if orgID != "" {
			if _, already := s.orgMembers[orgID][uid]; !already {
				newMembers++
			}
		}
	}
	if orgID != "" && newMembers > 0 {
		if err := s.checkOrgQuotaLocked(orgID, domain.QuotaMembers, newMembers); err != nil {
			return err
		}
	}
	if len(set) == 0 {
		delete(s.memberships, groupID)
	} else {
		s.memberships[groupID] = set
		for uid := range set {
			s.addMembership(groupID, uid, at)
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

// addMembership records one (groupID, userID) edge in both directions. At is
// the command timestamp supplied by the leader; a group membership implies a
// direct org membership, whose creation time is recorded only when new.
func (s *fsmState) addMembership(groupID, userID string, at time.Time) {
	if s.memberships[groupID] == nil {
		s.memberships[groupID] = make(map[string]struct{})
	}
	s.memberships[groupID][userID] = struct{}{}
	if s.membershipsByUser[userID] == nil {
		s.membershipsByUser[userID] = make(map[string]struct{})
	}
	s.membershipsByUser[userID][groupID] = struct{}{}
	if g, ok := s.groups[groupID]; ok && g.OrgID != "" {
		if s.orgMembers[g.OrgID] == nil {
			s.orgMembers[g.OrgID] = make(map[string]domain.OrgRole)
		}
		if _, exists := s.orgMembers[g.OrgID][userID]; !exists {
			s.orgMembers[g.OrgID][userID] = domain.OrgMember
			s.setOrgMemberSinceLocked(g.OrgID, userID, at)
		}
		if s.orgMembersByUser[userID] == nil {
			s.orgMembersByUser[userID] = make(map[string]struct{})
		}
		s.orgMembersByUser[userID][g.OrgID] = struct{}{}
	}
}

// --- projects ---------------------------------------------------------------

func (s *fsmState) upsertProject(p *domain.Project, create bool) error {
	if p == nil {
		return fmt.Errorf("project: %w", domain.ErrValidation)
	}
	bootstrapped := s.meta[domain.BootstrapOrgMetaKey] != ""
	if bootstrapped {
		if p.OrgID == "" {
			return fmt.Errorf("org_id is required: %w", domain.ErrValidation)
		}
		if _, ok := s.orgs[p.OrgID]; !ok {
			return fmt.Errorf("org %s: %w", p.OrgID, domain.ErrNotFound)
		}
	}
	if p.GroupID != "" {
		grp, ok := s.groups[p.GroupID]
		if !ok {
			return fmt.Errorf("group %s: %w", p.GroupID, domain.ErrNotFound)
		}
		if grp.OrgID != p.OrgID {
			return fmt.Errorf("group belongs to another organization: %w", domain.ErrValidation)
		}
	}
	lower := orgEntityNameKey(p.OrgID, p.Name)
	existing, ok := s.projects[p.ID]
	if !ok {
		if !create {
			return fmt.Errorf("update project %s: %w", p.ID, domain.ErrNotFound)
		}
		if _, dup := s.projectsByOrgName[lower]; dup {
			return fmt.Errorf("project %s: %w", p.Name, domain.ErrConflict)
		}
		if bootstrapped {
			if err := s.checkOrgQuotaLocked(p.OrgID, domain.QuotaProjects, 1); err != nil {
				return err
			}
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
	if existing.OrgID != p.OrgID {
		return fmt.Errorf("project %s: org_id cannot be changed: %w", p.ID, domain.ErrValidation)
	}
	if other, dup := s.projectsByOrgName[lower]; dup && other != p.ID {
		return fmt.Errorf("project %s: %w", p.Name, domain.ErrConflict)
	}
	p.CreatedAt = existing.CreatedAt
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = existing.UpdatedAt
	}
	delete(s.projectsByOrgName, orgEntityNameKey(existing.OrgID, existing.Name))
	s.storeProject(p)
	return nil
}

func (s *fsmState) deleteProject(id string) error {
	p, ok := s.projects[id]
	if !ok {
		return fmt.Errorf("delete project %s: %w", id, domain.ErrNotFound)
	}
	delete(s.projects, id)
	delete(s.projectsByOrgName, orgEntityNameKey(p.OrgID, p.Name))
	return nil
}

// --- tokens -----------------------------------------------------------------

func (s *fsmState) upsertToken(t *domain.APIToken) error {
	// Org-scoped dag_<org>_ tokens use a synthetic "org:<orgID>" subject that
	// has no users-table row. Validate the org exists instead of requiring a
	// real user owner.
	if strings.HasPrefix(t.UserID, "org:") {
		orgID := strings.TrimPrefix(t.UserID, "org:")
		if _, ok := s.orgs[orgID]; !ok {
			return fmt.Errorf("api token org %s: %w", orgID, domain.ErrNotFound)
		}
	} else {
		// The owning user must exist and not be blocked before any old token is
		// removed or replaced. A blocked owner yields no token side effect.
		owner, ok := s.users[t.UserID]
		if !ok {
			return fmt.Errorf("api token owner %s: %w", t.UserID, domain.ErrNotFound)
		}
		if owner.DeletedAt != nil || owner.DisabledAt != nil {
			return fmt.Errorf("api token owner %s: %w", t.UserID, domain.ErrAccountBlocked)
		}
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

//nolint:gocritic // command payloads are value-semantic
func (s *fsmState) upsertTraceProvision(p cmdUpsertTraceProvision) {
	existing, ok := s.traces[p.TraceID]
	if !ok {
		s.traces[p.TraceID] = &domain.TraceMeta{
			TraceID:   p.TraceID,
			OrgID:     p.OrgID,
			UserID:    p.UserID,
			Version:   p.Version,
			UpdatedAt: p.UpdatedAt,
		}
		return
	}
	// user_id/org_id: first non-empty wins; version: new non-empty wins.
	if existing.UserID == "" {
		existing.UserID = p.UserID
	}
	if existing.OrgID == "" && p.OrgID != "" {
		existing.OrgID = p.OrgID
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

func (f *FSM) readGroupByName(orgID, name string) (*domain.Group, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	id, ok := f.state.groupsByOrgName[orgEntityNameKey(orgID, name)]
	if !ok {
		return nil, fmt.Errorf("group %s: %w", name, domain.ErrNotFound)
	}
	g := f.state.groups[id]
	cp := *g
	return &cp, nil
}

// listGroupsByOrg returns orgID's groups sorted by name.
func (f *FSM) listGroupsByOrg(orgID string) []*domain.Group {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	out := make([]*domain.Group, 0)
	for _, g := range f.state.groups {
		if g.OrgID == orgID {
			cp := *g
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// groupsForUserInOrg returns userID's groups inside orgID.
func (f *FSM) groupsForUserInOrg(orgID, userID string) []*domain.Group {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	out := make([]*domain.Group, 0)
	for gid := range f.state.membershipsByUser[userID] {
		if g, ok := f.state.groups[gid]; ok && g.OrgID == orgID {
			cp := *g
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
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

func (f *FSM) readProjectByName(orgID, name string) (*domain.Project, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	id, ok := f.state.projectsByOrgName[orgEntityNameKey(orgID, name)]
	if !ok {
		return nil, fmt.Errorf("project %s: %w", name, domain.ErrNotFound)
	}
	p := f.state.projects[id]
	cp := *p
	return &cp, nil
}

// listProjectsByOrg returns orgID's projects.
func (f *FSM) listProjectsByOrg(orgID string) []*domain.Project {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	out := make([]*domain.Project, 0)
	for _, p := range f.state.projects {
		if p.OrgID == orgID {
			cp := *p
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// listProjectsByNameAcrossOrgs returns every project named name (case
// insensitive) across all organizations.
func (f *FSM) listProjectsByNameAcrossOrgs(name string) []*domain.Project {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	out := make([]*domain.Project, 0)
	for _, p := range f.state.projects {
		if strings.EqualFold(p.Name, name) {
			cp := *p
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OrgID == out[j].OrgID {
			return out[i].ID < out[j].ID
		}
		return out[i].OrgID < out[j].OrgID
	})
	return out
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
func (f *FSM) traceStats(orgID string) (int, time.Time) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()
	var oldest time.Time
	count := 0
	for _, m := range f.state.traces {
		if orgID != "" && m.OrgID != orgID {
			continue
		}
		count++
		k := traceSortKey(m)
		if k.IsZero() {
			continue // unknown age never counts as the oldest
		}
		if oldest.IsZero() || k.Before(oldest) {
			oldest = k
		}
	}
	return count, oldest
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
	if f.OrgID != "" && m.OrgID != "" && m.OrgID != f.OrgID {
		return false
	}
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
