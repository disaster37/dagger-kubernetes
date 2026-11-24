package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// parityConfig is a permissive GraphQL config for parity tests.
func parityConfig() domain.GraphQLConfig {
	return domain.GraphQLConfig{MaxDepth: 30, MaxSelections: 1000, MaxQueryBytes: 1 << 20, MaxParallelism: 10}
}

// parityEnv builds a CloudGraphQL with an org the caller is a member of.
type parityEnv struct {
	g        *CloudGraphQL
	store    *RaftStore
	orgRepo  *OrgRepo
	user     *domain.User
	org      *domain.Org
	traces   *TraceMetaRepo
	checks   *CheckRunRepo
	forges   *ForgeSourceRepo
	projects *ProjectRepo
}

func newParityEnv(t *testing.T, withRerunner bool) *parityEnv {
	t.Helper()
	store := newTestRaftStore(t)
	user := seedUser(t, store, "parity-user")
	orgRepo := NewOrgRepo(store)
	org := &domain.Org{ID: "acme", Name: "acme", DisplayName: "Acme", Features: domain.DefaultOrgFeatures, CreatedAt: time.Now().UTC()}
	if err := orgRepo.Create(context.Background(), org); err != nil {
		t.Fatal(err)
	}
	if err := orgRepo.SetMember(context.Background(), "acme", user.ID, domain.OrgMember); err != nil {
		t.Fatal(err)
	}
	traceRepo := NewTraceMetaRepo(store)
	checkRepo := NewCheckRunRepo(store)
	forgeRepo := NewForgeSourceRepo(store)
	projectRepo := NewProjectRepo(store)
	g := NewCloudGraphQL(NewUserRepo(store), orgRepo, NewGroupRepo(store), traceRepo, checkRepo, parityConfig()).
		WithForgeSources(forgeRepo).WithProjects(projectRepo).WithPublicURL("https://example.com")
	if withRerunner {
		g.WithCheckRerunner(func(_ context.Context, runID string) (*domain.CheckRunRecord, error) {
			return &domain.CheckRunRecord{ID: runID, Status: "running"}, nil
		})
	}
	return &parityEnv{g: g, store: store, orgRepo: orgRepo, user: user, org: org, traces: traceRepo, checks: checkRepo, forges: forgeRepo, projects: projectRepo}
}

func (e *parityEnv) ctx() context.Context {
	return WithGraphQLIdentity(context.Background(), GraphQLIdentity{UserID: e.user.ID})
}

// exec runs a query and decodes the JSON response.
func execJSON(t *testing.T, g *CloudGraphQL, ctx context.Context, query string, variables map[string]any) map[string]any {
	t.Helper()
	raw := g.Exec(ctx, query, "", variables)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode response: %v (%s)", err, raw)
	}
	return out
}

func hasErrors(m map[string]any) bool {
	_, ok := m["errors"]
	return ok
}

func errorMessages(t *testing.T, m map[string]any) []string {
	t.Helper()
	raw, _ := m["errors"].([]any)
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		em, _ := e.(map[string]any)
		if msg, ok := em["message"].(string); ok {
			out = append(out, msg)
		}
	}
	return out
}

func seedTrace(t *testing.T, repo domain.TraceMetaRepository, m *domain.TraceMeta) {
	t.Helper()
	if m.StartedAt.IsZero() {
		m.StartedAt = time.Now().UTC()
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = m.StartedAt
	}
	if err := repo.UpsertIngest(context.Background(), m); err != nil {
		t.Fatalf("seed trace: %v", err)
	}
}

// --- Family A -----------------------------------------------------------------

func TestParityGetOrgDetails(t *testing.T) {
	e := newParityEnv(t, false)
	res := execJSON(t, e.g, e.ctx(), `{ org(name: "acme") { id name createdAt subscription { status trialStart trialEnd subscriptionID planID hasCaching } features { name status trialStart trialEnd } cliNotice { level title body } } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	org := res["data"].(map[string]any)["org"].(map[string]any)
	sub := org["subscription"].(map[string]any)
	if sub["subscriptionID"] != "self-hosted" || sub["planID"] != "self-hosted" || sub["hasCaching"] != true {
		t.Fatalf("subscription = %v", sub)
	}
	features := org["features"].([]any)
	if len(features) != 2 {
		t.Fatalf("features = %v", features)
	}
	first := features[0].(map[string]any)
	if first["status"] != "ACTIVE" {
		t.Fatalf("feature = %v", first)
	}
}

func TestParityOrgDetailsUnauthenticated(t *testing.T) {
	e := newParityEnv(t, false)
	res := execJSON(t, e.g, context.Background(), `{ org(name: "acme") { id } }`, nil)
	if !hasErrors(res) {
		t.Fatalf("unauthenticated must error: %v", res)
	}
}

func TestParityOrgDetailsForbidden(t *testing.T) {
	e := newParityEnv(t, false)
	other := seedUser(t, e.store, "other")
	otherCtx := WithGraphQLIdentity(context.Background(), GraphQLIdentity{UserID: other.ID})
	res := execJSON(t, e.g, otherCtx, `{ org(name: "acme") { id } }`, nil)
	if !hasErrors(res) {
		t.Fatalf("non-member must be forbidden: %v", res)
	}
}

// --- Family B -----------------------------------------------------------------

func TestParityCreatePortalSession(t *testing.T) {
	e := newParityEnv(t, false)
	res := execJSON(t, e.g, e.ctx(), `mutation { createPortalSession(org: "acme") }`, nil)
	if !hasErrors(res) {
		t.Fatalf("createPortalSession must error: %v", res)
	}
	msgs := errorMessages(t, res)
	if len(msgs) == 0 || !strings.Contains(msgs[0], "billing is not available") {
		t.Fatalf("message = %v", msgs)
	}
}

func TestParityCreatePortalSessionUnauthenticated(t *testing.T) {
	e := newParityEnv(t, false)
	res := execJSON(t, e.g, context.Background(), `mutation { createPortalSession(org: "acme") }`, nil)
	if !hasErrors(res) {
		t.Fatalf("unauthenticated must error: %v", res)
	}
}

// --- Family C -----------------------------------------------------------------

func TestParityBillingUsageZeroState(t *testing.T) {
	e := newParityEnv(t, false)
	res := execJSON(t, e.g, e.ctx(), `{ billingUsage(org: "acme", month: "2026-11") { month revision calculation asOf refreshStatus warnings pricing { planID } bill { totalUSD resources { resource } } } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	u := res["data"].(map[string]any)["billingUsage"].(map[string]any)
	if u["month"] != "2026-11" || u["revision"].(float64) != 0 {
		t.Fatalf("usage = %v", u)
	}
	if w, ok := u["warnings"].([]any); !ok || len(w) != 0 {
		t.Fatalf("warnings must be a non-nil empty list: %v", u["warnings"])
	}
	if u["pricing"] != nil || u["bill"] != nil {
		t.Fatalf("pricing/bill must be nil: %v", u)
	}
}

func TestParityBillingUsageInvalidMonth(t *testing.T) {
	e := newParityEnv(t, false)
	res := execJSON(t, e.g, e.ctx(), `{ billingUsage(org: "acme", month: "nope") { month } }`, nil)
	msgs := errorMessages(t, res)
	if len(msgs) == 0 || !strings.Contains(msgs[0], "invalid month") {
		t.Fatalf("message = %v", msgs)
	}
}

func TestParityBillingUsageNestedResolvers(t *testing.T) {
	u := &graphqlBillingUsage{month: "2026-11"}
	if u.Pricing() != nil || u.Bill() != nil {
		t.Fatal("zero-state pricing/bill must be nil")
	}
	p := &graphqlBillingUsagePricing{planID: "self-hosted", name: "Self-hosted", minimumUSD: 0}
	if p.PlanID() != "self-hosted" || p.Name() != "Self-hosted" || p.MinimumUSD() != 0 {
		t.Fatalf("pricing = %+v", p)
	}
	bill := &graphqlBillingUsageBill{}
	if got := bill.Resources(); got == nil {
		t.Fatal("bill.resources must be non-nil")
	}
	r := &graphqlBillingUsageResource{resource: "cpu", unit: "vCPU"}
	if r.Resource() != "cpu" || r.Unit() != "vCPU" {
		t.Fatalf("resource = %+v", r)
	}
	// exercise remaining accessor methods for coverage.
	_ = r.NewQuantity() + r.RepeatRan() + r.RepeatCached() + r.RepeatEstimated() +
		r.NewPrice() + r.RepeatPrice() + r.NewChargeUSD() + r.RepeatChargeUSD()
	_ = r.Estimated()
	_ = bill.UsageUSD() + bill.MinimumAdjustmentUSD() + bill.TotalUSD()
}

func TestParityBillingUsageUnauthorized(t *testing.T) {
	e := newParityEnv(t, false)
	other := seedUser(t, e.store, "outsider")
	res := execJSON(t, e.g, WithGraphQLIdentity(context.Background(), GraphQLIdentity{UserID: other.ID}), `{ billingUsage(org: "acme", month: "2026-11") { month } }`, nil)
	if !hasErrors(res) {
		t.Fatalf("outsider must be forbidden: %v", res)
	}
}

// --- Family D: traces ---------------------------------------------------------

func TestParityTraceListRunningVsTerminal(t *testing.T) {
	e := newParityEnv(t, false)
	base := time.Now().UTC().Add(-time.Hour)
	seedTrace(t, e.traces, &domain.TraceMeta{TraceID: "aaaa000000000001", OrgID: "acme", UserID: e.user.ID, ProjectName: "acme/api", Status: "", StartedAt: base, UpdatedAt: base})
	seedTrace(t, e.traces, &domain.TraceMeta{TraceID: "aaaa000000000002", OrgID: "acme", UserID: e.user.ID, ProjectName: "acme/api", Status: "success", DurationMS: 0, StartedAt: base.Add(time.Minute), UpdatedAt: base.Add(time.Minute)})
	seedTrace(t, e.traces, &domain.TraceMeta{TraceID: "aaaa000000000003", OrgID: "acme", UserID: e.user.ID, ProjectName: "acme/api", Status: "failed", FailureReason: "boom", DurationMS: 1500, StartedAt: base.Add(2 * time.Minute), UpdatedAt: base.Add(2 * time.Minute)})
	seedTrace(t, e.traces, &domain.TraceMeta{TraceID: "aaaa000000000004", OrgID: "acme", UserID: e.user.ID, ProjectName: "acme/api", Status: "success", CIProvider: "github", CIRepo: "acme/api", DurationMS: 10, StartedAt: base.Add(3 * time.Minute), UpdatedAt: base.Add(3 * time.Minute)})

	res := execJSON(t, e.g, e.ctx(), `{ org(name: "acme") { traces(sort: START, first: 10) { nodes { id status { code message } endTime local ci { isNativeCI provider repository } } } } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	nodes := res["data"].(map[string]any)["org"].(map[string]any)["traces"].(map[string]any)["nodes"].([]any)
	if len(nodes) != 4 {
		t.Fatalf("nodes = %d", len(nodes))
	}
	byID := map[string]map[string]any{}
	for _, n := range nodes {
		m := n.(map[string]any)
		byID[m["id"].(string)] = m
	}
	// running: status null, endTime null
	if byID["aaaa000000000001"]["status"] != nil || byID["aaaa000000000001"]["endTime"] != nil {
		t.Fatalf("running trace must have null status/endTime: %v", byID["aaaa000000000001"])
	}
	// success 0ms: endTime must still be non-nil
	if byID["aaaa000000000002"]["endTime"] == nil {
		t.Fatalf("success trace endTime must be non-nil: %v", byID["aaaa000000000002"])
	}
	if byID["aaaa000000000002"]["status"].(map[string]any)["code"] != "STATUS_CODE_OK" {
		t.Fatalf("success status = %v", byID["aaaa000000000002"]["status"])
	}
	// failed: error code + message
	failed := byID["aaaa000000000003"]
	if failed["status"].(map[string]any)["code"] != "STATUS_CODE_ERROR" || failed["status"].(map[string]any)["message"] != "boom" {
		t.Fatalf("failed status = %v", failed["status"])
	}
	// CI trace: local false + ci set
	if byID["aaaa000000000004"]["local"] != false {
		t.Fatalf("ci trace local = %v", byID["aaaa000000000004"]["local"])
	}
	ci := byID["aaaa000000000004"]["ci"].(map[string]any)
	if ci["provider"] != "github" || ci["repository"] != "acme/api" {
		t.Fatalf("ci = %v", ci)
	}
	if byID["aaaa000000000001"]["ci"] != nil {
		t.Fatalf("local trace ci must be nil: %v", byID["aaaa000000000001"])
	}
	if byID["aaaa000000000001"]["local"] != true {
		t.Fatalf("local trace local = %v", byID["aaaa000000000001"]["local"])
	}
}

func TestParityTraceListCursorRoundTrip(t *testing.T) {
	e := newParityEnv(t, false)
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		seedTrace(t, e.traces, &domain.TraceMeta{
			TraceID:     fmt.Sprintf("bbbb00000000000%d", i),
			OrgID:       "acme",
			UserID:      e.user.ID,
			ProjectName: "acme/api",
			Status:      "success",
			StartedAt:   base.Add(time.Duration(i) * time.Minute),
			UpdatedAt:   base.Add(time.Duration(i) * time.Minute),
		})
	}
	page1 := execJSON(t, e.g, e.ctx(), `{ org(name: "acme") { traces(sort: START, first: 2) { nodes { id } pageInfo { hasNextPage endCursor } } } }`, nil)
	pi := page1["data"].(map[string]any)["org"].(map[string]any)["traces"].(map[string]any)["pageInfo"].(map[string]any)
	if pi["hasNextPage"] != true {
		t.Fatalf("page1 must have next: %v", pi)
	}
	cursor := pi["endCursor"].(string)

	page2 := execJSON(t, e.g, e.ctx(), `query($after: Time) { org(name: "acme") { traces(sort: START, first: 2, after: $after) { nodes { id } pageInfo { hasNextPage endCursor } } } }`, map[string]any{"after": cursor})
	if hasErrors(page2) {
		t.Fatalf("page2 errors: %v", errorMessages(t, page2))
	}
	nodes2 := page2["data"].(map[string]any)["org"].(map[string]any)["traces"].(map[string]any)["nodes"].([]any)
	if len(nodes2) != 2 {
		t.Fatalf("page2 nodes = %d (cursor from %s)", len(nodes2), cursor)
	}
	// no overlap
	if nodes2[0].(map[string]any)["id"] == page1["data"].(map[string]any)["org"].(map[string]any)["traces"].(map[string]any)["nodes"].([]any)[0].(map[string]any)["id"] {
		t.Fatal("page2 must not overlap page1")
	}
}

func TestParityTraceListSortDurationCursorRejected(t *testing.T) {
	e := newParityEnv(t, false)
	cursor := encodeTimeCursor(2)
	res := execJSON(t, e.g, e.ctx(), `query($after: Time) { org(name: "acme") { traces(sort: DURATION, first: 2, after: $after) { nodes { id } } } }`, map[string]any{"after": cursor})
	msgs := errorMessages(t, res)
	if len(msgs) == 0 || !strings.Contains(msgs[0], "cursor pagination is not supported with DURATION sort") {
		t.Fatalf("message = %v", msgs)
	}
}

func TestParityTraceListInvalidSort(t *testing.T) {
	e := newParityEnv(t, false)
	// invalid enum value is rejected by schema validation itself.
	res := execJSON(t, e.g, e.ctx(), `{ org(name: "acme") { traces(sort: BOGUS) { nodes { id } } } }`, nil)
	if !hasErrors(res) {
		t.Fatalf("invalid sort enum must error: %v", res)
	}
}

func TestParityTraceListSortDuration(t *testing.T) {
	e := newParityEnv(t, false)
	base := time.Now().UTC().Add(-time.Hour)
	seedTrace(t, e.traces, &domain.TraceMeta{TraceID: "cccc000000000001", OrgID: "acme", UserID: e.user.ID, DurationMS: 100, Status: "success", StartedAt: base, UpdatedAt: base})
	seedTrace(t, e.traces, &domain.TraceMeta{TraceID: "cccc000000000002", OrgID: "acme", UserID: e.user.ID, DurationMS: 5000, Status: "success", StartedAt: base.Add(time.Minute), UpdatedAt: base.Add(time.Minute)})
	res := execJSON(t, e.g, e.ctx(), `{ org(name: "acme") { traces(sort: DURATION) { nodes { id durationMS } pageInfo { hasNextPage endCursor } } } }`, nil)
	nodes := res["data"].(map[string]any)["org"].(map[string]any)["traces"].(map[string]any)["nodes"].([]any)
	if len(nodes) < 2 || nodes[0].(map[string]any)["id"] != "cccc000000000002" {
		t.Fatalf("DURATION sort must order longest first: %v", nodes)
	}
	pi := res["data"].(map[string]any)["org"].(map[string]any)["traces"].(map[string]any)["pageInfo"].(map[string]any)
	if pi["hasNextPage"] != false || pi["endCursor"] != nil {
		t.Fatalf("DURATION single page: %v", pi)
	}
}

func TestParityTraceListFilters(t *testing.T) {
	e := newParityEnv(t, false)
	other := seedUser(t, e.store, "trace-other")
	base := time.Now().UTC().Add(-time.Hour)
	seedTrace(t, e.traces, &domain.TraceMeta{TraceID: "dddd000000000001", OrgID: "acme", UserID: e.user.ID, ProjectName: "acme/api", Status: "success", DurationMS: 2000, StartedAt: base, UpdatedAt: base})
	seedTrace(t, e.traces, &domain.TraceMeta{TraceID: "dddd000000000002", OrgID: "acme", UserID: other.ID, ProjectName: "acme/web", Status: "success", DurationMS: 100, StartedAt: base.Add(time.Minute), UpdatedAt: base.Add(time.Minute)})
	seedTrace(t, e.traces, &domain.TraceMeta{TraceID: "dddd000000000003", OrgID: "acme", UserID: e.user.ID, ProjectName: "acme/api", Status: "failed", DurationMS: 500, StartedAt: base.Add(2 * time.Minute), UpdatedAt: base.Add(2 * time.Minute)})

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"status passed", `{ org(name: "acme") { traces(filter: {status: "PASSED"}) { nodes { id } } } }`, []string{"dddd000000000001", "dddd000000000002"}},
		{"mine true", `{ org(name: "acme") { traces(filter: {mine: true}) { nodes { id } } } }`, []string{"dddd000000000001", "dddd000000000003"}},
		{"local true", `{ org(name: "acme") { traces(filter: {local: true}) { nodes { id } } } }`, []string{"dddd000000000001", "dddd000000000002", "dddd000000000003"}},
		{"name glob", `{ org(name: "acme") { traces(filter: {name: "acme/a*"}) { nodes { id } } } }`, []string{"dddd000000000001", "dddd000000000003"}},
		{"repos filter", `{ org(name: "acme") { traces(filter: {repos: ["acme/web"]}) { nodes { id } } } }`, []string{"dddd000000000002"}},
		{"minDuration", `{ org(name: "acme") { traces(filter: {minDuration: 1}) { nodes { id } } } }`, []string{"dddd000000000001"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := execJSON(t, e.g, e.ctx(), tc.query, nil)
			if hasErrors(res) {
				t.Fatalf("errors: %v", errorMessages(t, res))
			}
			nodes := res["data"].(map[string]any)["org"].(map[string]any)["traces"].(map[string]any)["nodes"].([]any)
			got := map[string]bool{}
			for _, n := range nodes {
				got[n.(map[string]any)["id"].(string)] = true
			}
			for _, want := range tc.want {
				if !got[want] {
					t.Fatalf("%s: missing %s in %v", tc.name, want, got)
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestParityTraceListUserFilter(t *testing.T) {
	e := newParityEnv(t, false)
	seedTrace(t, e.traces, &domain.TraceMeta{TraceID: "eeee000000000001", OrgID: "acme", UserID: e.user.ID, ProjectName: "acme/api", Status: "success"})
	res := execJSON(t, e.g, e.ctx(), `{ org(name: "acme") { traces(filter: {user: "PARITY-USER"}) { nodes { id } } } }`, nil)
	nodes := res["data"].(map[string]any)["org"].(map[string]any)["traces"].(map[string]any)["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("user filter (case-insensitive username) = %v", nodes)
	}
}

func TestParityTraceListSinceUntil(t *testing.T) {
	e := newParityEnv(t, false)
	base := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	seedTrace(t, e.traces, &domain.TraceMeta{TraceID: "ffff000000000001", OrgID: "acme", UserID: e.user.ID, Status: "success", StartedAt: base, UpdatedAt: base})
	since := base.Add(time.Hour).Format(time.RFC3339Nano)
	until := base.Add(-time.Hour).Format(time.RFC3339Nano)
	res := execJSON(t, e.g, e.ctx(), `query($s: Time, $u: Time) { org(name: "acme") { traces(filter: {since: $s, until: $u}) { nodes { id } } } }`, map[string]any{"s": since, "u": until})
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	nodes := res["data"].(map[string]any)["org"].(map[string]any)["traces"].(map[string]any)["nodes"].([]any)
	if len(nodes) != 0 {
		t.Fatalf("since>trace must exclude: %v", nodes)
	}
}

func TestParityGetTraceMetadata(t *testing.T) {
	e := newParityEnv(t, false)
	base := time.Now().UTC().Add(-time.Hour)
	seedTrace(t, e.traces, &domain.TraceMeta{TraceID: "abcd000000000001", OrgID: "acme", UserID: e.user.ID, ProjectName: "acme/api", Status: "success", StartedAt: base, UpdatedAt: base})
	res := execJSON(t, e.g, e.ctx(), `{ trace(id: "abcd000000000001", org: "acme") { id name status { code } timestamp } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	tr := res["data"].(map[string]any)["trace"]
	if tr == nil {
		t.Fatal("trace must resolve")
	}
	// unknown trace -> null
	missing := execJSON(t, e.g, e.ctx(), `{ trace(id: "abcd0000000000ff", org: "acme") { id } }`, nil)
	if hasErrors(missing) || missing["data"].(map[string]any)["trace"] != nil {
		t.Fatalf("missing trace must be null: %v", missing)
	}
	// org mismatch (no explicit org arg) is forbidden -> null (anti-enumeration)
	other := seedUser(t, e.store, "trace-snoop")
	snoop := execJSON(t, e.g, WithGraphQLIdentity(context.Background(), GraphQLIdentity{UserID: other.ID}), `{ trace(id: "abcd000000000001") { id } }`, nil)
	if hasErrors(snoop) {
		t.Fatalf("org mismatch must not error, must return null: %v", snoop)
	}
	if snoop["data"].(map[string]any)["trace"] != nil {
		t.Fatalf("org mismatch must be null: %v", snoop)
	}
	// explicit inaccessible org arg is forbidden
	forbidden := execJSON(t, e.g, WithGraphQLIdentity(context.Background(), GraphQLIdentity{UserID: other.ID}), `{ trace(id: "abcd000000000001", org: "acme") { id } }`, nil)
	if !hasErrors(forbidden) {
		t.Fatalf("inaccessible org arg must be forbidden: %v", forbidden)
	}
}

func TestParityLastUserTrace(t *testing.T) {
	e := newParityEnv(t, false)
	base := time.Now().UTC().Add(-time.Hour)
	seedTrace(t, e.traces, &domain.TraceMeta{TraceID: "1234000000000001", OrgID: "acme", UserID: e.user.ID, ProjectName: "acme/api", Status: "success", StartedAt: base, UpdatedAt: base})
	seedTrace(t, e.traces, &domain.TraceMeta{TraceID: "1234000000000002", OrgID: "acme", UserID: e.user.ID, ProjectName: "acme/api", Status: "success", StartedAt: base.Add(time.Minute), UpdatedAt: base.Add(time.Minute)})
	res := execJSON(t, e.g, e.ctx(), `{ org(name: "acme") { lastUserTrace { id } } }`, nil)
	last := res["data"].(map[string]any)["org"].(map[string]any)["lastUserTrace"].(map[string]any)
	if last["id"] != "1234000000000002" {
		t.Fatalf("lastUserTrace = %v", last)
	}
}

func TestParityLastUserTraceEmpty(t *testing.T) {
	e := newParityEnv(t, false)
	res := execJSON(t, e.g, e.ctx(), `{ org(name: "acme") { lastUserTrace { id } } }`, nil)
	if res["data"].(map[string]any)["org"].(map[string]any)["lastUserTrace"] != nil {
		t.Fatalf("empty lastUserTrace must be null: %v", res)
	}
}

func TestGraphQLTimeScalarRoundTrip(t *testing.T) {
	var ts graphqlTime
	now := time.Now().UTC()
	raw, _ := json.Marshal(ts)
	if string(raw) != "null" {
		t.Fatalf("zero time marshal = %s", raw)
	}
	if err := ts.UnmarshalGraphQL(now); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(ts)
	want := now.Format(time.RFC3339Nano)
	if !strings.Contains(string(raw), want[:19]) {
		t.Fatalf("marshal = %s want %s", raw, want)
	}
	var cursor graphqlTime
	if err := cursor.UnmarshalGraphQL(encodeTimeCursor(3)); err != nil || cursor.cursor == "" {
		t.Fatalf("cursor unmarshal = %+v err=%v", cursor, err)
	}
	if err := cursor.UnmarshalGraphQL([]byte(now.Format(time.RFC3339Nano))); err != nil {
		t.Fatalf("bytes unmarshal: %v", err)
	}
	if err := (func() error { var x graphqlTime; return x.UnmarshalGraphQL("") })(); err == nil {
		t.Fatal("empty must error")
	}
	if err := (func() error { var x graphqlTime; return x.UnmarshalGraphQL(int(1)) })(); err == nil {
		t.Fatal("unsupported type must error")
	}
	if err := (func() error { var x graphqlTime; return x.UnmarshalGraphQL("!!!not-base64!!!") })(); err == nil {
		t.Fatal("bad cursor must error")
	}
	if !(graphqlTime{}).ImplementsGraphQLType("Time") || (graphqlTime{}).ImplementsGraphQLType("Other") {
		t.Fatal("ImplementsGraphQLType")
	}
}

// --- Family E: checks ---------------------------------------------------------

func seedForgeSource(t *testing.T, repo domain.ForgeSourceRepository, src *domain.ForgeSource) {
	t.Helper()
	if src.CreatedAt.IsZero() {
		src.CreatedAt = time.Now().UTC()
	}
	if err := repo.Upsert(context.Background(), src); err != nil {
		t.Fatalf("seed forge source: %v", err)
	}
}

func seedCheckRun(t *testing.T, repo domain.CheckRunRepository, rec *domain.CheckRunRecord) {
	t.Helper()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	if err := repo.Upsert(context.Background(), rec); err != nil {
		t.Fatalf("seed check run: %v", err)
	}
}

func TestParityOrgChecksUnionVariants(t *testing.T) {
	e := newParityEnv(t, false)
	now := time.Now().UTC().Add(-time.Minute)
	seedCheckRun(t, e.checks, &domain.CheckRunRecord{
		ID: "check-pr", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/api",
		SHA: "sha-pr", Ref: "refs/pull/7/head", PRNumber: 7, Status: "success", Event: "pull_request",
		CreatedAt: now, UpdatedAt: now,
		Checks: []domain.CheckItem{{Name: "test", State: domain.CheckSuccess, TraceID: "t1", UpdatedAt: now}},
	})
	seedCheckRun(t, e.checks, &domain.CheckRunRecord{
		ID: "check-tag", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/api",
		SHA: "sha-tag", Tag: "v1.0.0", Ref: "refs/tags/v1.0.0", Status: "failed", Event: "tag",
		CreatedAt: now, UpdatedAt: now,
	})
	seedCheckRun(t, e.checks, &domain.CheckRunRecord{
		ID: "check-branch", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/api",
		SHA: "sha-branch", Ref: "refs/heads/main", Status: "running", Event: "push",
		CreatedAt: now, UpdatedAt: now,
	})

	res := execJSON(t, e.g, e.ctx(), `{ org(name: "acme") { checks(first: 10) { nodes { repo commitSHA events { provider type } refs { ... on CheckCommitTagRef { name } ... on CheckCommitBranchRef { name } ... on CheckCommitPullRequestRef { number state } } checks { id name status traceId moduleRef moduleVersion internal } org { id name } } } } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	nodes := res["data"].(map[string]any)["org"].(map[string]any)["checks"].(map[string]any)["nodes"].([]any)
	if len(nodes) != 3 {
		t.Fatalf("nodes = %d", len(nodes))
	}
	kinds := map[string]bool{}
	for _, n := range nodes {
		m := n.(map[string]any)
		refs := m["refs"].([]any)
		if len(refs) != 1 {
			t.Fatalf("each commit has one ref: %v", refs)
		}
		ref := refs[0].(map[string]any)
		if _, ok := ref["number"]; ok {
			kinds["pr"] = true
		}
		if _, ok := ref["state"]; ok {
			kinds["pr"] = true
		}
		if v, ok := ref["name"]; ok {
			if v == "v1.0.0" {
				kinds["tag"] = true
			}
			if v == "main" {
				kinds["branch"] = true
			}
		}
		if m["org"].(map[string]any)["name"] != "acme" {
			t.Fatalf("org name = %v", m["org"])
		}
	}
	for _, k := range []string{"pr", "tag", "branch"} {
		if !kinds[k] {
			t.Fatalf("missing %s ref; got %v", k, kinds)
		}
	}
}

func TestParityOrgChecksLoadFallback(t *testing.T) {
	e := newParityEnv(t, false)
	seedCheckRun(t, e.checks, &domain.CheckRunRecord{ID: "check-load", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/api", SHA: "sha", Status: "pending", CreatedAt: time.Now().UTC()})
	res := execJSON(t, e.g, e.ctx(), `{ org(name: "acme") { checks(first: 10) { nodes { checks { id name status internal } } } } }`, nil)
	nodes := res["data"].(map[string]any)["org"].(map[string]any)["checks"].(map[string]any)["nodes"].([]any)
	checks := nodes[0].(map[string]any)["checks"].([]any)
	if len(checks) != 1 {
		t.Fatalf("checks = %v", checks)
	}
	c := checks[0].(map[string]any)
	if c["name"] != "load" || c["internal"] != true || c["status"] != "pending" {
		t.Fatalf("synthesized load check = %v", c)
	}
}

func TestParityUserChecks(t *testing.T) {
	e := newParityEnv(t, false)
	seedCheckRun(t, e.checks, &domain.CheckRunRecord{ID: "check-u1", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/api", SHA: "s1", Status: "success", CreatedAt: time.Now().UTC(), Checks: []domain.CheckItem{{Name: "test", State: domain.CheckSuccess}}})
	res := execJSON(t, e.g, e.ctx(), `{ user { checks(first: 10) { nodes { repo checks { name } } } } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	nodes := res["data"].(map[string]any)["user"].(map[string]any)["checks"].(map[string]any)["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("user checks = %v", nodes)
	}
}

func TestParityUserChecksGlobalCap(t *testing.T) {
	e := newParityEnv(t, false)
	other := &domain.Org{ID: "beta", Name: "beta", DisplayName: "Beta", Features: domain.DefaultOrgFeatures, CreatedAt: time.Now().UTC()}
	if err := e.orgRepo.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if err := e.orgRepo.SetMember(context.Background(), "beta", e.user.ID, domain.OrgMember); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		seedCheckRun(t, e.checks, &domain.CheckRunRecord{ID: fmt.Sprintf("check-a%d", i), OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/api", SHA: "s", Status: "success", CreatedAt: time.Now().UTC()})
		seedCheckRun(t, e.checks, &domain.CheckRunRecord{ID: fmt.Sprintf("check-b%d", i), OrgID: "beta", Provider: domain.ForgeGitHub, Repo: "beta/api", SHA: "s", Status: "success", CreatedAt: time.Now().UTC()})
	}
	// first (2) is smaller than each org's per-org total (3): the cap is global.
	res := execJSON(t, e.g, e.ctx(), `{ user { checks(first: 2) { nodes { repo } } } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	nodes := res["data"].(map[string]any)["user"].(map[string]any)["checks"].(map[string]any)["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("global cap nodes = %d, want 2: %v", len(nodes), nodes)
	}
}

func TestParityModuleChecks(t *testing.T) {
	e := newParityEnv(t, false)
	seedCheckRun(t, e.checks, &domain.CheckRunRecord{ID: "check-mod", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/mod", SHA: "abc123", Ref: "refs/heads/main", Status: "success", CreatedAt: time.Now().UTC()})
	res := execJSON(t, e.g, e.ctx(), `{ org(name: "acme") { moduleChecks(moduleRef: "github.com/acme/mod", moduleVersion: "abc123") { repo commitSHA } } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	arr := res["data"].(map[string]any)["org"].(map[string]any)["moduleChecks"].([]any)
	if len(arr) != 1 {
		t.Fatalf("moduleChecks = %v", arr)
	}
}

func TestParityRerunChecksEnabled(t *testing.T) {
	e := newParityEnv(t, true)
	seedCheckRun(t, e.checks, &domain.CheckRunRecord{ID: "check-rerun", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/api", SHA: "s", Status: "failed", CreatedAt: time.Now().UTC(), Checks: []domain.CheckItem{{Name: "test", State: domain.CheckFailure}}})
	res := execJSON(t, e.g, e.ctx(), `mutation { rerunChecks(org: "acme", checkIds: ["check-rerun/test"], cleanSlate: true) { id name status } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	arr := res["data"].(map[string]any)["rerunChecks"].([]any)
	if len(arr) != 1 {
		t.Fatalf("rerunChecks = %v", arr)
	}
	if arr[0].(map[string]any)["status"] != "pending" {
		t.Fatalf("rerun status = %v", arr[0])
	}
}

func TestParityRerunChecksDisabled(t *testing.T) {
	e := newParityEnv(t, false)
	res := execJSON(t, e.g, e.ctx(), `mutation { rerunChecks(org: "acme", checkIds: ["a/load"]) { id } }`, nil)
	msgs := errorMessages(t, res)
	if len(msgs) == 0 || !strings.Contains(msgs[0], "check rerun is not available") {
		t.Fatalf("message = %v", msgs)
	}
}

func TestParityRerunChecksIDOR(t *testing.T) {
	e := newParityEnv(t, true)
	// A run in another org must not be reachable.
	seedCheckRun(t, e.checks, &domain.CheckRunRecord{ID: "check-other", OrgID: "otheracme", Provider: domain.ForgeGitHub, Repo: "x/y", SHA: "s", Status: "failed", CreatedAt: time.Now().UTC()})
	res := execJSON(t, e.g, e.ctx(), `mutation { rerunChecks(org: "acme", checkIds: ["check-other/load"]) { id } }`, nil)
	msgs := errorMessages(t, res)
	if len(msgs) == 0 || !strings.Contains(msgs[0], "check not found") {
		t.Fatalf("cross-org must be check not found: %v", msgs)
	}
}

func TestParityRerunLoad(t *testing.T) {
	e := newParityEnv(t, true)
	seedCheckRun(t, e.checks, &domain.CheckRunRecord{ID: "check-loadfail", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/api", SHA: "s", Status: "failed", CreatedAt: time.Now().UTC()})
	res := execJSON(t, e.g, e.ctx(), `mutation { rerunLoad(org: "acme", checkId: "check-loadfail/load") { id name status internal } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	c := res["data"].(map[string]any)["rerunLoad"].(map[string]any)
	if c["name"] != "load" || c["status"] != "pending" || c["internal"] != true {
		t.Fatalf("rerunLoad = %v", c)
	}
}

func TestParityRerunLoadNotLoad(t *testing.T) {
	e := newParityEnv(t, true)
	seedCheckRun(t, e.checks, &domain.CheckRunRecord{ID: "check-x", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/api", SHA: "s", Status: "failed", CreatedAt: time.Now().UTC()})
	res := execJSON(t, e.g, e.ctx(), `mutation { rerunLoad(org: "acme", checkId: "check-x/test") { id } }`, nil)
	msgs := errorMessages(t, res)
	if len(msgs) == 0 || !strings.Contains(msgs[0], "only a failed load check") {
		t.Fatalf("message = %v", msgs)
	}
}

func TestParityCheckHelpers(t *testing.T) {
	if got := canonicalCheckRepo(domain.ForgeGitHub, "acme/api"); got != "github.com/acme/api" {
		t.Fatalf("canonical github short = %q", got)
	}
	if got := canonicalCheckRepo(domain.ForgeGitHub, "github.com/acme/api"); got != "github.com/acme/api" {
		t.Fatalf("canonical full = %q", got)
	}
	if got := canonicalCheckRepo(domain.ForgeGitLab, "group/proj"); got != "gitlab.com/group/proj" {
		t.Fatalf("canonical gitlab = %q", got)
	}
	if got := canonicalCheckModuleRef(&domain.CheckRunRecord{Provider: domain.ForgeGitHub, Repo: "https://github.com/acme/api.git"}); got != "github.com/acme/api" {
		t.Fatalf("module ref = %q", got)
	}
	if !sameNormalizedRepo("github.com/Acme/Api", "acme/api") {
		t.Fatal("sameNormalizedRepo must fold host+case")
	}
	if runID, name := splitCheckID("run-1/load"); runID != "run-1" || name != "load" {
		t.Fatalf("splitCheckID = %q/%q", runID, name)
	}
}

// --- Family F: integrations ---------------------------------------------------

func TestParitySources(t *testing.T) {
	e := newParityEnv(t, false)
	seedForgeSource(t, e.forges, &domain.ForgeSource{ID: "src-gh", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/api", InstallationID: "12345"})
	seedForgeSource(t, e.forges, &domain.ForgeSource{ID: "src-gl", OrgID: "acme", Provider: domain.ForgeGitLab, Repo: "group/proj", BaseURL: "https://gitlab.com/api/v4"})
	res := execJSON(t, e.g, e.ctx(), `{ sources { id name type configuredAt orgId orgName configUrl } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	sources := res["data"].(map[string]any)["sources"].([]any)
	if len(sources) != 2 {
		t.Fatalf("sources = %v", sources)
	}
	byID := map[string]map[string]any{}
	for _, s := range sources {
		m := s.(map[string]any)
		byID[m["id"].(string)] = m
	}
	if !strings.Contains(byID["src-gh"]["configUrl"].(string), "installations/12345") {
		t.Fatalf("gh configUrl = %v", byID["src-gh"]["configUrl"])
	}
	if byID["src-gh"]["orgName"] != "acme" || byID["src-gh"]["orgId"] != "acme" {
		t.Fatalf("gh org = %v", byID["src-gh"])
	}
	if !strings.Contains(byID["src-gl"]["configUrl"].(string), "gitlab.com/group/proj") {
		t.Fatalf("gl configUrl = %v", byID["src-gl"]["configUrl"])
	}
}

func TestParityGithubConnection(t *testing.T) {
	e := newParityEnv(t, false)
	// no GitHub forge source in any accessible org -> null
	res := execJSON(t, e.g, e.ctx(), `{ githubConnection { githubLogin connectedAt } }`, nil)
	if res["data"].(map[string]any)["githubConnection"] != nil {
		t.Fatalf("no-source connection must be null: %v", res)
	}
	// a GitHub forge source in an accessible org -> derived connection
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	seedForgeSource(t, e.forges, &domain.ForgeSource{ID: "src-gh", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "disaster37/api", CreatedAt: at})
	res = execJSON(t, e.g, e.ctx(), `{ githubConnection { githubLogin connectedAt } }`, nil)
	conn := res["data"].(map[string]any)["githubConnection"].(map[string]any)
	if conn["githubLogin"] != "disaster37" {
		t.Fatalf("githubLogin = %v", conn)
	}
	if conn["connectedAt"] != at.Format(time.RFC3339) {
		t.Fatalf("connectedAt = %v", conn)
	}
}

func TestParityGithubConnectionForgeScoping(t *testing.T) {
	e := newParityEnv(t, false)
	// Non-GitHub sources and an inaccessible org's GitHub source must not leak.
	seedForgeSource(t, e.forges, &domain.ForgeSource{ID: "src-gl", OrgID: "acme", Provider: domain.ForgeGitLab, Repo: "group/proj"})
	other := &domain.Org{ID: "beta", Name: "beta", DisplayName: "Beta", Features: domain.DefaultOrgFeatures, CreatedAt: time.Now().UTC()}
	if err := e.orgRepo.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	seedForgeSource(t, e.forges, &domain.ForgeSource{ID: "src-gh-beta", OrgID: "beta", Provider: domain.ForgeGitHub, Repo: "other/repo"})
	res := execJSON(t, e.g, e.ctx(), `{ githubConnection { githubLogin } }`, nil)
	if res["data"].(map[string]any)["githubConnection"] != nil {
		t.Fatalf("inaccessible org's source must not leak: %v", res)
	}
}

func TestParityGithubOAuthURL(t *testing.T) {
	e := newParityEnv(t, false)
	res := execJSON(t, e.g, e.ctx(), `{ githubOAuthURL(redirectURI: "https://example.com/cb") }`, nil)
	msgs := errorMessages(t, res)
	if len(msgs) == 0 || !strings.Contains(msgs[0], "GitHub OAuth app integration is not configured") {
		t.Fatalf("message = %v", msgs)
	}
}

func TestParitySourceRepositories(t *testing.T) {
	e := newParityEnv(t, false)
	seedForgeSource(t, e.forges, &domain.ForgeSource{ID: "src-gh", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/api", InstallationID: "999"})
	res := execJSON(t, e.g, e.ctx(), `{ sourceRepositories(installationId: "999", org: "acme") { repository selected eligible claimedByOrgName } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	repos := res["data"].(map[string]any)["sourceRepositories"].([]any)
	if len(repos) != 1 {
		t.Fatalf("sourceRepositories = %v", repos)
	}
	// unknown installation -> empty
	res = execJSON(t, e.g, e.ctx(), `{ sourceRepositories(installationId: "nope") { repository } }`, nil)
	if len(res["data"].(map[string]any)["sourceRepositories"].([]any)) != 0 {
		t.Fatalf("unknown installation must be empty: %v", res)
	}
}

func TestParitySourceRepositoriesOrgScoped(t *testing.T) {
	e := newParityEnv(t, false)
	other := &domain.Org{ID: "beta", Name: "beta", DisplayName: "Beta", Features: domain.DefaultOrgFeatures, CreatedAt: time.Now().UTC()}
	if err := e.orgRepo.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if err := e.orgRepo.SetMember(context.Background(), "beta", e.user.ID, domain.OrgMember); err != nil {
		t.Fatal(err)
	}
	seedForgeSource(t, e.forges, &domain.ForgeSource{ID: "src-beta", OrgID: "beta", Provider: domain.ForgeGitHub, Repo: "beta/api", InstallationID: "111"})
	// An unscoped call sees the other org's source...
	res := execJSON(t, e.g, e.ctx(), `{ sourceRepositories(installationId: "111") { repository claimedByOrgName } }`, nil)
	if len(res["data"].(map[string]any)["sourceRepositories"].([]any)) != 1 {
		t.Fatalf("unscoped sourceRepositories = %v", res)
	}
	// ...but scoping to acme must not expose beta's source.
	res = execJSON(t, e.g, e.ctx(), `{ sourceRepositories(installationId: "111", org: "acme") { repository } }`, nil)
	if len(res["data"].(map[string]any)["sourceRepositories"].([]any)) != 0 {
		t.Fatalf("org-scoped sourceRepositories must not leak: %v", res)
	}
}

func TestParityGitLabSources(t *testing.T) {
	e := newParityEnv(t, false)
	seedForgeSource(t, e.forges, &domain.ForgeSource{ID: "src-gl", OrgID: "acme", Provider: domain.ForgeGitLab, Repo: "group/sub/proj", BaseURL: "https://gitlab.example.com/api/v4"})
	seedForgeSource(t, e.forges, &domain.ForgeSource{ID: "src-gh", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/api"})
	res := execJSON(t, e.g, e.ctx(), `{ gitlabOrganizationSources(org: "acme") { id instanceURL namespacePath access(org: "acme") { connectionID } } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	arr := res["data"].(map[string]any)["gitlabOrganizationSources"].([]any)
	if len(arr) != 1 {
		t.Fatalf("gitlab sources = %v", arr)
	}
	m := arr[0].(map[string]any)
	if m["instanceURL"] != "https://gitlab.example.com" {
		t.Fatalf("instanceURL = %v", m["instanceURL"])
	}
	if m["namespacePath"] != "group/sub" {
		t.Fatalf("namespacePath = %v", m["namespacePath"])
	}
	if m["access"].(map[string]any)["connectionID"] != "src-gl" {
		t.Fatalf("access = %v", m["access"])
	}

	// gitlabSources alias
	res = execJSON(t, e.g, e.ctx(), `{ gitlabSources(org: "acme") { id } }`, nil)
	if len(res["data"].(map[string]any)["gitlabSources"].([]any)) != 1 {
		t.Fatalf("gitlabSources alias = %v", res)
	}
}

func TestParityUseGitLabAccount(t *testing.T) {
	e := newParityEnv(t, false)
	res := execJSON(t, e.g, e.ctx(), `mutation { useGitLabAccount(org: "acme", source: "src") { connectionID } }`, nil)
	msgs := errorMessages(t, res)
	if len(msgs) == 0 || !strings.Contains(msgs[0], "personal GitLab account linking is not supported") {
		t.Fatalf("message = %v", msgs)
	}
}

func TestParityEntire(t *testing.T) {
	e := newParityEnv(t, false)
	res := execJSON(t, e.g, e.ctx(), `{ entireInstallations(org: "acme") { id name manageURL } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	if len(res["data"].(map[string]any)["entireInstallations"].([]any)) != 0 {
		t.Fatalf("entireInstallations must be empty: %v", res)
	}
	res = execJSON(t, e.g, e.ctx(), `{ entireInstallURL(org: "acme", redirectURI: "https://example.com") }`, nil)
	msgs := errorMessages(t, res)
	if len(msgs) == 0 || !strings.Contains(msgs[0], "Entire integration is not available") {
		t.Fatalf("message = %v", msgs)
	}
}

func TestParityUserRepositories(t *testing.T) {
	e := newParityEnv(t, false)
	seedForgeSource(t, e.forges, &domain.ForgeSource{ID: "src-gh", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/api", InstallationID: "42"})
	res := execJSON(t, e.g, e.ctx(), `{ user { repositories(refs: ["github.com/acme/api"]) { ref settings { repo isPublic effectiveRunChecksOnDefaultBranch effectiveRunChecksOnPullRequest } mappedSource { provider sourceName installationId configUrl org { id name } } } } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	repos := res["data"].(map[string]any)["user"].(map[string]any)["repositories"].([]any)
	if len(repos) != 1 {
		t.Fatalf("repositories = %v", repos)
	}
	m := repos[0].(map[string]any)
	if m["ref"] != "github.com/acme/api" {
		t.Fatalf("ref = %v", m["ref"])
	}
	if m["mappedSource"].(map[string]any)["provider"] != "GITHUB" {
		t.Fatalf("mappedSource = %v", m["mappedSource"])
	}
	// non-matching ref filter -> empty
	res = execJSON(t, e.g, e.ctx(), `{ user { repositories(refs: ["other.com/x/y"]) { ref } } }`, nil)
	if len(res["data"].(map[string]any)["user"].(map[string]any)["repositories"].([]any)) != 0 {
		t.Fatalf("filtered repositories = %v", res)
	}
}

// --- Family G: repos / settings ----------------------------------------------

func TestParityGetRepos(t *testing.T) {
	e := newParityEnv(t, false)
	if err := e.projects.Create(context.Background(), &domain.Project{ID: "p1", OrgID: "acme", Name: "github.com/acme/api"}); err != nil {
		t.Fatal(err)
	}
	seedForgeSource(t, e.forges, &domain.ForgeSource{ID: "src", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/web"})
	res := execJSON(t, e.g, e.ctx(), `{ getRepos { name fullName private } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	repos := res["data"].(map[string]any)["getRepos"].([]any)
	if len(repos) != 2 {
		t.Fatalf("getRepos = %v", repos)
	}
	names := map[string]bool{}
	for _, r := range repos {
		m := r.(map[string]any)
		names[m["name"].(string)] = true
		if m["fullName"] == "github.com/acme/api" {
			t.Fatalf("fullName host must be stripped: %v", m)
		}
	}
	if !names["api"] || !names["web"] {
		t.Fatalf("names = %v", names)
	}
}

func TestParityOrgRepoSettings(t *testing.T) {
	e := newParityEnv(t, false)
	seedForgeSource(t, e.forges, &domain.ForgeSource{ID: "src", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/api"})
	res := execJSON(t, e.g, e.ctx(), `{ org(name: "acme") { repoSettings { repo isPublic effectiveRunChecksOnDefaultBranch effectiveRunChecksOnPullRequest } } }`, nil)
	arr := res["data"].(map[string]any)["org"].(map[string]any)["repoSettings"].([]any)
	if len(arr) != 1 {
		t.Fatalf("repoSettings = %v", arr)
	}
	if arr[0].(map[string]any)["repo"] != "github.com/acme/api" {
		t.Fatalf("repo = %v", arr[0])
	}
	// filtered
	res = execJSON(t, e.g, e.ctx(), `{ org(name: "acme") { repoSettings(repo: "acme/other") { repo } } }`, nil)
	if len(res["data"].(map[string]any)["org"].(map[string]any)["repoSettings"].([]any)) != 0 {
		t.Fatalf("filtered repoSettings = %v", res)
	}
}

func TestParityRepoSettingsMutations(t *testing.T) {
	e := newParityEnv(t, false)
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"update", `mutation { updateOrgRepoSetting(org: "acme", setting: {repo: "acme/api", isPublic: true}) }`, "repository visibility settings are not supported"},
		{"delete", `mutation { deleteOrgRepoSetting(org: "acme", repoName: "acme/api") }`, "repository visibility settings are not supported"},
		{"updates", `mutation { updateOrgRepoSettings(org: "acme", settings: [{repo: "acme/api"}]) }`, "repository check triggers are managed via forge integrations"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := execJSON(t, e.g, e.ctx(), tc.query, nil)
			msgs := errorMessages(t, res)
			if len(msgs) == 0 || !strings.Contains(msgs[0], tc.want) {
				t.Fatalf("message = %v want %q", msgs, tc.want)
			}
		})
	}
}

func TestParityRepoSettingsMutationForbidden(t *testing.T) {
	e := newParityEnv(t, false)
	other := seedUser(t, e.store, "settings-outsider")
	res := execJSON(t, e.g, WithGraphQLIdentity(context.Background(), GraphQLIdentity{UserID: other.ID}), `mutation { updateOrgRepoSetting(org: "acme", setting: {repo: "x"}) }`, nil)
	msgs := errorMessages(t, res)
	if len(msgs) == 0 || msgs[0] != "forbidden" {
		t.Fatalf("outsider must be forbidden before feature error: %v", msgs)
	}
}

func TestParityAccessibleOrgsOrgScoped(t *testing.T) {
	e := newParityEnv(t, false)
	// Org-scoped token sees only its own org.
	scoped := WithGraphQLIdentity(context.Background(), GraphQLIdentity{UserID: e.user.ID, OrgID: "acme"})
	res := execJSON(t, e.g, scoped, `{ user { orgs { id } } }`, nil)
	orgs := res["data"].(map[string]any)["user"].(map[string]any)["orgs"].([]any)
	if len(orgs) != 1 || orgs[0].(map[string]any)["id"] != "acme" {
		t.Fatalf("org-scoped orgs = %v", orgs)
	}
}

// --- Family H: GitHub integration resolvers ---------------------------------

type fakeGitHubIntegrator struct {
	oauthEnabled  bool
	buildURL      string
	items         []domain.SourceRepositoryItem
	updateErr     error
	deleteErr     error
	lastMutations []domain.RepoSettingMutation
	deletedRepo   string
}

func (f *fakeGitHubIntegrator) OAuthEnabled() bool { return f.oauthEnabled }
func (f *fakeGitHubIntegrator) BuildOAuthURL(context.Context, string, string, string) (string, error) {
	return f.buildURL, nil
}
func (f *fakeGitHubIntegrator) ListSourceRepositories(context.Context, *domain.Org, string) ([]domain.SourceRepositoryItem, error) {
	return f.items, nil
}
func (f *fakeGitHubIntegrator) UpdateOrgRepoSettings(_ context.Context, _ string, _ bool, _ *domain.Org, settings []domain.RepoSettingMutation) error {
	f.lastMutations = settings
	return f.updateErr
}
func (f *fakeGitHubIntegrator) DeleteOrgRepoSetting(_ context.Context, _ string, _ bool, _ *domain.Org, repo string) error {
	f.deletedRepo = repo
	return f.deleteErr
}

func TestParityGithubOAuthURLConfigured(t *testing.T) {
	e := newParityEnv(t, false)
	integrator := &fakeGitHubIntegrator{oauthEnabled: true, buildURL: "https://github.com/login/oauth/authorize?state=x"}
	e.g.WithGitHubIntegrator(integrator)
	res := execJSON(t, e.g, e.ctx(), `{ githubOAuthURL(redirectURI: "https://example.com/github/callback") }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	if got := res["data"].(map[string]any)["githubOAuthURL"]; got != integrator.buildURL {
		t.Fatalf("url = %v", got)
	}
	integrator.oauthEnabled = false
	res = execJSON(t, e.g, e.ctx(), `{ githubOAuthURL(redirectURI: "https://example.com/github/callback") }`, nil)
	msgs := errorMessages(t, res)
	if len(msgs) == 0 || !strings.Contains(msgs[0], "GitHub OAuth app integration is not configured") {
		t.Fatalf("disabled message = %v", msgs)
	}
}

func TestParityGithubConnectionUserScoped(t *testing.T) {
	e := newParityEnv(t, false)
	conns := NewGitHubConnectionRepo(e.store)
	ctx := context.Background()
	at := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)
	if err := conns.Upsert(ctx, &domain.UserGitHubConnection{
		UserID: e.user.ID, GitHubLogin: "alice", BaseURL: "https://api.github.com",
		Scopes: []string{"repo", "read:org"}, TokenCiphertext: "ct", Status: domain.GitHubConnectionActive,
		ConnectedAt: at, UpdatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	e.g.WithGitHubConnections(conns)
	res := execJSON(t, e.g, e.ctx(), `{ githubConnection { githubLogin connectedAt scopes status } }`, nil)
	conn := res["data"].(map[string]any)["githubConnection"].(map[string]any)
	if conn["githubLogin"] != "alice" || conn["status"] != "active" || conn["connectedAt"] != at.Format(time.RFC3339) {
		t.Fatalf("connection = %v", conn)
	}
	if scopes := conn["scopes"].([]any); len(scopes) != 2 || scopes[0] != "repo" {
		t.Fatalf("scopes = %v", scopes)
	}
	// A revoked connection must not be reported as connected.
	revoked := &domain.UserGitHubConnection{UserID: e.user.ID, GitHubLogin: "alice", TokenCiphertext: "ct", Status: domain.GitHubConnectionRevoked, ConnectedAt: at, UpdatedAt: at}
	if err := conns.Upsert(ctx, revoked); err != nil {
		t.Fatal(err)
	}
	res = execJSON(t, e.g, e.ctx(), `{ githubConnection { githubLogin } }`, nil)
	if res["data"].(map[string]any)["githubConnection"] != nil {
		t.Fatalf("revoked connection must fall through: %v", res)
	}
}

func TestParitySourcesDedupAccountOwners(t *testing.T) {
	e := newParityEnv(t, false)
	accounts := NewGitHubAccountSourceRepo(e.store)
	ctx := context.Background()
	at := time.Now().UTC()
	if err := accounts.Upsert(ctx, &domain.GitHubAccountSource{ID: "ghacct-1", OrgID: "acme", Owner: "Acme", BaseURL: "https://api.github.com", Status: domain.GitHubConnectionActive, CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	seedForgeSource(t, e.forges, &domain.ForgeSource{ID: "src-new", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/new", AccountSourceID: "ghacct-1"})
	seedForgeSource(t, e.forges, &domain.ForgeSource{ID: "src-old", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/old"})
	seedForgeSource(t, e.forges, &domain.ForgeSource{ID: "src-old-2", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/old2"})
	seedForgeSource(t, e.forges, &domain.ForgeSource{ID: "src-gl", OrgID: "acme", Provider: domain.ForgeGitLab, Repo: "group/proj"})
	e.g.WithGitHubAccountSources(accounts)
	res := execJSON(t, e.g, e.ctx(), `{ sources { id name type } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	sources := res["data"].(map[string]any)["sources"].([]any)
	if len(sources) != 2 {
		t.Fatalf("sources = %v", sources)
	}
	ids := map[string]bool{}
	for _, raw := range sources {
		ids[raw.(map[string]any)["id"].(string)] = true
	}
	for _, want := range []string{"ghacct-1", "src-gl"} {
		if !ids[want] {
			t.Fatalf("missing %s in %v", want, ids)
		}
	}
	for _, skipped := range []string{"src-old", "src-old-2", "src-new"} {
		if ids[skipped] {
			t.Fatalf("owner-covered source %s must be deduplicated: %v", skipped, ids)
		}
	}
}

func TestParitySourceRepositoriesAccount(t *testing.T) {
	e := newParityEnv(t, false)
	accounts := NewGitHubAccountSourceRepo(e.store)
	ctx := context.Background()
	if err := accounts.Upsert(ctx, &domain.GitHubAccountSource{ID: "ghacct-1", OrgID: "acme", Owner: "alice", BaseURL: "https://api.github.com", Status: domain.GitHubConnectionActive}); err != nil {
		t.Fatal(err)
	}
	claimed := "other"
	integrator := &fakeGitHubIntegrator{items: []domain.SourceRepositoryItem{
		{Repository: "github.com/alice/api", Selected: true, Eligible: true},
		{Repository: "github.com/alice/other", Eligible: false, ClaimedByOrgName: &claimed},
	}}
	e.g.WithGitHubAccountSources(accounts).WithGitHubIntegrator(integrator)
	res := execJSON(t, e.g, e.ctx(), `{ sourceRepositories(installationId: "ghacct-1", org: "acme") { repository selected eligible claimedByOrgName } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	repos := res["data"].(map[string]any)["sourceRepositories"].([]any)
	if len(repos) != 2 {
		t.Fatalf("repos = %v", repos)
	}
	if repos[1].(map[string]any)["claimedByOrgName"] != "other" || repos[1].(map[string]any)["eligible"] != false {
		t.Fatalf("other repo = %v", repos[1])
	}
}

func TestParityPersistedRepoSettingsResolvers(t *testing.T) {
	e := newParityEnv(t, false)
	ctx := context.Background()
	accounts := NewGitHubAccountSourceRepo(e.store)
	repoSets := NewRepoSettingRepo(e.store)
	if err := accounts.Upsert(ctx, &domain.GitHubAccountSource{ID: "ghacct-1", OrgID: "acme", Owner: "acme", BaseURL: "https://api.github.com", Status: domain.GitHubConnectionActive}); err != nil {
		t.Fatal(err)
	}
	if err := repoSets.BatchUpsert(ctx, "acme", []domain.RepoSettingWrite{{
		Setting:     &domain.RepoSetting{OrgID: "acme", Repo: "github.com/acme/api", Provider: domain.ForgeGitHub, SourceID: "ghacct-1", ForgeSourceID: "ghrepo-1", IsPublic: true, RunChecksOnDefaultBranch: true},
		ForgeSource: &domain.ForgeSource{ID: "ghrepo-1", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/api", AccountSourceID: "ghacct-1"},
	}}); err != nil {
		t.Fatal(err)
	}
	e.g.WithGitHubAccountSources(accounts).WithRepoSettings(repoSets)
	res := execJSON(t, e.g, e.ctx(), `{ user { repositories(refs: ["github.com/acme/api"]) { ref settings { repo isPublic effectiveRunChecksOnDefaultBranch effectiveRunChecksOnPullRequest effectiveRunChecksOnBranchPush } mappedSource { provider sourceName installationId configUrl } } } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	repos := res["data"].(map[string]any)["user"].(map[string]any)["repositories"].([]any)
	if len(repos) != 1 {
		t.Fatalf("repositories = %v", repos)
	}
	m := repos[0].(map[string]any)
	settings := m["settings"].(map[string]any)
	if m["ref"] != "github.com/acme/api" || settings["isPublic"] != true || settings["effectiveRunChecksOnBranchPush"] != false {
		t.Fatalf("repo = %v", m)
	}
	mapped := m["mappedSource"].(map[string]any)
	if mapped["provider"] != "GITHUB" || mapped["sourceName"] != "acme" || mapped["installationId"] != "ghacct-1" {
		t.Fatalf("mappedSource = %v", mapped)
	}
	if !strings.Contains(mapped["configUrl"].(string), "/orgs/acme/integrations/github") {
		t.Fatalf("configUrl = %v", mapped["configUrl"])
	}
	res = execJSON(t, e.g, e.ctx(), `{ org(name: "acme") { repoSettings { repo isPublic effectiveRunChecksOnDefaultBranch effectiveRunChecksOnPullRequest effectiveRunChecksOnBranchPush } } }`, nil)
	arr := res["data"].(map[string]any)["org"].(map[string]any)["repoSettings"].([]any)
	if len(arr) != 1 || arr[0].(map[string]any)["effectiveRunChecksOnDefaultBranch"] != true {
		t.Fatalf("repoSettings = %v", arr)
	}
}

func TestParityUpdateOrgRepoSettingsIntegrator(t *testing.T) {
	e := newParityEnv(t, false)
	integrator := &fakeGitHubIntegrator{}
	e.g.WithGitHubIntegrator(integrator)
	res := execJSON(t, e.g, e.ctx(), `mutation { updateOrgRepoSettings(org: "acme", settings: [{repo: "github.com/acme/api", sourceId: "ghacct-1", runChecksOnDefaultBranch: true, runChecksOnPullRequest: false}]) }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	if len(integrator.lastMutations) != 1 {
		t.Fatalf("mutations = %+v", integrator.lastMutations)
	}
	m := integrator.lastMutations[0]
	if m.Repo != "github.com/acme/api" || m.SourceID == nil || *m.SourceID != "ghacct-1" || m.RunChecksOnPullRequest == nil || *m.RunChecksOnPullRequest {
		t.Fatalf("mutation = %+v", m)
	}
	res = execJSON(t, e.g, e.ctx(), `mutation { updateOrgRepoSetting(org: "acme", setting: {repo: "github.com/acme/api", isPublic: true}) }`, nil)
	if hasErrors(res) {
		t.Fatalf("single errors: %v", errorMessages(t, res))
	}
	res = execJSON(t, e.g, e.ctx(), `mutation { deleteOrgRepoSetting(org: "acme", repoName: "github.com/acme/api") }`, nil)
	if hasErrors(res) || integrator.deletedRepo != "github.com/acme/api" {
		t.Fatalf("delete result = %v deleted=%q", res, integrator.deletedRepo)
	}
	integrator.updateErr = fmt.Errorf("repository github.com/acme/api does not belong to installation ghacct-2: %w", domain.ErrRepoNotInInstallation)
	res = execJSON(t, e.g, e.ctx(), `mutation { updateOrgRepoSettings(org: "acme", settings: [{repo: "github.com/acme/api"}]) }`, nil)
	msgs := errorMessages(t, res)
	if len(msgs) == 0 || !strings.Contains(msgs[0], "does not belong to installation") {
		t.Fatalf("error message = %v", msgs)
	}
}

func TestParityOrgFeaturesBackfill(t *testing.T) {
	e := newParityEnv(t, false)
	empty := &domain.Org{ID: "empty", Name: "empty", DisplayName: "Empty", CreatedAt: time.Now().UTC()}
	if err := e.orgRepo.Create(context.Background(), empty); err != nil {
		t.Fatal(err)
	}
	if err := e.orgRepo.SetMember(context.Background(), "empty", e.user.ID, domain.OrgMember); err != nil {
		t.Fatal(err)
	}
	res := execJSON(t, e.g, e.ctx(), `{ org(name: "empty") { features { name status } } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	features := res["data"].(map[string]any)["org"].(map[string]any)["features"].([]any)
	names := map[string]bool{}
	for _, raw := range features {
		names[raw.(map[string]any)["name"].(string)] = true
	}
	if !names["CLOUD_CHECKS"] || !names["CLOUD_MODULES"] {
		t.Fatalf("features = %v", names)
	}
}

func TestParitySourcesDedupStandaloneOwners(t *testing.T) {
	e := newParityEnv(t, false)
	seedForgeSource(t, e.forges, &domain.ForgeSource{ID: "src-1", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/api", InstallationID: "1"})
	seedForgeSource(t, e.forges, &domain.ForgeSource{ID: "src-2", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/web", InstallationID: "2"})
	res := execJSON(t, e.g, e.ctx(), `{ sources { id name type } }`, nil)
	if hasErrors(res) {
		t.Fatalf("errors: %v", errorMessages(t, res))
	}
	sources := res["data"].(map[string]any)["sources"].([]any)
	if len(sources) != 1 || sources[0].(map[string]any)["name"] != "acme" {
		t.Fatalf("sources = %v", sources)
	}
}
