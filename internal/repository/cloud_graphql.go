package repository

import (
	"context"
	"encoding/json"
	"fmt"
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

const cloudGraphQLSchema = `
schema { query: Query }

type Query {
  user: User!
  org(name: String!): Org
  trace(id: ID!, org: String): Trace
}

type User {
  id: ID!
  nickname: String!
  email: String!
  orgs: [Org!]!
}

type Org {
  id: ID!
  name: String!
  createdAt: String!
  subscription: Subscription!
  features: [String!]!
  cliNotice: String
  lastUserTrace: Trace
  traces(filter: String, sort: String, first: Int, after: String): TraceConnection!
}

type Subscription {
  status: String!
  planID: String!
  hasCaching: Boolean!
}

type Trace {
  id: ID!
  status: String!
  project: String!
  version: String!
  durationMS: Int!
  startedAt: String!
  updatedAt: String!
}

type TraceConnection {
  nodes: [Trace!]!
  pageInfo: PageInfo!
}

type PageInfo {
  hasNextPage: Boolean!
  endCursor: String
}
`

// CloudGraphQL is the Cloud-compatible GraphQL schema and resolver root.
type CloudGraphQL struct {
	schema *graphql.Schema
}

// NewCloudGraphQL builds the schema over the supplied repositories.
func NewCloudGraphQL(users domain.UserRepository, orgs domain.OrgRepository, traces domain.TraceMetaRepository) *CloudGraphQL {
	root := &graphqlRoot{users: users, orgs: orgs, traces: traces}
	return &CloudGraphQL{schema: graphql.MustParseSchema(cloudGraphQLSchema, root)}
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
	users  domain.UserRepository
	orgs   domain.OrgRepository
	traces domain.TraceMetaRepository
}

func (r *graphqlRoot) User(ctx context.Context) (*graphqlUser, error) {
	id := graphqlIdentity(ctx)
	if id.UserID == "" {
		return nil, fmt.Errorf("unauthenticated")
	}
	u, err := r.users.Get(ctx, id.UserID)
	if err != nil {
		return nil, err
	}
	return &graphqlUser{root: r, user: u}, nil
}

func (r *graphqlRoot) Org(ctx context.Context, args struct{ Name string }) (*graphqlOrg, error) {
	id := graphqlIdentity(ctx)
	o, err := r.orgs.GetByName(ctx, args.Name)
	if err != nil {
		return nil, err
	}
	if !r.canAccessOrg(ctx, id, o.ID) {
		return nil, fmt.Errorf("forbidden")
	}
	return &graphqlOrg{root: r, org: o}, nil
}

func (r *graphqlRoot) Trace(ctx context.Context, args struct {
	ID  string
	Org *string
}) (*graphqlTrace, error) {
	id := graphqlIdentity(ctx)
	m, err := r.traces.Get(ctx, args.ID)
	if err != nil {
		return nil, err
	}
	orgID := m.OrgID
	if orgID == "" {
		orgID = domain.DefaultOrgID
	}
	if !r.canAccessOrg(ctx, id, orgID) {
		return nil, fmt.Errorf("forbidden")
	}
	return &graphqlTrace{meta: m}, nil
}

// canAccessOrg reports whether the caller may read orgID: the caller's own
// org-scoped token, a global admin, or a direct org membership (CWE-862).
func (r *graphqlRoot) canAccessOrg(ctx context.Context, id GraphQLIdentity, orgID string) bool {
	if id.OrgID == orgID {
		return true
	}
	if u, err := r.users.Get(ctx, id.UserID); err == nil && u.Role == domain.RoleAdmin {
		return true
	}
	orgs, err := r.orgs.OrgsForUser(ctx, id.UserID)
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

type graphqlUser struct {
	root *graphqlRoot
	user *domain.User
}

func (u *graphqlUser) ID() graphql.ID   { return graphql.ID(u.user.ID) }
func (u *graphqlUser) Nickname() string { return u.user.Username }
func (u *graphqlUser) Email() string    { return "" }

func (u *graphqlUser) Orgs(ctx context.Context) ([]*graphqlOrg, error) {
	id := graphqlIdentity(ctx)
	orgs, err := u.root.orgs.OrgsForUser(ctx, id.UserID)
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

func (o *graphqlOrg) ID() graphql.ID     { return graphql.ID(o.org.ID) }
func (o *graphqlOrg) Name() string       { return o.org.Name }
func (o *graphqlOrg) CreatedAt() string  { return o.org.CreatedAt.UTC().Format(time.RFC3339) }
func (o *graphqlOrg) Features() []string { return o.org.Features }
func (o *graphqlOrg) CliNotice() *string { return nil }

func (o *graphqlOrg) Subscription() *graphqlSubscription {
	return &graphqlSubscription{}
}

func (o *graphqlOrg) LastUserTrace(ctx context.Context) (*graphqlTrace, error) {
	rows, err := o.root.traces.List(ctx, &domain.TraceFilter{OrgID: o.org.ID, Limit: 1})
	if err != nil || len(rows) == 0 {
		return nil, nil
	}
	return &graphqlTrace{meta: &rows[0].TraceMeta}, nil
}

func (o *graphqlOrg) Traces(ctx context.Context, args struct {
	Filter *string
	Sort   *string
	First  *int32
	After  *string
}) (*graphqlTraceConnection, error) {
	limit := domain.DefaultTraceLimit
	if args.First != nil && *args.First > 0 {
		limit = int(*args.First)
	}
	rows, err := o.root.traces.List(ctx, &domain.TraceFilter{OrgID: o.org.ID, Limit: limit})
	if err != nil {
		return nil, err
	}
	nodes := make([]*graphqlTrace, 0, len(rows))
	for i := range rows {
		nodes = append(nodes, &graphqlTrace{meta: &rows[i].TraceMeta})
	}
	return &graphqlTraceConnection{nodes: nodes}, nil
}

type graphqlSubscription struct{}

func (s *graphqlSubscription) Status() string   { return "active" }
func (s *graphqlSubscription) PlanID() string   { return "self-hosted" }
func (s *graphqlSubscription) HasCaching() bool { return true }

type graphqlTrace struct {
	meta *domain.TraceMeta
}

func (t *graphqlTrace) ID() graphql.ID  { return graphql.ID(t.meta.TraceID) }
func (t *graphqlTrace) Status() string  { return t.meta.Status }
func (t *graphqlTrace) Project() string { return t.meta.ProjectName }
func (t *graphqlTrace) Version() string { return t.meta.Version }
func (t *graphqlTrace) DurationMS() int32 {
	if t.meta.DurationMS > int64(^uint32(0)>>1) {
		return int32(^uint32(0) >> 1)
	}
	return int32(t.meta.DurationMS) // #nosec G115 -- clamped above
}
func (t *graphqlTrace) StartedAt() string { return t.meta.StartedAt.UTC().Format(time.RFC3339) }
func (t *graphqlTrace) UpdatedAt() string { return t.meta.UpdatedAt.UTC().Format(time.RFC3339) }

type graphqlTraceConnection struct {
	nodes []*graphqlTrace
}

func (c *graphqlTraceConnection) Nodes() []*graphqlTrace     { return c.nodes }
func (c *graphqlTraceConnection) PageInfo() *graphqlPageInfo { return &graphqlPageInfo{} }

type graphqlPageInfo struct{}

func (p *graphqlPageInfo) HasNextPage() bool  { return false }
func (p *graphqlPageInfo) EndCursor() *string { return nil }
