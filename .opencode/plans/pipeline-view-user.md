# Plan — Always display the user that ran the pipeline on Pipeline view

## Goal

The Pipeline **detail** view (`/pipelines/:id`, `ui/src/pipeline/PipelineView.vue`)
must always display the user that ran the pipeline. Today the **list** view
(`Pipelines.vue`) shows `@username` (joined from the users table in
`fsm.listTraces`), but the detail view shows no user at all because
`domain.TraceInfo` has no user fields and `handleTracesDetail` does not enrich
the Tempo response with user info — even though `trace_meta.user_id` is already
populated at provision/ingest time.

This is a pure display/enrichment fix: no new persistence, no new config keys,
no auth changes, no schema migration.

## Design summary

1. Add `UserID` and `Username` fields to `domain.TraceInfo` (display-only fields
   populated by the handler, not by the Tempo reconstructor).
2. In `handleTracesDetail`, after the existing `traceMeta.Get` enrichment block,
   set `trace.UserID = meta.UserID` and best-effort look up the username via
   `s.users.Get(ctx, meta.UserID)`. Failures (user deleted, legacy/anonymous
   run with empty `UserID`) are logged at debug and leave `Username` empty.
3. Add `user_id?` and `username?` to the frontend `TraceDetail` type.
4. In `PipelineView.vue`, render an `@username` chip in the header (next to
   the status badge) and a new "User" row in the Details table. When the
   username is empty/missing, render a neutral `anonymous` chip and `-` in the
   table row (consistent with how the list view falls back to `-`).

## Affected files

### Existing files to modify

| File | Change |
|------|--------|
| `internal/domain/telemetry.go` | Add `UserID` + `Username` fields to `TraceInfo` |
| `internal/handler/traces.go` | Enrich `handleTracesDetail` with user fields from `traceMeta` + `users` |
| `ui/src/api/types.ts` | Add `user_id?` + `username?` to `TraceDetail` |
| `ui/src/pipeline/PipelineView.vue` | Render `@username` chip in header + "User" row in Details table |
| `internal/handler/middleware_test.go` | (No structural change — already registers `handleTracesDetail` on the test engine) |
| `docs/README.md` | Update "Pipeline UI" section to mention the user is shown in the detail view |

### New files

None.

## Detailed changes

### 1. `internal/domain/telemetry.go` — add user fields to `TraceInfo`

Add two fields to the `TraceInfo` struct. They are display-only fields populated
by the handler from `trace_meta` + the users table; the Tempo reconstructor
(`SpanTreeReconstructor.reconstruct`) does NOT set them.

```go
type TraceInfo struct {
	TraceID    string        `json:"trace_id"`
	RootSpan   *SpanNode     `json:"root_span"`
	Status     string        `json:"status"`
	StartTime  time.Time     `json:"start_time"`
	Duration   time.Duration `json:"duration_ns"`
	DurationMS int64         `json:"duration_ms"`
	Version    string        `json:"version"`
	CIProvider string        `json:"ci_provider,omitempty"`
	CIRepo     string        `json:"ci_repo,omitempty"`
	UserID     string        `json:"user_id,omitempty"`     // NEW: owner from trace_meta
	Username   string        `json:"username,omitempty"`   // NEW: joined from users table
}
```

Field semantics:
- `UserID`: the `trace_meta.user_id` value. Empty string for legacy/anonymous
  runs (synthetic legacy-token identity writes `""` via `attributionUserID`).
- `Username`: the `users.username` value joined from the users table. Empty
  when `UserID` is empty OR the user has been deleted.

Both fields use `omitempty` so existing API consumers that ignore unknown
fields are unaffected, and Tempo-only traces (no `trace_meta` row) simply omit
them.

### 2. `internal/handler/traces.go` — enrich `handleTracesDetail`

The handler already calls `s.traceMeta.Get(context.Background(), traceID)` and
merges `Version`/`CIProvider`/`CIRepo`/`Status`/`DurationMS` into the Tempo
response. Extend that same enrichment block to set `UserID` and look up the
`Username`.

Current code (lines 69-91 of `traces.go`):

```go
if meta, err := s.traceMeta.Get(context.Background(), traceID); err == nil {
	if trace.Version == "" {
		trace.Version = meta.Version
	}
	// ... existing CIProvider/CIRepo/Status/DurationMS merges ...
} else {
	s.logger.WithError(err).WithField("trace_id", traceID).Debug("trace_meta enrichment failed")
}
```

New code (add user enrichment inside the `err == nil` branch, after the existing
merges):

```go
if meta, err := s.traceMeta.Get(context.Background(), traceID); err == nil {
	if trace.Version == "" {
		trace.Version = meta.Version
	}
	if trace.CIProvider == "" {
		trace.CIProvider = meta.CIProvider
	}
	if trace.CIRepo == "" {
		trace.CIRepo = meta.CIRepo
		if trace.CIRepo == "" {
			trace.CIRepo = meta.ProjectName
		}
	}
	if (trace.Status == "running" || trace.Status == "") && meta.Status != "" {
		trace.Status = meta.Status
	}
	if trace.DurationMS == 0 && meta.DurationMS != 0 {
		trace.DurationMS = meta.DurationMS
		trace.Duration = time.Duration(meta.DurationMS) * time.Millisecond
	}
	// NEW: user attribution. UserID comes straight from trace_meta; Username
	// is joined best-effort from the users table. Legacy/anonymous runs have
	// an empty UserID; deleted users leave Username empty (logged at debug).
	trace.UserID = meta.UserID
	if meta.UserID != "" {
		if u, err := s.users.Get(context.Background(), meta.UserID); err == nil {
			trace.Username = u.Username
		} else {
			s.logger.WithError(err).WithField("user_id", meta.UserID).Debug("trace user lookup failed")
		}
	}
} else {
	s.logger.WithError(err).WithField("trace_id", traceID).Debug("trace_meta enrichment failed")
}
```

Notes:
- `s.users` is already injected on `Server` (`*service.UserService`) and used
  by `handleMe` etc. No new dependency injection needed.
- The lookup is best-effort: a deleted user (ON DELETE SET NULL on
  `trace_meta.user_id` per `fsm.go` line 490) yields `ErrNotFound`, which is
  logged at debug (not error) and leaves `Username` empty. The UI renders
  `anonymous` in that case.
- `context.Background()` is used (matching the existing `traceMeta.Get` call
  in the same handler) because the request context is not passed into
  `handleTracesDetail` (the signature is `func(s *Server) handleTracesDetail(_ context.Context, c *app.RequestContext)`).
  This is consistent with the existing code; no change to the signature.

### 3. `ui/src/api/types.ts` — add user fields to `TraceDetail`

```ts
export interface TraceDetail {
  trace_id: string
  root_span: SpanNode | null
  status: string
  start_time: string
  duration_ns: number
  duration_ms: number
  version: string
  ci_provider?: string
  ci_repo?: string
  user_id?: string       // NEW
  username?: string      // NEW
}
```

### 4. `ui/src/pipeline/PipelineView.vue` — render the user

#### 4a. Header — `@username` chip next to the status badge

In the `.header-meta` div (currently holds the status badge + duration), add a
user chip. The chip is shown only when `trace.username` is non-empty; when
empty, show a neutral `anonymous` chip so the user is *always* represented.

Current template (lines 9-12):

```html
<div class="header-meta">
  <span :class="['badge', `badge-${trace.status}`]">{{ trace.status }}</span>
  <span class="duration">{{ formatDuration(trace.duration_ms) }}</span>
</div>
```

New template:

```html
<div class="header-meta">
  <span :class="['badge', `badge-${trace.status}`]">{{ trace.status }}</span>
  <span class="user-chip" :title="trace.user_id ? `user_id: ${trace.user_id}` : ''">
    {{ trace.username ? `@${trace.username}` : 'anonymous' }}
  </span>
  <span class="duration">{{ formatDuration(trace.duration_ms) }}</span>
</div>
```

#### 4b. Details table — new "User" row

In the Details `<table>` (lines 134-142), add a row at the top (before Status):

```html
<tr>
  <td>User</td>
  <td>
    <span v-if="trace.username">{{ trace.username }}</span>
    <span v-else class="empty-value">anonymous</span>
  </td>
</tr>
```

#### 4c. Initial `trace` ref — include the new fields

The initial `ref<TraceDetail>` (lines 156-164) does not need to list the new
optional fields (they default to `undefined`), but for clarity and to avoid
`undefined`-chaining in the template, no change is required — Vue handles
missing optional fields fine. The `user_id`/`username` fields arrive with the
first `fetchTrace` response.

#### 4d. CSS — add `.user-chip` and `.empty-value` styles

Add to the `<style scoped>` block:

```css
.user-chip {
  font-size: 13px;
  font-weight: 600;
  color: #58a6ff;
  background: #1f2a3a;
  border-radius: 10px;
  padding: 2px 10px;
}

.empty-value {
  color: #8b949e;
  font-style: italic;
}
```

The `.user-chip` reuses the same blue-on-dark palette as the existing
`.step-logs-badge` / `.service-running` chips for visual consistency.

### 5. `docs/README.md` — Pipeline UI section

In the "Pipeline UI" section (around line 911), update the "Trace viewer"
bullet to mention the user is shown. Add after the existing "Trace viewer"
description:

> The trace viewer header shows an `@username` chip (or `anonymous` for
> legacy/anonymous runs) next to the status badge, and the Details table
> includes a "User" row — so the pipeline owner is always visible on the
> detail view, matching the list view's `@username · org/repo` identity.

## Edge cases

| Scenario | `trace_meta.user_id` | `trace.UserID` | `trace.Username` | UI display |
|----------|----------------------|----------------|-------------------|------------|
| Normal user run (JWT/API token) | user UUID | user UUID | username | `@username` chip + username row |
| Legacy flat-file token (synthetic identity) | `""` (attributionUserID returns "") | `""` | `""` | `anonymous` chip + `anonymous` row |
| Anonymous run (no auth, dev mode) | `""` | `""` | `""` | `anonymous` chip + `anonymous` row |
| User deleted after run (ON DELETE SET NULL) | `""` (FK nulled) | `""` | `""` | `anonymous` chip + `anonymous` row |
| User deleted but `trace_meta.user_id` not yet nulled (race) | stale UUID | stale UUID | `""` (lookup fails) | `anonymous` chip + `anonymous` row |
| Tempo-only trace (no `trace_meta` row, admin viewing unknown trace) | n/a | `""` | `""` | `anonymous` chip + `anonymous` row |
| Empty username string (should not happen — username regex requires `^[a-zA-Z0-9][a-zA-Z0-9._-]{1,63}$`) | — | — | `""` | `anonymous` chip + `anonymous` row |

Backwards compatibility:
- Existing persisted `trace_meta` rows already have `user_id` (added in
  ADR-010). No schema migration needed.
- The `TraceInfo` JSON response adds two `omitempty` fields; existing API
  consumers that ignore unknown fields are unaffected.
- The frontend gracefully handles missing `user_id`/`username` (renders
  `anonymous`).

## Error handling

- `traceMeta.Get` failure (trace not in `trace_meta`): already logged at debug
  (`trace_meta enrichment failed`). The user fields stay empty (`""`); UI shows
  `anonymous`. No behavior change from today.
- `users.Get` failure (user deleted, DB error): logged at debug
  (`trace user lookup failed`) with `user_id` field. `Username` stays empty;
  UI shows `anonymous`. This is best-effort enrichment, never a 500.
- No new HTTP error responses: the detail endpoint still returns 404 for
  unknown traces (Tempo) and 401/403/404 from `authorizeTraceRequest`. The
  user enrichment is inside the success path only.

All errors are wrapped with `%w` where a wrapped error is constructed (none
new here — the existing `fmt.Errorf` calls in `trace_store.go` already wrap).
Logrus logging follows the `WithError(err).WithField(...).Debug(...)` pattern
used throughout the handler package.

## Validation rules

- No new input validation: the user fields are derived from persisted
  `trace_meta` (already validated at provision/ingest time) and the users
  table. No client-supplied data enters this path.
- The `trace.UserID` value is the raw `trace_meta.user_id` (a UUID or `""`).
  It is rendered in the `title` attribute of the chip for debugging but is not
  user-facing text.
- The `trace.Username` value comes from `users.username`, which is validated at
  user-create time by `validateUsername` (`^[a-zA-Z0-9][a-zA-Z0-9._-]{1,63}$`).
  No re-validation needed in the view layer.

## Testing plan

Target: 100% coverage for the new code paths in `internal/handler/traces.go`
and `internal/domain/telemetry.go`. Standard `testing` package only,
table-driven, logrus logger with `io.Discard` output (via
`observ.NewTestLogger()`).

### Unit tests — `internal/handler/` (new test in `middleware_test.go` or a new `traces_test.go`)

The existing `newAuthEngine` helper already registers
`handleTracesDetail` at `/api/v1/traces/:traceID`. The existing `newTestEnv`
wires `users` (UserService) and `traceMeta` (TraceMetaRepo) into the server.

Add a table-driven test `TestHandleTracesDetailEnrichesUser` with cases:

| Case | Setup | Assert |
|------|-------|--------|
| `user_enriched` | Seed `trace_meta` with `UserID=admin.ID` via `UpsertProvision`; stub `traces` (SpanTreeReconstructor) to return a minimal `TraceInfo` | response JSON has `user_id == admin.ID` and `username == "admin"` |
| `legacy_anonymous` | Seed `trace_meta` with `UserID=""` | response has `user_id == ""` (omitted) and `username == ""` (omitted) |
| `user_deleted` | Seed `trace_meta` with `UserID="deleted-uuid"` (no users-table row) | response has `user_id == "deleted-uuid"` and `username == ""` (omitted); debug log emitted |
| `no_trace_meta` | No `trace_meta` row; admin viewer (Tempo returns a trace) | response has no `user_id`/`username` fields (omitted); debug log `trace_meta enrichment failed` |
| `tempo_only_admin` | No `trace_meta` row; Tempo returns 404 | 404 response (existing behavior, no enrichment path) |

The test must stub the `Traces` dependency (`domain.TraceRepository`) because
`newTestEnv` wires a real `SpanTreeReconstructor("")` that always fails. Use a
small stub implementing `GetTrace(traceID string) (*domain.TraceInfo, error)`
returning a fixed `TraceInfo` (or `ErrNotFound` for the 404 case). This stub
goes in `internal/handler/test_helper_test.go` next to the existing
`stubCacheStatsProvider` / `stubStatusProvider` stubs.

Stub signature:

```go
type stubTraceRepo struct {
	trace *domain.TraceInfo
	err   error
}

func (s *stubTraceRepo) GetTrace(traceID string) (*domain.TraceInfo, error) {
	return s.trace, s.err
}
```

To inject it, the test must construct a `Server` with the stub instead of
`newTestEnv`'s default `SpanTreeReconstructor`. Either:
- Add a `traces` field to `testEnv` and let the test swap it before building
  the engine, OR
- Build a minimal `Server` directly in the test using `NewServer` with a
  `Deps` that sets `Traces: &stubTraceRepo{...}`.

Prefer the second approach (minimal, self-contained) mirroring how
`TestWriteServiceErrorMapping` builds a bare `route.Engine` per case.

### Unit tests — `internal/domain/` (no new test file needed)

The `TraceInfo` struct change is additive (two `omitempty` fields). No new
logic to test in `domain`. Existing `version_test.go` is the only test file in
`domain/`; no change needed.

### Integration tests — `tests/integration/`

The existing `rbac_test.go` has a `getTrace` helper that hits
`GET /api/v1/traces/:traceID` and returns the status code. Extend it (or add a
new `getTraceBody` helper) to decode the JSON body and assert the `user_id`
and `username` fields.

Add `TestTraceDetailIncludesUser` in `rbac_test.go` (or a new
`traces_integration_test.go`):

1. Create a user `alice` with an API token.
2. `POST /v1/engines` as alice with a `trace_id` (this calls
   `attribution.Provision` which writes `trace_meta.user_id = alice.ID`).
3. `GET /api/v1/traces/<trace_id>` as alice.
4. Assert response JSON has `user_id == alice.ID` and `username == "alice"`.

This proves the feature works against the real provision → detail flow (the
Dagger client API contract for `POST /v1/engines` is exercised end-to-end).

Note: the integration test does NOT require a real Tempo — the
`SpanTreeReconstructor("")` will return 404 for the trace, but
`handleTracesDetail` returns 404 *before* the enrichment block runs (the
`writeError(c, StatusNotFound, "trace not found")` on line 60). So the
integration test must either:
- Assert 404 + skip the body assertion (proves auth/visibility only), OR
- Use a stubbed trace repo (unit test already covers this).

Given the integration test's purpose is to prove the real provision → detail
contract, the 404 path is acceptable: it proves `authorizeTrace` passes for
the owner. The user-enrichment assertion belongs in the unit test (which
stubs Tempo). Document this split in the test comments.

### Frontend tests

The project has no frontend test runner configured (`ui/package.json` has no
test script — only `build`, `dev`, `preview`). No frontend unit tests are
required. Manual verification on the live cluster covers the UI rendering
(see "Local cluster validation" below).

## Documentation changes

Per AGENTS.md §"Documentation maintenance":

- **`config/config.app.yaml.sample`**: No change. No new config keys are
  introduced. (The sample file must reflect all config keys; since none are
  added, it stays in sync.)
- **`docs/README.md`**: Update the "Pipeline UI" section as described in
  change #5 above.
- **`docs/design/`**: No new ADR. This is a minor display/enrichment fix, not
  an architectural decision. ADR-010 (SQLite multi-user RBAC) already
  documents the `trace_meta.user_id` attribution design; this plan just
  surfaces that existing data in the detail view. No ADR update needed.

## Config keys

None. No new viper keys, no `SetDefault` calls, no `config/loader.go` change.

## Local cluster validation (per AGENTS.local.md §6)

After implementing, the change MUST be redeployed and validated on the "home"
cluster:

1. **Build** (root `Dockerfile` builds the Vue UI, embeds it via
   `//go:embed all:ui-dist`, and builds `supervisor` + `dagger-cache-ci`):
   ```bash
   docker build -t docker.io/disaster/dagger-kubernetes:dev .
   ```
2. **Push**:
   ```bash
   docker push docker.io/disaster/dagger-kubernetes:dev
   ```
3. **Capture Helm values** (do NOT skip — `helm upgrade` without `-f` resets
   to chart defaults and wipes live overrides):
   ```bash
   helm --kubeconfig /home/user/.kube/home get values dagger-cache-test \
     -n dagger-cache-test -o yaml > /tmp/dagger-cache-test.values.yaml
   ```
4. **Upgrade**:
   ```bash
   helm --kubeconfig /home/user/.kube/home upgrade --install dagger-cache-test \
     ./deploy/helm/dagger-kubernetes \
     --namespace dagger-cache-test \
     -f /tmp/dagger-cache-test.values.yaml \
     --set supervisor.image.tag=dev \
     --set supervisor.image.pullPolicy=Always \
     --set supervisor.image.repository=docker.io/disaster/dagger-kubernetes
   ```
5. **Force rollout** (the `dev` tag is mutable; `imagePullPolicy: Always`
   requires a rollout restart to re-pull):
   ```bash
   kubectl --kubeconfig /home/user/.kube/home -n dagger-cache-test \
     rollout restart deploy/dagger-cache-test-dagger-kubernetes
   kubectl --kubeconfig /home/user/.kube/home -n dagger-cache-test \
     rollout status deploy/dagger-cache-test-dagger-kubernetes --timeout=300s
   ```
6. **Agent verification** (§5.1):
   - Pods Ready: `kubectl ... get pods -l app.kubernetes.io/name=dagger-kubernetes`
   - Probes: `curl -sk https://localhost:8080/healthz` → 200 `ok`; `/readyz` → 200
   - Authed API: `curl -sk https://localhost:8080/api/v1/traces/<id> -H "Authorization: Bearer $TOKEN"` → 200 with `user_id` + `username` fields
   - Logs: `kubectl ... logs deploy/dagger-cache-test-dagger-kubernetes --tail=100` → no fatal errors
7. **Human verification** (§5.2): A human must confirm on the live UI at
   `https://dagger.home.webcenter.fr`:
   - Navigate to Pipelines → click a pipeline → the detail view header shows
     the `@username` chip (or `anonymous` for legacy runs).
   - The Details table shows a "User" row with the username (or `anonymous`).
   - Run a fresh pipeline as a real user (via `dagger-cache-ci` with a real
     API token) and confirm the chip shows the correct `@username`.

Do NOT mark the work complete until both agent checks pass AND a human has
confirmed on the real cluster.

## Out of scope

- Adding the user to the SSE live event payload (`trace_update` /
  `logs_update`). The SSE events are lightweight re-fetch signals; the client
  re-fetches the full `TraceDetail` (which now includes the user) on each
  event. No need to duplicate the user in every SSE event.
- Backfilling `user_id` on legacy `trace_meta` rows. Legacy runs have
  `user_id == ""` by design (synthetic identity); the UI shows `anonymous`.
- Adding a filter-by-user control on the Pipelines list view. That is a
  separate feature.
- Frontend unit tests (no test runner configured in `ui/`).
