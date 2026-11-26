package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// pathOrgRepo wraps fakeOrgRepo with injectable failures.
type pathOrgRepo struct {
	*fakeOrgRepo
	getErr        error
	listErr       error
	createErr     error
	updateErr     error
	deleteErr     error
	deleteCascade error
	bootstrapErr  error
	bootstrapID   error
	getBootstrap  error
	setMemberErr  error
	removeErr     error
	membersErr    error
	distinctErr   error
	memberRoleErr error
	orgsForUser   error
	allMembersErr error
	bootstrapOrg  *domain.Org
	getCalls      int
}

func newPathOrgRepo() *pathOrgRepo { return &pathOrgRepo{fakeOrgRepo: newFakeOrgRepo()} }

func (r *pathOrgRepo) Get(ctx context.Context, id string) (*domain.Org, error) {
	r.getCalls++
	if r.getErr != nil {
		return nil, r.getErr
	}
	return r.fakeOrgRepo.Get(ctx, id)
}
func (r *pathOrgRepo) List(ctx context.Context) ([]*domain.Org, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.fakeOrgRepo.List(ctx)
}
func (r *pathOrgRepo) Create(ctx context.Context, o *domain.Org) error {
	if r.createErr != nil {
		return r.createErr
	}
	return r.fakeOrgRepo.Create(ctx, o)
}
func (r *pathOrgRepo) Update(ctx context.Context, o *domain.Org) error {
	if r.updateErr != nil {
		return r.updateErr
	}
	return r.fakeOrgRepo.Update(ctx, o)
}
func (r *pathOrgRepo) Delete(ctx context.Context, id string) error {
	if r.deleteErr != nil {
		return r.deleteErr
	}
	return r.fakeOrgRepo.Delete(ctx, id)
}
func (r *pathOrgRepo) DeleteCascade(ctx context.Context, id string) error {
	if r.deleteCascade != nil {
		return r.deleteCascade
	}
	return r.fakeOrgRepo.DeleteCascade(ctx, id)
}
func (r *pathOrgRepo) Bootstrap(ctx context.Context, o *domain.Org, _, _ string) (*domain.Org, error) {
	if r.bootstrapErr != nil {
		return nil, r.bootstrapErr
	}
	return r.fakeOrgRepo.Bootstrap(ctx, o, "", "")
}
func (r *pathOrgRepo) BootstrapOrgID(ctx context.Context) (string, error) {
	if r.bootstrapID != nil {
		return "", r.bootstrapID
	}
	return r.fakeOrgRepo.BootstrapOrgID(ctx)
}
func (r *pathOrgRepo) GetBootstrap(ctx context.Context) (*domain.Org, error) {
	if r.bootstrapOrg != nil {
		return r.bootstrapOrg, nil
	}
	if r.getBootstrap != nil {
		return nil, r.getBootstrap
	}
	return r.fakeOrgRepo.GetBootstrap(ctx)
}
func (r *pathOrgRepo) SetMember(ctx context.Context, orgID, userID string, role domain.OrgRole) error {
	if r.setMemberErr != nil {
		return r.setMemberErr
	}
	return r.fakeOrgRepo.SetMember(ctx, orgID, userID, role)
}
func (r *pathOrgRepo) RemoveMember(ctx context.Context, orgID, userID string) error {
	if r.removeErr != nil {
		return r.removeErr
	}
	return r.fakeOrgRepo.RemoveMember(ctx, orgID, userID)
}
func (r *pathOrgRepo) Members(ctx context.Context, orgID string) ([]*domain.OrgMembership, error) {
	if r.membersErr != nil {
		return nil, r.membersErr
	}
	return r.fakeOrgRepo.Members(ctx, orgID)
}
func (r *pathOrgRepo) DistinctMemberCount(ctx context.Context, orgID string) (int, error) {
	if r.distinctErr != nil {
		return 0, r.distinctErr
	}
	return r.fakeOrgRepo.DistinctMemberCount(ctx, orgID)
}
func (r *pathOrgRepo) GetMemberRole(ctx context.Context, orgID, userID string) (domain.OrgRole, bool, error) {
	if r.memberRoleErr != nil {
		return "", false, r.memberRoleErr
	}
	return r.fakeOrgRepo.GetMemberRole(ctx, orgID, userID)
}
func (r *pathOrgRepo) OrgsForUser(ctx context.Context, userID string) ([]*domain.Org, error) {
	if r.orgsForUser != nil {
		return nil, r.orgsForUser
	}
	return r.fakeOrgRepo.OrgsForUser(ctx, userID)
}
func (r *pathOrgRepo) AllMemberships(ctx context.Context) (map[string][]*domain.OrgMembership, error) {
	if r.allMembersErr != nil {
		return nil, r.allMembersErr
	}
	return r.fakeOrgRepo.AllMemberships(ctx)
}

// pathGroupRepo wraps fakeGroupRepo with injectable failures and fixtures.
type pathGroupRepo struct {
	*fakeGroupRepo
	createErr   error
	listErr     error
	groupsAll   []*domain.Group
	groupsUser  []*domain.Group
	groupsInOrg []*domain.Group
	errForUser  error
	errInOrg    error
}

func newPathGroupRepo() *pathGroupRepo { return &pathGroupRepo{fakeGroupRepo: &fakeGroupRepo{}} }

func (r *pathGroupRepo) Create(ctx context.Context, g *domain.Group) error {
	if r.createErr != nil {
		return r.createErr
	}
	return r.fakeGroupRepo.Create(ctx, g)
}
func (r *pathGroupRepo) ListByOrg(context.Context, string) ([]*domain.Group, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.groupsInOrg, nil
}
func (r *pathGroupRepo) GroupsForUser(context.Context, string) ([]*domain.Group, error) {
	if r.errForUser != nil {
		return nil, r.errForUser
	}
	return r.groupsUser, nil
}
func (r *pathGroupRepo) GroupsForUserInOrg(context.Context, string, string) ([]*domain.Group, error) {
	if r.errInOrg != nil {
		return nil, r.errInOrg
	}
	return r.groupsAll, nil
}
func (r *pathGroupRepo) List(context.Context) ([]*domain.Group, error) { return r.groupsAll, nil }

type fakeMetaStore struct {
	vals   map[string]string
	getErr error
	setErr error
	sets   map[string]string
}

func newFakeMetaStore() *fakeMetaStore {
	return &fakeMetaStore{vals: map[string]string{}, sets: map[string]string{}}
}
func (m *fakeMetaStore) Get(_ context.Context, key string) (string, error) {
	if m.getErr != nil {
		return m.vals[key], m.getErr
	}
	v, ok := m.vals[key]
	if !ok {
		return "", domain.ErrNotFound
	}
	return v, nil
}
func (m *fakeMetaStore) Set(_ context.Context, key, value string) error {
	if m.setErr != nil {
		return m.setErr
	}
	m.vals[key] = value
	m.sets[key] = value
	return nil
}

func newPathService(t *testing.T, repo *pathOrgRepo) *OrgService {
	t.Helper()
	return NewOrgService(repo, newPathGroupRepo(), nil, logrus.New())
}

func TestOrgServiceSetters(t *testing.T) {
	svc := newPathService(t, newPathOrgRepo())
	meta := newFakeMetaStore()
	if got := svc.WithMetaStore(meta); got != svc {
		t.Fatal("WithMetaStore must return the receiver")
	}
	if got := svc.WithDefaultQuotas(domain.OrgQuotas{MaxGroups: domain.QuotaThreshold{Hard: 2}}); got != svc {
		t.Fatal("WithDefaultQuotas must return the receiver")
	}
	if svc.defaultQuotas.MaxGroups.Hard != 2 {
		t.Fatalf("default quotas = %+v", svc.defaultQuotas)
	}
	svc.SetOnTokenChange(func(context.Context, string, string) {})
	if svc.onTokenChange == nil {
		t.Fatal("SetOnTokenChange did not install the callback")
	}
}

func TestOrgServiceCreateValidationAndErrors(t *testing.T) {
	cases := []struct {
		name  string
		input OrgInput
		repo  func(*pathOrgRepo)
		want  error
	}{
		{"invalid name", OrgInput{Name: "Bad Name"}, nil, domain.ErrValidation},
		{"display too long", OrgInput{Name: "acme", DisplayName: strings.Repeat("x", 129)}, nil, domain.ErrValidation},
		{"invalid quotas", OrgInput{Name: "acme", Quotas: domain.OrgQuotas{MaxGroups: domain.QuotaThreshold{Soft: 5, Hard: 1}}}, nil, domain.ErrValidation},
		{"list error", OrgInput{Name: "acme"}, func(r *pathOrgRepo) { r.listErr = errors.New("boom") }, nil},
		{"create error", OrgInput{Name: "acme"}, func(r *pathOrgRepo) { r.createErr = errors.New("boom") }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newPathOrgRepo()
			if tc.repo != nil {
				tc.repo(repo)
			}
			svc := newPathService(t, repo).WithFleetIsolation("shared")
			if tc.name == "invalid name" || tc.name == "display too long" || tc.name == "invalid quotas" {
				svc = newPathService(t, repo)
			}
			_, err := svc.Create(context.Background(), tc.input)
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}

	t.Run("default group creation error", func(t *testing.T) {
		repo := newPathOrgRepo()
		groups := newPathGroupRepo()
		groups.createErr = errors.New("boom")
		svc := NewOrgService(repo, groups, nil, logrus.New())
		if _, err := svc.Create(context.Background(), OrgInput{Name: "acme"}); err == nil {
			t.Fatal("expected default-group error")
		}
	})

	t.Run("display defaults to slug and features default", func(t *testing.T) {
		svc := newPathService(t, newPathOrgRepo())
		o, err := svc.Create(context.Background(), OrgInput{Name: "Acme"})
		if err != nil {
			t.Fatal(err)
		}
		if o.DisplayName != "acme" || len(o.Features) != len(domain.DefaultOrgFeatures) {
			t.Fatalf("org = %+v", o)
		}
	})
}

func TestOrgServiceNewOrgIDExhaustionAndError(t *testing.T) {
	t.Run("collision retries then fails", func(t *testing.T) {
		// Every generated ID looks taken: the retry loop must give up with
		// ErrConflict rather than returning a duplicate.
		alwaysHit := &alwaysFoundRepo{pathOrgRepo: newPathOrgRepo()}
		svc := NewOrgService(alwaysHit, newPathGroupRepo(), nil, logrus.New())
		if _, err := svc.Create(context.Background(), OrgInput{Name: "acme"}); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
	})

	t.Run("get error aborts", func(t *testing.T) {
		repo := newPathOrgRepo()
		repo.getErr = errors.New("boom")
		svc := newPathService(t, repo)
		if _, err := svc.Create(context.Background(), OrgInput{Name: "acme"}); err == nil {
			t.Fatal("expected error")
		}
	})
}

// alwaysFoundRepo reports every generated org ID as already taken.
type alwaysFoundRepo struct{ *pathOrgRepo }

func (r *alwaysFoundRepo) Get(_ context.Context, id string) (*domain.Org, error) {
	return &domain.Org{ID: id}, nil
}

func TestOrgServiceEnsureBootstrap(t *testing.T) {
	bootID := "org-0000000000000001"
	boot := &domain.Org{ID: bootID, Name: domain.DefaultOrgName, DisplayName: "Default"}
	newBootRepo := func() *pathOrgRepo {
		repo := newPathOrgRepo()
		if err := repo.Create(context.Background(), boot); err != nil {
			t.Fatal(err)
		}
		return repo
	}

	t.Run("bootstraps when missing", func(t *testing.T) {
		repo := newPathOrgRepo()
		svc := newPathService(t, repo).WithMetaStore(newFakeMetaStore())
		o, err := svc.EnsureBootstrap(context.Background(), "Home")
		if err != nil {
			t.Fatal(err)
		}
		if o.Name != domain.DefaultOrgName || o.DisplayName != "Home" {
			t.Fatalf("org = %+v", o)
		}
		if _, err := svc.BootstrapOrgID(context.Background()); err != nil {
			t.Fatalf("bootstrap id: %v", err)
		}
	})

	t.Run("bootstrap proposal error", func(t *testing.T) {
		repo := newPathOrgRepo()
		repo.bootstrapErr = errors.New("boom")
		if _, err := newPathService(t, repo).EnsureBootstrap(context.Background(), ""); err == nil {
			t.Fatal("expected bootstrap error")
		}
	})

	t.Run("slug owned by non-bootstrap org", func(t *testing.T) {
		repo := newPathOrgRepo()
		if err := repo.Create(context.Background(), &domain.Org{ID: "org-9999999999999999", Name: domain.DefaultOrgName}); err != nil {
			t.Fatal(err)
		}
		// The replicated bootstrap marker is not committed yet, but the slug is
		// already taken: bootstrap must refuse to adopt a foreign org.
		repo.getBootstrap = domain.ErrBootstrapNotReady
		if _, err := newPathService(t, repo).EnsureBootstrap(context.Background(), ""); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
	})

	t.Run("get bootstrap error", func(t *testing.T) {
		repo := newPathOrgRepo()
		repo.getBootstrap = errors.New("boom")
		if _, err := newPathService(t, repo).EnsureBootstrap(context.Background(), ""); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("corrupted slug", func(t *testing.T) {
		repo := newPathOrgRepo()
		repo.bootstrapOrg = &domain.Org{ID: bootID, Name: "other"}
		if _, err := newPathService(t, repo).EnsureBootstrap(context.Background(), ""); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("err = %v, want ErrValidation", err)
		}
	})

	t.Run("meta nil leaves display name", func(t *testing.T) {
		o, err := newPathService(t, newBootRepo()).EnsureBootstrap(context.Background(), "Home")
		if err != nil || o.DisplayName != "Default" {
			t.Fatalf("org = %+v err=%v", o, err)
		}
	})

	t.Run("first reconcile writes meta", func(t *testing.T) {
		repo := newBootRepo()
		meta := newFakeMetaStore()
		o, err := newPathService(t, repo).WithMetaStore(meta).EnsureBootstrap(context.Background(), "Home")
		if err != nil || o.DisplayName != "Home" {
			t.Fatalf("org = %+v err=%v", o, err)
		}
		if meta.sets[domain.DefaultOrgDisplayNameMetaKey] != "Home" {
			t.Fatalf("meta = %+v", meta.sets)
		}
	})

	t.Run("unchanged configured value is a no-op", func(t *testing.T) {
		repo := newBootRepo()
		meta := newFakeMetaStore()
		meta.vals[domain.DefaultOrgDisplayNameMetaKey] = "Home"
		o, err := newPathService(t, repo).WithMetaStore(meta).EnsureBootstrap(context.Background(), "Home")
		if err != nil || o.DisplayName != "Default" {
			t.Fatalf("org = %+v err=%v", o, err)
		}
	})

	t.Run("changed configured value updates display name", func(t *testing.T) {
		repo := newBootRepo()
		meta := newFakeMetaStore()
		meta.vals[domain.DefaultOrgDisplayNameMetaKey] = "Old"
		o, err := newPathService(t, repo).WithMetaStore(meta).EnsureBootstrap(context.Background(), "New")
		if err != nil || o.DisplayName != "New" {
			t.Fatalf("org = %+v err=%v", o, err)
		}
		if meta.sets[domain.DefaultOrgDisplayNameMetaKey] != "New" {
			t.Fatalf("meta = %+v", meta.sets)
		}
	})

	t.Run("meta read error propagates", func(t *testing.T) {
		repo := newBootRepo()
		meta := newFakeMetaStore()
		meta.vals[domain.DefaultOrgDisplayNameMetaKey] = "Home"
		meta.getErr = errors.New("boom")
		if _, err := newPathService(t, repo).WithMetaStore(meta).EnsureBootstrap(context.Background(), "Home"); err == nil {
			t.Fatal("expected meta error")
		}
	})

	t.Run("meta write error propagates", func(t *testing.T) {
		repo := newBootRepo()
		meta := newFakeMetaStore()
		meta.setErr = errors.New("boom")
		if _, err := newPathService(t, repo).WithMetaStore(meta).EnsureBootstrap(context.Background(), "Home"); err == nil {
			t.Fatal("expected meta error")
		}
	})

	t.Run("update error propagates", func(t *testing.T) {
		repo := newBootRepo()
		meta := newFakeMetaStore()
		meta.vals[domain.DefaultOrgDisplayNameMetaKey] = "Old"
		repo.updateErr = errors.New("boom")
		if _, err := newPathService(t, repo).WithMetaStore(meta).EnsureBootstrap(context.Background(), "New"); err == nil {
			t.Fatal("expected update error")
		}
	})
}

func TestOrgServiceAccessors(t *testing.T) {
	repo := newPathOrgRepo()
	o := &domain.Org{ID: "org-1111111111111111", Name: "acme", Quotas: domain.OrgQuotas{}}
	boot := &domain.Org{ID: "org-0000000000000001", Name: domain.DefaultOrgName}
	for _, item := range []*domain.Org{o, boot} {
		if err := repo.Create(context.Background(), item); err != nil {
			t.Fatal(err)
		}
	}
	svc := newPathService(t, repo)
	ctx := context.Background()

	if got, err := svc.Get(ctx, o.ID); err != nil || got.Name != "acme" {
		t.Fatalf("Get = %+v err=%v", got, err)
	}
	if got, err := svc.GetByName(ctx, "ACME"); err != nil || got.ID != o.ID {
		t.Fatalf("GetByName = %+v err=%v", got, err)
	}
	if list, err := svc.List(ctx); err != nil || len(list) != 2 {
		t.Fatalf("List = %+v err=%v", list, err)
	}
	if got, err := svc.GetBootstrap(ctx); err != nil || got.ID != boot.ID {
		t.Fatalf("GetBootstrap = %+v err=%v", got, err)
	}
	if _, err := svc.Resolve(ctx, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Resolve empty = %v", err)
	}
}

func TestOrgServiceUpdate(t *testing.T) {
	repo := newPathOrgRepo()
	o := &domain.Org{ID: "org-1111111111111111", Name: "acme", DisplayName: "Acme"}
	if err := repo.Create(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	svc := newPathService(t, repo)
	ctx := context.Background()

	t.Run("rename rejected", func(t *testing.T) {
		if _, err := svc.Update(ctx, o.ID, OrgUpdateInput{Name: "other"}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("err = %v, want ErrValidation", err)
		}
	})
	t.Run("empty display rejected", func(t *testing.T) {
		empty := ""
		if _, err := svc.Update(ctx, o.ID, OrgUpdateInput{DisplayName: &empty}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("err = %v, want ErrValidation", err)
		}
	})
	t.Run("display too long rejected", func(t *testing.T) {
		long := strings.Repeat("x", 129)
		if _, err := svc.Update(ctx, o.ID, OrgUpdateInput{DisplayName: &long}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("err = %v, want ErrValidation", err)
		}
	})
	t.Run("features and display updated", func(t *testing.T) {
		display := "Acme Corp"
		got, err := svc.Update(ctx, o.Name, OrgUpdateInput{DisplayName: &display, Features: []string{"CLOUD_CHECKS"}})
		if err != nil || got.DisplayName != display || len(got.Features) != 1 {
			t.Fatalf("org = %+v err=%v", got, err)
		}
	})
	t.Run("resolve error", func(t *testing.T) {
		if _, err := svc.Update(ctx, "missing", OrgUpdateInput{}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
	t.Run("update error", func(t *testing.T) {
		repo.updateErr = errors.New("boom")
		if _, err := svc.Update(ctx, o.ID, OrgUpdateInput{}); err == nil {
			t.Fatal("expected update error")
		}
		repo.updateErr = nil
	})
}

func TestOrgServiceUpdateQuotasAndDelete(t *testing.T) {
	ctx := context.Background()
	repo := newPathOrgRepo()
	boot := &domain.Org{ID: "org-0000000000000001", Name: domain.DefaultOrgName}
	other := &domain.Org{ID: "org-2222222222222222", Name: "acme"}
	for _, o := range []*domain.Org{boot, other} {
		if err := repo.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	svc := newPathService(t, repo)

	if _, err := svc.UpdateQuotas(ctx, nil, other.ID, domain.OrgQuotas{MaxGroups: domain.QuotaThreshold{Soft: 2, Hard: 1}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid quota = %v", err)
	}
	if _, err := svc.UpdateQuotas(ctx, &domain.Identity{UserID: "admin"}, "missing", domain.OrgQuotas{}); err == nil {
		t.Fatal("expected resolve error")
	}
	got, err := svc.UpdateQuotas(ctx, &domain.Identity{UserID: "admin", Username: "admin"}, other.ID, domain.OrgQuotas{MaxGroups: domain.QuotaThreshold{Hard: 9}})
	if err != nil || got.Quotas.MaxGroups.Hard != 9 {
		t.Fatalf("org = %+v err=%v", got, err)
	}
	if err := svc.Delete(ctx, boot.ID, false, nil); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("delete bootstrap = %v, want ErrForbidden", err)
	}
	if err := svc.Delete(ctx, "missing", false, nil); err == nil {
		t.Fatal("expected resolve error")
	}

	t.Run("force cascade runs external cleanup", func(t *testing.T) {
		var cascadeOrg, tokenOrg string
		svc.SetOnTokenChange(func(_ context.Context, orgID, _ string) { tokenOrg = orgID })
		err := svc.Delete(ctx, other.ID, true, func(_ context.Context, orgID string) error {
			cascadeOrg = orgID
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if cascadeOrg != other.ID || tokenOrg != other.ID {
			t.Fatalf("cascade=%q token=%q, want %q", cascadeOrg, tokenOrg, other.ID)
		}
	})

	t.Run("cascade hook error propagates", func(t *testing.T) {
		if err := repo.Create(ctx, &domain.Org{ID: "org-3333333333333333", Name: "beta"}); err != nil {
			t.Fatal(err)
		}
		if err := svc.Delete(ctx, "org-3333333333333333", true, func(context.Context, string) error {
			return errors.New("boom")
		}); err == nil {
			t.Fatal("expected cascade hook error")
		}
	})

	t.Run("cascade delete error propagates", func(t *testing.T) {
		if err := repo.Create(ctx, &domain.Org{ID: "org-4444444444444444", Name: "gamma"}); err != nil {
			t.Fatal(err)
		}
		repo.deleteCascade = errors.New("boom")
		if err := svc.Delete(ctx, "org-4444444444444444", true, nil); err == nil {
			t.Fatal("expected cascade error")
		}
		repo.deleteCascade = nil
	})

	t.Run("non-cascade delete error propagates", func(t *testing.T) {
		if err := repo.Create(ctx, &domain.Org{ID: "org-5555555555555555", Name: "delta"}); err != nil {
			t.Fatal(err)
		}
		repo.deleteErr = errors.New("boom")
		if err := svc.Delete(ctx, "org-5555555555555555", false, nil); err == nil {
			t.Fatal("expected delete error")
		}
		repo.deleteErr = nil
	})
}

func TestOrgServiceMembershipPaths(t *testing.T) {
	ctx := context.Background()
	repo := newPathOrgRepo()
	groups := newPathGroupRepo()
	if err := repo.Create(ctx, &domain.Org{ID: "org-1111111111111111", Name: "acme"}); err != nil {
		t.Fatal(err)
	}
	svc := NewOrgService(repo, groups, nil, logrus.New())

	if err := svc.SetMember(ctx, "org-1111111111111111", "u1", domain.OrgRole("bogus")); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid role = %v", err)
	}
	repo.setMemberErr = errors.New("boom")
	if err := svc.SetMember(ctx, "org-1111111111111111", "u1", domain.OrgMember); err == nil {
		t.Fatal("expected set error")
	}
	repo.setMemberErr = nil
	if err := svc.SetMember(ctx, "org-1111111111111111", "u1", domain.OrgMember); err != nil {
		t.Fatal(err)
	}
	repo.removeErr = errors.New("boom")
	if err := svc.RemoveMember(ctx, "org-1111111111111111", "u1"); err == nil {
		t.Fatal("expected remove error")
	}
	repo.removeErr = nil
	repo.membersErr = errors.New("boom")
	if _, err := svc.Members(ctx, "org-1111111111111111"); err == nil {
		t.Fatal("expected members error")
	}
	repo.membersErr = nil
	if ms, err := svc.Members(ctx, "org-1111111111111111"); err != nil || len(ms) != 1 {
		t.Fatalf("members = %+v err=%v", ms, err)
	}

	if role, ok, err := svc.MemberRole(ctx, "org-1111111111111111", "u1"); err != nil || !ok || role != domain.OrgMember {
		t.Fatalf("role = %q ok=%v err=%v", role, ok, err)
	}
	repo.memberRoleErr = errors.New("boom")
	if svc.IsOrgAdmin(ctx, "org-1111111111111111", "u1") {
		t.Fatal("IsOrgAdmin must be false on error")
	}
	if _, err := svc.IsOrgMember(ctx, "org-1111111111111111", "u1"); err == nil {
		t.Fatal("expected member-role error")
	}
	repo.memberRoleErr = nil

	t.Run("IsOrgMember via group", func(t *testing.T) {
		groups.groupsAll = []*domain.Group{{ID: "g1", OrgID: "org-1111111111111111"}}
		ok, err := svc.IsOrgMember(ctx, "org-1111111111111111", "u2")
		if err != nil || !ok {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
		groups.errInOrg = errors.New("boom")
		if _, err := svc.IsOrgMember(ctx, "org-1111111111111111", "u2"); err == nil {
			t.Fatal("expected group error")
		}
		groups.errInOrg = nil
		groups.groupsAll = nil
		if ok, err := svc.IsOrgMember(ctx, "org-1111111111111111", "u2"); err != nil || ok {
			t.Fatalf("ok=%v err=%v, want false", ok, err)
		}
	})
}

func TestOrgServiceOrgsForUserPaths(t *testing.T) {
	ctx := context.Background()
	repo := newPathOrgRepo()
	groups := newPathGroupRepo()
	for _, o := range []*domain.Org{
		{ID: "org-1111111111111111", Name: "acme"},
		{ID: "org-2222222222222222", Name: "beta"},
	} {
		if err := repo.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.SetMember(ctx, "org-1111111111111111", "u1", domain.OrgMember); err != nil {
		t.Fatal(err)
	}
	svc := NewOrgService(repo, groups, nil, logrus.New())

	groups.groupsUser = []*domain.Group{
		{ID: "g1", OrgID: "org-2222222222222222"},
		{ID: "g2", OrgID: "org-1111111111111111"}, // already direct
		{ID: "g3"}, // no org
		{ID: "g4", OrgID: "org-missing0000000000"}, // unknown org
	}
	orgs, err := svc.OrgsForUser(ctx, "u1")
	if err != nil || len(orgs) != 2 {
		t.Fatalf("orgs = %+v err=%v", orgs, err)
	}
	groups.errForUser = errors.New("boom")
	if _, err := svc.OrgsForUser(ctx, "u1"); err == nil {
		t.Fatal("expected group error")
	}
	groups.errForUser = nil
	repo.orgsForUser = errors.New("boom")
	if _, err := svc.OrgsForUser(ctx, "u1"); err == nil {
		t.Fatal("expected direct error")
	}
}

func TestOrgServiceTokens(t *testing.T) {
	ctx := context.Background()
	repo := newPathOrgRepo()
	groups := newPathGroupRepo()
	o := &domain.Org{ID: "org-1111111111111111", Name: "acme"}
	if err := repo.Create(ctx, o); err != nil {
		t.Fatal(err)
	}
	tokens := NewTokenService(newStubTokenRepo(), logrus.New(), []byte("0123456789abcdef0123456789abcdef"))
	svc := NewOrgService(repo, groups, tokens, logrus.New())

	if _, _, err := NewOrgService(repo, groups, nil, logrus.New()).GenerateToken(ctx, o.ID); err == nil {
		t.Fatal("expected token-service-unavailable error")
	}
	if _, _, err := svc.GenerateToken(ctx, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	var changedOrg string
	svc.SetOnTokenChange(func(_ context.Context, orgID, _ string) { changedOrg = orgID })
	pt, meta, err := svc.GenerateToken(ctx, o.ID)
	if err != nil || pt == "" || meta == nil || changedOrg != o.ID {
		t.Fatalf("token=%q meta=%+v err=%v changed=%q", pt, meta, err, changedOrg)
	}
	if strings.Contains(pt, "org-") {
		t.Fatalf("token plaintext leaks the org id: %q", pt)
	}

	if got, err := svc.EnsureToken(ctx, o.ID); err != nil || got == "" {
		t.Fatalf("EnsureToken = %q err=%v", got, err)
	}
	if m, err := svc.TokenMeta(ctx, o.ID); err != nil || m.Prefix == "" {
		t.Fatalf("TokenMeta = %+v err=%v", m, err)
	}
	if err := svc.RevokeToken(ctx, o.ID); err != nil || changedOrg != o.ID {
		t.Fatalf("RevokeToken = %v changed=%q", err, changedOrg)
	}
	if m, err := NewOrgService(repo, groups, nil, logrus.New()).TokenMeta(ctx, o.ID); !errors.Is(err, domain.ErrNotFound) || m != nil {
		t.Fatalf("nil tokens TokenMeta = %+v err=%v", m, err)
	}
	if err := NewOrgService(repo, groups, nil, logrus.New()).RevokeToken(ctx, o.ID); err != nil {
		t.Fatalf("nil tokens RevokeToken = %v", err)
	}
}

func TestOrgServiceEnsureTokenErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := NewOrgService(newPathOrgRepo(), newPathGroupRepo(), nil, logrus.New()).EnsureToken(ctx, "org-1111111111111111"); err == nil {
		t.Fatal("expected token-service-unavailable error")
	}
}

func TestParseOrgToken(t *testing.T) {
	cases := []struct {
		token  string
		want   string
		wantOK bool
	}{
		{"dag_acme_deadbeef", "acme", true},
		{"dag_team-a-b_0123", "team-a-b", true},
		{"dct_deadbeef", "", false},
		{"dag_", "", false},
		{"dag_acme_", "", false},
		{"_deadbeef", "", false},
	}
	for _, tc := range cases {
		got, ok := ParseOrgToken(tc.token)
		if got != tc.want || ok != tc.wantOK {
			t.Fatalf("ParseOrgToken(%q) = %q, %v; want %q, %v", tc.token, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestOrgServiceResolveOrgScopedTokenFailures(t *testing.T) {
	ctx := context.Background()
	repo := newPathOrgRepo()
	if err := repo.Create(ctx, &domain.Org{ID: "org-0000000000000001", Name: domain.DefaultOrgName}); err != nil {
		t.Fatal(err)
	}
	svc := NewOrgService(repo, newPathGroupRepo(), nil, logrus.New())
	if _, err := svc.ResolveForIdentity(ctx, &domain.Identity{UserID: "org:org-3333333333333333", OrgID: "org-3333333333333333"}, ""); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("deleted org token = %v, want ErrUnauthenticated", err)
	}
	repo.getErr = errors.New("boom")
	if _, err := svc.ResolveForIdentity(ctx, &domain.Identity{UserID: "org:org-0000000000000001", OrgID: "org-0000000000000001"}, ""); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("store error = %v, want ErrUnauthenticated", err)
	}
}

func TestOrgQuotasValidateAndEffective(t *testing.T) {
	var nilQuotas *domain.OrgQuotas
	if err := nilQuotas.Validate(); err != nil {
		t.Fatalf("nil Validate = %v", err)
	}
	if err := (&domain.OrgQuotas{MaxPipelineRetention: -time.Second}).Validate(); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("negative retention = %v", err)
	}
	if err := (&domain.OrgQuotas{MaxCacheBytes: domain.QuotaThreshold{Soft: -1}}).Validate(); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("negative threshold = %v", err)
	}
	if err := (domain.QuotaThreshold{Soft: 1, Hard: 1}).Validate("x"); err != nil {
		t.Fatalf("equal thresholds = %v", err)
	}

	defaults := domain.OrgQuotas{
		MaxGroups:            domain.QuotaThreshold{Soft: 5, Hard: 10},
		MaxProjects:          domain.QuotaThreshold{Soft: 6, Hard: 12},
		MaxPipelineRetention: 48 * time.Hour,
	}
	own := domain.OrgQuotas{
		MaxGroups:   domain.QuotaThreshold{Hard: 3},
		MaxProjects: domain.QuotaThreshold{Soft: 2},
	}
	eff := own.Effective(defaults)
	if eff.MaxGroups.Hard != 3 || eff.MaxProjects.Soft != 2 {
		t.Fatalf("own overrides lost: %+v", eff)
	}
	if eff.MaxMembers != defaults.MaxMembers || eff.MaxConcurrentSessions != defaults.MaxConcurrentSessions {
		t.Fatalf("defaults not inherited: %+v", eff)
	}
	if eff.MaxPipelineRetention != 48*time.Hour {
		t.Fatalf("retention = %v, want inherited 48h", eff.MaxPipelineRetention)
	}
	own.MaxPipelineRetention = time.Hour
	if got := own.Effective(defaults); got.MaxPipelineRetention != time.Hour {
		t.Fatalf("retention = %v, want own 1h", got.MaxPipelineRetention)
	}
}

func TestOrgHasFeature(t *testing.T) {
	var nilOrg *domain.Org
	if nilOrg.HasFeature("CLOUD_CHECKS") {
		t.Fatal("nil org must not have features")
	}
	o := &domain.Org{Features: []string{"CLOUD_CHECKS"}}
	if !o.HasFeature("CLOUD_CHECKS") || o.HasFeature("CLOUD_MODULES") {
		t.Fatalf("features = %+v", o.Features)
	}
}

func TestResolveForIdentityExplicitErrors(t *testing.T) {
	ctx := context.Background()
	bootID := "org-0000000000000001"
	newSvc := func(repo *pathOrgRepo) *OrgService {
		if err := repo.Create(ctx, &domain.Org{ID: bootID, Name: domain.DefaultOrgName}); err != nil {
			t.Fatal(err)
		}
		if err := repo.Create(ctx, &domain.Org{ID: "org-1111111111111111", Name: "acme"}); err != nil {
			t.Fatal(err)
		}
		return NewOrgService(repo, newPathGroupRepo(), nil, logrus.New())
	}

	t.Run("admin missing org is not found", func(t *testing.T) {
		svc := newSvc(newPathOrgRepo())
		if _, err := svc.ResolveForIdentity(ctx, &domain.Identity{UserID: "admin", Role: domain.RoleAdmin}, "globex"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("store error propagates", func(t *testing.T) {
		repo := newPathOrgRepo()
		svc := newSvc(repo)
		repo.getErr = errors.New("boom")
		if _, err := svc.ResolveForIdentity(ctx, &domain.Identity{UserID: "u1"}, "acme"); err == nil {
			t.Fatal("expected store error")
		}
	})

	t.Run("membership lookup error propagates", func(t *testing.T) {
		repo := newPathOrgRepo()
		svc := newSvc(repo)
		repo.orgsForUser = errors.New("boom")
		if _, err := svc.ResolveForIdentity(ctx, &domain.Identity{UserID: "u1"}, ""); err == nil {
			t.Fatal("expected membership lookup error")
		}
	})
}
