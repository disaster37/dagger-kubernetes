package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/graph-gophers/graphql-go"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// graphqlIdentityKey carries the authenticated caller into resolvers.
type graphqlIdentityKey struct{}

// GraphQLIdentity is the authenticated caller for a Cloud GraphQL request.
type GraphQLIdentity struct {
	UserID string
	OrgID  string
}

// WithGraphQLIdentity attaches the caller identity to ctx.
func WithGraphQLIdentity(ctx context.Context, id GraphQLIdentity) context.Context {
	return context.WithValue(ctx, graphqlIdentityKey{}, id)
}

func graphqlIdentity(ctx context.Context) GraphQLIdentity {
	id, _ := ctx.Value(graphqlIdentityKey{}).(GraphQLIdentity)
	return id
}

// CheckRerunner re-dispatches a Cloud Checks run identified by runID.
type CheckRerunner func(ctx context.Context, runID string) (*domain.CheckRunRecord, error)

const cloudGraphQLSchema = `
schema { query: Query mutation: Mutation }
scalar Time
type Query {
  user: User!
  org(name: String!): Org
  trace(id: ID!, org: ID): Trace
  billingUsage(org: ID!, month: String!): BillingUsage!
  sources: [Source!]!
  sourceRepositories(installationId: ID!, org: ID): [SourceRepository!]!
  githubConnection: GitHubConnection
  githubOAuthURL(redirectURI: String!): String!
  gitlabOrganizationSources(org: ID!): [GitLabSource!]!
  gitlabSources(org: ID!): [GitLabSource!]!
  entireInstallations(org: ID!): [EntireInstallation!]!
  entireInstallURL(org: ID!, redirectURI: String!): String!
  getRepos: [Repo!]!
}
type Mutation {
  createQuickstartOrg(name: String!): Org!
  createPortalSession(org: ID!): String!
  rerunChecks(org: ID!, checkIds: [ID!]!, cleanSlate: Boolean): [Check!]!
  rerunLoad(org: ID!, checkId: ID!): Check!
  useGitLabAccount(org: ID!, source: ID!): GitLabSourceAccess
  updateOrgRepoSetting(org: ID!, setting: RepoSettingInput!): Boolean!
  deleteOrgRepoSetting(org: ID!, repoName: String!): Boolean!
  updateOrgRepoSettings(org: ID!, settings: [RepoSettingInput!]!): Boolean!
}
type User { id: ID! nickname: String! email: String! orgs: [Org!]! checks(repos: [String!], first: Int): CheckCommitConnection! repositories(refs: [String!]): [UserRepository!]! }
type CLINotice { level: String! title: String! body: String! }
type Org {
  id: ID! name: String! createdAt: String!
  subscription: Subscription!
  features: [Feature!]!
  cliNotice: CLINotice
  lastUserTrace: Trace
  traces(filter: TraceFilter, sort: TraceSort, first: Int, after: Time): TraceConnection!
  checks(repos: [String!], first: Int): CheckCommitConnection!
  moduleChecks(moduleRef: String!, moduleVersion: String!): [CheckCommit!]!
  repoSettings(repo: String): [RepoSettings!]!
}
type Subscription { status: String! trialStart: String trialEnd: String subscriptionID: String! planID: String! hasCaching: Boolean! }
type Feature { name: String! status: String! trialStart: String trialEnd: String }
type BillingUsage {
  month: String! revision: Int! calculation: String!
  asOf: Time processedThrough: Time calculatedAt: Time
  refreshStatus: String! refreshMonth: String! nextRefreshAt: Time
  error: String! finalizedAt: Time invoiceID: String invoiceURL: String
  warnings: [String!]! pricing: BillingUsagePricing bill: BillingUsageBill
}
type BillingUsagePricing { planID: String! name: String! minimumUSD: Float! }
type BillingUsageBill { usageUSD: Float! minimumAdjustmentUSD: Float! totalUSD: Float! resources: [BillingUsageResource!]! }
type BillingUsageResource { resource: String! unit: String! newQuantity: Float! repeatRan: Float! repeatCached: Float! repeatEstimated: Float! newPrice: Float! repeatPrice: Float! newChargeUSD: Float! repeatChargeUSD: Float! estimated: Boolean! }
enum TraceSort { START DURATION }
input TraceFilter { repos: [String!] branch: String commit: String tag: String change: String status: String local: Boolean mine: Boolean user: String token: String author: String name: String provider: String since: Time until: Time minDuration: Float maxDuration: Float }
type Trace {
  id: ID! name: String! status: TraceStatus project: String! version: String! durationMS: Int! startedAt: String! updatedAt: String!
  timestamp: Time! endTime: Time local: Boolean! sender: TraceSender git: TraceGit ci: TraceCI
}
type TraceStatus { code: String! message: String! }
type TraceSender { name: String! }
type TraceGit { remote: String! title: String! ref: String! tag: String branch: String author: TraceGitAuthor }
type TraceGitAuthor { name: String! email: String! }
type TraceCI { isNativeCI: Boolean! provider: String repository: String change: TraceCIChange }
type TraceCIChange { id: String! title: String! branch: String! headSHA: String! }
type TraceConnection { nodes: [Trace!]! pageInfo: PageInfo! }
type PageInfo { hasNextPage: Boolean! endCursor: String }
type CheckCommitConnection { nodes: [CheckCommit!]! }
type CheckCommit { repo: String! commitSHA: String! commitMessage: String! timestamp: Time! authorName: String! authorEmail: String! events: [CheckEvent!]! refs: [CheckCommitRef!]! checks: [Check!]! org: CheckCommitOrg! }
type CheckCommitOrg { id: ID! name: String! }
type CheckEvent { provider: String! type: String! timestamp: Time! actor: CheckActor! }
type CheckActor { id: ID! login: String! name: String! avatarUrl: String! }
union CheckCommitRef = CheckCommitTagRef | CheckCommitBranchRef | CheckCommitPullRequestRef
type CheckCommitTagRef { name: String! url: String! }
type CheckCommitBranchRef { name: String! url: String! }
type CheckCommitPullRequestRef { number: Int! url: String! title: String! state: String! integrationCommit: String! }
type Check { id: ID! name: String! status: String! startedAt: Time endTime: Time duration: Int traceId: String! spanId: String! moduleRef: String! moduleVersion: String! internal: Boolean! }
type Source { id: ID! name: String! type: String! configuredAt: String! orgId: ID orgName: String configUrl: String! repositories: [SourceRepository!]! }
type SourceRepository { repository: String! selected: Boolean! eligible: Boolean! claimedByOrgName: String }
type UserRepository { ref: String! settings: RepoSettings mappedSource: MappedSource }
type MappedSource { provider: String! sourceName: String! installationId: String! configUrl: String! org: Org }
type GitHubConnection { githubLogin: String! connectedAt: String! scopes: [String!]! status: String! }
type GitLabSource { id: ID! instanceURL: String! namespacePath: String! access(org: ID!): GitLabSourceAccess }
type GitLabSourceAccess { connectionID: String! }
type EntireInstallation { id: ID! name: String! manageURL: String! }
type Repo { name: String! fullName: String! private: Boolean! }
type RepoSettings { repo: String! isPublic: Boolean! effectiveRunChecksOnDefaultBranch: Boolean! effectiveRunChecksOnPullRequest: Boolean! effectiveRunChecksOnBranchPush: Boolean! }
input RepoSettingInput { repo: String! isPublic: Boolean sourceId: ID runChecksOnDefaultBranch: Boolean runChecksOnPullRequest: Boolean runChecksOnBranchPush: Boolean }
`

// CloudGraphQL is the Cloud-compatible GraphQL schema and resolver root.
type CloudGraphQL struct {
	schema *graphql.Schema
	root   *graphqlRoot
}

// OrgCreator provisions a new organization (generated org-<16hex> ID and
// bootstrap default group). It is wired from cmd/api to the service layer to
// keep repository → service imports cycle-free.
type OrgCreator func(ctx context.Context, name, displayName string) (*domain.Org, error)

// WithOrgCreator installs the org provisioner used by CreateQuickstartOrg.
func (g *CloudGraphQL) WithOrgCreator(fn OrgCreator) *CloudGraphQL {
	g.root.orgCreator = fn
	return g
}

// WithForgeSources wires the forge integration repository used by the
// sources/repositories/integrations families.
func (g *CloudGraphQL) WithForgeSources(repo domain.ForgeSourceRepository) *CloudGraphQL {
	g.root.forgeSources = repo
	return g
}

// WithProjects wires the project repository used by GetRepos.
func (g *CloudGraphQL) WithProjects(repo domain.ProjectRepository) *CloudGraphQL {
	g.root.projects = repo
	return g
}

// WithGitHubConnections wires the user-scoped GitHub connection repository.
func (g *CloudGraphQL) WithGitHubConnections(repo domain.GitHubConnectionRepository) *CloudGraphQL {
	g.root.githubConns = repo
	return g
}

// WithGitHubAccountSources wires the org-scoped GitHub account repository.
func (g *CloudGraphQL) WithGitHubAccountSources(repo domain.GitHubAccountSourceRepository) *CloudGraphQL {
	g.root.accountSources = repo
	return g
}

// WithRepoSettings wires the per-repo claim/trigger repository.
func (g *CloudGraphQL) WithRepoSettings(repo domain.RepoSettingRepository) *CloudGraphQL {
	g.root.repoSettings = repo
	return g
}

// WithGitHubIntegrator wires the service-layer GitHub integration adapter.
func (g *CloudGraphQL) WithGitHubIntegrator(integrator domain.GitHubCloudIntegrator) *CloudGraphQL {
	g.root.githubIntegrator = integrator
	return g
}

// WithPublicURL wires the deployment's public base URL used to build the
// config URLs returned by the integrations family.
func (g *CloudGraphQL) WithPublicURL(url string) *CloudGraphQL {
	g.root.publicURL = strings.TrimRight(url, "/")
	return g
}

// WithCheckRerunner wires the Cloud Checks re-dispatcher used by rerunChecks
// and rerunLoad.
func (g *CloudGraphQL) WithCheckRerunner(fn CheckRerunner) *CloudGraphQL {
	g.root.checkRerunner = fn
	return g
}

// NewCloudGraphQL builds the schema over the supplied repositories. groups is
// used to derive org visibility from a user's group memberships.
func NewCloudGraphQL(users domain.UserRepository, orgs domain.OrgRepository, groups domain.GroupRepository, traces domain.TraceMetaRepository, checks domain.CheckRunRepository, cfg domain.GraphQLConfig) *CloudGraphQL {
	root := &graphqlRoot{users: users, orgs: orgs, groups: groups, traces: traces, checks: checks}
	opts := []graphql.SchemaOpt{
		graphql.MaxDepth(cfg.MaxDepth),
		graphql.MaxSelections(cfg.MaxSelections),
		graphql.MaxQueryLength(cfg.MaxQueryBytes),
		graphql.MaxParallelism(cfg.MaxParallelism),
	}
	if !cfg.IntrospectionEnabled {
		opts = append(opts, graphql.RestrictIntrospection(func(context.Context) bool { return false }))
	}
	return &CloudGraphQL{schema: graphql.MustParseSchema(cloudGraphQLSchema, root, opts...), root: root}
}

// Exec executes a GraphQL query and returns the JSON response body.
func (g *CloudGraphQL) Exec(ctx context.Context, query, operationName string, variables map[string]any) []byte {
	resp := g.schema.Exec(ctx, query, operationName, variables)
	out, err := json.Marshal(resp)
	if err != nil {
		return []byte(`{"errors":[{"message":"encode response"}]}`)
	}
	return out
}

type graphqlRoot struct {
	users            domain.UserRepository
	orgs             domain.OrgRepository
	groups           domain.GroupRepository
	traces           domain.TraceMetaRepository
	checks           domain.CheckRunRepository
	forgeSources     domain.ForgeSourceRepository
	projects         domain.ProjectRepository
	githubConns      domain.GitHubConnectionRepository
	accountSources   domain.GitHubAccountSourceRepository
	repoSettings     domain.RepoSettingRepository
	githubIntegrator domain.GitHubCloudIntegrator
	publicURL        string
	orgCreator       OrgCreator
	checkRerunner    CheckRerunner
}

// requireAuthenticated returns the caller identity, rejecting requests without
// a resolved user (CWE-306).
func (r *graphqlRoot) requireAuthenticated(ctx context.Context) (GraphQLIdentity, error) {
	id := graphqlIdentity(ctx)
	if id.UserID == "" && id.OrgID == "" {
		return id, fmt.Errorf("unauthenticated")
	}
	return id, nil
}

// resolveAccessibleOrg resolves a caller-supplied org ID (or, when it is not a
// known ID, an org slug) and enforces access. Missing and inaccessible orgs
// both return "forbidden" (anti-enumeration).
func (r *graphqlRoot) resolveAccessibleOrg(ctx context.Context, rawOrg string) (*domain.Org, error) {
	id, err := r.requireAuthenticated(ctx)
	if err != nil {
		return nil, err
	}
	if rawOrg == "" {
		return nil, fmt.Errorf("forbidden")
	}
	org, err := r.orgs.Get(ctx, rawOrg)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			return nil, err
		}
		org, err = r.orgs.GetByName(ctx, rawOrg)
		if err != nil {
			return nil, fmt.Errorf("forbidden")
		}
	}
	if !r.canAccessOrg(ctx, id, org.ID) {
		return nil, fmt.Errorf("forbidden")
	}
	return org, nil
}

// accessibleOrgs lists the orgs the caller may act on: an org-scoped token sees
// only its own org, a global admin sees all orgs, and everyone else sees their
// direct + group-derived memberships.
func (r *graphqlRoot) accessibleOrgs(ctx context.Context) ([]*domain.Org, error) {
	id, err := r.requireAuthenticated(ctx)
	if err != nil {
		return nil, err
	}
	if id.OrgID != "" {
		org, err := r.orgs.Get(ctx, id.OrgID)
		if err != nil {
			return nil, err
		}
		return []*domain.Org{org}, nil
	}
	if u, err := r.users.Get(ctx, id.UserID); err == nil && u.Role == domain.RoleAdmin {
		return r.orgs.List(ctx)
	}
	return r.userOrgs(ctx, id.UserID)
}

// scopedOrgs resolves the orgs a resolver may act on: when an org argument is
// supplied it must be accessible (anti-enumeration via resolveAccessibleOrg)
// and becomes the sole scope; otherwise all accessible orgs are returned.
func (r *graphqlRoot) scopedOrgs(ctx context.Context, rawOrg *graphql.ID) ([]*domain.Org, error) {
	if rawOrg == nil {
		return r.accessibleOrgs(ctx)
	}
	org, err := r.resolveAccessibleOrg(ctx, string(*rawOrg))
	if err != nil {
		return nil, err
	}
	return []*domain.Org{org}, nil
}

func (r *graphqlRoot) User(ctx context.Context) (*graphqlUser, error) {
	id, err := r.requireAuthenticated(ctx)
	if err != nil {
		return nil, err
	}
	u, err := r.users.Get(ctx, id.UserID)
	if err != nil {
		return nil, err
	}
	return &graphqlUser{root: r, user: u}, nil
}

func (r *graphqlRoot) Org(ctx context.Context, args struct{ Name string }) (*graphqlOrg, error) {
	id, err := r.requireAuthenticated(ctx)
	if err != nil {
		return nil, err
	}
	o, err := r.orgs.GetByName(ctx, args.Name)
	if err != nil {
		return nil, err
	}
	if !r.canAccessOrg(ctx, id, o.ID) {
		return nil, fmt.Errorf("forbidden")
	}
	return &graphqlOrg{root: r, org: o}, nil
}

// BillingUsage returns a self-hosted zero-state usage report for the month. It
// exists so the beta.28 CLI's usage command resolves; the deployment performs
// no metering.
func (r *graphqlRoot) BillingUsage(ctx context.Context, args struct {
	Org   graphql.ID
	Month string
}) (*graphqlBillingUsage, error) {
	if _, err := r.resolveAccessibleOrg(ctx, string(args.Org)); err != nil {
		return nil, err
	}
	if _, err := time.Parse("2006-01", args.Month); err != nil {
		return nil, fmt.Errorf("invalid month %q: expected YYYY-MM", args.Month)
	}
	return &graphqlBillingUsage{month: args.Month}, nil
}

// CreatePortalSession is rejected: self-hosted deployments have no billing
// portal.
func (r *graphqlRoot) CreatePortalSession(ctx context.Context, args struct{ Org graphql.ID }) (string, error) {
	if _, err := r.resolveAccessibleOrg(ctx, string(args.Org)); err != nil {
		return "", err
	}
	return "", fmt.Errorf("billing is not available on this self-hosted deployment")
}

// CreateQuickstartOrg creates an organization for a platform administrator.
// Non-admin callers are rejected: organization creation is never
// self-service.
func (r *graphqlRoot) CreateQuickstartOrg(ctx context.Context, args struct{ Name string }) (*graphqlOrg, error) {
	id := graphqlIdentity(ctx)
	// Require an authenticated human user (reject org-scoped synthetic tokens).
	if id.UserID == "" {
		return nil, fmt.Errorf("unauthenticated")
	}
	u, err := r.users.Get(ctx, id.UserID)
	if err != nil {
		return nil, fmt.Errorf("unauthenticated")
	}
	if u.DisabledAt != nil || u.DeletedAt != nil {
		return nil, fmt.Errorf("unauthenticated")
	}
	if u.Role != domain.RoleAdmin {
		return nil, fmt.Errorf("forbidden: organization creation requires a platform administrator")
	}

	base := sanitizeQuickstartOrgSlug(args.Name)
	if existing, err := r.orgs.GetByName(ctx, base); err == nil {
		return &graphqlOrg{root: r, org: existing}, nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, fmt.Errorf("lookup org %q: %w", base, err)
	}
	if r.orgCreator == nil {
		return nil, fmt.Errorf("organization provisioning unavailable")
	}
	org, err := r.orgCreator(ctx, base, base)
	if err != nil {
		return nil, err
	}
	if err := r.orgs.SetMember(ctx, org.ID, id.UserID, domain.OrgAdmin); err != nil {
		return nil, err
	}
	return &graphqlOrg{root: r, org: org}, nil
}

// sanitizeQuickstartOrgSlug normalizes a requested quickstart org name into a
// DNS-safe slug: lowercase ASCII [a-z0-9-], runs of [-_. ] collapse to "-",
// leading/trailing "-" trimmed, truncated to 55 chars (so a "-<suffix>" fits a
// 63-char DNS label), falling back to "org" when empty.
func sanitizeQuickstartOrgSlug(raw string) string {
	var b strings.Builder
	lastDash := false
	for _, c := range strings.ToLower(raw) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
			lastDash = false
		case c == '-' || c == '_' || c == '.' || c == ' ':
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 55 {
		slug = strings.Trim(slug[:55], "-")
	}
	if slug == "" {
		return "org"
	}
	return slug
}

// canAccessOrg reports whether the caller may read orgID: the caller's own
// org-scoped token, a global admin, or a direct/group-derived org membership
// (CWE-862).
func (r *graphqlRoot) canAccessOrg(ctx context.Context, id GraphQLIdentity, orgID string) bool {
	if id.OrgID == orgID {
		return true
	}
	if u, err := r.users.Get(ctx, id.UserID); err == nil && u.Role == domain.RoleAdmin {
		return true
	}
	orgs, err := r.userOrgs(ctx, id.UserID)
	if err != nil {
		return false
	}
	for _, o := range orgs {
		if o.ID == orgID {
			return true
		}
	}
	return false
}

// userOrgs returns the union of direct org memberships and orgs derived from
// the user's active group memberships (mirrors OrgService.OrgsForUser), so a
// user assigned to an org-scoped group via OIDC/Admin UI sees that org without
// a redundant quickstart org.
func (r *graphqlRoot) userOrgs(ctx context.Context, userID string) ([]*domain.Org, error) {
	direct, err := r.orgs.OrgsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(direct))
	out := make([]*domain.Org, 0, len(direct))
	for _, o := range direct {
		seen[o.ID] = true
		out = append(out, o)
	}
	if r.groups != nil {
		groups, err := r.groups.GroupsForUser(ctx, userID)
		if err != nil {
			return nil, err
		}
		for _, g := range groups {
			if g.OrgID == "" || seen[g.OrgID] {
				continue
			}
			o, err := r.orgs.Get(ctx, g.OrgID)
			if err != nil {
				continue
			}
			seen[o.ID] = true
			out = append(out, o)
		}
	}
	return out, nil
}

type graphqlUser struct {
	root *graphqlRoot
	user *domain.User
}

func (u *graphqlUser) ID() graphql.ID   { return graphql.ID(u.user.ID) }
func (u *graphqlUser) Nickname() string { return u.user.Username }
func (u *graphqlUser) Email() string    { return "" }

func (u *graphqlUser) Orgs(ctx context.Context) ([]*graphqlOrg, error) {
	orgs, err := u.root.accessibleOrgs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*graphqlOrg, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, &graphqlOrg{root: u.root, org: o})
	}
	return out, nil
}

type graphqlOrg struct {
	root *graphqlRoot
	org  *domain.Org
}

func (o *graphqlOrg) ID() graphql.ID    { return graphql.ID(o.org.ID) }
func (o *graphqlOrg) Name() string      { return o.org.Name }
func (o *graphqlOrg) CreatedAt() string { return o.org.CreatedAt.UTC().Format(time.RFC3339) }

func (o *graphqlOrg) Features() []*graphqlFeature {
	seen := make(map[string]bool, len(domain.DefaultOrgFeatures)+len(o.org.Features))
	names := make([]string, 0, len(domain.DefaultOrgFeatures)+len(o.org.Features))
	for _, f := range append(append([]string(nil), domain.DefaultOrgFeatures...), o.org.Features...) {
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		names = append(names, f)
	}
	out := make([]*graphqlFeature, 0, len(names))
	for _, f := range names {
		out = append(out, &graphqlFeature{name: f})
	}
	return out
}

func (o *graphqlOrg) CliNotice() *graphqlCLINotice {
	// No CLI notice is configured for self-hosted orgs; returning nil (JSON
	// null) satisfies the CLI's `cliNotice { level title body }` query.
	return nil
}

// graphqlCLINotice is the CLINotice GraphQL object type.
type graphqlCLINotice struct {
	level string
	title string
	body  string
}

func (n *graphqlCLINotice) Level() string { return n.level }
func (n *graphqlCLINotice) Title() string { return n.title }
func (n *graphqlCLINotice) Body() string  { return n.body }

// graphqlFeature is the Feature GraphQL object type.
type graphqlFeature struct {
	name string
}

func (f *graphqlFeature) Name() string        { return f.name }
func (f *graphqlFeature) Status() string      { return "ACTIVE" }
func (f *graphqlFeature) TrialStart() *string { return nil }
func (f *graphqlFeature) TrialEnd() *string   { return nil }

func (o *graphqlOrg) Subscription() *graphqlSubscription {
	return &graphqlSubscription{}
}

type graphqlSubscription struct{}

func (s *graphqlSubscription) Status() string         { return "active" }
func (s *graphqlSubscription) TrialStart() *string    { return nil }
func (s *graphqlSubscription) TrialEnd() *string      { return nil }
func (s *graphqlSubscription) SubscriptionID() string { return "self-hosted" }
func (s *graphqlSubscription) PlanID() string         { return "self-hosted" }
func (s *graphqlSubscription) HasCaching() bool       { return true }

// graphqlBillingUsage is the self-hosted zero-state BillingUsage report.
type graphqlBillingUsage struct {
	month string
}

func (b *graphqlBillingUsage) Month() string                        { return b.month }
func (b *graphqlBillingUsage) Revision() int32                      { return 0 }
func (b *graphqlBillingUsage) Calculation() string                  { return "" }
func (b *graphqlBillingUsage) AsOf() *graphqlTime                   { return nil }
func (b *graphqlBillingUsage) ProcessedThrough() *graphqlTime       { return nil }
func (b *graphqlBillingUsage) CalculatedAt() *graphqlTime           { return nil }
func (b *graphqlBillingUsage) RefreshStatus() string                { return "" }
func (b *graphqlBillingUsage) RefreshMonth() string                 { return "" }
func (b *graphqlBillingUsage) NextRefreshAt() *graphqlTime          { return nil }
func (b *graphqlBillingUsage) Error() string                        { return "" }
func (b *graphqlBillingUsage) FinalizedAt() *graphqlTime            { return nil }
func (b *graphqlBillingUsage) InvoiceID() *string                   { return nil }
func (b *graphqlBillingUsage) InvoiceURL() *string                  { return nil }
func (b *graphqlBillingUsage) Warnings() []string                   { return []string{} }
func (b *graphqlBillingUsage) Pricing() *graphqlBillingUsagePricing { return nil }
func (b *graphqlBillingUsage) Bill() *graphqlBillingUsageBill       { return nil }

// graphqlBillingUsagePricing is the BillingUsagePricing object type.
type graphqlBillingUsagePricing struct {
	planID     string
	name       string
	minimumUSD float64
}

func (p *graphqlBillingUsagePricing) PlanID() string      { return p.planID }
func (p *graphqlBillingUsagePricing) Name() string        { return p.name }
func (p *graphqlBillingUsagePricing) MinimumUSD() float64 { return p.minimumUSD }

// graphqlBillingUsageBill is the BillingUsageBill object type.
type graphqlBillingUsageBill struct {
	usageUSD             float64
	minimumAdjustmentUSD float64
	totalUSD             float64
	resources            []*graphqlBillingUsageResource
}

func (b *graphqlBillingUsageBill) UsageUSD() float64             { return b.usageUSD }
func (b *graphqlBillingUsageBill) MinimumAdjustmentUSD() float64 { return b.minimumAdjustmentUSD }
func (b *graphqlBillingUsageBill) TotalUSD() float64             { return b.totalUSD }
func (b *graphqlBillingUsageBill) Resources() []*graphqlBillingUsageResource {
	if b.resources == nil {
		return []*graphqlBillingUsageResource{}
	}
	return b.resources
}

// graphqlBillingUsageResource is the BillingUsageResource object type.
type graphqlBillingUsageResource struct {
	resource        string
	unit            string
	newQuantity     float64
	repeatRan       float64
	repeatCached    float64
	repeatEstimated float64
	newPrice        float64
	repeatPrice     float64
	newChargeUSD    float64
	repeatChargeUSD float64
	estimated       bool
}

func (r *graphqlBillingUsageResource) Resource() string         { return r.resource }
func (r *graphqlBillingUsageResource) Unit() string             { return r.unit }
func (r *graphqlBillingUsageResource) NewQuantity() float64     { return r.newQuantity }
func (r *graphqlBillingUsageResource) RepeatRan() float64       { return r.repeatRan }
func (r *graphqlBillingUsageResource) RepeatCached() float64    { return r.repeatCached }
func (r *graphqlBillingUsageResource) RepeatEstimated() float64 { return r.repeatEstimated }
func (r *graphqlBillingUsageResource) NewPrice() float64        { return r.newPrice }
func (r *graphqlBillingUsageResource) RepeatPrice() float64     { return r.repeatPrice }
func (r *graphqlBillingUsageResource) NewChargeUSD() float64    { return r.newChargeUSD }
func (r *graphqlBillingUsageResource) RepeatChargeUSD() float64 { return r.repeatChargeUSD }
func (r *graphqlBillingUsageResource) Estimated() bool          { return r.estimated }
