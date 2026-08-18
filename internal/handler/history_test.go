package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestHandleHistoryInfoAuthGating(t *testing.T) {
	env := newTestEnv(t)
	e := newTestEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/history", nil)
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.Result().StatusCode())
	}

	auth := env.loginAsAdmin(t)
	resp = ut.PerformRequest(e, "GET", "/api/v1/history", nil, ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Result().StatusCode())
	}
}

func TestHandleHistoryInfoShape(t *testing.T) {
	env := newTestEnv(t)
	e := newTestEngine(env.server)

	auth := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/history", nil, ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Result().StatusCode())
	}

	var info domain.HistoryStats
	if err := json.Unmarshal(resp.Result().Body(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.TraceCount != 0 {
		t.Fatalf("info = %+v", info)
	}
}

func TestHandleHistoryPurgeAdminOnly(t *testing.T) {
	env := newTestEnv(t)
	e := newTestEngine(env.server)

	body := `{"trace_id":"aaaaaaaaaaaaaaaa"}`
	// No token → 401.
	resp := ut.PerformRequest(e, "POST", "/api/v1/history/purge", &ut.Body{Body: strings.NewReader(body), Len: len(body)}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.Result().StatusCode())
	}

	// Regular user → 403.
	alice, _ := env.createUserAndToken(t)
	resp = ut.PerformRequest(e, "POST", "/api/v1/history/purge", &ut.Body{Body: strings.NewReader(body), Len: len(body)}, ut.Header{Key: "Authorization", Value: alice}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusForbidden {
		t.Fatalf("expected 403 for user, got %d", resp.Result().StatusCode())
	}

	// Admin → 200 with stub result.
	auth := env.loginAsAdmin(t)
	resp = ut.PerformRequest(e, "POST", "/api/v1/history/purge", &ut.Body{Body: strings.NewReader(body), Len: len(body)}, ut.Header{Key: "Authorization", Value: auth}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200 for admin, got %d", resp.Result().StatusCode())
	}
}

func TestHandleHistoryPurgeInvalidTraceID(t *testing.T) {
	env := newTestEnv(t)
	e := newTestEngine(env.server)
	auth := env.loginAsAdmin(t)

	body := `{"trace_id":"not-hex!"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/history/purge", &ut.Body{Body: strings.NewReader(body), Len: len(body)}, ut.Header{Key: "Authorization", Value: auth}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.Result().StatusCode())
	}
}

func TestHandleHistoryPurgeAllAdminOnly(t *testing.T) {
	env := newTestEnv(t)
	e := newTestEngine(env.server)

	resp := ut.PerformRequest(e, "POST", "/api/v1/history/purge-all", nil)
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.Result().StatusCode())
	}

	auth := env.loginAsAdmin(t)
	resp = ut.PerformRequest(e, "POST", "/api/v1/history/purge-all", nil, ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Result().StatusCode())
	}
}

func TestHandleHistoryPurgeErrorMapping(t *testing.T) {
	env := newTestEnv(t)
	env.server.historyPurger = &stubHistoryPurger{err: domain.ErrHistoryPurgeRunning}
	e := newTestEngine(env.server)
	auth := env.loginAsAdmin(t)

	body := `{"trace_id":"aaaaaaaaaaaaaaaa"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/history/purge", &ut.Body{Body: strings.NewReader(body), Len: len(body)}, ut.Header{Key: "Authorization", Value: auth}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.Result().StatusCode())
	}
}
