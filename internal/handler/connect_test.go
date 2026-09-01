package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

func decodeSnapshot(t *testing.T, body []byte) domain.ConnectEnvSnapshot {
	t.Helper()
	var snap domain.ConnectEnvSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	return snap
}

func snapshotEnvValue(snap *domain.ConnectEnvSnapshot, name string) string {
	for _, e := range snap.EnvVars {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// loginJWTUserWithToken creates a user, generates an API token, and returns a
// JWT bearer for the user (the "JWT user with a recoverable token" shape).
func loginJWTUserWithToken(t *testing.T, env *testEnv) string {
	t.Helper()
	u, err := env.users.Create(context.Background(), "alice", "password123", domain.RoleUser)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, _, err := env.tokens.Generate(context.Background(), u.ID); err != nil {
		t.Fatalf("generate token: %v", err)
	}
	return loginAsUser(t, env)
}

// loginAsUser logs in via username/password and returns the JWT bearer for the
// pre-seeded "alice" user.
func loginAsUser(t *testing.T, env *testEnv) string {
	t.Helper()
	access, _, _, err := env.auth.Login(context.Background(), "alice", "password123")
	if err != nil {
		t.Fatalf("login alice: %v", err)
	}
	return "Bearer " + access
}

func TestConnectEnvRequiresAuth(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/connect/env", nil)
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("unauth: %d, want 401", resp.Result().StatusCode())
	}
}

func TestConnectEnvUnavailable(t *testing.T) {
	env := newTestEnv(t)
	env.server.connect = nil
	e := newAuthEngine(env.server)
	bearer := loginJWTUserWithToken(t, env)

	resp := ut.PerformRequest(e, "GET", "/api/v1/connect/env", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusInternalServerError {
		t.Fatalf("nil connect: %d, want 500", resp.Result().StatusCode())
	}
}

func TestConnectEnvDefaultMasked(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	bearer := loginJWTUserWithToken(t, env)

	resp := ut.PerformRequest(e, "GET", "/api/v1/connect/env", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("default: %d, want 200", resp.Result().StatusCode())
	}
	snap := decodeSnapshot(t, resp.Result().Body())
	if !snap.Token.Exists || !snap.Token.Recoverable {
		t.Fatalf("Token = %+v, want exists+recoverable", snap.Token)
	}
	if got := snapshotEnvValue(&snap, "DAGGER_CLOUD_TOKEN"); got != "" {
		t.Fatalf("masked token value = %q, want empty", got)
	}
	cache, ok := env.server.cacheBackend.(*service.Cache)
	if !ok {
		t.Fatal("cache backend is not *service.Cache")
	}
	want := cache.BuildCacheConfig("max")
	if got := snapshotEnvValue(&snap, "_EXPERIMENTAL_DAGGER_CACHE_CONFIG"); got != want {
		t.Fatalf("CACHE_CONFIG = %q, want %q", got, want)
	}
}

func TestConnectEnvRevealed(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	u, err := env.users.Create(context.Background(), "alice", "password123", domain.RoleUser)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	plaintext, _, err := env.tokens.Generate(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	bearer := loginAsUser(t, env)

	resp := ut.PerformRequest(e, "GET", "/api/v1/connect/env?reveal=true", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("revealed: %d, want 200", resp.Result().StatusCode())
	}
	snap := decodeSnapshot(t, resp.Result().Body())
	if got := snapshotEnvValue(&snap, "DAGGER_CLOUD_TOKEN"); got != plaintext {
		t.Fatalf("revealed token = %q, want plaintext", got)
	}
	if cc := string(resp.Result().Header.Peek("Cache-Control")); !strings.Contains(cc, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
}

func TestConnectEnvWithVersion(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	bearer := loginJWTUserWithToken(t, env)

	resp := ut.PerformRequest(e, "GET", "/api/v1/connect/env?version=v0.21.4", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("with version: %d, want 200", resp.Result().StatusCode())
	}
	snap := decodeSnapshot(t, resp.Result().Body())
	if snap.SelectedVersion != "v0.21.4" {
		t.Fatalf("SelectedVersion = %q", snap.SelectedVersion)
	}
	if got := snapshotEnvValue(&snap, "_EXPERIMENTAL_DAGGER_TAG"); got == "" {
		t.Fatal("missing _EXPERIMENTAL_DAGGER_TAG")
	}
	if got := snapshotEnvValue(&snap, "_EXPERIMENTAL_DAGGER_CACHE_CONFIG"); got == "" {
		t.Fatal("missing _EXPERIMENTAL_DAGGER_CACHE_CONFIG")
	}
}

func TestConnectEnvInvalidVersion(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	bearer := loginJWTUserWithToken(t, env)

	resp := ut.PerformRequest(e, "GET", "/api/v1/connect/env?version=notaversion", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("invalid version: %d, want 400", resp.Result().StatusCode())
	}
}

func TestConnectEnvDisallowedVersion(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	bearer := loginJWTUserWithToken(t, env)

	resp := ut.PerformRequest(e, "GET", "/api/v1/connect/env?version=v0.10.0", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("disallowed version: %d, want 400", resp.Result().StatusCode())
	}
}

func TestConnectEnvTokenMissing(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	if _, err := env.users.Create(context.Background(), "alice", "password123", domain.RoleUser); err != nil {
		t.Fatalf("create user: %v", err)
	}
	bearer := loginAsUser(t, env)

	resp := ut.PerformRequest(e, "GET", "/api/v1/connect/env", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("token missing: %d, want 200", resp.Result().StatusCode())
	}
	snap := decodeSnapshot(t, resp.Result().Body())
	if snap.Token.Exists {
		t.Fatalf("Token = %+v, want missing", snap.Token)
	}
}

func TestConnectEnvTokenNotRecoverable(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	ctx := context.Background()

	u, err := env.users.Create(ctx, "alice", "password123", domain.RoleUser)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Persist a pre-v2 token (no ciphertext).
	if err := repository.NewTokenRepo(env.store).Upsert(ctx, &domain.APIToken{
		ID:        "t-pre-v2",
		UserID:    u.ID,
		TokenHash: service.HashAPIToken("dct_pre-v2"),
		Prefix:    "dct_pre-v2",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("upsert pre-v2 token: %v", err)
	}
	bearer := loginAsUser(t, env)

	resp := ut.PerformRequest(e, "GET", "/api/v1/connect/env?reveal=true", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("pre-v2: %d, want 200", resp.Result().StatusCode())
	}
	snap := decodeSnapshot(t, resp.Result().Body())
	if snap.Token.Recoverable {
		t.Fatal("pre-v2 token should not be recoverable")
	}
	if got := snapshotEnvValue(&snap, "DAGGER_CLOUD_TOKEN"); got != "" {
		t.Fatalf("token value = %q, want empty", got)
	}
}

func TestConnectEnvCacheControlHeader(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)
	bearer := loginJWTUserWithToken(t, env)

	for _, path := range []string{"/api/v1/connect/env", "/api/v1/connect/env?reveal=true"} {
		resp := ut.PerformRequest(e, "GET", path, nil, ut.Header{Key: "Authorization", Value: bearer})
		if cc := string(resp.Result().Header.Peek("Cache-Control")); !strings.Contains(cc, "no-store") {
			t.Fatalf("%s: Cache-Control = %q, want no-store", path, cc)
		}
	}
}

func TestConnectEnvNoTokenInLogs(t *testing.T) {
	env := newTestEnv(t)

	var buf bytes.Buffer
	logger := logrus.New()
	logger.SetOutput(&buf)
	logger.SetFormatter(&logrus.TextFormatter{})
	env.server.logger = logger

	cache, ok := env.server.cacheBackend.(*service.Cache)
	if !ok {
		t.Fatal("cache backend is not *service.Cache")
	}
	env.server.connect = service.NewConnectService(&domain.Config{
		Server:  domain.ServerConfig{PublicURL: "https://supv.example.com", DataHost: "data.example.com"},
		Cache:   domain.CacheConfig{Backend: "registry"},
		Version: domain.VersionConfig{Floor: "v0.19.0"},
	}, cache, env.server.versionResolver, env.server.tokens, logger)

	e := newAuthEngine(env.server)
	u, err := env.users.Create(context.Background(), "alice", "password123", domain.RoleUser)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	plaintext, _, err := env.tokens.Generate(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	bearer := loginAsUser(t, env)

	resp := ut.PerformRequest(e, "GET", "/api/v1/connect/env?reveal=true", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("revealed: %d, want 200", resp.Result().StatusCode())
	}
	if strings.Contains(buf.String(), plaintext) {
		t.Fatalf("token plaintext leaked into logs: %s", buf.String())
	}
}
