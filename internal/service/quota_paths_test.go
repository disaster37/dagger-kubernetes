package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
)

// quotaProjectRepo is a minimal ProjectRepository for quota usage tests.
type quotaProjectRepo struct {
	projects []*domain.Project
	err      error
}

func (r *quotaProjectRepo) Create(context.Context, *domain.Project) error { return nil }
func (r *quotaProjectRepo) Get(context.Context, string) (*domain.Project, error) {
	return nil, domain.ErrNotFound
}
func (r *quotaProjectRepo) GetByName(context.Context, string, string) (*domain.Project, error) {
	return nil, domain.ErrNotFound
}
func (r *quotaProjectRepo) ListByOrg(context.Context, string) ([]*domain.Project, error) {
	return r.projects, r.err
}
func (r *quotaProjectRepo) ListByNameAcrossOrgs(context.Context, string) ([]*domain.Project, error) {
	return nil, nil
}
func (r *quotaProjectRepo) List(context.Context) ([]*domain.Project, error) {
	return r.projects, r.err
}
func (r *quotaProjectRepo) Update(context.Context, *domain.Project) error { return nil }
func (r *quotaProjectRepo) Delete(context.Context, string) error          { return nil }

// quotaGroupRepo is a GroupRepository with injectable failures, groups, and
// memberships for usage tests.
type quotaGroupRepo struct {
	*stubGroupRepo
	listErr    error
	getErr     error
	allErr     error
	groups     []*domain.Group
	perUser    []*domain.Group
	perOrg     []*domain.Group
	perUserErr error
	perOrgErr  error
}

func newQuotaGroupRepo(groups ...*domain.Group) *quotaGroupRepo {
	return &quotaGroupRepo{stubGroupRepo: newStubGroupRepo(groups...), groups: groups}
}

func (r *quotaGroupRepo) ListByOrg(context.Context, string) ([]*domain.Group, error) {
	return r.groups, r.listErr
}
func (r *quotaGroupRepo) Get(_ context.Context, id string) (*domain.Group, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	for _, g := range r.groups {
		if g.ID == id {
			return g, nil
		}
	}
	return nil, domain.ErrNotFound
}
func (r *quotaGroupRepo) AllMemberships(context.Context) (map[string][]string, error) {
	if r.allErr != nil {
		return nil, r.allErr
	}
	return r.stubGroupRepo.AllMemberships(context.Background())
}
func (r *quotaGroupRepo) GroupsForUserInOrg(context.Context, string, string) ([]*domain.Group, error) {
	return r.perOrg, r.perOrgErr
}
func (r *quotaGroupRepo) GroupsForUser(context.Context, string) ([]*domain.Group, error) {
	return r.perUser, r.perUserErr
}

func TestQuotaSettersAndLock(t *testing.T) {
	q := NewQuotaService(&stubSessionStore{}, newQuotaGroupRepo(), testLogger()).
		WithMetrics(observ.NewMetrics(nil)).
		WithDefaults(domain.OrgQuotas{MaxGroups: domain.QuotaThreshold{Hard: 1}}, 24*time.Hour).
		WithEngineUsage(func(context.Context, string) (int64, error) { return 3, nil }).
		WithCacheUsage(func(string) (int64, error) { return 4, nil })
	if q.metrics == nil || q.retention != 24*time.Hour || q.engineUsage == nil || q.cacheUsage == nil {
		t.Fatalf("wiring incomplete: %+v", q)
	}
	unlock := q.LockOrg("acme")
	unlock()
	// A different org may map to the same stripe but the lock must be released.
	unlock = q.LockOrg("acme")
	unlock()
}

func TestQuotaEffectiveValues(t *testing.T) {
	ctx := context.Background()
	repo := newPathOrgRepo()
	o := &domain.Org{ID: "org-1111111111111111", Name: "acme", Quotas: domain.OrgQuotas{
		MaxCacheBytes:        domain.QuotaThreshold{Hard: 1024},
		MaxPipelineRetention: time.Hour,
	}}
	if err := repo.Create(ctx, o); err != nil {
		t.Fatal(err)
	}
	defaults := domain.OrgQuotas{MaxGroups: domain.QuotaThreshold{Hard: 5}}

	q := NewQuotaService(&stubSessionStore{}, newQuotaGroupRepo(), testLogger()).
		WithDefaults(defaults, 48*time.Hour)
	if eff, err := q.EffectiveQuotas(ctx, ""); err != nil || eff.MaxGroups.Hard != 5 {
		t.Fatalf("empty org = %+v err=%v", eff, err)
	}

	q.WithOrgRepository(repo, &quotaProjectRepo{})
	if eff, err := q.EffectiveQuotas(ctx, o.ID); err != nil || eff.MaxCacheBytes.Hard != 1024 {
		t.Fatalf("org = %+v err=%v", eff, err)
	}
	if got := q.EffectiveCacheMaxBytes(ctx, o.ID, 99); got != 1024 {
		t.Fatalf("cache max = %d, want 1024", got)
	}
	if got := q.EffectiveCacheMaxBytes(ctx, "missing", 99); got != 99 {
		t.Fatalf("cache fallback = %d, want 99", got)
	}
	if got := q.EffectiveRetention(ctx, o.ID); got != time.Hour {
		t.Fatalf("retention = %v, want 1h", got)
	}
	if got := q.EffectiveRetention(ctx, "missing"); got != 48*time.Hour {
		t.Fatalf("retention fallback = %v, want 48h", got)
	}
	repo.getErr = errors.New("boom")
	if _, err := q.EffectiveQuotas(ctx, o.ID); err == nil {
		t.Fatal("expected store error")
	}
	if got := q.EffectiveCacheMaxBytes(ctx, o.ID, 7); got != 7 {
		t.Fatalf("cache error fallback = %d, want 7", got)
	}
	if got := q.EffectiveRetention(ctx, o.ID); got != 48*time.Hour {
		t.Fatalf("retention error fallback = %v, want 48h", got)
	}
}

func TestQuotaCheckResourceMatrix(t *testing.T) {
	ctx := context.Background()
	repo := newPathOrgRepo()
	if err := repo.Create(ctx, &domain.Org{ID: "org-1111111111111111", Name: "acme", Quotas: domain.OrgQuotas{
		MaxGroups:             domain.QuotaThreshold{Soft: 1, Hard: 2},
		MaxProjects:           domain.QuotaThreshold{Hard: 1},
		MaxMembers:            domain.QuotaThreshold{Hard: 1},
		MaxConcurrentSessions: domain.QuotaThreshold{Hard: 1},
		MaxEngines:            domain.QuotaThreshold{Hard: 1},
		MaxCacheBytes:         domain.QuotaThreshold{Hard: 10},
	}}); err != nil {
		t.Fatal(err)
	}
	groups := newQuotaGroupRepo(&domain.Group{ID: "g1", OrgID: "org-1111111111111111"})
	if err := groups.SetMembers(ctx, "g1", []string{"u1"}); err != nil {
		t.Fatal(err)
	}
	sessions := &stubSessionStore{}
	q := NewQuotaService(sessions, groups, testLogger()).
		WithOrgRepository(repo, &quotaProjectRepo{projects: []*domain.Project{{ID: "p1"}}}).
		WithEngineUsage(func(context.Context, string) (int64, error) { return 1, nil }).
		WithCacheUsage(func(string) (int64, error) { return 10, nil }).
		WithMetrics(observ.NewMetrics(nil))

	id := &domain.Identity{UserID: "u1"}
	cases := []struct {
		name     string
		res      domain.QuotaResource
		delta    int64
		wantErr  bool
		wantWarn bool
	}{
		{"groups hard", domain.QuotaGroups, 2, true, false},
		{"groups soft", domain.QuotaGroups, 1, false, true},
		{"projects hard", domain.QuotaProjects, 1, true, false},
		{"members hard", domain.QuotaMembers, 2, true, false},
		{"sessions hard", domain.QuotaConcurrentSessions, 2, true, false},
		{"engines hard", domain.QuotaEngines, 1, true, false},
		{"cache hard", domain.QuotaCacheBytes, 1, true, false},
		{"unknown resource", domain.QuotaResource("bogus"), 99, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warn, err := q.CheckResource(ctx, id, "org-1111111111111111", tc.res, tc.delta)
			if tc.wantErr && !errors.Is(err, domain.ErrOrgQuotaExceeded) {
				t.Fatalf("err = %v, want ErrOrgQuotaExceeded", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected err = %v", err)
			}
			if tc.wantWarn && warn == "" {
				t.Fatal("expected a soft-limit warning")
			}
			if !tc.wantWarn && warn != "" {
				t.Fatalf("unexpected warning %q", warn)
			}
		})
	}

	t.Run("nil identity is safe on hard limit", func(t *testing.T) {
		if _, err := q.CheckResource(ctx, nil, "org-1111111111111111", domain.QuotaGroups, 99); !errors.Is(err, domain.ErrOrgQuotaExceeded) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("usage error propagates", func(t *testing.T) {
		repo.distinctErr = errors.New("boom")
		if _, err := q.CheckResource(ctx, id, "org-1111111111111111", domain.QuotaMembers, 0); err == nil {
			t.Fatal("expected usage error")
		}
		repo.distinctErr = nil
	})

	t.Run("effective quota error propagates", func(t *testing.T) {
		repo.getErr = errors.New("boom")
		if _, err := q.CheckResource(ctx, id, "org-1111111111111111", domain.QuotaGroups, 0); err == nil {
			t.Fatal("expected quota lookup error")
		}
		repo.getErr = nil
	})
}

func TestQuotaTypedChecks(t *testing.T) {
	ctx := context.Background()
	repo := newPathOrgRepo()
	if err := repo.Create(ctx, &domain.Org{ID: "org-1111111111111111", Name: "acme", Quotas: domain.OrgQuotas{
		MaxProjects:   domain.QuotaThreshold{Hard: 1},
		MaxMembers:    domain.QuotaThreshold{Hard: 1},
		MaxEngines:    domain.QuotaThreshold{Hard: 1},
		MaxCacheBytes: domain.QuotaThreshold{Hard: 5},
	}}); err != nil {
		t.Fatal(err)
	}
	groups := newQuotaGroupRepo(&domain.Group{ID: "g1", OrgID: "org-1111111111111111"})
	q := NewQuotaService(&stubSessionStore{}, groups, testLogger()).
		WithOrgRepository(repo, &quotaProjectRepo{projects: []*domain.Project{{ID: "p1"}}}).
		WithEngineUsage(func(context.Context, string) (int64, error) { return 1, nil }).
		WithCacheUsage(func(string) (int64, error) { return 5, nil })
	id := &domain.Identity{UserID: "u1"}

	if _, err := q.CheckGroupCreate(ctx, id, "org-1111111111111111"); err != nil {
		t.Fatalf("group create = %v", err)
	}
	if _, err := q.CheckProjectCreate(ctx, id, "org-1111111111111111"); !errors.Is(err, domain.ErrOrgQuotaExceeded) {
		t.Fatalf("project create = %v", err)
	}
	if _, err := q.CheckMemberAdd(ctx, id, "org-1111111111111111", 2); !errors.Is(err, domain.ErrOrgQuotaExceeded) {
		t.Fatalf("member add = %v", err)
	}
	if _, err := q.CheckEngineScaleUp(ctx, "org-1111111111111111", id, 1); !errors.Is(err, domain.ErrOrgQuotaExceeded) {
		t.Fatalf("engine scale-up = %v", err)
	}
	if _, err := q.CheckCacheWrite(ctx, "org-1111111111111111", 1); !errors.Is(err, domain.ErrOrgQuotaExceeded) {
		t.Fatalf("cache write = %v", err)
	}
}

func TestQuotaUsageSnapshot(t *testing.T) {
	ctx := context.Background()
	repo := newPathOrgRepo()
	if err := repo.Create(ctx, &domain.Org{ID: "org-1111111111111111", Name: "acme", Quotas: domain.OrgQuotas{
		MaxGroups: domain.QuotaThreshold{Soft: 1, Hard: 2},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetMember(ctx, "org-1111111111111111", "u1", domain.OrgMember); err != nil {
		t.Fatal(err)
	}
	groups := newQuotaGroupRepo(&domain.Group{ID: "g1", OrgID: "org-1111111111111111"})
	sessions := &stubSessionStore{}
	sessions.Register("org-1111111111111111", "fp", "v", "pod", "inst", "trace", "u1")

	q := NewQuotaService(sessions, groups, testLogger()).
		WithOrgRepository(repo, &quotaProjectRepo{projects: []*domain.Project{{ID: "p1"}, {ID: "p2"}}}).
		WithEngineUsage(func(context.Context, string) (int64, error) { return 3, nil }).
		WithCacheUsage(func(string) (int64, error) { return 40, nil }).
		WithMetrics(observ.NewMetrics(nil))

	usage, err := q.UsageSnapshot(ctx, "org-1111111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Groups != 1 || usage.Projects != 2 || usage.Members != 1 || usage.ConcurrentSessions != 1 || usage.Engines != 3 || usage.CacheBytes != 40 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.Effective.MaxGroups.Hard != 2 {
		t.Fatalf("effective quotas = %+v", usage.Effective)
	}

	groups.listErr = errors.New("boom")
	if _, err := q.UsageSnapshot(ctx, "org-1111111111111111"); err == nil {
		t.Fatal("expected usage error")
	}
	groups.listErr = nil
	repo.getErr = errors.New("boom")
	if _, err := q.UsageSnapshot(ctx, "org-1111111111111111"); err == nil {
		t.Fatal("expected quota error")
	}
	repo.getErr = nil
}

func TestQuotaUsageSnapshotNilProviders(t *testing.T) {
	q := NewQuotaService(nil, nil, testLogger())
	usage, err := q.UsageSnapshot(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Groups != 0 || usage.Projects != 0 || usage.Members != 0 || usage.ConcurrentSessions != 0 || usage.Engines != 0 || usage.CacheBytes != 0 {
		t.Fatalf("nil providers usage = %+v", usage)
	}
}

func TestQuotaUsageSnapshotProviderErrors(t *testing.T) {
	ctx := context.Background()
	groups := newQuotaGroupRepo()
	projects := &quotaProjectRepo{err: errors.New("boom")}
	q := NewQuotaService(nil, groups, testLogger()).WithOrgRepository(newPathOrgRepo(), projects)
	if _, err := q.UsageSnapshot(ctx, ""); err == nil {
		t.Fatal("expected project usage error")
	}
	q.projects = nil
	q.engineUsage = func(context.Context, string) (int64, error) { return 0, errors.New("boom") }
	q.defaults = domain.OrgQuotas{}
	if _, err := q.UsageSnapshot(ctx, ""); err == nil {
		t.Fatal("expected engine usage error")
	}
	q.engineUsage = nil
	q.cacheUsage = func(string) (int64, error) { return 0, errors.New("boom") }
	if _, err := q.UsageSnapshot(ctx, ""); err == nil {
		t.Fatal("expected cache usage error")
	}
}

func TestQuotaEngineAccessErrorPaths(t *testing.T) {
	ctx := context.Background()
	groups := newQuotaGroupRepo()
	groups.perOrgErr = errors.New("boom")
	q := NewQuotaService(&stubSessionStore{}, groups, testLogger())
	id := &domain.Identity{UserID: "u1"}
	if _, err := q.CheckEngineAccess(ctx, "acme", id); err == nil {
		t.Fatal("expected group lookup error")
	}

	groups.perOrgErr = nil
	groups.perOrg = []*domain.Group{{ID: "g1", OrgID: "acme", AgentAvailable: true}}
	groups.groups = []*domain.Group{{ID: "g1", OrgID: "acme", AgentAvailable: true}}
	orgToken := &domain.Identity{UserID: "org:acme", OrgID: "acme"}
	// No per-user groups, but the org token may use every org group.
	if _, err := q.CheckEngineAccess(ctx, "acme", orgToken); err != nil {
		t.Fatalf("org token = %v", err)
	}

	groups.perOrg = nil
	groups.groups = nil
	if _, err := q.CheckEngineAccess(ctx, "acme", orgToken); err != nil {
		t.Fatalf("fresh org token = %v", err)
	}
	groups.listErr = errors.New("boom")
	if _, err := q.CheckEngineAccess(ctx, "acme", orgToken); err == nil {
		t.Fatal("expected org group list error")
	}
	groups.listErr = nil

	// A org-scoped identity with no groups at all is refused for humans.
	groups.perOrg = nil
	if _, err := q.CheckEngineAccess(ctx, "acme", id); !errors.Is(err, domain.ErrNoGroups) {
		t.Fatalf("no groups = %v", err)
	}
}

func TestQuotaUsageSnapshotGroupScoping(t *testing.T) {
	ctx := context.Background()
	groups := newQuotaGroupRepo(
		&domain.Group{ID: "g1", OrgID: "acme"},
		&domain.Group{ID: "g2", OrgID: "beta"},
	)
	if err := groups.SetMembers(ctx, "g1", []string{"u1"}); err != nil {
		t.Fatal(err)
	}
	if err := groups.SetMembers(ctx, "g2", []string{"u1"}); err != nil {
		t.Fatal(err)
	}
	sessions := &stubSessionStore{}
	sessions.Register("acme", "fp1", "v", "pod", "inst", "trace", "u1")
	sessions.Register("beta", "fp2", "v", "pod", "inst", "trace", "u1")
	sessions.Register("acme", "fp3", "v", "pod", "inst", "trace", "") // no user

	q := NewQuotaService(sessions, groups, testLogger())
	usage := q.usageSnapshot(ctx, "acme")
	if usage["g1"] != 1 || usage["g2"] != 0 {
		t.Fatalf("org-scoped usage = %+v", usage)
	}
	all := q.usageSnapshot(ctx, "")
	if all["g1"] != 2 || all["g2"] != 2 {
		t.Fatalf("all-org usage = %+v", all)
	}

	groups.getErr = errors.New("boom")
	if usage := q.usageSnapshot(ctx, "acme"); len(usage) != 0 {
		t.Fatalf("unknown group usage = %+v", usage)
	}
	groups.getErr = nil
	groups.allErr = errors.New("boom")
	if usage := q.usageSnapshot(ctx, ""); len(usage) != 0 {
		t.Fatalf("membership error usage = %+v", usage)
	}
}
