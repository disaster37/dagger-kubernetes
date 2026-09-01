package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type connectFixture struct {
	svc    *ConnectService
	tokens *TokenService
	users  *UserService
	vr     *Resolver
	cfg    *domain.Config
	cache  *Cache
}

func newConnectFixture(t *testing.T, cache *Cache, key []byte) *connectFixture {
	t.Helper()
	cfg := &domain.Config{
		Server:  domain.ServerConfig{PublicURL: "https://supv.example.com", DataHost: "data.example.com"},
		Cache:   domain.CacheConfig{Backend: cache.Type},
		Version: domain.VersionConfig{Floor: "v0.19.0"},
	}
	vr, err := NewResolver("v0.19.0", nil, nil)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	r := newServiceDB(t)
	ts := NewTokenService(r.tokens, testLogger(), key)
	us := NewUserService(r.users, r.groups, testLogger())
	cs := NewConnectService(cfg, cache, vr, ts, testLogger())
	return &connectFixture{svc: cs, tokens: ts, users: us, vr: vr, cfg: cfg, cache: cache}
}

// setReleases re-wires the fixture's resolver (and the ConnectService that
// holds it) with a fresh resolver populated with releases. Replaces the removed
// Resolver.SetReleases mutation.
func (fx *connectFixture) setReleases(t *testing.T, releases map[string][]string) {
	t.Helper()
	vr, err := NewResolver("v0.19.0", nil, releases)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	fx.vr = vr
	fx.svc = NewConnectService(fx.cfg, fx.cache, vr, fx.tokens, testLogger())
}

func registryCache() *Cache {
	return &Cache{Type: "registry", Registry: "cache.reg/dagger-cache"}
}

func envValue(envs []domain.ConnectEnvVar, name string) string {
	for _, e := range envs {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func TestConnectEnvNoVersionMasked(t *testing.T) {
	fx := newConnectFixture(t, registryCache(), testEncKey())
	ctx := context.Background()
	u := seedUserSvc(t, fx.users, "u1")
	if _, _, err := fx.tokens.Generate(ctx, u.ID); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	snap, err := fx.svc.ConnectEnv(ctx, u.ID, "", false)
	if err != nil {
		t.Fatalf("ConnectEnv: %v", err)
	}
	if snap.ServerURL != "https://supv.example.com" {
		t.Fatalf("ServerURL = %q", snap.ServerURL)
	}
	if snap.DataHostname != "data.example.com" {
		t.Fatalf("DataHostname = %q", snap.DataHostname)
	}
	if snap.CacheBackend != "registry" {
		t.Fatalf("CacheBackend = %q", snap.CacheBackend)
	}
	if snap.VersionFloor != "v0.19.0" {
		t.Fatalf("VersionFloor = %q", snap.VersionFloor)
	}
	if !snap.Token.Exists || !snap.Token.Recoverable {
		t.Fatalf("Token = %+v, want exists+recoverable", snap.Token)
	}
	if snap.Token.Prefix == "" {
		t.Fatal("Token.Prefix empty")
	}
	if len(snap.EnvVars) != 4 {
		t.Fatalf("EnvVars = %d, want 4", len(snap.EnvVars))
	}
	if got := envValue(snap.EnvVars, "DAGGER_CLOUD_TOKEN"); got != "" {
		t.Fatalf("masked token value = %q, want empty", got)
	}
	if got := envValue(snap.EnvVars, "DAGGER_CLOUD_URL"); got != "https://supv.example.com" {
		t.Fatalf("DAGGER_CLOUD_URL = %q", got)
	}
	if got := envValue(snap.EnvVars, "_EXPERIMENTAL_DAGGER_RUNNER_HOST"); got != "dagger-cloud://self" {
		t.Fatalf("runner host = %q", got)
	}
	if got := envValue(snap.EnvVars, "_EXPERIMENTAL_DAGGER_TAG"); got != "" {
		t.Fatalf("TAG = %q, want empty (not pinned)", got)
	}
	want := registryCache().BuildCacheConfig("max")
	if got := envValue(snap.EnvVars, "_EXPERIMENTAL_DAGGER_CACHE_CONFIG"); got != want {
		t.Fatalf("CACHE_CONFIG = %q, want %q", got, want)
	}
}

func TestConnectEnvNoVersionRevealed(t *testing.T) {
	fx := newConnectFixture(t, registryCache(), testEncKey())
	ctx := context.Background()
	u := seedUserSvc(t, fx.users, "u1")
	plaintext, _, err := fx.tokens.Generate(ctx, u.ID)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	snap, err := fx.svc.ConnectEnv(ctx, u.ID, "", true)
	if err != nil {
		t.Fatalf("ConnectEnv: %v", err)
	}
	if got := envValue(snap.EnvVars, "DAGGER_CLOUD_TOKEN"); got != plaintext {
		t.Fatalf("revealed token = %q, want plaintext", got)
	}
	want := registryCache().BuildCacheConfig("max")
	if got := envValue(snap.EnvVars, "_EXPERIMENTAL_DAGGER_CACHE_CONFIG"); got != want {
		t.Fatalf("CACHE_CONFIG = %q, want %q", got, want)
	}
}

func TestConnectEnvWithVersion(t *testing.T) {
	fx := newConnectFixture(t, registryCache(), testEncKey())
	ctx := context.Background()
	u := seedUserSvc(t, fx.users, "u1")

	snap, err := fx.svc.ConnectEnv(ctx, u.ID, "v0.21.4", false)
	if err != nil {
		t.Fatalf("ConnectEnv: %v", err)
	}
	if snap.SelectedVersion != "v0.21.4" {
		t.Fatalf("SelectedVersion = %q", snap.SelectedVersion)
	}
	if len(snap.EnvVars) != 5 {
		t.Fatalf("EnvVars = %d, want 5", len(snap.EnvVars))
	}
	if got := envValue(snap.EnvVars, "_EXPERIMENTAL_DAGGER_TAG"); got != "v0.21.4" {
		t.Fatalf("TAG = %q", got)
	}
	if got := envValue(snap.EnvVars, "_EXPERIMENTAL_DAGGER_CACHE_CONFIG"); got != registryCache().BuildCacheConfig("max") {
		t.Fatalf("CACHE_CONFIG = %q, want %q", got, registryCache().BuildCacheConfig("max"))
	}
}

func TestConnectEnvS3Backend(t *testing.T) {
	cache := &Cache{Type: "s3", S3: domain.S3Ref{Bucket: "my-bucket", Region: "us-east-1"}}
	fx := newConnectFixture(t, cache, testEncKey())
	ctx := context.Background()
	u := seedUserSvc(t, fx.users, "u1")

	snap, err := fx.svc.ConnectEnv(ctx, u.ID, "v0.21.4", false)
	if err != nil {
		t.Fatalf("ConnectEnv: %v", err)
	}
	if snap.CacheBackend != "s3" {
		t.Fatalf("CacheBackend = %q", snap.CacheBackend)
	}
	if got := envValue(snap.EnvVars, "_EXPERIMENTAL_DAGGER_CACHE_CONFIG"); got != cache.BuildCacheConfig("max") {
		t.Fatalf("CACHE_CONFIG = %q, want %q", got, cache.BuildCacheConfig("max"))
	}
}

func TestConnectEnvRegistryPublicHost(t *testing.T) {
	cache := &Cache{Type: "registry", Registry: "cache.reg/dagger-cache", PublicHost: "cache.example.com"}
	fx := newConnectFixture(t, cache, testEncKey())
	ctx := context.Background()
	u := seedUserSvc(t, fx.users, "u1")

	snap, err := fx.svc.ConnectEnv(ctx, u.ID, "v0.21.4", false)
	if err != nil {
		t.Fatalf("ConnectEnv: %v", err)
	}
	want := cache.BuildCacheConfig("max")
	if got := envValue(snap.EnvVars, "_EXPERIMENTAL_DAGGER_CACHE_CONFIG"); got != want {
		t.Fatalf("CACHE_CONFIG = %q, want %q", got, want)
	}
	if envValue(snap.EnvVars, "_EXPERIMENTAL_DAGGER_CACHE_CONFIG") == "" {
		t.Fatal("empty cache config")
	}
}

func TestConnectEnvInvalidVersion(t *testing.T) {
	fx := newConnectFixture(t, registryCache(), testEncKey())
	ctx := context.Background()
	u := seedUserSvc(t, fx.users, "u1")

	if _, err := fx.svc.ConnectEnv(ctx, u.ID, "notaversion", false); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid version: %v, want ErrValidation", err)
	}
}

func TestConnectEnvDisallowedVersion(t *testing.T) {
	fx := newConnectFixture(t, registryCache(), testEncKey())
	ctx := context.Background()
	u := seedUserSvc(t, fx.users, "u1")

	if _, err := fx.svc.ConnectEnv(ctx, u.ID, "v0.10.0", false); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("disallowed version: %v, want ErrValidation", err)
	}
}

func TestConnectEnvTokenMissing(t *testing.T) {
	fx := newConnectFixture(t, registryCache(), testEncKey())
	ctx := context.Background()
	u := seedUserSvc(t, fx.users, "u1")

	snap, err := fx.svc.ConnectEnv(ctx, u.ID, "", false)
	if err != nil {
		t.Fatalf("ConnectEnv: %v", err)
	}
	if snap.Token.Exists || snap.Token.Recoverable {
		t.Fatalf("Token = %+v, want missing", snap.Token)
	}
}

func TestConnectEnvTokenNotRecoverable(t *testing.T) {
	fx := newConnectFixture(t, registryCache(), testEncKey())
	ctx := context.Background()
	u := seedUserSvc(t, fx.users, "u1")

	// Pre-v2 token: no ciphertext.
	if err := fx.tokens.tokens.Upsert(ctx, &domain.APIToken{
		ID:        newID(),
		UserID:    u.ID,
		TokenHash: HashAPIToken("dct_pre-v2"),
		Prefix:    "dct_pre-v2",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	snap, err := fx.svc.ConnectEnv(ctx, u.ID, "", true)
	if err != nil {
		t.Fatalf("ConnectEnv: %v", err)
	}
	if snap.Token.Recoverable {
		t.Fatal("pre-v2 token should not be recoverable")
	}
	if got := envValue(snap.EnvVars, "DAGGER_CLOUD_TOKEN"); got != "" {
		t.Fatalf("token value = %q, want empty", got)
	}
}

func TestConnectEnvEmptyUserID(t *testing.T) {
	fx := newConnectFixture(t, registryCache(), testEncKey())
	ctx := context.Background()

	snap, err := fx.svc.ConnectEnv(ctx, "", "", false)
	if err != nil {
		t.Fatalf("ConnectEnv: %v", err)
	}
	if snap.Token.Exists {
		t.Fatal("empty user id should yield missing token")
	}
	if len(snap.EnvVars) != 4 {
		t.Fatalf("EnvVars = %d, want 4", len(snap.EnvVars))
	}
}

func TestConnectEnvAllowedVersions(t *testing.T) {
	fx := newConnectFixture(t, registryCache(), testEncKey())
	ctx := context.Background()
	u := seedUserSvc(t, fx.users, "u1")

	fx.setReleases(t, map[string][]string{"0.21": {"v0.21.4"}})
	snap, err := fx.svc.ConnectEnv(ctx, u.ID, "", false)
	if err != nil {
		t.Fatalf("ConnectEnv: %v", err)
	}
	want := []string{"v0.21.4"}
	if len(snap.AllowedVersions) != len(want) || snap.AllowedVersions[0] != want[0] {
		t.Fatalf("AllowedVersions = %v, want %v", snap.AllowedVersions, want)
	}
}

func TestConnectEnvNoVersionLatestRelease(t *testing.T) {
	fx := newConnectFixture(t, registryCache(), testEncKey())
	ctx := context.Background()
	u := seedUserSvc(t, fx.users, "u1")

	fx.setReleases(t, map[string][]string{
		"0.19": {"v0.19.0"},
		"0.21": {"v0.21.4"},
	})

	snap, err := fx.svc.ConnectEnv(ctx, u.ID, "", false)
	if err != nil {
		t.Fatalf("ConnectEnv: %v", err)
	}
	releases := fx.vr.AllReleases()
	latest := releases[len(releases)-1]
	if latest.String() != "v0.21.4" {
		t.Fatalf("latest release = %q, want v0.21.4", latest)
	}
	if latest.String() == fx.vr.Floor().String() {
		t.Fatalf("latest release %q equals floor, want a later release", latest)
	}
	want := registryCache().BuildCacheConfig("max")
	if got := envValue(snap.EnvVars, "_EXPERIMENTAL_DAGGER_CACHE_CONFIG"); got != want {
		t.Fatalf("CACHE_CONFIG = %q, want %q", got, want)
	}
	if got := envValue(snap.EnvVars, "_EXPERIMENTAL_DAGGER_TAG"); got != "" {
		t.Fatalf("TAG = %q, want empty (not pinned)", got)
	}
}

func TestConnectEnvNoVersionS3(t *testing.T) {
	cache := &Cache{Type: "s3", S3: domain.S3Ref{Bucket: "my-bucket", Region: "us-east-1"}}
	fx := newConnectFixture(t, cache, testEncKey())
	ctx := context.Background()
	u := seedUserSvc(t, fx.users, "u1")

	snap, err := fx.svc.ConnectEnv(ctx, u.ID, "", false)
	if err != nil {
		t.Fatalf("ConnectEnv: %v", err)
	}
	want := cache.BuildCacheConfig("max")
	if got := envValue(snap.EnvVars, "_EXPERIMENTAL_DAGGER_CACHE_CONFIG"); got != want {
		t.Fatalf("CACHE_CONFIG = %q, want %q", got, want)
	}
}

func TestConnectEnvNoVersionUnknownBackend(t *testing.T) {
	cache := &Cache{Type: "unknown"}
	fx := newConnectFixture(t, cache, testEncKey())
	ctx := context.Background()
	u := seedUserSvc(t, fx.users, "u1")

	snap, err := fx.svc.ConnectEnv(ctx, u.ID, "", false)
	if err != nil {
		t.Fatalf("ConnectEnv: %v", err)
	}
	if got := envValue(snap.EnvVars, "_EXPERIMENTAL_DAGGER_CACHE_CONFIG"); got != "" {
		t.Fatalf("CACHE_CONFIG = %q, want empty", got)
	}
	if len(snap.EnvVars) != 3 {
		t.Fatalf("EnvVars = %d, want 3", len(snap.EnvVars))
	}
}

// errorTokenRepo returns a non-NotFound error from GetByUser to exercise the
// tokenMeta failure branch.
func TestConnectEnvTokenMetaError(t *testing.T) {
	cache := registryCache()
	cfg := &domain.Config{
		Server:  domain.ServerConfig{PublicURL: "https://supv.example.com", DataHost: "data.example.com"},
		Cache:   domain.CacheConfig{Backend: "registry"},
		Version: domain.VersionConfig{Floor: "v0.19.0"},
	}
	vr, err := NewResolver("v0.19.0", nil, nil)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	ts := NewTokenService(errorTokenRepo{}, testLogger(), testEncKey())
	cs := NewConnectService(cfg, cache, vr, ts, testLogger())

	snap, err := cs.ConnectEnv(context.Background(), "u1", "", false)
	if err != nil {
		t.Fatalf("ConnectEnv: %v", err)
	}
	if snap.Token.Exists {
		t.Fatal("token meta error should yield missing token")
	}
}

func TestConnectEnvRevealDecryptFails(t *testing.T) {
	fx := newConnectFixture(t, registryCache(), testEncKey())
	ctx := context.Background()
	u := seedUserSvc(t, fx.users, "u1")

	// Non-empty corrupt ciphertext => recoverable=true but Reveal fails, so the
	// token value stays empty and the failure is logged, not returned.
	if err := fx.tokens.tokens.Upsert(ctx, &domain.APIToken{
		ID:              newID(),
		UserID:          u.ID,
		TokenHash:       HashAPIToken("dct_corrupt"),
		TokenCiphertext: "corrupt-ciphertext-that-is-long-enough",
		Prefix:          "dct_corrupt",
		CreatedAt:       time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	snap, err := fx.svc.ConnectEnv(ctx, u.ID, "", true)
	if err != nil {
		t.Fatalf("ConnectEnv: %v", err)
	}
	if !snap.Token.Recoverable {
		t.Fatal("corrupt-but-present ciphertext should be recoverable=true")
	}
	if got := envValue(snap.EnvVars, "DAGGER_CLOUD_TOKEN"); got != "" {
		t.Fatalf("token value = %q, want empty (reveal failed)", got)
	}
}
