package repository

import (
	"context"
	"strings"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func newGraphQLForTest(t *testing.T, cfg domain.GraphQLConfig) (*CloudGraphQL, context.Context) {
	t.Helper()
	store := newTestRaftStore(t)
	user := seedUser(t, store, "graphql-user")
	orgRepo := NewOrgRepo(store)
	if err := orgRepo.Create(context.Background(), &domain.Org{ID: "acme", Name: "acme", DisplayName: "Acme"}); err != nil {
		t.Fatal(err)
	}
	if err := orgRepo.SetMember(context.Background(), "acme", user.ID, domain.OrgMember); err != nil {
		t.Fatal(err)
	}
	g := NewCloudGraphQL(NewUserRepo(store), orgRepo, NewGroupRepo(store), NewTraceMetaRepo(store), NewCheckRunRepo(store), cfg)
	ctx := WithGraphQLIdentity(context.Background(), GraphQLIdentity{UserID: user.ID})
	return g, ctx
}

func TestCloudGraphQLIntrospectionDisabled(t *testing.T) {
	g, ctx := newGraphQLForTest(t, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10})
	out := string(g.Exec(ctx, `{ __schema { types { name } } }`, "", nil))
	if strings.Contains(out, "types") {
		t.Fatalf("introspection must be disabled: %s", out)
	}
}

func TestCloudGraphQLIntrospectionEnabled(t *testing.T) {
	g, ctx := newGraphQLForTest(t, domain.GraphQLConfig{IntrospectionEnabled: true, MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10})
	out := string(g.Exec(ctx, `{ __schema { types { name } } }`, "", nil))
	if strings.Contains(out, "errors") {
		t.Fatalf("introspection must be enabled: %s", out)
	}
}

func TestCloudGraphQLDepthAndLengthLimits(t *testing.T) {
	g, ctx := newGraphQLForTest(t, domain.GraphQLConfig{MaxDepth: 2, MaxSelections: 3, MaxQueryBytes: 40, MaxParallelism: 10})
	deep := `{ user { orgs { traces { nodes { id } } } } }`
	if out := string(g.Exec(ctx, deep, "", nil)); !strings.Contains(out, "errors") {
		t.Fatalf("deep query must be rejected: %s", out)
	}
	long := `{ user { id nickname email orgs { id name createdAt features } } }`
	if out := string(g.Exec(ctx, long, "", nil)); !strings.Contains(out, "errors") {
		t.Fatalf("long query must be rejected: %s", out)
	}
}

func TestCloudGraphQLTracesPagination(t *testing.T) {
	g, ctx := newGraphQLForTest(t, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10})
	out := string(g.Exec(ctx, `{ org(name: "acme") { traces(first: 1) { nodes { id } pageInfo { hasNextPage endCursor } } } }`, "", nil))
	if strings.Contains(out, "errors") {
		t.Fatalf("traces query failed: %s", out)
	}
}

// newQuickstartEnv builds a fresh env with no pre-existing memberships, so the
// caller has zero orgs (the dagger cloud login quickstart path).
func newQuickstartEnv(t *testing.T) (*CloudGraphQL, *RaftStore, string) {
	t.Helper()
	store := newTestRaftStore(t)
	user := seedUser(t, store, "graphql-user")
	users := NewUserRepo(store)
	u, _ := users.Get(context.Background(), user.ID)
	u.Role = domain.RoleAdmin
	if err := users.Update(context.Background(), u); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	orgRepo := NewOrgRepo(store)
	g := NewCloudGraphQL(users, orgRepo, NewGroupRepo(store), NewTraceMetaRepo(store), NewCheckRunRepo(store),
		domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10})
	g.WithOrgCreator(func(ctx context.Context, name, display string) (*domain.Org, error) {
		o := &domain.Org{ID: "org-0123456789abcdef", Name: name, DisplayName: display}
		return o, orgRepo.Create(ctx, o)
	})
	return g, store, user.ID
}

func TestCloudGraphQLCreateQuickstartOrg(t *testing.T) {
	g, store, userID := newQuickstartEnv(t)
	ctx := WithGraphQLIdentity(context.Background(), GraphQLIdentity{UserID: userID})

	out := string(g.Exec(ctx, `mutation { createQuickstartOrg(name: "My Org!") { id name } }`, "", nil))
	if strings.Contains(out, "errors") {
		t.Fatalf("createQuickstartOrg failed: %s", out)
	}
	if !strings.Contains(out, `"name":"my-org"`) {
		t.Fatalf("expected sanitized slug my-org: %s", out)
	}

	// The caller is now org_admin and the org appears in user.orgs.
	orgs, err := NewOrgRepo(store).OrgsForUser(context.Background(), userID)
	if err != nil || len(orgs) != 1 || !strings.HasPrefix(orgs[0].ID, "org-") || orgs[0].Name != "my-org" {
		t.Fatalf("OrgsForUser = %+v, err=%v; want one generated org named my-org", orgs, err)
	}
	members, _ := NewOrgRepo(store).Members(context.Background(), orgs[0].ID)
	if len(members) != 1 || members[0].UserID != userID || members[0].Role != domain.OrgAdmin {
		t.Fatalf("members = %+v, want caller as org_admin", members)
	}

	userOut := string(g.Exec(ctx, `{ user { orgs { id name } } }`, "", nil))
	if !strings.Contains(userOut, `"my-org"`) {
		t.Fatalf("user.orgs must include created org: %s", userOut)
	}
}

func TestCloudGraphQLCreateQuickstartOrgIdempotent(t *testing.T) {
	g, store, userID := newQuickstartEnv(t)
	ctx := WithGraphQLIdentity(context.Background(), GraphQLIdentity{UserID: userID})

	first := string(g.Exec(ctx, `mutation { createQuickstartOrg(name: "admin") { id name } }`, "", nil))
	if strings.Contains(first, "errors") || !strings.Contains(first, `"name":"admin"`) {
		t.Fatalf("first create failed: %s", first)
	}
	second := string(g.Exec(ctx, `mutation { createQuickstartOrg(name: "admin") { id name } }`, "", nil))
	if strings.Contains(second, "errors") || !strings.Contains(second, `"name":"admin"`) {
		t.Fatalf("second create must be idempotent: %s", second)
	}
	if strings.Contains(second, "admin-2") {
		t.Fatalf("idempotent retry must not create admin-2: %s", second)
	}
	orgs, _ := NewOrgRepo(store).List(context.Background())
	if len(orgs) != 1 || orgs[0].Name != "admin" {
		t.Fatalf("orgs = %+v, want exactly one admin org", orgs)
	}
}

func TestCloudGraphQLCreateQuickstartOrgCollision(t *testing.T) {
	store := newTestRaftStore(t)
	caller := seedUser(t, store, "graphql-user")
	other := seedUser(t, store, "other-user")
	orgRepo := NewOrgRepo(store)
	// "acme" belongs to another user.
	if err := orgRepo.Create(context.Background(), &domain.Org{ID: "org-1111111111111111", Name: "acme", DisplayName: "Acme"}); err != nil {
		t.Fatal(err)
	}
	if err := orgRepo.SetMember(context.Background(), "org-1111111111111111", other.ID, domain.OrgAdmin); err != nil {
		t.Fatal(err)
	}
	users := NewUserRepo(store)
	cu, _ := users.Get(context.Background(), caller.ID)
	cu.Role = domain.RoleAdmin
	if err := users.Update(context.Background(), cu); err != nil {
		t.Fatal(err)
	}
	g := NewCloudGraphQL(users, orgRepo, NewGroupRepo(store), NewTraceMetaRepo(store), NewCheckRunRepo(store),
		domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10})
	ctx := WithGraphQLIdentity(context.Background(), GraphQLIdentity{UserID: caller.ID})

	out := string(g.Exec(ctx, `mutation { createQuickstartOrg(name: "acme") { id name } }`, "", nil))
	if strings.Contains(out, "errors") || !strings.Contains(out, `"name":"acme"`) {
		t.Fatalf("existing slug must be returned idempotently: %s", out)
	}
	orgs, _ := orgRepo.List(context.Background())
	if len(orgs) != 1 {
		t.Fatalf("orgs = %+v, want no duplicate", orgs)
	}
}

func TestCloudGraphQLCreateQuickstartOrgUnauthenticated(t *testing.T) {
	g, _ := newGraphQLForTest(t, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10})
	out := string(g.Exec(context.Background(), `mutation { createQuickstartOrg(name: "x") { id name } }`, "", nil))
	if !strings.Contains(out, "errors") {
		t.Fatalf("unauthenticated createQuickstartOrg must error: %s", out)
	}
}

func TestCloudGraphQLGroupDerivedOrgs(t *testing.T) {
	store := newTestRaftStore(t)
	user := seedUser(t, store, "graphql-user")
	orgRepo := NewOrgRepo(store)
	groupRepo := NewGroupRepo(store)
	// The user is NOT a direct member of "teamorg", only a member of a group in it.
	if err := orgRepo.Create(context.Background(), &domain.Org{ID: "teamorg", Name: "teamorg", DisplayName: "Team"}); err != nil {
		t.Fatal(err)
	}
	if err := groupRepo.Create(context.Background(), &domain.Group{ID: "g1", Name: "g1", OrgID: "teamorg"}); err != nil {
		t.Fatal(err)
	}
	if err := groupRepo.SetMembers(context.Background(), "g1", []string{user.ID}); err != nil {
		t.Fatal(err)
	}
	g := NewCloudGraphQL(NewUserRepo(store), orgRepo, groupRepo, NewTraceMetaRepo(store), NewCheckRunRepo(store),
		domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10})
	ctx := WithGraphQLIdentity(context.Background(), GraphQLIdentity{UserID: user.ID})

	out := string(g.Exec(ctx, `{ user { orgs { id name } } }`, "", nil))
	if strings.Contains(out, "errors") || !strings.Contains(out, `"teamorg"`) {
		t.Fatalf("group-derived org must appear in user.orgs: %s", out)
	}
	// And the org is accessible via the org query.
	orgOut := string(g.Exec(ctx, `{ org(name: "teamorg") { id } }`, "", nil))
	if strings.Contains(orgOut, "errors") {
		t.Fatalf("group-derived org must be accessible: %s", orgOut)
	}
}

func TestCloudGraphQLCLINoticeNull(t *testing.T) {
	g, ctx := newGraphQLForTest(t, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10})
	out := string(g.Exec(ctx, `{ org(name: "acme") { cliNotice { level title body } } }`, "", nil))
	if strings.Contains(out, "errors") {
		t.Fatalf("cliNotice selection must not error: %s", out)
	}
	if !strings.Contains(out, `"cliNotice":null`) {
		t.Fatalf("cliNotice must be null: %s", out)
	}
}
