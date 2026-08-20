package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// TestHandleTracesURL proves the GET /api/v1/traces/:traceID/url endpoint:
// admins get the self-hosted URL; non-owner non-admins get 404; a misconfigured
// (empty) pipeline base yields 500.
func TestHandleTracesURL(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	adminBearer := env.loginAsAdmin(t)

	t.Run("admin success", func(t *testing.T) {
		e := newAuthEngine(env.server)
		resp := ut.PerformRequest(e, "GET", "/api/v1/traces/trace-url-1/url", nil, ut.Header{Key: "Authorization", Value: adminBearer})
		if resp.Result().StatusCode() != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.Result().StatusCode())
		}
		var body traceURLResponse
		if err := json.Unmarshal(resp.Result().Body(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.TraceID != "trace-url-1" {
			t.Fatalf("trace_id = %q, want trace-url-1", body.TraceID)
		}
		if body.URL != "https://supv.example.com/pipelines/trace-url-1" {
			t.Fatalf("url = %q, want https://supv.example.com/pipelines/trace-url-1", body.URL)
		}
	})

	t.Run("non owner non admin forbidden", func(t *testing.T) {
		bearer, _ := env.createUserAndToken(t)
		// Seed a trace owned by the admin so alice (non-owner, no group) is
		// denied visibility (404, not 403, to avoid leaking existence).
		admin, err := env.users.GetByUsername(ctx, "admin")
		if err != nil {
			t.Fatalf("get admin: %v", err)
		}
		if err := env.server.traceMeta.UpsertProvision(ctx, "trace-url-denied", admin.ID, ""); err != nil {
			t.Fatalf("upsert provision: %v", err)
		}

		e := newAuthEngine(env.server)
		resp := ut.PerformRequest(e, "GET", "/api/v1/traces/trace-url-denied/url", nil, ut.Header{Key: "Authorization", Value: bearer})
		if resp.Result().StatusCode() != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.Result().StatusCode())
		}
	})

	t.Run("misconfigured base", func(t *testing.T) {
		env.server.cfg.PipelineURL = ""
		e := newAuthEngine(env.server)
		resp := ut.PerformRequest(e, "GET", "/api/v1/traces/trace-url-500/url", nil, ut.Header{Key: "Authorization", Value: adminBearer})
		if resp.Result().StatusCode() != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", resp.Result().StatusCode())
		}
	})

	t.Run("invalid trace id charset yields 400", func(t *testing.T) {
		e := newAuthEngine(env.server)
		// "%20" decodes to a space, which fails the charset regex but
		// survives routing and authorizeTrace (admin allowed for unknown).
		resp := ut.PerformRequest(e, "GET", "/api/v1/traces/bad%20id/url", nil, ut.Header{Key: "Authorization", Value: adminBearer})
		if resp.Result().StatusCode() != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for invalid trace id charset", resp.Result().StatusCode())
		}
	})
}

// TestTraceIDParamMissing covers the empty-:traceID path of the trace helpers
// (traceIDParam writes 400 "missing trace ID").
func TestTraceIDParamMissing(t *testing.T) {
	c := app.NewContext(0)
	if _, ok := traceIDParam(c); ok {
		t.Fatal("traceIDParam with empty param = ok, want false")
	}
	if c.Response.StatusCode() != consts.StatusBadRequest {
		t.Fatalf("status = %d, want 400", c.Response.StatusCode())
	}
}

// TestHandleTracesDetailIncludesURL proves the url field is present and
// correct on trace detail, and omitted (status still 200) when the pipeline
// base is misconfigured.
func TestHandleTracesDetailIncludesURL(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	adminBearer := env.loginAsAdmin(t)

	minimalTrace := func(traceID string) *domain.TraceInfo {
		return &domain.TraceInfo{TraceID: traceID, Status: "running"}
	}

	t.Run("url present", func(t *testing.T) {
		if err := env.server.traceMeta.UpsertProvision(ctx, "trace-detail-url", "", ""); err != nil {
			t.Fatalf("upsert provision: %v", err)
		}
		env.server.traces = &stubTraceRepo{trace: minimalTrace("trace-detail-url")}
		e := newAuthEngine(env.server)

		resp := ut.PerformRequest(e, "GET", "/api/v1/traces/trace-detail-url", nil, ut.Header{Key: "Authorization", Value: adminBearer})
		if resp.Result().StatusCode() != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.Result().StatusCode())
		}
		var body map[string]any
		if err := json.Unmarshal(resp.Result().Body(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		got, present := body["url"]
		if !present {
			t.Fatal("url field missing from detail response")
		}
		if got != "https://supv.example.com/pipelines/trace-detail-url" {
			t.Fatalf("url = %v, want https://supv.example.com/pipelines/trace-detail-url", got)
		}
	})

	t.Run("url omitted when base empty", func(t *testing.T) {
		env.server.cfg.PipelineURL = ""
		if err := env.server.traceMeta.UpsertProvision(ctx, "trace-detail-no-url", "", ""); err != nil {
			t.Fatalf("upsert provision: %v", err)
		}
		env.server.traces = &stubTraceRepo{trace: minimalTrace("trace-detail-no-url")}
		e := newAuthEngine(env.server)

		resp := ut.PerformRequest(e, "GET", "/api/v1/traces/trace-detail-no-url", nil, ut.Header{Key: "Authorization", Value: adminBearer})
		if resp.Result().StatusCode() != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.Result().StatusCode())
		}
		var body map[string]any
		if err := json.Unmarshal(resp.Result().Body(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, present := body["url"]; present {
			t.Fatalf("url field should be omitted, got %v", body["url"])
		}
	})
}
