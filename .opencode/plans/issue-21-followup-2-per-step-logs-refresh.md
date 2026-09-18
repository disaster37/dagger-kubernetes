# Issue #21 follow-up 2: dedicated per-step log panel + non-disruptive live refresh

- **Branch:** `fix/pipeline-by-level` (PR #22, still open — continue there, do **not** branch from `main`).
- **Status:** implementation-ready
- **Scope:** UI only (panel placement/identity + refresh policy) plus one Go serving change (cache headers). **No backend search change** — the current subtree scoping (`span_id = focus.span_id`) is already correct and is kept.

---

## 1. Problem statement

The user tested the live pipeline view and reported (verbatim, trimmed):

> "The pipeline UI is the same as before. I should to have dedicated span log on each step when I zoom on it. Actually I have only one span at the end for all. And now it's is refresh lot of time and redisplay at the begin on each refresh...."

Three distinct, confirmed problems:

1. **Panel not perceived as dedicated to the focused step.** `StepTree.vue` renders the breadcrumb, then all child rows, then a single `<LogPanel>` at the very bottom. It *does* re-scope on zoom (sends `span_id = focus.span_id`), but because it sits after every row it reads as "one log panel at the end for all". It must be clearly dedicated to the focused step: labeled with the step's name/identity, attached to the focus (directly under the breadcrumb / focused-step header, **before** the child rows), scoped to that step's subtree, and collapsible.

2. **Refresh churn resets the view.** `PipelineView.vue` bumps `searchRefreshKey` on every `logs_update` SSE event and the 5s poll; `StepTree.vue` watches it and calls `reload()`, which does `entries.value = []`, resets `nextCursor`/`counts`, refetches page 1, and (via `v-follow-logs`) jumps the list back to the bottom. The user sees "refresh a lot" and "redisplay at the begin". Fix: never clear/reset the list or scroll on a background refresh; merge/append new entries; pause auto-refresh while the user is scrolled up (reading) or a search is active; surface a "N new logs" affordance and resume on scroll-to-bottom / indicator click / search clear.

3. **"The pipeline UI is the same as before."** Two possible causes, both addressed:
   - **(a) Stale SPA shell served to the browser.** `internal/handler/ui.go`'s `serveFile` sets **no** `Cache-Control`/`ETag`/`Last-Modified`. Vite emits content-hashed assets (`index-<hash>.js`), but the un-hashed `index.html` that references those hashes is served with no revalidation directive, so a browser can hold a stale `index.html` pointing at the old asset filenames. Fix: emit `Cache-Control: no-cache` for the HTML shell and `public, max-age=31536000, immutable` for hashed `/assets/*`.
   - **(b) The branch was not redeployed after follow-up #1.** The live release runs whatever image was last pushed (AGENTS.local.md revision history shows `fix/pipeline-view-issues`, not `fix/pipeline-by-level`). The mandatory §4–§6 redeploy (in the checklist) is what actually ships the new bundle; human verification must confirm the served `index.html` references a freshly-built asset hash.

---

## 2. Chosen design & rationale

### 2.1 Dedicated panel per focused step

- Move `<LogPanel>` in `StepTree.vue` to sit **directly under a new "focused step header"**, and **above** the child rows. The header shows the focused step's name, status dot, live duration, and a "logs for this step and its descendants" subtitle; a chevron collapses/expands the panel (`panelOpen`).
- The breadcrumb remains the navigation; the header immediately beneath it *is* the focus identity, so the panel reads as "this step's logs". At the root, the header is the root's identity and the panel shows the whole trace (the existing `span_id` omission for root is unchanged).
- The panel keeps its current subtree scope (`span_id = focus.span_id`) — no backend change.
- `<LogPanel>` is keyed by `focus.span_id` so a focus change remounts it; `v-follow-logs`'s `mounted` hook re-pins + scrolls to bottom on the new step (correct "show me this step's latest logs" behavior on zoom).

**Rationale:** the user explicitly rejected inline-per-row panels and one global panel; a single panel that follows the focus, clearly labeled and placed as the focus's own section, satisfies "dedicated panel per focused step" with the least state and no backend change.

### 2.2 Non-disruptive live refresh

Split the single `reload()` into two functions:

- `resetAndLoad()` — full reset (clear entries/cursor/counts/seen/`newCount`), fetch page 1. Called only on deliberate navigation: mount, zoom in/out, query change, mode change, retry.
- `refresh()` — **non-disruptive** live update, called on the debounced `refreshKey` bump (SSE `logs_update` / 5s poll). It computes `cursor = maxEntryTimestampNanos(entries) + 1`, fetches only strictly-newer matches (same `focus`/`query`/`mode`), **merges/append**s them (dedupe by `timestamp + span_id + line`), and updates `nextCursor` from the page. It never clears entries, never resets scroll, and never touches `counts`.

**Pause/resume.** A `paused` computed freezes the visible list update when the user is scrolled up **or** a search is active:

- `pinned` (autofollow state) is surfaced from `LogPanel` → `StepTree` via a new `pinned-change` emit (reads the existing `v-follow-logs` element state).
- `searchActive = query !== ''`.
- `paused = !pinned || searchActive`.

When `refresh()` appends entries while `paused`, it increments `newCount`; the appended lines are below the viewport so the user's reading position never moves. When not paused, `v-follow-logs` auto-scrolls as today and `newCount` stays 0.

**Resume triggers** (all reset `newCount = 0`): scroll to bottom (pinned→true with no search), click the "↓ N new logs" indicator, or clear the search (which is a `resetAndLoad()` anyway).

**Why append + cursor, not page-1 refetch:** refetching page 1 every poll re-renders the whole list (the "redisplay at the begin" symptom) and would *miss* new lines once there are >`PAGE_SIZE` matches (new lines are past page 1). A forward `cursor` fetch returns exactly the new tail, append-only, preserving scroll.

### 2.3 Cache-busting the SPA shell

`serveFile` gains a two-branch `Cache-Control` header (see §4.2): `no-cache` for `index.html` (and any extension-less SPA route), `public, max-age=31536000, immutable` for `/assets/*`. This is a 3-line Go change with a unit test; no config key, no chart change.

---

## 3. Alternatives considered

- **Inline per-row log panels.** Rejected by the user (cluttered, duplicates the search box).
- **One panel at the end but re-labeled.** Insufficient — the user wants the panel *attached to the focus*, not merely re-titled.
- **Keep `reload()` and only debounce harder.** Does not fix the reset; the list still clears and re-renders, and a page-1 refetch misses the live tail beyond 500 matches.
- **Buffer new logs in a separate array and swap on resume.** More state, same UX; appending below the viewport (with the indicator) achieves the same non-disruption with less code.
- **Client-side-only "freeze" without fetching.** Cannot show an accurate "N new logs" count; rejected.
- **Backend `since=` param for refresh.** The existing forward `cursor` already expresses "strictly after", so a new param would be redundant.
- **Whole-trace `/logs` fetch for counts on refresh.** Rejected (query-aware counts must match the panel; counts refresh on `resetAndLoad` only).

---

## 4. Files to create / modify

| Action | Path |
|---|---|
| Modify | `ui/src/pipeline/StepTree.vue` — layout reorder (focused header + panel above rows), `resetAndLoad`/`refresh` split, pause/resume state, merge/dedupe, keyed panel. |
| Modify | `ui/src/pipeline/LogPanel.vue` — `newCount` prop, `pinned-change`/`resume` emits, "N new logs" indicator, scroll → pinned emit. |
| Modify | `ui/src/directives/followLogs.ts` — export `followLogsPinned` / `followLogsPin` helpers. |
| Modify | `ui/src/pipeline/spanTree.ts` — add pure `entryKey` + `maxEntryTimestampNanos`. |
| Modify | `internal/handler/ui.go` — cache headers in `serveFile`. |
| Create | `internal/handler/ui_test.go` — `serveFile` cache-header test. |
| Regenerate | `internal/handler/ui-dist/**` — rebuild + copy `ui/dist` (tracked embed). |
| Modify | `docs/design/ADR-038-pipeline-tree-zoom-search.md` |
| Modify | `docs/README.md` |

No change to `config/config.app.yaml.sample`, `DAGGER.md`, `dagger/`, or `.github/workflows/` (no new config key, no CI/script change).

---

## 5. Data structures & signatures

### 5.1 `ui/src/directives/followLogs.ts`

```ts
// followLogsPinned reports whether the directive considers the element pinned
// to the bottom (user at/near the bottom). Used by LogPanel to surface the
// autofollow state to StepTree for the pause/resume refresh policy.
export function followLogsPinned(el: HTMLElement | null): boolean {
  if (!el) return true
  return readState(el).pinned
}

// followLogsPin forces the element back into the pinned state and scrolls it to
// the bottom. Used when the user clicks the "N new logs" affordance to resume.
export function followLogsPin(el: HTMLElement | null): void {
  if (!el) return
  readState(el).pinned = true
  scrollToBottom(el)
}
```

(`readState`/`scrollToBottom` are already module-private in the file; these two exported wrappers are the only new public surface.)

### 5.2 `ui/src/pipeline/spanTree.ts`

Add `TraceLogEntry` to the existing `@/api/types` import, then:

```ts
// entryKey returns a stable dedupe key for a log entry. Timestamp alone is not
// unique enough (same-nanosecond lines, the refresh cursor re-sending a boundary
// line), so key on the full identity: timestamp + span_id + raw line.
export function entryKey(e: TraceLogEntry): string {
  return `${e.timestamp}\u0000${e.span_id ?? ''}\u0000${e.line}`
}

// maxEntryTimestampNanos returns the max timestamp among entries as unix nanos
// (0 when empty). JS Date.parse has millisecond precision, so the caller adds
// +1ns and relies on entryKey dedupe to drop any boundary re-fetch.
export function maxEntryTimestampNanos(entries: TraceLogEntry[]): number {
  let max = 0
  for (const e of entries) {
    const t = Date.parse(e.timestamp)
    if (!Number.isNaN(t)) max = Math.max(max, t * 1_000_000)
  }
  return max
}
```

### 5.3 `ui/src/pipeline/LogPanel.vue`

Props (add one):

```ts
newCount: number   // new logs arrived while paused; drives the "N new logs" indicator
```

Emits (add two):

```ts
(e: 'pinned-change', pinned: boolean): void
(e: 'resume'): void
```

Template: add a `ref="logEl"` and `@scroll="onScroll"` to the existing `<div v-follow-logs class="logs">`, and render the indicator just above the list:

```html
<button v-if="newCount > 0" class="new-logs" @click="onResume">
  ↓ {{ newCount }} new logs
</button>
<div ref="logEl" v-follow-logs class="logs" @scroll="onScroll">…</div>
```

Script (new):

```ts
import { onMounted, ref } from 'vue'           // extend existing import
import { followLogsPin, followLogsPinned } from '@/directives/followLogs'

const logEl = ref<HTMLElement | null>(null)

function onScroll() {
  // Defer one frame so the directive's own scroll handler has updated `pinned`
  // before we read it (independent of listener registration order).
  requestAnimationFrame(() => emit('pinned-change', followLogsPinned(logEl.value)))
}

function onResume() {
  followLogsPin(logEl.value)
  emit('pinned-change', true)
  emit('resume')
}

onMounted(() => emit('pinned-change', followLogsPinned(logEl.value)))
```

Add `.new-logs` styles (floating bar at the top of the list, `position: sticky` or a simple banner above `.logs`).

### 5.4 `ui/src/pipeline/StepTree.vue`

New state:

```ts
const panelOpen = ref(true)
const pinned = ref(true)
const newCount = ref(0)
const seen = new Set<string>()   // entryKey dedupe set for merge/append
```

New computeds:

```ts
const searchActive = computed(() => query.value !== '')
const paused = computed(() => !pinned.value || searchActive.value)
```

New/changed functions:

```ts
// resetAndLoad — full reset + page-1 fetch (navigation / retry only).
async function resetAndLoad()                 // (was reload)

// refresh — non-disruptive live update on refreshKey bump.
async function refresh()

// mergeEntries — append + dedupe, returns the number of entries actually added.
function mergeEntries(newEntries: TraceLogEntry[]): number

// fetchPage — page-1 (cursor undefined) resets entries/seen/counts; load-more
// (cursor set) appends through the same dedupe path.
async function fetchPage(cursor: number | undefined)

function onPinnedChange(p: boolean) {
  pinned.value = p
  if (p && !searchActive.value) newCount.value = 0
}
function onResume() {
  pinned.value = true
  newCount.value = 0
}
```

`fetchPage` body (cursor branch now dedupes):

```ts
if (cursor === undefined) {
  entries.value = page.entries
  seen.clear()
  for (const e of page.entries) seen.add(entryKey(e))
  counts.value = page.counts ?? {}
} else {
  for (const e of page.entries) {
    const k = entryKey(e)
    if (seen.has(k)) continue
    seen.add(k)
    entries.value.push(e)
  }
}
nextCursor.value = page.next ?? null
```

`refresh` body:

```ts
async function refresh() {
  if (entries.value.length === 0) { await fetchPage(undefined); return } // first load
  const cursor = maxEntryTimestampNanos(entries.value) + 1
  loading.value = true
  error.value = null
  try {
    const page = await fetchTraceSearch(props.traceId, {
      span_id: focus.value?.span_id,
      q: query.value || undefined,
      mode: mode.value,
      limit: PAGE_SIZE,
      cursor,
    })
    const appended = mergeEntries(page.entries)
    nextCursor.value = page.next ?? null
    if (paused.value && appended > 0) newCount.value += appended
  } catch (e) {
    // Non-disruptive: keep the current list on a failed background refresh.
    console.error('Failed to refresh logs', e)
  } finally {
    loading.value = false
  }
}
```

Call-site changes: the `refreshKey` watcher calls `refresh()` (not `reload`); `zoomIn`/`zoomTo`/`onQueryChange`/`onModeChange` call `resetAndLoad()` and `zoomIn`/`zoomTo` also set `panelOpen.value = true`; the retry emit and `onMounted` call `resetAndLoad()`.

Template reorder (inside the `v-else` root branch):

```html
<div class="breadcrumb">…</div>

<div class="focus-header">
  <span :class="['dot', `dot-${focus?.status ?? 'unset'}`]"></span>
  <span class="focus-name">{{ focus?.name || focus?.span_id }}</span>
  <span class="focus-duration">{{ focus ? formatDuration(liveSpanDuration(focus, now)) : '' }}</span>
  <span class="focus-sub">logs for this step and its descendants</span>
  <button class="chevron" @click="panelOpen = !panelOpen">{{ panelOpen ? '▾' : '▸' }}</button>
</div>

<LogPanel
  v-if="panelOpen"
  :key="focus?.span_id ?? 'none'"
  :entries="entries" :query="query" :mode="mode" :loading="loading" :error="error"
  :has-more="nextCursor !== null" :total-shown="entries.length" :attribution="attribution"
  :new-count="newCount"
  @update:query="onQueryChange" @update:mode="onModeChange" @load-more="loadMore"
  @retry="resetAndLoad" @select-span="selectSpan"
  @pinned-change="onPinnedChange" @resume="onResume"
/>

<div v-if="rows.length === 0" class="empty">No steps for this level</div>
<div v-for="row in rows" …>…</div>
```

Add `.focus-header`/`.focus-name`/`.focus-sub`/`.focus-duration` styles (monospace name, muted subtitle).

### 5.5 `internal/handler/ui.go`

```go
// In serveFile, after setting Content-Type:
if strings.HasPrefix(rel, "assets/") {
    c.Response.Header.Set("Cache-Control", "public, max-age=31536000, immutable")
} else {
    c.Response.Header.Set("Cache-Control", "no-cache")
}
```

(`strings` is already imported.)

### 5.6 `internal/handler/ui_test.go` (new)

`TestServeFileCacheHeaders` — table-driven, stdlib `testing`, using `testing/fstest.MapFS` and the existing `ut`/`route` test harness (`route.NewEngine(config.NewOptions(nil))` + `ut.PerformRequest`, as in `traces_search_test.go`). Cases:
- `index.html` (and an extension-less SPA route) → `Cache-Control: no-cache`.
- `assets/index-abc.js` → `Cache-Control` contains `immutable` and `max-age=31536000`.
- missing file → 404 path unchanged.

---

## 6. Exact UI behavior

- **Placement & label:** breadcrumb → focused-step header (status dot, name, live duration, subtitle, collapse chevron) → collapsible `LogPanel` → child rows. The header names the focused step, so the panel is unambiguously "this step's logs". At the root the header is the root identity ("logs for this step and its descendants" = whole trace).
- **Scope:** unchanged — `span_id = focus.span_id` (omitted at root). Whole subtree (step + descendants) per the user's requirement; backend already correct.
- **Zoom / breadcrumb navigation:** changes `focusPath` → `resetAndLoad()` (page 1, counts) → panel keyed remount re-pins + scrolls to bottom. Breadcrumb back-outs behave the same.
- **Collapse/expand:** `panelOpen` chevron in the header; collapsed hides the panel only, child rows remain.
- **Refresh policy:**
  - "Scrolled up" = `followLogsPinned` false (user > `FOLLOW_PIN_THRESHOLD` from bottom, with the directive's hysteresis).
  - "Search active" = `query !== ''`.
  - `paused = !pinned || searchActive`.
  - Refresh **always** fetches the new tail (cursor = max loaded ts + 1) and appends; when `paused` it increments `newCount` and does not scroll; when not paused `v-follow-logs` auto-scrolls.
  - `newCount` affordance: "↓ N new logs" button above the list; click → `followLogsPin` (re-pin + scroll bottom) + reset count.
  - Resume also on scroll-to-bottom (`pinned-change(true)` with no search) and on search clear (a full `resetAndLoad`).
- **Merge/dedupe:** append-only, dedupe by `entryKey` (`timestamp + span_id + line`). `refresh` and `loadMore` both go through the same `seen` set. Scroll position is preserved because existing rows keep their index keys and nothing is prepended.
- **Pagination ("Load more"):** appends the next page (deduped); does not change `newCount`; does not auto-scroll when unpinned.
- **Count/step badges:** unchanged; `counts` are refreshed only on `resetAndLoad` (first page) so count badges are query-aware but not re-scanned every 5s. During a live run the badges may lag until the next navigation — documented, acceptable.
- **Live updates while paused:** buffered in-place (appended below the viewport) with the count surfaced; no data is dropped.

---

## 7. Edge cases & error handling

- **No logs:** empty `entries` → "No logs for this level"; `refresh` with empty entries falls back to `fetchPage(undefined)` (page 1 + counts).
- **Search active + new logs:** `refresh` applies the query, appends new *matching* lines, `paused` true → `newCount` grows; indicator visible; clearing search resets.
- **Focus change while paused:** keyed panel remount re-pins (`pinned-change(true)`), `resetAndLoad` clears `newCount`/`seen`; no stale entries bleed across steps.
- **Pagination while paused:** `loadMore` appends (user-initiated), no auto-scroll, `newCount` untouched.
- **SSE reconnect / poll overlap:** idempotent — `cursor` + `entryKey` dedupe drop double-fetched boundary lines.
- **Very large volume:** each `refresh` fetches ≤ `PAGE_SIZE`; `entries` grows only by genuinely-new lines. `newCount` is unbounded but is just a number. Documented limitation: an extremely chatty pipeline keeps appending (memory grows) until the next navigation; a future cap/trim is out of scope (trimming the oldest would shift scroll while reading).
- **Duplicate entries on merge:** `entryKey` dedupe.
- **Scroll preservation:** append-only + index keys; never prepend.
- **XSS:** no `v-html` anywhere; header name, subtitle, indicator count, and badges are all `{{ }}` interpolated.
- **Unicode / long names:** CSS ellipsis on `.focus-name`/badges; interpolation handles unicode.
- **Failed background refresh:** keep current entries (no wipe), log to console only; the next tick retries.
- **Invalid regex:** unchanged — `resetAndLoad` pre-validates and shows inline error; `refresh` reuses the same validated query.

---

## 8. Validation strategy

**Go (stdlib `testing`, table-driven):**

- `internal/handler/ui_test.go` — `TestServeFileCacheHeaders` as in §5.6.

**UI:**

```bash
cd ui && npm run typecheck && npm run build
```

**CI gate (AGENTS.md):**

```bash
# full gate when a Docker daemon is available:
dagger call -m ./dagger --src . ci export --path out
# minimum fallback:
go build ./... && go vet ./... && go test ./...
dagger call -m ./dagger --src . lint
cd ui && npm run typecheck && npm run build
```

**Regenerate the tracked embed** (required — `internal/handler/ui-dist/` is git-tracked and `//go:embed all:ui-dist` consumes it):

```bash
cd ui && npm ci && npm run build && rm -rf ../internal/handler/ui-dist && cp -r dist ../internal/handler/ui-dist
```

**Live redeploy + verification (mandatory, AGENTS.local.md §4–§6):** rebuild image, push, `helm get values` capture, `helm upgrade`, `rollout restart` + wait, §5.1 agent checks, then §5.2 human verification — specifically confirm the served `index.html` references a freshly-built asset hash (curl the page and diff the `assets/index-*.js` name against the last known good bundle).

---

## 9. Documentation updates

- **`docs/design/ADR-038-pipeline-tree-zoom-search.md`** — update §4 (UI) to describe the dedicated focused-step panel (header + placement above child rows + collapse) and the refresh policy (append-merge via forward cursor, pause/resume, "N new logs"). Update Consequences: replace "Live search during streaming is kept simple (refresh on logs_update/poll)" with the new non-disruptive merge + pause/resume behavior; add a line that the SPA shell is served `Cache-Control: no-cache` while hashed `/assets/*` are immutable.
- **`docs/README.md`** — update the "Aggregated logs per level" bullet (dedicated, labeled, collapsible panel under the focused-step header, above child rows) and the "Live updates" / "Log viewer" bullets (non-disruptive append-only refresh, pause while scrolled-up/searching, "N new logs" affordance, and the `no-cache`/`immutable` cache policy).

No change to `config/config.app.yaml.sample` or `DAGGER.md` (nothing affected).

---

## 10. Ordered implementation checklist

1. `ui/src/directives/followLogs.ts` — add `followLogsPinned` / `followLogsPin`.
2. `ui/src/pipeline/spanTree.ts` — add `entryKey` / `maxEntryTimestampNanos` (+ `TraceLogEntry` import).
3. `ui/src/pipeline/LogPanel.vue` — `newCount` prop, `pinned-change`/`resume` emits, indicator, scroll wiring, styles.
4. `ui/src/pipeline/StepTree.vue` — reorder template (focused header + keyed panel above rows), `resetAndLoad`/`refresh`/`mergeEntries`, pause/resume state, dedupe in `fetchPage`, wire new emits.
5. `internal/handler/ui.go` — cache headers in `serveFile`.
6. `internal/handler/ui_test.go` — `TestServeFileCacheHeaders`.
7. Regenerate `internal/handler/ui-dist/` (build + copy).
8. Docs: ADR-038 + `docs/README.md`.
9. `cd ui && npm run typecheck && npm run build`.
10. `go build ./... && go vet ./... && go test ./...` (watch for dead symbols; none expected — `reload` is renamed, not orphaned).
11. `dagger call -m ./dagger --src . lint`; full `dagger call -m ./dagger --src . ci export --path out` when Docker is available.
12. Redeploy + agent checks + human verification (AGENTS.local.md §4–§6), confirming the fresh asset hash in the served `index.html`.

---

## 11. Verification checklist

- [ ] `go build ./... && go vet ./... && go test ./...` green (incl. `ui_test.go`).
- [ ] `cd ui && npm run typecheck && npm run build` green.
- [ ] `dagger call -m ./dagger --src . lint` green (no unused/dead symbols).
- [ ] Panel renders directly under the focused-step header, above child rows, labeled with the step name/status/duration; collapsible.
- [ ] Zooming re-scopes the panel to that step's subtree and re-pins to the latest logs; breadcrumb returns.
- [ ] Scrolling up freezes auto-scroll; new logs show "↓ N new logs"; clicking it / scrolling to bottom resumes and clears the count.
- [ ] Search active freezes auto-scroll; clearing search resets cleanly.
- [ ] Live refresh never clears the list or jumps scroll to the start; "Load more" still works and dedupes.
- [ ] Served `index.html` → `Cache-Control: no-cache`; `/assets/*` → `immutable`.
- [ ] No `v-html`; long/unicode names truncated safely.
- [ ] Live UI verified by a human at `https://dagger.home.webcenter.fr` with the fresh bundle hash.

---

## 12. Git workflow

Continue on `fix/pipeline-by-level` and update open PR #22 (do **not** branch from `main`; the feature is not merged). Commit the UI changes, the `ui.go` cache-header change + test, the regenerated `ui-dist`, and the doc updates in one changeset; push; update the PR description to note the follow-up (dedicated per-step panel + non-disruptive refresh + SPA cache-busting).

---

## 13. Open questions / risks

- **Count badges lag during a live run** (counts only re-scan on navigation, since a cursor-based refresh omits `counts`). Accepted: re-scanning every 5s is wasteful and the badges are query-aware anyway. Flag if a future request wants live counts.
- **Unbounded `entries` growth** on a very chatty pipeline while following live (append-only). Acceptable for v1; a cap-with-trim would shift scroll while reading, which is the exact bug being fixed.
- **Live "gap" between old page-1 tail and the live tail** for a viewer who has only loaded page 1 and then follows live: refresh appends the newest lines directly, so the middle isn't loaded until "Load more" pages forward. This is inherent to forward-only cursor pagination (ADR-038) and is the normal `tail -f` shape; documented, not a regression.
- **`index.html` cache fix only helps after a fresh fetch** — users with a currently-cached stale shell need one hard refresh after this deploys (or the browser revalidates on the now-sent `no-cache` on subsequent loads). Human verification should use a clean/hard-reload.
