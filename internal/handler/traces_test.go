package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// TestHandleTracesDetailEnrichesUser covers the user-attribution enrichment in
// handleTracesDetail: the Tempo span tree is merged with trace_meta.user_id
// and a best-effort users-table join for the username. The traces dependency
// (Tempo) is stubbed because newTestEnv wires a SpanTreeReconstructor("") that
// cannot reach a real Tempo.
func TestHandleTracesDetailEnrichesUser(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	admin, err := env.users.GetByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}
	bearer := env.loginAsAdmin(t)

	minimalTrace := func(traceID string) *domain.TraceInfo {
		return &domain.TraceInfo{TraceID: traceID, Status: "running"}
	}

	cases := []struct {
		name       string
		traceID    string
		seed       bool // seed a trace_meta row before the request
		seedUser   string
		trace      *domain.TraceInfo
		traceErr   error
		wantCode   int
		wantUserID string // "" asserts the JSON key is omitted
		wantUser   string // "" asserts the JSON key is omitted
	}{
		{
			name:       "user_enriched",
			traceID:    "trace-user",
			seed:       true,
			seedUser:   admin.ID,
			trace:      minimalTrace("trace-user"),
			wantCode:   http.StatusOK,
			wantUserID: admin.ID,
			wantUser:   "admin",
		},
		{
			name:     "legacy_anonymous",
			traceID:  "trace-legacy",
			seed:     true,
			seedUser: "",
			trace:    minimalTrace("trace-legacy"),
			wantCode: http.StatusOK,
		},
		{
			name:       "user_deleted",
			traceID:    "trace-deleted",
			seed:       true,
			seedUser:   "deleted-uuid",
			trace:      minimalTrace("trace-deleted"),
			wantCode:   http.StatusOK,
			wantUserID: "deleted-uuid",
			// Username stays empty: the users-table lookup fails and is logged
			// at debug, leaving the JSON key omitted.
		},
		{
			name:     "no_trace_meta",
			traceID:  "trace-tempo-only",
			seed:     false,
			trace:    minimalTrace("trace-tempo-only"),
			wantCode: http.StatusOK,
			// No trace_meta row: enrichment is skipped (debug log emitted) and
			// both user fields remain omitted.
		},
		{
			name:     "tempo_only_admin",
			traceID:  "trace-tempo-404",
			seed:     false,
			traceErr: domain.ErrNotFound,
			wantCode: http.StatusNotFound,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.seed {
				if err := env.server.traceMeta.UpsertProvision(ctx, tc.traceID, tc.seedUser, ""); err != nil {
					t.Fatalf("upsert provision: %v", err)
				}
			}
			env.server.traces = &stubTraceRepo{trace: tc.trace, err: tc.traceErr}
			e := newAuthEngine(env.server)

			resp := ut.PerformRequest(e, "GET", fmt.Sprintf("/api/v1/traces/%s", tc.traceID), nil, ut.Header{Key: "Authorization", Value: bearer})
			if resp.Result().StatusCode() != tc.wantCode {
				t.Fatalf("status: %d, want %d", resp.Result().StatusCode(), tc.wantCode)
			}
			if tc.wantCode != http.StatusOK {
				return
			}

			var body map[string]any
			if err := json.Unmarshal(resp.Result().Body(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			assertOmitOrEqual(t, body, "user_id", tc.wantUserID)
			assertOmitOrEqual(t, body, "username", tc.wantUser)
		})
	}
}

// assertOmitOrEqual checks that body[key] is omitted when want is empty and
// otherwise equals want.
func assertOmitOrEqual(t *testing.T, body map[string]any, key, want string) {
	t.Helper()
	got, present := body[key]
	if want == "" {
		if present {
			t.Fatalf("%s should be omitted, got %v", key, got)
		}
		return
	}
	if got != want {
		t.Fatalf("%s = %v, want %v", key, got, want)
	}
}
