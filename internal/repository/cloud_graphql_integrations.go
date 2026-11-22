package repository

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/graph-gophers/graphql-go"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// --- Family E: Cloud Checks -------------------------------------------------

const (
	maxCheckIDs          = 500
	defaultCheckPageSize = 100
)

// canonicalCheckRepo normalizes a forge source or project repo slug into a
// canonical `host/owner/name` form.
func canonicalCheckRepo(provider domain.ForgeKind, repo string) string {
	repo = strings.TrimSuffix(strings.TrimPrefix(repo, "https://"), ".git")
	host := ""
	switch provider {
	case domain.ForgeGitHub:
		host = "github.com/"
	case domain.ForgeGitLab:
		host = "gitlab.com/"
	}
	if host == "" || strings.Count(repo, "/") >= 2 {
		return repo
	}
	return host + repo
}

// canonicalModuleRef normalizes a module reference (optionally scheme- and
// host-qualified) into a canonical repo slug.
func canonicalModuleRef(ref string) string {
	if !strings.Contains(ref, "://") {
		return canonicalCheckRepo("", ref)
	}
	_, rest, _ := strings.Cut(ref, "://")
	return strings.TrimSuffix(rest, ".git")
}

// canonicalCheckModuleRef normalizes a check run's repo into a canonical repo
// slug.
func canonicalCheckModuleRef(rec *domain.CheckRunRecord) string {
	ref := canonicalModuleRef(rec.Repo)
	if ref == "" {
		return ""
	}
	if strings.Count(ref, "/") < 2 {
		return canonicalCheckRepo(rec.Provider, ref)
	}
	return ref
}

// checkCommitFromRecord maps a CheckRunRecord into a CheckCommit.
func (r *graphqlRoot) checkCommitFromRecord(org *domain.Org, rec *domain.CheckRunRecord) *graphqlCheckCommit {
	commit := &graphqlCheckCommit{
		repo:          canonicalCheckModuleRef(rec),
		commitSHA:     rec.SHA,
		timestamp:     graphqlTime{time: rec.CreatedAt},
		org:           &graphqlCheckCommitOrg{id: rec.OrgID, name: org.Name},
		events:        []*graphqlCheckEvent{},
		refs:          []*graphqlCheckCommitRef{},
		checks:        []*graphqlCheck{},
		commitMessage: "",
		authorName:    "",
		authorEmail:   "",
	}
	if rec.Provider != "" {
		commit.events = append(commit.events, &graphqlCheckEvent{
			provider:  strings.ToUpper(string(rec.Provider)),
			eventType: rec.Event,
			timestamp: graphqlTime{time: rec.CreatedAt},
		})
	}
	commit.refs = checkCommitRefs(rec)

	if len(rec.Checks) == 0 {
		commit.checks = append(commit.checks, synthesizedLoadCheck(rec))
		return commit
	}
	for i := range rec.Checks {
		commit.checks = append(commit.checks, checkFromItem(rec, &rec.Checks[i]))
	}
	return commit
}

func checkCommitRefs(rec *domain.CheckRunRecord) []*graphqlCheckCommitRef {
	switch {
	case rec.PRNumber > 0:
		return []*graphqlCheckCommitRef{{
			pr: &graphqlCheckCommitPullRequestRef{number: clampInt32(rec.PRNumber), state: "open", integrationCommit: rec.SHA},
		}}
	case rec.Tag != "" || strings.HasPrefix(rec.Ref, "refs/tags/"):
		name := rec.Tag
		if name == "" {
			name = strings.TrimPrefix(rec.Ref, "refs/tags/")
		}
		return []*graphqlCheckCommitRef{{tag: &graphqlCheckCommitTagRef{name: name}}}
	case strings.HasPrefix(rec.Ref, "refs/heads/") || rec.Ref != "":
		name := strings.TrimPrefix(rec.Ref, "refs/heads/")
		return []*graphqlCheckCommitRef{{branch: &graphqlCheckCommitBranchRef{name: name}}}
	default:
		return []*graphqlCheckCommitRef{}
	}
}

func fourByteOrZero(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// clampInt32 converts a non-negative n to int32, saturating at the int32
// maximum (callers pass PR numbers, which are always >= 0).
func clampInt32(n int) int32 {
	const maxInt32 = int(^uint32(0) >> 1)
	if n > maxInt32 || n < 0 {
		return int32(^uint32(0) >> 1)
	}
	return int32(n)
}

func isTerminalCheckState(state domain.CheckState) bool {
	switch state {
	case domain.CheckSuccess, domain.CheckFailure, domain.CheckError:
		return true
	default:
		return false
	}
}

func checkFromItem(rec *domain.CheckRunRecord, item *domain.CheckItem) *graphqlCheck {
	c := &graphqlCheck{
		id:            fmt.Sprintf("%s/%s", rec.ID, item.Name),
		name:          item.Name,
		status:        string(item.State),
		traceID:       item.TraceID,
		moduleRef:     canonicalCheckModuleRef(rec),
		moduleVersion: rec.SHA,
	}
	if !rec.CreatedAt.IsZero() {
		c.startedAt = graphqlTime{time: rec.CreatedAt}
	}
	if isTerminalCheckState(item.State) {
		if end := fourByteOrZero(item.UpdatedAt); end != nil {
			c.endTime = &graphqlTime{time: *end}
			if !rec.CreatedAt.IsZero() {
				d := int32(item.UpdatedAt.Sub(rec.CreatedAt).Seconds())
				if d >= 0 {
					c.duration = &d
				}
			}
		}
	}
	return c
}

// synthesizedLoadCheck renders a single internal "load" check when a run has no
// discovered check items yet.
func synthesizedLoadCheck(rec *domain.CheckRunRecord) *graphqlCheck {
	status := "pending"
	switch rec.Status {
	case "success":
		status = "success"
	case "failed", "canceled":
		status = "error"
	}
	c := &graphqlCheck{
		id:            fmt.Sprintf("%s/load", rec.ID),
		name:          "load",
		status:        status,
		moduleRef:     canonicalCheckModuleRef(rec),
		moduleVersion: rec.SHA,
		internal:      true,
	}
	if !rec.CreatedAt.IsZero() {
		c.startedAt = graphqlTime{time: rec.CreatedAt}
	}
	return c
}

// Org.Checks returns a connection of check commits for the org, optionally
// filtered by repo.
func (o *graphqlOrg) Checks(ctx context.Context, args struct {
	Repos *[]string
	First *int32
}) (*graphqlCheckCommitConnection, error) {
	if o.root.checks == nil {
		return &graphqlCheckCommitConnection{nodes: []*graphqlCheckCommit{}}, nil
	}
	limit := defaultCheckPageSize
	if args.First != nil && *args.First > 0 {
		limit = int(*args.First)
	}
	if limit > domain.MaxTraceLimit {
		limit = domain.MaxTraceLimit
	}
	rows, err := o.root.checks.List(ctx, &domain.CheckRunFilter{OrgID: o.org.ID, Limit: limit})
	if err != nil {
		return nil, err
	}
	nodes := make([]*graphqlCheckCommit, 0, len(rows))
	for _, rec := range rows {
		if args.Repos != nil && len(*args.Repos) > 0 && !repoFilterMatches(*args.Repos, rec.Repo) {
			continue
		}
		nodes = append(nodes, o.root.checkCommitFromRecord(o.org, rec))
	}
	return &graphqlCheckCommitConnection{nodes: nodes}, nil
}

// User.Checks returns checks across the caller's accessible orgs.
func (u *graphqlUser) Checks(ctx context.Context, args struct {
	Repos *[]string
	First *int32
}) (*graphqlCheckCommitConnection, error) {
	orgs, err := u.root.accessibleOrgs(ctx)
	if err != nil {
		return nil, err
	}
	limit := defaultCheckPageSize
	if args.First != nil && *args.First > 0 {
		limit = int(*args.First)
	}
	if limit > domain.MaxTraceLimit {
		limit = domain.MaxTraceLimit
	}
	nodes := make([]*graphqlCheckCommit, 0)
	for _, org := range orgs {
		if u.root.checks == nil || len(nodes) >= limit {
			break
		}
		rows, err := u.root.checks.List(ctx, &domain.CheckRunFilter{OrgID: org.ID, Limit: limit})
		if err != nil {
			return nil, err
		}
		for _, rec := range rows {
			if len(nodes) >= limit {
				break
			}
			if args.Repos != nil && len(*args.Repos) > 0 && !repoFilterMatches(*args.Repos, rec.Repo) {
				continue
			}
			nodes = append(nodes, u.root.checkCommitFromRecord(org, rec))
		}
	}
	return &graphqlCheckCommitConnection{nodes: nodes}, nil
}

// Org.ModuleChecks returns check commits matching a module ref + version.
func (o *graphqlOrg) ModuleChecks(ctx context.Context, args struct {
	ModuleRef     string
	ModuleVersion string
}) ([]*graphqlCheckCommit, error) {
	if o.root.checks == nil {
		return []*graphqlCheckCommit{}, nil
	}
	target := canonicalModuleRef(args.ModuleRef)
	rows, err := o.root.checks.List(ctx, &domain.CheckRunFilter{OrgID: o.org.ID, Limit: domain.MaxTraceLimit})
	if err != nil {
		return nil, err
	}
	out := make([]*graphqlCheckCommit, 0)
	for _, rec := range rows {
		if !sameNormalizedRepo(canonicalCheckModuleRef(rec), target) {
			continue
		}
		if args.ModuleVersion != "" && !moduleVersionMatches(args.ModuleVersion, rec) {
			continue
		}
		out = append(out, o.root.checkCommitFromRecord(o.org, rec))
	}
	return out, nil
}

func moduleVersionMatches(version string, rec *domain.CheckRunRecord) bool {
	if rec.SHA == version || strings.HasPrefix(rec.SHA, version) {
		return true
	}
	ref := strings.TrimPrefix(rec.Ref, "refs/heads/")
	ref = strings.TrimPrefix(ref, "refs/tags/")
	return ref == version || rec.Tag == version
}

// RerunChecks re-dispatches the runs referenced by checkIds.
func (r *graphqlRoot) RerunChecks(ctx context.Context, args struct {
	Org        graphql.ID
	CheckIds   []graphql.ID
	CleanSlate *bool
}) ([]*graphqlCheck, error) {
	org, err := r.resolveAccessibleOrg(ctx, string(args.Org))
	if err != nil {
		return nil, err
	}
	if len(args.CheckIds) > maxCheckIDs {
		return nil, fmt.Errorf("too many check IDs: %d exceeds %d", len(args.CheckIds), maxCheckIDs)
	}
	if r.checkRerunner == nil {
		return nil, fmt.Errorf("check rerun is not available on this self-hosted deployment (checks.enabled is false)")
	}
	seen := make(map[string]bool)
	out := make([]*graphqlCheck, 0, len(args.CheckIds))
	for _, raw := range args.CheckIds {
		runID, checkName := splitCheckID(string(raw))
		if seen[runID] {
			continue
		}
		rec, err := r.checks.Get(ctx, runID)
		if err != nil {
			return nil, fmt.Errorf("check not found")
		}
		if rec.OrgID != org.ID {
			return nil, fmt.Errorf("check not found")
		}
		if _, err := r.checkRerunner(ctx, runID); err != nil {
			return nil, err
		}
		seen[runID] = true
		out = append(out, &graphqlCheck{
			id:            string(raw),
			name:          checkName,
			status:        "pending",
			traceID:       "",
			moduleRef:     canonicalCheckModuleRef(rec),
			moduleVersion: rec.SHA,
		})
	}
	return out, nil
}

// RerunLoad re-dispatches a failed load check.
func (r *graphqlRoot) RerunLoad(ctx context.Context, args struct {
	Org     graphql.ID
	CheckId graphql.ID
}) (*graphqlCheck, error) {
	org, err := r.resolveAccessibleOrg(ctx, string(args.Org))
	if err != nil {
		return nil, err
	}
	if r.checkRerunner == nil {
		return nil, fmt.Errorf("check rerun is not available on this self-hosted deployment (checks.enabled is false)")
	}
	runID, checkName := splitCheckID(string(args.CheckId))
	rec, err := r.checks.Get(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("check not found")
	}
	if rec.OrgID != org.ID {
		return nil, fmt.Errorf("check not found")
	}
	if checkName != "load" {
		return nil, fmt.Errorf("only a failed load check can be re-run")
	}
	if rec.Status != "failed" && rec.Status != "canceled" {
		return nil, fmt.Errorf("only a failed load check can be re-run")
	}
	if _, err := r.checkRerunner(ctx, runID); err != nil {
		return nil, err
	}
	return &graphqlCheck{
		id:            string(args.CheckId),
		name:          "load",
		status:        "pending",
		moduleRef:     canonicalCheckModuleRef(rec),
		moduleVersion: rec.SHA,
		internal:      true,
	}, nil
}

func splitCheckID(raw string) (runID, checkName string) {
	if idx := strings.LastIndex(raw, "/"); idx >= 0 {
		return raw[:idx], raw[idx+1:]
	}
	return raw, ""
}

// repoFilterMatches reports whether repo matches any of the requested filters.
func repoFilterMatches(filters []string, repo string) bool {
	for _, f := range filters {
		if sameNormalizedRepo(f, repo) {
			return true
		}
	}
	return false
}

// sameNormalizedRepo reports whether two repo slugs refer to the same repo,
// comparing case-insensitively on the canonical (host-stripped) form.
func sameNormalizedRepo(a, b string) bool {
	return strings.EqualFold(normalizeRepoTail(a), normalizeRepoTail(b))
}

// normalizeRepoTail strips a leading host segment and lowercases the remainder.
func normalizeRepoTail(repo string) string {
	repo = strings.TrimSuffix(repo, ".git")
	repo = strings.ToLower(repo)
	parts := strings.Split(repo, "/")
	if len(parts) >= 3 && strings.Contains(parts[0], ".") {
		return strings.Join(parts[1:], "/")
	}
	return repo
}

// --- Family F: integrations -------------------------------------------------

// Sources returns the org's configured forge integrations.
func (r *graphqlRoot) Sources(ctx context.Context) ([]*graphqlSource, error) {
	orgs, err := r.accessibleOrgs(ctx)
	if err != nil {
		return nil, err
	}
	if r.forgeSources == nil {
		return []*graphqlSource{}, nil
	}
	out := make([]*graphqlSource, 0)
	for _, org := range orgs {
		sources, err := r.forgeSources.ListByOrg(ctx, org.ID)
		if err != nil {
			return nil, err
		}
		for _, src := range sources {
			out = append(out, r.toGraphQLSource(org, src))
		}
	}
	return out, nil
}

func (r *graphqlRoot) toGraphQLSource(org *domain.Org, src *domain.ForgeSource) *graphqlSource {
	name := ownerOfRepo(src.Repo)
	if name == "" {
		name = src.Repo
	}
	s := &graphqlSource{
		id:           src.ID,
		name:         name,
		sourceType:   string(src.Provider),
		configuredAt: src.CreatedAt.UTC().Format(time.RFC3339),
		configURL:    r.sourceConfigURL(src),
	}
	orgID := org.ID
	orgName := org.Name
	s.orgID = &orgID
	s.orgName = &orgName
	return s
}

func (r *graphqlRoot) sourceConfigURL(src *domain.ForgeSource) string {
	switch src.Provider {
	case domain.ForgeGitHub:
		if src.InstallationID != "" {
			return fmt.Sprintf("https://github.com/settings/installations/%s", src.InstallationID)
		}
		base := "https://github.com"
		if src.BaseURL != "" {
			base = strings.TrimRight(src.BaseURL, "/")
		}
		return fmt.Sprintf("%s/%s", base, src.Repo)
	case domain.ForgeGitLab:
		base := "https://gitlab.com"
		if src.BaseURL != "" {
			base = strings.TrimRight(strings.TrimSuffix(src.BaseURL, "/api/v4"), "/")
		}
		return fmt.Sprintf("%s/%s", base, src.Repo)
	case domain.ForgeGitea:
		if r.publicURL != "" {
			org := ownerOfRepo(src.Repo)
			return fmt.Sprintf("%s/orgs/%s/integrations", r.publicURL, org)
		}
		return strings.TrimRight(src.BaseURL, "/")
	default:
		return src.BaseURL
	}
}

// GithubConnection reports the caller's connected GitHub forge source as a
// GitHubConnection, or null when the caller cannot access any org with a
// GitHub forge source. The login is the owner segment of the source's repo and
// connectedAt is the first such source's creation time (deterministic order:
// accessible orgs, then sources per org).
func (r *graphqlRoot) GithubConnection(ctx context.Context) (*graphqlGitHubConnection, error) {
	if _, err := r.requireAuthenticated(ctx); err != nil {
		return nil, err
	}
	if r.forgeSources == nil {
		return nil, nil
	}
	orgs, err := r.accessibleOrgs(ctx)
	if err != nil {
		return nil, err
	}
	for _, org := range orgs {
		sources, err := r.forgeSources.ListByOrg(ctx, org.ID)
		if err != nil {
			return nil, err
		}
		for _, src := range sources {
			if src.Provider != domain.ForgeGitHub {
				continue
			}
			return &graphqlGitHubConnection{
				githubLogin: githubLoginForSource(org, src),
				connectedAt: src.CreatedAt.UTC().Format(time.RFC3339),
			}, nil
		}
	}
	return nil, nil
}

// githubLoginForSource derives the GitHub login for a forge source: the owner
// segment of the source repo, falling back to the raw repo and then the org
// name when the repo carries no owner segment.
func githubLoginForSource(org *domain.Org, src *domain.ForgeSource) string {
	if owner, _, found := strings.Cut(src.Repo, "/"); found {
		return owner
	}
	if src.Repo != "" {
		return src.Repo
	}
	return org.Name
}

// GithubOAuthURL is rejected: self-hosted deployments configure forge
// integrations out of band.
func (r *graphqlRoot) GithubOAuthURL(ctx context.Context, args struct {
	RedirectURI string
}) (string, error) {
	if _, err := r.requireAuthenticated(ctx); err != nil {
		return "", err
	}
	return "", fmt.Errorf("GitHub OAuth app integration is not configured on this self-hosted deployment; configure forge integrations via /api/v1/orgs/<org>/integrations")
}

// SourceRepositories lists the repositories of a forge installation.
func (r *graphqlRoot) SourceRepositories(ctx context.Context, args struct {
	InstallationID graphql.ID
	Org            *graphql.ID
}) ([]*graphqlSourceRepository, error) {
	orgs, err := r.scopedOrgs(ctx, args.Org)
	if err != nil {
		return nil, err
	}
	if r.forgeSources == nil {
		return []*graphqlSourceRepository{}, nil
	}
	out := make([]*graphqlSourceRepository, 0)
	for _, org := range orgs {
		sources, err := r.forgeSources.ListByOrg(ctx, org.ID)
		if err != nil {
			return nil, err
		}
		for _, src := range sources {
			if src.ID != string(args.InstallationID) && src.InstallationID != string(args.InstallationID) {
				continue
			}
			claimed := org.Name
			out = append(out, &graphqlSourceRepository{
				repository:       canonicalCheckRepo(src.Provider, src.Repo),
				selected:         true,
				eligible:         true,
				claimedByOrgName: &claimed,
			})
		}
	}
	return out, nil
}

// User.Repositories returns the caller's forge repositories.
func (u *graphqlUser) Repositories(ctx context.Context, args struct{ Refs *[]string }) ([]*graphqlUserRepository, error) {
	orgs, err := u.root.accessibleOrgs(ctx)
	if err != nil {
		return nil, err
	}
	if u.root.forgeSources == nil {
		return []*graphqlUserRepository{}, nil
	}
	out := make([]*graphqlUserRepository, 0)
	for _, org := range orgs {
		sources, err := u.root.forgeSources.ListByOrg(ctx, org.ID)
		if err != nil {
			return nil, err
		}
		for _, src := range sources {
			ref := canonicalCheckRepo(src.Provider, src.Repo)
			if args.Refs != nil && len(*args.Refs) > 0 && !repoFilterMatches(*args.Refs, ref) {
				continue
			}
			out = append(out, &graphqlUserRepository{
				ref: ref,
				settings: &graphqlRepoSettings{
					repo:                              ref,
					isPublic:                          false,
					effectiveRunChecksOnDefaultBranch: true,
					effectiveRunChecksOnPullRequest:   true,
				},
				mappedSource: &graphqlMappedSource{
					provider:       strings.ToUpper(string(src.Provider)),
					sourceName:     ownerOfRepo(src.Repo),
					installationID: firstNonEmpty(src.InstallationID, src.ID),
					configURL:      u.root.sourceConfigURL(src),
					org:            &graphqlOrg{root: u.root, org: org},
				},
			})
		}
	}
	return out, nil
}

// GitLabOrganizationSources / GitLabSources return the org's GitLab sources.
func (r *graphqlRoot) GitLabOrganizationSources(ctx context.Context, args struct{ Org graphql.ID }) ([]*graphqlGitLabSource, error) {
	return r.gitlabSources(ctx, string(args.Org))
}

// GitLabSources is an alias for GitLabOrganizationSources.
func (r *graphqlRoot) GitLabSources(ctx context.Context, args struct{ Org graphql.ID }) ([]*graphqlGitLabSource, error) {
	return r.gitlabSources(ctx, string(args.Org))
}

func (r *graphqlRoot) gitlabSources(ctx context.Context, rawOrg string) ([]*graphqlGitLabSource, error) {
	org, err := r.resolveAccessibleOrg(ctx, rawOrg)
	if err != nil {
		return nil, err
	}
	if r.forgeSources == nil {
		return []*graphqlGitLabSource{}, nil
	}
	sources, err := r.forgeSources.ListByOrg(ctx, org.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*graphqlGitLabSource, 0)
	for _, src := range sources {
		if src.Provider != domain.ForgeGitLab {
			continue
		}
		out = append(out, &graphqlGitLabSource{
			root:          r,
			id:            src.ID,
			instanceURL:   gitlabInstanceURL(src.BaseURL),
			namespacePath: path.Dir(src.Repo),
		})
	}
	return out, nil
}

// UseGitLabAccount is rejected: personal GitLab linking is unsupported.
func (r *graphqlRoot) UseGitLabAccount(ctx context.Context, args struct {
	Org    graphql.ID
	Source graphql.ID
}) (*graphqlGitLabSourceAccess, error) {
	if _, err := r.resolveAccessibleOrg(ctx, string(args.Org)); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("personal GitLab account linking is not supported on this self-hosted deployment; configure GitLab integrations via /api/v1/orgs/<org>/integrations")
}

// EntireInstallations returns no installs: Entire is unsupported.
func (r *graphqlRoot) EntireInstallations(ctx context.Context, args struct{ Org graphql.ID }) ([]*graphqlEntireInstallation, error) {
	if _, err := r.resolveAccessibleOrg(ctx, string(args.Org)); err != nil {
		return nil, err
	}
	return []*graphqlEntireInstallation{}, nil
}

// EntireInstallURL is rejected: Entire is unsupported.
func (r *graphqlRoot) EntireInstallURL(ctx context.Context, args struct {
	Org         graphql.ID
	RedirectURI string
}) (string, error) {
	if _, err := r.resolveAccessibleOrg(ctx, string(args.Org)); err != nil {
		return "", err
	}
	return "", fmt.Errorf("Entire integration is not available on this self-hosted deployment") //nolint:staticcheck // user-facing message mirrors the Cloud CLI wording
}

func gitlabInstanceURL(base string) string {
	base = strings.TrimRight(base, "/")
	base = strings.TrimSuffix(base, "/api/v4")
	if base == "" {
		return "https://gitlab.com"
	}
	return base
}

// --- Family G: repos / repo settings ---------------------------------------

// GetRepos returns the caller's distinct repos across projects and sources.
func (r *graphqlRoot) GetRepos(ctx context.Context) ([]*graphqlRepo, error) {
	orgs, err := r.accessibleOrgs(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	out := make([]*graphqlRepo, 0)
	collect := func(fullName string) {
		if fullName == "" || len(out) >= domain.MaxTraceLimit {
			return
		}
		key := normalizeRepoTail(fullName)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, &graphqlRepo{name: lastSegment(fullName), fullName: trimRepoHost(fullName), private: true})
	}
	for _, org := range orgs {
		if r.projects != nil {
			projects, err := r.projects.ListByOrg(ctx, org.ID)
			if err != nil {
				return nil, err
			}
			for _, p := range projects {
				collect(p.Name)
			}
		}
		if r.forgeSources != nil {
			sources, err := r.forgeSources.ListByOrg(ctx, org.ID)
			if err != nil {
				return nil, err
			}
			for _, src := range sources {
				collect(canonicalCheckRepo(src.Provider, src.Repo))
			}
		}
	}
	return out, nil
}

// Org.RepoSettings returns repo visibility settings for the org's sources.
func (o *graphqlOrg) RepoSettings(ctx context.Context, args struct{ Repo *string }) ([]*graphqlRepoSettings, error) {
	if o.root.forgeSources == nil {
		return []*graphqlRepoSettings{}, nil
	}
	sources, err := o.root.forgeSources.ListByOrg(ctx, o.org.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*graphqlRepoSettings, 0)
	for _, src := range sources {
		ref := canonicalCheckRepo(src.Provider, src.Repo)
		if args.Repo != nil && *args.Repo != "" && !sameNormalizedRepo(*args.Repo, ref) {
			continue
		}
		out = append(out, &graphqlRepoSettings{
			repo:                              ref,
			isPublic:                          false,
			effectiveRunChecksOnDefaultBranch: true,
			effectiveRunChecksOnPullRequest:   true,
		})
	}
	return out, nil
}

// UpdateOrgRepoSetting is rejected: repo visibility is managed via forge
// integrations on self-hosted deployments.
func (r *graphqlRoot) UpdateOrgRepoSetting(ctx context.Context, args struct {
	Org     graphql.ID
	Setting graphqlRepoSettingInput
}) (bool, error) {
	if _, err := r.resolveAccessibleOrg(ctx, string(args.Org)); err != nil {
		return false, err
	}
	return false, fmt.Errorf("repository visibility settings are not supported on this self-hosted deployment; manage forge integrations via /api/v1/orgs/<org>/integrations")
}

// DeleteOrgRepoSetting is rejected: see UpdateOrgRepoSetting.
func (r *graphqlRoot) DeleteOrgRepoSetting(ctx context.Context, args struct {
	Org      graphql.ID
	RepoName string
}) (bool, error) {
	if _, err := r.resolveAccessibleOrg(ctx, string(args.Org)); err != nil {
		return false, err
	}
	return false, fmt.Errorf("repository visibility settings are not supported on this self-hosted deployment; manage forge integrations via /api/v1/orgs/<org>/integrations")
}

// UpdateOrgRepoSettings is rejected: check triggers are managed via forge
// integrations on self-hosted deployments.
func (r *graphqlRoot) UpdateOrgRepoSettings(ctx context.Context, args struct {
	Org      graphql.ID
	Settings []graphqlRepoSettingInput
}) (bool, error) {
	if _, err := r.resolveAccessibleOrg(ctx, string(args.Org)); err != nil {
		return false, err
	}
	return false, fmt.Errorf("repository check triggers are managed via forge integrations on this self-hosted deployment (/api/v1/orgs/<org>/integrations)")
}

func ownerOfRepo(repo string) string {
	dir := path.Dir(repo)
	if dir == "." || dir == "/" {
		return repo
	}
	return path.Base(dir)
}

func lastSegment(repo string) string { return path.Base(repo) }

// trimRepoHost strips a leading host segment from a `host/owner/name` slug.
func trimRepoHost(fullName string) string {
	parts := strings.Split(fullName, "/")
	if len(parts) >= 3 && strings.Contains(parts[0], ".") {
		return strings.Join(parts[1:], "/")
	}
	return fullName
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// --- GraphQL object types ---------------------------------------------------

type graphqlCheckCommitConnection struct {
	nodes []*graphqlCheckCommit
}

func (c *graphqlCheckCommitConnection) Nodes() []*graphqlCheckCommit { return c.nodes }

type graphqlCheckCommit struct {
	repo          string
	commitSHA     string
	commitMessage string
	timestamp     graphqlTime
	authorName    string
	authorEmail   string
	events        []*graphqlCheckEvent
	refs          []*graphqlCheckCommitRef
	checks        []*graphqlCheck
	org           *graphqlCheckCommitOrg
}

func (c *graphqlCheckCommit) Repo() string                   { return c.repo }
func (c *graphqlCheckCommit) CommitSHA() string              { return c.commitSHA }
func (c *graphqlCheckCommit) CommitMessage() string          { return c.commitMessage }
func (c *graphqlCheckCommit) Timestamp() graphqlTime         { return c.timestamp }
func (c *graphqlCheckCommit) AuthorName() string             { return c.authorName }
func (c *graphqlCheckCommit) AuthorEmail() string            { return c.authorEmail }
func (c *graphqlCheckCommit) Events() []*graphqlCheckEvent   { return c.events }
func (c *graphqlCheckCommit) Refs() []*graphqlCheckCommitRef { return c.refs }
func (c *graphqlCheckCommit) Checks() []*graphqlCheck        { return c.checks }
func (c *graphqlCheckCommit) Org() *graphqlCheckCommitOrg    { return c.org }

type graphqlCheckCommitOrg struct {
	id   string
	name string
}

func (o *graphqlCheckCommitOrg) ID() graphql.ID { return graphql.ID(o.id) }
func (o *graphqlCheckCommitOrg) Name() string   { return o.name }

type graphqlCheckEvent struct {
	provider  string
	eventType string
	timestamp graphqlTime
}

func (e *graphqlCheckEvent) Provider() string          { return e.provider }
func (e *graphqlCheckEvent) Type() string              { return e.eventType }
func (e *graphqlCheckEvent) Timestamp() graphqlTime    { return e.timestamp }
func (e *graphqlCheckEvent) Actor() *graphqlCheckActor { return &graphqlCheckActor{} }

type graphqlCheckActor struct{}

func (a *graphqlCheckActor) ID() graphql.ID    { return "" }
func (a *graphqlCheckActor) Login() string     { return "" }
func (a *graphqlCheckActor) Name() string      { return "" }
func (a *graphqlCheckActor) AvatarUrl() string { return "" }

// graphqlCheckCommitRef is the union of tag/branch/PR refs with exactly one
// member set (mutual exclusivity enforced by constructors).
type graphqlCheckCommitRef struct {
	tag    *graphqlCheckCommitTagRef
	branch *graphqlCheckCommitBranchRef
	pr     *graphqlCheckCommitPullRequestRef
}

// ToCheckCommitTagRef implements the union member accessor.
func (r *graphqlCheckCommitRef) ToCheckCommitTagRef() (*graphqlCheckCommitTagRef, bool) {
	if r.tag == nil {
		return nil, false
	}
	return r.tag, true
}

// ToCheckCommitBranchRef implements the union member accessor.
func (r *graphqlCheckCommitRef) ToCheckCommitBranchRef() (*graphqlCheckCommitBranchRef, bool) {
	if r.branch == nil {
		return nil, false
	}
	return r.branch, true
}

// ToCheckCommitPullRequestRef implements the union member accessor.
func (r *graphqlCheckCommitRef) ToCheckCommitPullRequestRef() (*graphqlCheckCommitPullRequestRef, bool) {
	if r.pr == nil {
		return nil, false
	}
	return r.pr, true
}

type graphqlCheckCommitTagRef struct{ name string }

func (r *graphqlCheckCommitTagRef) Name() string { return r.name }
func (r *graphqlCheckCommitTagRef) URL() string  { return "" }

type graphqlCheckCommitBranchRef struct{ name string }

func (r *graphqlCheckCommitBranchRef) Name() string { return r.name }
func (r *graphqlCheckCommitBranchRef) URL() string  { return "" }

type graphqlCheckCommitPullRequestRef struct {
	number            int32
	url               string
	title             string
	state             string
	integrationCommit string
}

func (r *graphqlCheckCommitPullRequestRef) Number() int32 { return r.number }
func (r *graphqlCheckCommitPullRequestRef) URL() string   { return r.url }
func (r *graphqlCheckCommitPullRequestRef) Title() string { return r.title }
func (r *graphqlCheckCommitPullRequestRef) State() string { return r.state }
func (r *graphqlCheckCommitPullRequestRef) IntegrationCommit() string {
	return r.integrationCommit
}

type graphqlCheck struct {
	id            string
	name          string
	status        string
	startedAt     graphqlTime
	endTime       *graphqlTime
	duration      *int32
	traceID       string
	spanID        string
	moduleRef     string
	moduleVersion string
	internal      bool
}

func (c *graphqlCheck) ID() graphql.ID          { return graphql.ID(c.id) }
func (c *graphqlCheck) Name() string            { return c.name }
func (c *graphqlCheck) Status() string          { return c.status }
func (c *graphqlCheck) StartedAt() *graphqlTime { return &c.startedAt }
func (c *graphqlCheck) EndTime() *graphqlTime   { return c.endTime }
func (c *graphqlCheck) Duration() *int32        { return c.duration }
func (c *graphqlCheck) TraceId() string         { return c.traceID }
func (c *graphqlCheck) SpanId() string          { return c.spanID }
func (c *graphqlCheck) ModuleRef() string       { return c.moduleRef }
func (c *graphqlCheck) ModuleVersion() string   { return c.moduleVersion }
func (c *graphqlCheck) Internal() bool          { return c.internal }

type graphqlSource struct {
	id           string
	name         string
	sourceType   string
	configuredAt string
	orgID        *string
	orgName      *string
	configURL    string
}

func (s *graphqlSource) ID() graphql.ID       { return graphql.ID(s.id) }
func (s *graphqlSource) Name() string         { return s.name }
func (s *graphqlSource) Type() string         { return s.sourceType }
func (s *graphqlSource) ConfiguredAt() string { return s.configuredAt }
func (s *graphqlSource) OrgId() *graphql.ID {
	if s.orgID == nil {
		return nil
	}
	id := graphql.ID(*s.orgID)
	return &id
}
func (s *graphqlSource) OrgName() *string  { return s.orgName }
func (s *graphqlSource) ConfigUrl() string { return s.configURL }

type graphqlSourceRepository struct {
	repository       string
	selected         bool
	eligible         bool
	claimedByOrgName *string
}

func (s *graphqlSourceRepository) Repository() string        { return s.repository }
func (s *graphqlSourceRepository) Selected() bool            { return s.selected }
func (s *graphqlSourceRepository) Eligible() bool            { return s.eligible }
func (s *graphqlSourceRepository) ClaimedByOrgName() *string { return s.claimedByOrgName }

type graphqlUserRepository struct {
	ref          string
	settings     *graphqlRepoSettings
	mappedSource *graphqlMappedSource
}

func (u *graphqlUserRepository) Ref() string                        { return u.ref }
func (u *graphqlUserRepository) Settings() *graphqlRepoSettings     { return u.settings }
func (u *graphqlUserRepository) MappedSource() *graphqlMappedSource { return u.mappedSource }

type graphqlMappedSource struct {
	provider       string
	sourceName     string
	installationID string
	configURL      string
	org            *graphqlOrg
}

func (m *graphqlMappedSource) Provider() string       { return m.provider }
func (m *graphqlMappedSource) SourceName() string     { return m.sourceName }
func (m *graphqlMappedSource) InstallationId() string { return m.installationID }
func (m *graphqlMappedSource) ConfigUrl() string      { return m.configURL }
func (m *graphqlMappedSource) Org() *graphqlOrg       { return m.org }

type graphqlGitHubConnection struct {
	githubLogin string
	connectedAt string
}

func (c *graphqlGitHubConnection) GithubLogin() string { return c.githubLogin }
func (c *graphqlGitHubConnection) ConnectedAt() string { return c.connectedAt }

type graphqlGitLabSource struct {
	root          *graphqlRoot
	id            string
	instanceURL   string
	namespacePath string
}

func (s *graphqlGitLabSource) ID() graphql.ID        { return graphql.ID(s.id) }
func (s *graphqlGitLabSource) InstanceURL() string   { return s.instanceURL }
func (s *graphqlGitLabSource) NamespacePath() string { return s.namespacePath }

func (s *graphqlGitLabSource) Access(ctx context.Context, args struct{ Org graphql.ID }) (*graphqlGitLabSourceAccess, error) {
	org, err := s.root.resolveAccessibleOrg(ctx, string(args.Org))
	if err != nil {
		return nil, err
	}
	if s.root.forgeSources == nil {
		return nil, nil
	}
	sources, err := s.root.forgeSources.ListByOrg(ctx, org.ID)
	if err != nil {
		return nil, err
	}
	for _, src := range sources {
		if src.ID == s.id {
			return &graphqlGitLabSourceAccess{connectionID: src.ID}, nil
		}
	}
	return nil, nil
}

type graphqlGitLabSourceAccess struct{ connectionID string }

func (a *graphqlGitLabSourceAccess) ConnectionID() string { return a.connectionID }

type graphqlEntireInstallation struct {
	id        string
	name      string
	manageURL string
}

func (i *graphqlEntireInstallation) ID() graphql.ID    { return graphql.ID(i.id) }
func (i *graphqlEntireInstallation) Name() string      { return i.name }
func (i *graphqlEntireInstallation) ManageURL() string { return i.manageURL }

type graphqlRepo struct {
	name     string
	fullName string
	private  bool
}

func (r *graphqlRepo) Name() string     { return r.name }
func (r *graphqlRepo) FullName() string { return r.fullName }
func (r *graphqlRepo) Private() bool    { return r.private }

type graphqlRepoSettings struct {
	repo                              string
	isPublic                          bool
	effectiveRunChecksOnDefaultBranch bool
	effectiveRunChecksOnPullRequest   bool
}

func (s *graphqlRepoSettings) Repo() string   { return s.repo }
func (s *graphqlRepoSettings) IsPublic() bool { return s.isPublic }
func (s *graphqlRepoSettings) EffectiveRunChecksOnDefaultBranch() bool {
	return s.effectiveRunChecksOnDefaultBranch
}
func (s *graphqlRepoSettings) EffectiveRunChecksOnPullRequest() bool {
	return s.effectiveRunChecksOnPullRequest
}

// graphqlRepoSettingInput is the RepoSettingInput SDL input type.
type graphqlRepoSettingInput struct {
	Repo                     string
	IsPublic                 *bool
	SourceId                 *graphql.ID
	RunChecksOnDefaultBranch *bool
	RunChecksOnPullRequest   *bool
	RunChecksOnBranchPush    *bool
}
