# Plan — Issue #21: Pipeline-by-level tree, zoom/drill-down, and log search

## 1. Problem statement

The pipeline view (`/pipelines/<id>`, `ui/src/pipeline/PipelineView.vue`) renders
Dagger spans as a flat, two-level list: "steps" (top-level spans) with sub-spans
flattened inline (indented by depth). There is no way to navigate the Dagger
code/function call hierarchy as a *tree*, no way to "zoom" into a level and
descend to leaves, and no log search.

Goal: render the span tree that mirrors the Dagger function nesting; at the top
level show the main levels with **all** logs of their sub-levels aggregated;
allow **zoom / drill-down** into a level (breadcrumb navigation) down to leaf
steps; and allow **keyword + regex log search** scoped to the currently
displayed level (node + descendants). Search must be server-side and the log
view must page beyond the current 1000-line cap.

## 2. Chosen design & rationale

**Backend already serves a full span tree** (`GET /api/v1/traces/:id` →
`domain.TraceInfo.RootSpan` with `SpanNode.Children`/`ParentSpanID`, built by
`repository.SpanTreeReconstructor.reconstruct`). **No tree-building backend
change is needed.** The feature is:

1. **UI**: replace the flat "Steps" card with a drill-down tree + breadcrumb +
   an aggregated log panel + search box (`StepTree.vue`, `LogPanel.vue`).
2. **Backend**: add one server-side search endpoint
   `GET /api/v1/traces/:id/search` that (a) resolves the focused node's
   descendant span IDs from the reconstructed tree (mirroring the UI's
   internal/passthrough log-ownership rules), (b) applies keyword/regex matching
   in Go (`regexp` = RE2, linear-time, no catastrophic backtracking), and (c)
   pages results so the UI can page beyond 1000 lines.
3. Pagination is a forward timestamp **cursor** (`next` unix-nanos) returned by
   the endpoint and re-sent by the client as `cursor`.

**Why server-side search** (user decision): the search scope is a *subtree*
(node + all descendants), which Loki's `span_id` label cannot express without
walking the span tree — so the supervisor (which owns the reconstructed tree) is
the right place. Text matching stays in Go `regexp`/`strings` (RE2-safe) rather
than LogQL line filters, avoiding LogQL escaping pitfalls and reusing the
existing `LogsClient` Loki fetch.

**Why cursor (not offset) pagination**: Loki returns forward-sorted entries; a
timestamp cursor is the natural, stateless pager. See §4 for the exact contract
and its one documented boundary limitation.

**Why a separate `LogPanel`/`StepTree` split**: keeps focus/search state in one
place (parent) and log rendering presentational, mirroring the existing
`MetricChart.vue` component style.

### Alternatives considered

- **Pure client-side search + in-memory paging**: rejected by user — capped at
  the 1000-line whole-trace fetch, cannot search beyond it.
- **Push span/text filters into LogQL** (`span_id=~"hex1|hex2|..."` +
  `|=`/`|~`): rejected — label-regex escaping, span-ID base64→hex round-trip,
  and URL length limits for large subtrees; supervisor-side filtering is simpler
  and reuses existing code.
- **Keep the old Steps card alongside the new tree**: rejected by user — one
  cohesive tree view avoids duplicated log rendering.
- **Offset-based pagination**: rejected — Loki has no stable global offset; a
  timestamp cursor is the only reliable forward pager.

## 3. Files to create / modify

### Backend (Go)

| Action | Path |
|---|---|
| Modify | `internal/domain/telemetry.go` — add `LogSearchMode`, `LogSearchRequest`, `LogSearchPage`, `ErrInvalidRegex`; extend `LogRepository` with `SearchTraceLogs`. |
| Modify | `internal/repository/log_store.go` — implement `SearchTraceLogs` + `matchLogLine`; add search batch/scan constants. |
| Create | `internal/repository/log_search_test.go` — table-driven tests for matching, span filtering, pagination, invalid regex, unicode, limits. |
| Create | `internal/service/telemetry_search.go` — `LevelSpanIDs` (scope resolution) + `collect` + `isInternalSpanNode`. |
| Create | `internal/service/telemetry_search_test.go` — tests for `LevelSpanIDs`. |
| Create | `internal/handler/traces_search.go` — `handleTracesSearch` + query parsing/clamps + `searchWindow`. |
| Create | `internal/handler/traces_search_test.go` — handler tests via `newTestEnv` + stub log repo. |
| Modify | `internal/handler/server.go` — register `h.GET("/api/v1/traces/:traceID/search", s.handleTracesSearch)`. |
| Modify | `tests/integration/ci_steps_test.go` — add `SearchTraceLogs` to `stepsLogRepo` (interface widened). |
| Create | `tests/integration/pipeline_search_test.go` — end-to-end search against fake Tempo + Loki. |

### UI (Vue 3 + TypeScript)

| Action | Path |
|---|---|
| Modify | `ui/src/api/types.ts` — add `LogSearchMode`, `LogSearchRequest`, `LogSearchPage`. |
| Modify | `ui/src/api/client.ts` — add `fetchTraceSearch`. |
| Create | `ui/src/pipeline/spanTree.ts` — shared pure span-tree helpers (visibility/ownership rules). |
| Create | `ui/src/pipeline/StepTree.vue` — drill-down tree + breadcrumb + focus state. |
| Create | `ui/src/pipeline/LogPanel.vue` — aggregated log panel + search + highlight + load-more. |
| Modify | `ui/src/pipeline/PipelineView.vue` — replace Steps card with `<StepTree>`, remove dead step logic, import shared helpers. |

### Docs

| Action | Path |
|---|---|
| Create | `docs/design/ADR-038-pipeline-tree-zoom-search.md` |
| Modify | `docs/design/index.md` — add ADR-038 row. |
| Modify | `docs/README.md` — Pipeline UI section + Pipeline API table. |

No `config/config.app.yaml.sample` change (no new config keys) and no `DAGGER.md`
change (no `dagger/`, CI, or `.github` changes).

## 4. Data structures & signatures

### `internal/domain/telemetry.go`

```go
// ErrInvalidRegex is wrapped by LogRepository.SearchTraceLogs when the query
// mode is "regex" and the pattern does not compile.
var ErrInvalidRegex = errors.New("invalid regex")

// LogSearchMode enumerates supported log text match modes.
type LogSearchMode string

const (
    LogSearchContains LogSearchMode = "contains"
    LogSearchRegex    LogSearchMode = "regex"
)

// LogSearchRequest is a subtree-scoped, text-filtered, paginated log query.
type LogSearchRequest struct {
    SpanIDs []string      // descendant span IDs (base64) to include; nil/empty = all spans
    Query   string        // text to match; empty = no text filter
    Mode    LogSearchMode // LogSearchContains | LogSearchRegex
    Start   time.Time     // inclusive window start (handler defaults to last 24h)
    End     time.Time     // inclusive window end (handler defaults to now)
    Limit   int           // max matching entries to return this page
    Cursor  int64         // unix nanos; return entries strictly after this timestamp
}

// LogSearchPage is one page of subtree-scoped search results.
type LogSearchPage struct {
    Entries []LogEntry `json:"entries"`
    Next    int64      `json:"next,omitempty"` // cursor for the next page; 0 = no more
}

type LogRepository interface {
    QueryTraceLogs(traceID string, start, end time.Time, limit int) ([]LogEntry, error)
    // SearchTraceLogs returns one page of trace logs filtered by the request
    // (span set + text match), ascending by timestamp, after Cursor.
    SearchTraceLogs(ctx context.Context, traceID string, req LogSearchRequest) (LogSearchPage, error)
    DeleteTraceLogs(ctx context.Context, traceID string) error
}
```

### `internal/repository/log_store.go`

```go
const (
    logSearchBatch   = 1000 // raw Loki entries per internal query_range
    logSearchMaxScan = 5000 // hard cap on raw entries scanned per SearchTraceLogs call (CWE-400)
)

func (c *LogsClient) SearchTraceLogs(ctx context.Context, traceID string, req domain.LogSearchRequest) (domain.LogSearchPage, error)

// matchLogLine reports whether line matches query under mode. re is precompiled
// for regex mode; nil for contains mode.
func matchLogLine(line, query string, mode domain.LogSearchMode, re *regexp.Regexp) bool
```

`SearchTraceLogs` algorithm (contract):

1. Validate `traceID` (`hexTraceID`) and non-empty `lokiURL` (same as `QueryTraceLogs`).
2. If `mode == regex && query != ""`, `regexp.Compile`; on error return
   `fmt.Errorf("%w: %v", domain.ErrInvalidRegex, err)`.
3. `cursor := req.Cursor` (if zero → `req.Start`). `lastRawTs` tracks the last
   raw entry's timestamp. `matches`, `scanned` accumulate.
4. Loop (bounded):
   - Query Loki `{trace_id="<id>"}` with `start=cursor`, `end=req.End`,
     `limit=logSearchBatch`, `direction=forward`.
   - Decode + `normalizeSpanID`; **sort the raw entries ascending (stable)** by
     timestamp (the existing per-stream append is not globally sorted).
   - For each raw entry: if `len(req.SpanIDs) > 0 && !inSpanSet(entry.SpanID)`
     skip; if `!matchLogLine(...)` skip; else append to `matches`.
   - `lastRawTs` = timestamp of the last raw entry examined.
   - Break when `len(matches) >= req.Limit`, or the raw batch had
     `len(raw) < logSearchBatch` (Loki exhausted), or `scanned >= logSearchMaxScan`.
   - Advance `cursor = lastRawTs + 1ns`; `scanned += len(raw)`.
5. Return `LogSearchPage{Entries: matches[:min(len, req.Limit)], Next: nextCursor}`
   where `nextCursor = 0` when Loki was exhausted (raw batch short), else
   `lastRawTs + 1`.

Boundary note (documented): if more than `req.Limit` entries share the exact
final nanosecond returned in a page, the overflow beyond the page is picked up
by the next `cursor` only at `lastRawTs+1ns`, so same-nanosecond surplus is
dropped. In practice Dagger log lines carry distinct Loki timestamps; this is
the accepted, stateless pager trade-off.

### `internal/service/telemetry_search.go`

```go
// LevelSpanIDs returns the base64 span IDs whose logs belong to the level rooted
// at focus (focus itself included). Internal spans (dagger.io/ui.internal or
// name-internal per ci_steps.go) are excluded; their non-internal descendants
// are kept. focus == "" or focus == root.SpanID selects the whole trace
// (returns nil, true). found=false when focus is not present.
func LevelSpanIDs(root *domain.SpanNode, focus string) (ids []string, found bool)

// collect walks n's subtree appending non-internal span IDs (depth-capped).
func collect(n *domain.SpanNode, depth int, ids *[]string)

// isInternalSpanNode reports whether n is internal noise (attribute or name).
func isInternalSpanNode(n *domain.SpanNode) bool
```

Reuses package-private `isInternalSpanName` from `ci_steps.go`. `collect` uses a
depth cap (`ciMaxAbsoluteDepth` = 1024) to bound recursion (CWE-674).

### `internal/handler/traces_search.go`

```go
const (
    defaultTraceSearchLimit = 500
    maxTraceSearchLimit     = 2000
)

func (s *Server) handleTracesSearch(_ context.Context, c *app.RequestContext)
func clampSearchLimit(raw string) int       // default 500, cap 2000
func parseSearchCursor(raw string) (int64, error) // "" -> 0; non-integer/negative -> error
func searchWindow(c *app.RequestContext) (time.Time, time.Time) // default last 24h
```

Handler flow (auth-gated by `authorizeTraceRequest`):

1. `mode := LogSearchMode(c.Query("mode"))`; empty → `contains`; anything else → 400.
2. `cursor, err := parseSearchCursor(c.Query("cursor"))`; error → 400.
3. `focus := strings.TrimSpace(c.Query("span_id"))`.
4. If `focus != ""`: `trace, err := s.traces.GetTrace(traceID)` (err → 404
   "trace not found"); `spanIDs, found := service.LevelSpanIDs(trace.RootSpan, focus)`;
   `!found` → 404 "span not found". If `focus == ""` → `spanIDs = nil` (all).
5. `page, err := s.logs.SearchTraceLogs(context.Background(), traceID, ...)`.
   - `errors.Is(err, domain.ErrInvalidRegex)` → 400 "invalid regex".
   - other error → 502 "log search failed".
6. `writeJSON(c, page)`.

**Response shape** (HTTP 200):
```json
{ "entries": [ { "timestamp": "...", "line": "...", "span_id": "..." } ], "next": 1699999999999000000 }
```

**Request**: `GET /api/v1/traces/:traceID/search?span_id=<base64>&q=<text>&mode=contains|regex&limit=<n>&cursor=<unixns>`.
All query params optional.

### `tests/integration/ci_steps_test.go`

Add to `stepsLogRepo` (to satisfy the widened interface):
```go
func (s *stepsLogRepo) SearchTraceLogs(context.Context, string, domain.LogSearchRequest) (domain.LogSearchPage, error) {
    return domain.LogSearchPage{}, nil
}
```

## 5. UI behavior

### `ui/src/api/types.ts` / `client.ts`

```ts
export type LogSearchMode = 'contains' | 'regex'
export interface LogSearchRequest {
  span_id?: string; q?: string; mode?: LogSearchMode; limit?: number; cursor?: number
}
export interface LogSearchPage { entries: TraceLogEntry[]; next?: number }

export async function fetchTraceSearch(id: string, params: LogSearchRequest): Promise<LogSearchPage>
```

### `ui/src/pipeline/spanTree.ts` (shared, pure)

Move/export from `PipelineView.vue` so both `PipelineView` (services/owners) and
`StepTree` share one truth: `attrBool`, `isInternalSpanName`,
`INTERNAL_SPAN_PREFIXES`, `INTERNAL_SPAN_EXACT`, `isInternalSpan`, `isHiddenSpan`,
`isTransparentSpan`, `visibleChildren(nodes)` (port of `topLevelSpans`),
`flattenVisible(node, depth)` (port, returns `{spans, hidden}`), `DisplaySpan`.

### `ui/src/pipeline/StepTree.vue`

Props: `traceId: string`, `trace: TraceDetail`.

State:
- `focusPath: SpanNode[]` — breadcrumb root→focus, init `[root]`.
- `expanded: Set<string>` — in-place expanded span IDs.
- `query: string`, `mode: LogSearchMode`, `entries: TraceLogEntry[]`,
  `nextCursor: number | null`, `loading: boolean`, `error: string | null`.
- `PAGE_SIZE = 500`.

Behavior:
- **Breadcrumb bar** above the list: `root ▸ … ▸ focus`; each crumb clickable →
  truncate `focusPath` to that node (zoom out), reset `expanded`, reload page.
- **List** = `visibleChildren(focus)` (passthrough-promoted, internal-hidden).
  Each row: status dot, name, duration, log-presence dot. Row has two affordances:
  - chevron → toggle in-place expand, rendering `flattenVisible(child)` indented;
  - name click → **zoom in**: push child onto `focusPath`, reset `expanded`, reload.
- **Aggregated log panel** (`LogPanel`) bound to the current focus: fetches
  `/search` with `span_id = focus.span_id` (omitted for the root = whole trace).
  Default `q=''` (show all subtree logs). On focus/query/mode change, reload
  (reset cursor). On `load-more`, append the next page using `nextCursor`.
- Live: on the existing SSE `logs_update`/5s poll, refresh the current page
  (debounced 300ms) — same pattern as the current viewer.

### `ui/src/pipeline/LogPanel.vue`

Props: `entries`, `query`, `mode`, `loading`, `error`, `hasMore`, `totalShown`.
Emits: `update:query`, `update:mode`, `load-more`.
Renders:
- Toolbar: text input (debounced 300ms), contains/regex toggle, match count,
  "Load more" button when `hasMore`.
- Log lines: reuse `logText` (extract body, strip ANSI/base64) — move `logText`
  into `spanTree.ts` or a small `logText.ts` so `LogPanel` and `PipelineView`
  share it. Highlight matches with `highlightSegments(text, query, mode)`:
  split into `{text, match}` segments and render `<mark>{{seg}}</mark>` —
  **never `v-html`** (XSS). `v-follow-logs` on the list.
- Invalid-regex handling (client-side): validate `new RegExp(query)` in
  try/catch before firing the request; show inline "invalid regex" and skip.

### `ui/src/pipeline/PipelineView.vue`

- Replace the "Steps" card body with `<StepTree :trace-id="traceId" :trace="trace" />`.
- Delete now-unused step machinery: `Step`, `DisplaySpan` (moved to spanTree),
  `steps` ref, `computeSteps`, `flattenVisibleChildren`, `topLevelSpans`,
  `recomputeSteps`, `stepLogCount`, `logsForSpan`. Import `attrBool`,
  `isInternalSpan*`, `isTransparentSpan`, `isHiddenSpan` from `spanTree.ts`
  (still used by Services/Unmatched ownership).
- Keep: header, metrics, services, details, unmatched logs, `logsForSubtree`
  (still used by `computeServices`), duration helpers.

## 6. Edge cases & error handling

- **Malformed spans / nil root / empty span IDs**: `LevelSpanIDs` guards nil
  nodes, skips empty span IDs while still recursing; `GetTrace` error → 404.
- **Missing parent / orphan**: reconstructor already re-roots orphans under the
  picked root; search scoping follows the reconstructed tree.
- **Cycles / missing root**: reconstructor's `pickRoot(order)` fallback; search
  unaffected (operates on the returned tree).
- **Deep nesting**: `collect` depth-capped at `ciMaxAbsoluteDepth` (1024) to
  bound recursion (CWE-674).
- **Huge log volume**: `logSearchMaxScan` (5000 raw entries/call) + `next`
  cursor; page size capped at `maxTraceSearchLimit` (2000). UI pages via "Load more".
- **Concurrent/live updates**: search is a snapshot; SSE/poll refetch the panel
  (debounced), preserving query + focus.
- **Regex catastrophic backtracking**: Go `regexp` is RE2 (linear) — server is
  authoritative and safe. Client only re-runs the regex to highlight the
  already-limited (≤ page size) server results; a pathological JS regex is a
  low-risk, best-effort display path. Documented.
- **Invalid regex**: server returns 400 via `domain.ErrInvalidRegex`; client
  pre-validates and shows inline error without calling the API.
- **XSS**: log text rendered via `{{ }}` interpolation only; highlight uses
  `<mark>` + text segments, never `v-html`.
- **Unicode**: `strings.Contains` (UTF-8 byte-substring) and `regexp`
  (rune-aware) behave correctly for literal text; no special handling.
- **Empty states**: no tree → "No steps for this pipeline"; empty panel → "No
  logs for this level"; no matches → "No matching logs".
- **Loading/error**: panel spinner; backend error surfaces inline with retry.

## 7. Validation strategy

### Go unit tests (table-driven, stdlib only)

- `internal/repository/log_search_test.go`: `matchLogLine` (contains, regex,
  invalid-regex compile, unicode, empty query); `SearchTraceLogs` against
  `httptest` Loki — span-set filtering, text filtering, pagination cursor
  (first page + `next` + follow-up), limit cap, empty result → `next=0`, invalid
  trace ID, unconfigured Loki.
- `internal/service/telemetry_search_test.go`: `LevelSpanIDs` — whole-trace
  (empty/root focus), found node, missing focus (`found=false`), internal span
  excluded but its non-internal child kept, passthrough span kept, deep tree.
- `internal/handler/traces_search_test.go` (uses `newTestEnv` + a stub
  `domain.LogRepository`): 200 + page passthrough, `mode` validation (400),
  invalid cursor (400), unknown `span_id` (404), invalid regex (400), auth gate
  (401), limit clamp.

### Integration test

`tests/integration/pipeline_search_test.go` (model `pipeline_metrics_test.go`):
`freeListener(t)` + `listenerAddr`, real `Server.Start` with a fake Tempo
(`httptest` returning a nested root→child→grandchild tree) and fake Loki
(`httptest` returning span-correlated lines). Assert: whole-trace keyword search,
scoped `span_id` search returns only that subtree, pagination (`limit=1` → `next`
→ second page), invalid regex 400, unknown span 404. Shut down with a timed
context in `t.Cleanup`.

### UI

No test runner exists (`package.json`: `dev/build/preview/typecheck`). Validate:
`cd ui && npm run typecheck && npm run build`. New pure helpers in `spanTree.ts`
are the only logic that *could* later be unit-tested if a runner is introduced
(out of scope here).

### CI commands (AGENTS.md gate)

```bash
go build ./... && go vet ./... && go test ./...
cd ui && npm run typecheck && npm run build
dagger call -m ./dagger --src . lint
dagger call -m ./dagger --src . ci export --path out   # full gate when Docker is available
```

### Mandatory live redeploy (AGENTS.local.md §6)

Follow §4 (build→push→helm upgrade→rollout) and §5.1 agent checks, then §5.2
human verification of the tree/zoom/search against real cluster data at
`https://dagger.home.webcenter.fr`.

## 8. Documentation updates

- **ADR-038** (`docs/design/ADR-038-pipeline-tree-zoom-search.md`): document the
  server-side subtree-scoped search endpoint, cursor pagination contract, the
  `LevelSpanIDs` internal-span scoping rule, RE2 safety, and the UI drill-down +
  breadcrumb + aggregated-panel model. Add row to `docs/design/index.md`.
- **`docs/README.md`**:
  - Pipeline UI section: add bullets for the tree view (drill-down + breadcrumb,
    zoom to leaves), aggregated log panel per level, and keyword/regex search
    scoped to the current level.
  - Pipeline API table: add `GET /api/v1/traces/:id/search` row with its params.

## 9. Implementation checklist (ordered for a coder)

1. `internal/domain/telemetry.go`: add types + `ErrInvalidRegex` + interface method.
2. `internal/repository/log_store.go`: implement `SearchTraceLogs` + `matchLogLine`.
3. `internal/service/telemetry_search.go`: `LevelSpanIDs` + helpers.
4. `internal/handler/traces_search.go`: `handleTracesSearch` + parsing/clamps.
5. `internal/handler/server.go`: register the route.
6. `tests/integration/ci_steps_test.go`: add `SearchTraceLogs` to `stepsLogRepo`.
7. Unit tests (repository/service/handler) — run `go test ./...`.
8. Integration test `tests/integration/pipeline_search_test.go`.
9. `ui/src/api/types.ts` + `client.ts`.
10. `ui/src/pipeline/spanTree.ts` (extract shared helpers incl. `logText`/`highlightSegments`).
11. `ui/src/pipeline/LogPanel.vue`.
12. `ui/src/pipeline/StepTree.vue`.
13. `ui/src/pipeline/PipelineView.vue`: swap Steps card → `<StepTree>`, remove dead code.
14. `cd ui && npm run typecheck && npm run build`.
15. Docs: ADR-038 + index + `docs/README.md`.
16. Full CI gate + `golangci-lint` (watch for dead symbols; `go vet`).
17. Redeploy + verify live (AGENTS.local.md §4–§5), request human verification.
18. Branch `fix/pipeline-by-level` from `main`; open PR when green.

## 10. Verification checklist

- [ ] `go build ./...`, `go vet ./...`, `go test ./...` pass.
- [ ] `dagger call -m ./dagger --src . lint` passes (no unused/dead symbols).
- [ ] `cd ui && npm run typecheck && npm run build` pass.
- [ ] Full `dagger call -m ./dagger --src . ci export --path out` passes (when Docker available).
- [ ] Search endpoint: keyword, regex, invalid-regex 400, unknown-span 404, pagination `next` loop.
- [ ] UI: breadcrumb zoom in/out, aggregated logs per level, search highlight,
      load-more, empty/error states, live refresh preserves focus/query.
- [ ] No `v-html` in log rendering; no config/sample or `DAGGER.md` changes needed.
- [ ] Live cluster redeploy + human verification (AGENTS.local.md §6).

## 11. Open questions / risks

- **Match target**: search matches the raw Loki JSON line (not the UI's
  `logText`-decoded body). JSON escaping (`\n`, quotes) can cause rare
  contains/regex misses; accepted for v1, documented in the ADR.
- **Services/Unmatched cards** still use the 1000-line whole-trace fetch
  (unchanged); only the aggregated panel + search are paginated via `/search`.
- **`/logs` endpoint** still ignores the `start`/`end`/`limit` params the CI
  wrapper already sends — pre-existing, out of scope for #21.
- **Live search during streaming**: kept simple (refresh on `logs_update`/poll);
  a search result may lag a few seconds behind a just-ingested line.
