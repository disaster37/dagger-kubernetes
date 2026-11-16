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
	g := NewCloudGraphQL(NewUserRepo(store), orgRepo, NewTraceMetaRepo(store), NewCheckRunRepo(store), cfg)
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
