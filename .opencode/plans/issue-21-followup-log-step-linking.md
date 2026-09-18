# Issue #21 follow-up: restore bidirectional log ↔ step linking in the tree view

- **Branch:** `fix/pipeline-by-level` (PR #22, still open — this is a correction, not a new feature branch)
- **Status:** implementation-ready
- **Scope:** UI attribution + server-side per-span counts for the drill-down tree / aggregated log panel introduced by ADR-038.

---

## 1. Problem statement

After PR #22, the pipeline "Steps" card renders a drill-down tree (`StepTree.vue`) and a
single aggregated, searchable log panel (`LogPanel.vue`) at the bottom. The panel renders
each log line as timestamp + message only, and the tree rows show status/name/duration but
no log indicator. `TraceLogEntry.span_id` is returned by `/api/v1/traces/:traceID/search`
(`domain.LogEntry.SpanID`, `json:"span_id,omitempty"`) and the UI type already declares it
(`ui/src/api/types.ts`), but the UI never maps it back to a step. The user's complaint:

> "The log span is no more linked with any step. We have step with childs, and then at the
> end all logs without links."

**Confirmed root cause:** purely a UI gap. `LogPanel.vue` ignores `entry.span_id`; the
per-row "log-presence" indicator planned in the PR was dropped. The span-ID encodings are
already consistent end-to-end (`internal/repository/log_store.go` `normalizeSpanID`
converts the collector's 16-char hex Loki label to base64; `trace_store.go` reads Tempo's
base64 `spanId` verbatim) — so string equality between a log's `span_id` and a tree node's
`span_id` already holds. No encoding fix is needed.

## 2. Chosen design

### 2.1 Log → step (client-side attribution, no backend change)

Each search entry already carries `span_id`. Map it to the **visible row** at the current
focus level via a new shared helper `computeRowOwners(focus)`, then render a step-name
badge on each log line. Clicking the badge scrolls to + highlights that row (the row is
always already visible at the current focus level). Logs that cannot be attributed
(internal-span, empty/unknown span) render a muted "unattributed" badge.

### 2.2 Step → logs (server-side counts map, decided with the user)

Extend `GET /api/v1/traces/:traceID/search` to return a `counts` map
(`span_id -> matching-log count`) computed in the **same bounded scan** already used for
pagination, on the **first page only** (`IncludeCounts`). The client rolls these raw
per-span counts up to visible rows (same `computeRowOwners` walk), renders a count badge
per row, and the badge click **zooms into the step** (reuses `zoomIn`, breadcrumb = back).

**Why server-side:** counts must reflect *matching* logs (query-aware) and be consistent
with the panel (same Go `matchLogLine`, same raw Loki line, same `logSearchMaxScan=5000`
bound). Client-side-only counts under-report until "Load more" is exhausted (at the root,
page 1 = 500 lines biases toward early steps); a whole-trace-fetch fallback re-implements
matching in JS (Go RE2 vs JS regex drift) and disagrees with the paginated panel.

### 2.3 Unmatched logs (merged into the panel)

Remove the redundant "Unmatched / general logs" `<details>` block in `PipelineView.vue`.
Un-attributable logs are now labeled "unattributed" inline in the panel (visible at the
root level, where the search has no span filter). Note: at a sub-focus, empty-span logs are
already excluded server-side by `LevelSpanIDs`' span set, so they only appear at the root
level — acceptable.

## 3. Alternatives considered

- **Client-side counts from loaded entries** — zero backend change but under-reports
  (badges fill in only as pages load); rejected as not "complete".
- **Client-side counts from the whole-trace `/logs` fetch** — complete to 1000 lines but
  re-implements query matching in JS (regex dialect drift) and can disagree with the panel;
  rejected.
- **Aggregate counts to rows server-side** — pushes the visibility/ownership rules into Go,
  duplicating `spanTree.ts` and risking drift; rejected (keep one source of truth for
  visibility in the TS layer; server returns raw per-span counts only).
- **Step badge click = separate "filter panel" state** — adds a new state + clear-filter
  affordance for little gain over zoom; rejected in favour of reuse.

## 4. Backend changes (Go)

### 4.1 `internal/domain/telemetry.go`

```go
// LogSearchRequest is a subtree-scoped, text-filtered, paginated log query.
type LogSearchRequest struct {
	SpanIDs       []string      // descendant span IDs (base64) to include; nil/empty = all spans
	Query         string        // text to match; empty = no text filter
	Mode          LogSearchMode // LogSearchContains | LogSearchRegex
	Start         time.Time     // inclusive window start (handler defaults to last 24h)
	End           time.Time     // inclusive window end (handler defaults to now)
	Limit         int           // max matching entries to return this page
	Cursor        int64         // unix nanos; return entries strictly after this timestamp
	IncludeCounts bool          // compute per-span matching counts (first page only)
}

// LogSearchPage is one page of subtree-scoped search results.
type LogSearchPage struct {
	Entries []LogEntry       `json:"entries"`
	Next    int64            `json:"next,omitempty"`  // cursor for the next page; 0 = no more
	Counts  map[string]int64 `json:"counts,omitempty"` // span_id -> matching log count (first page only)
}
```

### 4.2 `internal/repository/log_store.go`

Rewrite the scan loop in `SearchTraceLogs` so that, when `req.IncludeCounts` is true, it
keeps scanning past a full page (up to `logSearchMaxScan` or exhaustion) to count matching
logs per span, while still returning only `limit` entries and a cursor anchored at the last
*returned* entry. **Delete `appendMatches`** (it becomes unused — `golangci-lint` `unused`
will otherwise fail CI).

```go
//nolint:gocritic // req is passed by value to satisfy domain.LogRepository.
func (c *LogsClient) SearchTraceLogs(ctx context.Context, traceID string, req domain.LogSearchRequest) (domain.LogSearchPage, error) {
	// ... unchanged: traceID validation, lokiURL check, regexp compile, limit,
	// spanSet build, cursor init ...

	var matches []domain.LogEntry
	counts := make(map[string]int64)
	scanned := 0
	exhausted := false
	var lastRawTs, pageAnchorTs int64
	pageFull := false
	wantCounts := req.IncludeCounts

scan:
	for scanned < logSearchMaxScan {
		raw, err := c.queryRawLogs(ctx, traceID, cursor, req.End, logSearchBatch)
		if err != nil {
			return domain.LogSearchPage{}, err
		}
		if len(raw) == 0 {
			exhausted = true
			break
		}
		sort.SliceStable(raw, func(i, j int) bool {
			return raw[i].Timestamp.Before(raw[j].Timestamp)
		})
		for _, entry := range raw {
			lastRawTs = entry.Timestamp.UnixNano()
			if len(spanSet) > 0 {
				if _, ok := spanSet[entry.SpanID]; !ok {
					continue
				}
			}
			if !matchLogLine(entry.Line, req.Query, req.Mode, re) {
				continue
			}
			if wantCounts && entry.SpanID != "" {
				counts[entry.SpanID]++
			}
			if pageFull {
				continue
			}
			matches = append(matches, entry)
			pageAnchorTs = lastRawTs
			if len(matches) >= limit {
				pageFull = true
				if !wantCounts {
					break scan
				}
			}
		}
		scanned += len(raw)
		if len(raw) < logSearchBatch {
			exhausted = true
			break
		}
		cursor = lastRawTs + 1
	}

	if matches == nil {
		matches = []domain.LogEntry{}
	}
	page := domain.LogSearchPage{Entries: matches, Counts: counts}
	if pageFull {
		page.Next = pageAnchorTs + 1
	} else if !exhausted {
		page.Next = lastRawTs + 1
	}
	return page, nil
}
```

Key invariants (already exercised by the existing tests, which must keep passing):
- Pagination cursor is still the timestamp of the last *returned* entry + 1ns (unchanged
  same-nanosecond boundary limitation, ADR-038).
- On a load-more page (`IncludeCounts=false`), the old fast path (`break scan` on page-full)
  is preserved and `Counts` is nil → omitted from JSON.

### 4.3 `internal/handler/traces_search.go`

Set the new flag on the first page:

```go
page, err := s.logs.SearchTraceLogs(context.Background(), traceID, domain.LogSearchRequest{
	SpanIDs:       spanIDs,
	Query:         c.Query("q"),
	Mode:          mode,
	Start:         start,
	End:           end,
	Limit:         clampSearchLimit(c.Query("limit")),
	Cursor:        cursor,
	IncludeCounts: cursor == 0,
})
```

No other handler change (`writeJSON(c, page)` already serializes `Counts`).

## 5. Frontend changes (Vue/TS)

### 5.1 `ui/src/api/types.ts`

```ts
export interface LogSearchPage {
  entries: TraceLogEntry[]
  next?: number
  counts?: Record<string, number> // span_id -> matching log count (first page only)
}
```

### 5.2 `ui/src/api/client.ts`

`fetchTraceSearch` returns `counts` (default `{}` when absent):

```ts
return { entries: page.entries ?? [], next: page.next, counts: page.counts ?? {} }
```

### 5.3 `ui/src/pipeline/spanTree.ts`

Add two pure helpers (no DOM, unit-visible for typecheck):

```ts
// computeRowOwners maps every span in focus's subtree to the span_id of the
// visible row (a direct visible child of focus) that owns its logs. Spans that
// roll up to focus itself map to focus.span_id. Internal spans are omitted
// (their logs are "unattributed"). Mirrors the old ownerBySpanID/passthrough
// rules (internal first, then transparent).
export function computeRowOwners(focus: SpanNode): Map<string, string> {
  const map = new Map<string, string>()
  map.set(focus.span_id, focus.span_id)
  const assign = (n: SpanNode, rowOwner: string) => {
    if (isInternalSpan(n)) {
      for (const c of n.children) assign(c, rowOwner)
      return
    }
    if (isTransparentSpan(n)) {
      map.set(n.span_id, rowOwner)
      for (const c of n.children) assign(c, rowOwner)
      return
    }
    map.set(n.span_id, n.span_id)
    for (const c of n.children) assign(c, n.span_id)
  }
  for (const c of focus.children) assign(c, focus.span_id)
  return map
}

// formatCount renders a badge count compactly for large values.
export function formatCount(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}m`
  if (n >= 1000) return `${(n / 1000).toFixed(1)}k`
  return String(n)
}
```

Do **not** move `ownerBySpanID` out of `PipelineView.vue` — it has different semantics
(nearest *visible* ancestor across the whole tree, for Services) and stays where it is.

### 5.4 `ui/src/pipeline/LogPanel.vue`

- New prop `attribution: Map<string, { label: string; ownerSpanID: string }>` (span_id →
  step). New emit `select-span(ownerSpanID: string)`.
- `RenderedLine` gains `badge: { label: string; ownerSpanID: string } | null`; the `rendered`
  computed resolves it from `props.attribution.get(entry.span_id)` (undefined → `null`).
- Template: render the badge before the message; clickable when attributed, muted otherwise.
  No `v-html` anywhere (badge text is `{{ }}` interpolated).

```html
<span v-if="line.badge" class="log-step" :title="line.badge.label"
      @click="$emit('select-span', line.badge.ownerSpanID)">{{ line.badge.label }}</span>
<span v-else class="log-step log-step-unattributed">unattributed</span>
```

- Add `.log-step` styles (chip, `flex-shrink:0`, `max-width:180px`, ellipsis, monospace)
  and `.log-step-unattributed` (muted grey, no pointer).

### 5.5 `ui/src/pipeline/StepTree.vue`

State/computeds:

```ts
const counts = ref<Record<string, number>>({})
const highlightedSpanID = ref<string | null>(null)
let highlightTimer: number | undefined

const rowOwners = computed(() => (focus.value ? computeRowOwners(focus.value) : new Map<string, string>()))

const rowCounts = computed<Map<string, number>>(() => {
  const m = new Map<string, number>()
  for (const [spanID, n] of Object.entries(counts.value)) {
    const owner = rowOwners.value.get(spanID)
    if (owner !== undefined) m.set(owner, (m.get(owner) ?? 0) + n)
  }
  return m
})

const attribution = computed<Map<string, { label: string; ownerSpanID: string }>>(() => {
  const name = new Map<string, string>()
  if (focus.value) name.set(focus.value.span_id, focus.value.name || focus.value.span_id)
  for (const r of rows.value) name.set(r.node.span_id, r.node.name || r.node.span_id)
  const out = new Map<string, { label: string; ownerSpanID: string }>()
  for (const [spanID, ownerID] of rowOwners.value) {
    out.set(spanID, { label: name.get(ownerID) ?? ownerID, ownerSpanID: ownerID })
  }
  return out
})
```

`fetchPage` stores counts only on the first page and preserves them across load-more:

```ts
entries.value = cursor === undefined ? page.entries : [...entries.value, ...page.entries]
nextCursor.value = page.next ?? null
if (cursor === undefined) counts.value = page.counts ?? {}
```

Row template: add the count badge (click = zoom) and a highlight class + ref:

```html
<div v-for="row in rows" :key="row.node.span_id" class="step"
     :class="{ 'step-highlight': highlightedSpanID === row.node.span_id }"
     :ref="(el) => setRowRef(row.node.span_id, el as HTMLElement | null)">
  <div class="step-row">
    ...
    <button v-if="rowCounts.get(row.node.span_id)" class="step-logs-badge"
            @click.stop="zoomIn(row.node)">{{ formatCount(rowCounts.get(row.node.span_id)!) }}</button>
    ...
```

New functions:

```ts
function setRowRef(spanID: string, el: HTMLElement | null) { /* store into a ref map */ }
function selectSpan(ownerSpanID: string) {
  highlightedSpanID.value = ownerSpanID
  void nextTick(() => {
    const el = rowEls[ownerSpanID]
    el?.scrollIntoView({ block: 'center', behavior: 'smooth' })
  })
  if (highlightTimer) window.clearTimeout(highlightTimer)
  highlightTimer = window.setTimeout(() => { highlightedSpanID.value = null }, 1500)
}
```

Pass `attribution` + `@select-span="selectSpan"` to `LogPanel`. Add `nextTick` import and
clear `highlightTimer` in `onUnmounted`.

### 5.6 `ui/src/pipeline/PipelineView.vue`

- Remove the `<details class="card">` "Unmatched / general logs" block (lines 86–98) and the
  `unmatchedLogs` computed.
- Keep `logs`, `logsBySpan`, `logsByOwner`, `logsForSubtree`, `ownerBySpanID` (all still
  used by the Services card). Keep `.logs/.log-line/.log-ts/.log-msg` CSS (used by Services).
- The `v-follow-logs` import/directive stays (Services preview uses it).

## 6. Edge cases & error handling

- **Hidden/transparent spans:** passthrough/encapsulated span logs promote to the nearest
  visible row (`computeRowOwners`); matches the old `ownerBySpanID` and ADR-037 semantics.
- **Internal spans:** `computeRowOwners` omits them, so their logs render "unattributed" and
  contribute to no badge (server `LevelSpanIDs` already excludes them for sub-focus searches;
  at the root the search has no span filter, so they appear labeled, not dropped).
- **Unknown / empty `span_id`:** `attribution.get` returns undefined → "unattributed" badge;
  never breaks rendering.
- **Spans outside the current focus subtree** (e.g. after a focus change/live refresh):
  `computeRowOwners` doesn't contain them → "unattributed".
- **Duplicate span names:** badges show names, but navigation keys on `span_id` (unique), so
  scroll/highlight is unambiguous.
- **Very long names / unicode:** CSS ellipsis on the badge; interpolation handles unicode.
- **XSS:** no `v-html`; badge text is plain `{{ }}` interpolation.
- **Huge counts:** `formatCount` compacts (1.2k / 1.3m); server counts are int64.
- **Concurrent live refresh:** debounced `reload()` re-fetches page 1 (counts refresh);
  `refreshKey`/`logs_update` path already preserves focus + query. `computeRowOwners`/
  `attribution` are `computed` (cached) — recomputed only when `focus`/`rows` change.
- **Pagination:** counts are set once on page 1 and preserved across "Load more"; the panel
  badge for a line beyond page 1 resolves against the same `attribution` map.
- **Loading/error:** unchanged — panel shows "Loading logs…"/"No matching logs" + retry; rows
  show no badge while `counts` is empty.
- **Backend scan bound:** counts are bounded by `logSearchMaxScan` (5000 raw entries) — the
  same documented honesty boundary as search itself (ADR-038).

## 7. Validation strategy

**Go unit tests (stdlib `testing`, table-driven):**

- `internal/repository/log_search_test.go` — add `TestSearchTraceLogsCounts`:
  - first page (`IncludeCounts: true`) returns per-span counts for matching logs only;
  - `IncludeCounts: false` (cursor non-zero) returns no/empty `Counts` and keeps the old
    fast path;
  - span-filter and query-filter both narrow the counts;
  - `limit < total matches` still counts the full matching set (page-full + counting);
  - empty `span_id` entries are not counted.
- `internal/handler/traces_search_test.go` — extend `stubLogRepo` to record
  `lastReq.IncludeCounts`; add cases asserting `IncludeCounts=true` on first page and
  `IncludeCounts=false` with `?cursor=123`; assert `Counts` is passed through in the JSON.
- `tests/integration/pipeline_search_test.go` — assert the whole-trace `q=error` response
  includes `counts` (`{"Y2hpbGQ=":1,"Z3JhbmQ=":1}` for the existing fixtures).

**UI validation (no test runner in `ui/`):**

```bash
cd ui && npm run typecheck && npm run build
```

**CI gate (per AGENTS.md):**

```bash
dagger call -m ./dagger --src . ci export --path out
# fallback when no Docker daemon:
go build ./... && go vet ./... && go test ./...
dagger call -m ./dagger --src . lint
```

**Live redeploy + verification (mandatory, AGENTS.local.md §4–§6):** rebuild image
(`docker build -t docker.io/disaster/dagger-kubernetes:dev .`), push, capture Helm values,
`helm upgrade`, `rollout restart`, then the §5.1 agent checks and §5.2 human verification of
the pipeline view at `https://dagger.home.webcenter.fr`.

## 8. Documentation (same changeset)

- **`docs/design/ADR-038-pipeline-tree-zoom-search.md`** — update §2 (response shape now
  includes `counts`), §4 (UI: per-line step badges, row count badges → zoom, unattributed
  label, merged unmatched section), Consequences, and resolve the "Open question" about
  paging the Unmatched card (now merged/removed).
- **`docs/README.md`** — update the "Aggregated logs per level", "Log search", "Log viewer"
  bullets (~lines 1679–1727) and the `/search` row in the Pipeline API table (~line 1754):
  log lines carry a step badge; step rows carry a count badge that zooms; unattributed logs
  are labeled; the "unmatched" section is gone; search response includes `counts`.
- **No change** to `config/config.app.yaml.sample` or `DAGGER.md` (no config key and no
  `dagger/`/CI script touched).

## 9. Ordered implementation checklist

1. `internal/domain/telemetry.go` — add `IncludeCounts` + `Counts`.
2. `internal/repository/log_store.go` — rewrite `SearchTraceLogs` loop; delete `appendMatches`.
3. `internal/handler/traces_search.go` — set `IncludeCounts: cursor == 0`.
4. Go tests: `log_search_test.go`, `traces_search_test.go`, `pipeline_search_test.go`.
5. `ui/src/api/types.ts` + `ui/src/api/client.ts` — `counts` in the page shape.
6. `ui/src/pipeline/spanTree.ts` — `computeRowOwners` + `formatCount`.
7. `ui/src/pipeline/LogPanel.vue` — badge rendering + `select-span` emit + styles.
8. `ui/src/pipeline/StepTree.vue` — counts/attribution/row-owners computeds, row badges,
   scroll+highlight, `fetchPage` counts capture.
9. `ui/src/pipeline/PipelineView.vue` — remove unmatched section + `unmatchedLogs`.
10. Docs: ADR-038 + `docs/README.md`.
11. Full CI gate + `cd ui && npm run typecheck && npm run build`.
12. Live redeploy + agent/human verification (AGENTS.local.md §4–§6).

## 10. Verification checklist

- [ ] `go build ./... && go vet ./... && go test ./...` green (incl. `-race`).
- [ ] `dagger call -m ./dagger --src . lint` green (no `unused` failure from `appendMatches`).
- [ ] `cd ui && npm run typecheck && npm run build` green.
- [ ] `/search` first page returns `counts`; load-more pages omit it; cursor semantics unchanged.
- [ ] At the root: every log line shows a step badge or "unattributed"; clicking a badge
      scrolls to + flashes the owning row.
- [ ] Step rows show count badges reflecting the active query; clicking a badge zooms in;
      breadcrumb returns.
- [ ] Search + "Load more" keeps badges/attribution consistent; live refresh preserves
      focus/query.
- [ ] No `v-html`; long/unicode names truncated safely.
- [ ] Live UI verified by a human at `https://dagger.home.webcenter.fr`.

## 11. Git workflow

Continue on `fix/pipeline-by-level` and update open PR #22 (do **not** branch from `main`;
the feature code is not merged). Commit the fix on the existing branch, push, and update the
PR description to note the follow-up (bidirectional log↔step linking + counts).

## 12. Open questions / risks

- **Encapsulated spans:** `visibleChildren` shows them as rows, but `computeRowOwners`
  promotes their logs to the parent (matches the old `ownerBySpanID`). This is pre-existing
  behaviour, preserved as-is; flag for a follow-up if it ever looks wrong on a real trace.
- **Root-level internal noise:** at the root the search has no span filter, so internal-span
  logs render as "unattributed" (not dropped). Acceptable and honest; a future change could
  exclude internal spans at the root too (would also require preserving empty-span logs).
- **Counts scan cost:** first page now scans up to `logSearchMaxScan` even when the page
  fills early (sparse queries already did this). Bounded and acceptable on the single-binary
  Loki; revisit if search latency regresses in production.
