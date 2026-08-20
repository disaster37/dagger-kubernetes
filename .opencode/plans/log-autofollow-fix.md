# Plan — Live pipeline UI: step log auto-scroll / auto-follow fix

## 1. Problem statement (from user report)

In the live pipeline UI (`/pipelines/:id`, `ui/src/pipeline/PipelineView.vue`), when a
user follows a running pipeline and expands a step to read its logs:

1. The log view does **not** auto-scroll to the end on open — the user must scroll
   down manually.
2. While pinned at the bottom, the view does **not** keep following newly arriving
   log lines (no stick-to-bottom / auto-follow). Scrolling up should unpin;
   scrolling back to the bottom should re-pin.

Expected: opening a step's log view scrolls to the end immediately, and while the
view is pinned at the bottom, incoming log lines keep it scrolled to the end, with
the standard unpin-on-scroll-up / re-pin-on-scroll-to-bottom UX.

## 2. Root-cause analysis (exact file + line references)

All relevant code is in **`ui/src/pipeline/PipelineView.vue`** (single-file Vue 3
component, `<script setup lang="ts">`). The backend needs **no** changes.

### 2.1 Primary root cause — subspan logs have no scroll container / no follow directive

The step detail template (`PipelineView.vue:88-119`) renders two log regions:

- **`PipelineView.vue:89-96`** — a scrollable `v-follow-logs` container, but it only
  shows logs **directly** attached to the step span:
  ```html
  <div v-if="logsForSpan(step.span).length > 0" v-follow-logs class="logs">
    <template v-for="(log, i) in logsForSpan(step.span)" :key="`s-${i}`"> ...
  ```
  `logsForSpan` (`PipelineView.vue:390-393`) returns only
  `logsBySpan.get(node.span_id)` — i.e. log records whose `span_id` equals the
  step span's own id.

- **`PipelineView.vue:97-117`** — the `.subspans` block, which renders each
  sub-span's logs **inline** inside `.subspan-block`, with **no** scroll container
  and **no** `v-follow-logs` directive:
  ```html
  <div class="subspans">
    <div v-for="s in step.subSpans" class="subspan-block" ...>
      <div class="subspan"> ...name... </div>
      <template v-for="(log, i) in logsForSubtree(s.node)" :key="`ss-${i}`">
        <div class="log-line subspan-log"> ... </div>   <!-- inline, grows the page -->
      </template>
    </div>
  </div>
  ```

In real Dagger traces the actual stdout/stderr log output is emitted on **leaf
sub-spans**, not on the top-level step span. The step's log-count badge confirms
this: it uses `stepLogCount(step)` = `logsForSubtree(step.span).length`
(`PipelineView.vue:85`, `406-408`), i.e. the **subtree** count. So a step typically
shows "N logs" on its row, but `logsForSpan(step.span)` (direct only) is empty,
the `v-follow-logs` container at line 89 is absent (`v-if` false), and the logs the
user actually reads are the inline `.subspan-log` lines at `109-114`. Those lines
are not inside any `overflow-y: auto` container and have no follow logic, so:

- There is no scroll container to scroll to the end on open → symptom 1.
- There is no follow logic to keep up with appended lines → symptom 2.

### 2.2 Secondary cause — the existing `v-follow-logs` directive is fragile

The directive (`PipelineView.vue:223-256`) is correct in principle (Vue 3's
`updated` hook fires after children update — confirmed via Vue docs), but it has
weaknesses that make follow unreliable in real conditions and that the rewrite
must fix:

- **No hysteresis.** A single threshold `FOLLOW_LOG_THRESHOLD = 8`
  (`PipelineView.vue:226`) is used for both pin and unpin. A 1px jitter near the
  bottom flips the state, and the programmatic scroll-to-bottom can fight the
  user.
- **Programmatic scroll re-enters the scroll handler.** `updated` sets
  `el.scrollTop = el.scrollHeight` (`PipelineView.vue:249-250`), which dispatches a
  `scroll` event; `followLogsOnScroll` (`228-232`) then re-evaluates `atBottom`.
  Because `scroll` dispatch is async in most browsers, this races with user
  scrolls and can momentarily unpick/re-pin.
- **No resize / layout-shift handling.** If the container height changes (window
  resize, font load, parent flex change) while pinned, the bottom position is not
  re-asserted.
- **`mounted` initial scroll can miss.** `el.scrollTop = el.scrollHeight`
  (`236`) runs at mount; the `requestAnimationFrame` re-assert (`242-246`) only
  fires once. If content arrives in the same frame after mount (e.g. the
  `v-if="logsForSpan(...).length > 0"` flips true on the first SSE refetch), the
  first paint may not have the final layout, and a single rAF is insufficient.
- **Directive is duplicated across 3 sites** (`42`, `89`, `126`) with no shared
  unit tests; the logic cannot be exercised in isolation.

### 2.3 Data flow (why no backend change is needed)

- SSE: `connectLiveTrace` (`ui/src/api/client.ts:235-239`) opens
  `GET /api/v1/traces/:id/live?token=...` → `handleTracesLive`
  (`internal/handler/traces.go:123-149`) → `liveHub.Subscribe`
  (`internal/repository/live_hub.go`). Events are lightweight refetch **signals**
  (`{type:'trace_update'}` / `{type:'logs_update'}`), debounced 300ms
  (`PipelineView.vue:337-349`).
- On a `logs_update` signal the client calls `loadLogs` (`379-388`) →
  `fetchTraceLogs` (`client.ts:187-191`) → `GET /api/v1/traces/:id/logs` →
  `queryAndWriteTraceLogs` (`internal/handler/logs.go:13-27`) which queries Loki
  (last 24h, limit 1000) and returns **all** entries. `logs.value` is replaced
  wholesale; `logsBySpan` (`261-270`) recomputes; the template re-renders.
- "Logs arriving before the view opens" is already handled: `onMounted`
  (`287-318`) calls `loadAll()` → `loadLogs()` which fetches the full 24h history
  up front. The SSE stream only triggers refetches of that same window. So there
  is no separate buffer-replay concern; the initial fetch replays history.

Conclusion: this is a **frontend-only** bug. The fix is to (a) give the subspan
log region a scrollable `v-follow-logs` container and (b) harden the directive.

## 3. Files to modify / create

| File | Action | Purpose |
|------|--------|---------|
| `ui/src/directives/followLogs.ts` | **CREATE** | Extracted, hardened, unit-testable `v-follow-logs` directive (pure DOM, no Vue import cycle). |
| `ui/src/pipeline/PipelineView.vue` | **MODIFY** | (1) Import the directive from the new module; remove the inline `vFollowLogs`/`followLogsOnScroll`/`FOLLOW_LOG_THRESHOLD` block (`223-256`). (2) Wrap the `.subspans` log output (`109-114`) in a scrollable `v-follow-logs` container so subspan logs auto-follow. (3) Keep the existing `v-follow-logs` usages at `42`, `89`, `126` (now using the imported directive). (4) Add CSS for the new subspan log scroll container. |
| `ui/src/directives/followLogs.test.ts` | **CREATE** (optional but recommended) | Vitest + jsdom unit tests for the directive's scroll math, pin/unpin hysteresis, programmatic-scroll guard, and resize handling. |
| `ui/package.json` | **MODIFY** (only if the optional tests are added) | Add `vitest`, `jsdom`, `@vue/test-utils` as **devDependencies** and a `test` script. Justification: the project has no frontend test runner today; the scroll math is the exact locus of this bug and is unsafe to ship without coverage. This is a dev-only addition; no runtime/bundle dependency changes. |
| `docs/README.md` | **MODIFY** | Update the "Log viewer" section (`~1117-1123`) to document the auto-follow / stick-to-bottom behavior and the unpin/re-pin UX. |
| `docs/design/ADR-020-log-autofollow-ux.md` | **CREATE** | ADR for the auto-follow UX decision (hysteresis thresholds, subspan scroll container, programmatic-scroll guard). |
| `docs/design/index.md` | **MODIFY** | Append row `020 | [Log auto-follow UX](ADR-020-log-autofollow-ux.md)` to the ADR table. |

**No Go files change.** `config/config.app.yaml.sample` is unchanged (no new
config keys). `internal/handler/*` is unchanged. The embedded `ui-dist` is
regenerated by the Docker build (see §9).

## 4. Data structures and function signatures

### 4.1 `ui/src/directives/followLogs.ts` (TypeScript, Vue 3 directive)

```ts
import type { Directive } from 'vue'

// Hysteresis: distance-from-bottom (px) at which the view is considered "pinned".
// PIN_THRESHOLD re-pins when the user scrolls back near the bottom; UNPIN_THRESHOLD
// is the larger value above which a scroll-up is treated as "user left the bottom".
// Unpin must be > pin so a tiny jitter near the bottom does not flap the state.
export const FOLLOW_PIN_THRESHOLD = 4    // re-pin when within 4px of bottom
export const FOLLOW_UNPIN_THRESHOLD = 24 // unpin once user is >24px from bottom

export interface FollowLogsState {
  pinned: boolean
  programmatic: boolean // true while a directive-driven scroll is in flight
  raf: number | null
  ro: ResizeObserver | null
}

function readState(el: HTMLElement): FollowLogsState {
  // Stored on the element so multiple directive instances are independent.
  return (el.__followLogs as FollowLogsState) ?? { pinned: true, programmatic: false, raf: null, ro: null }
}
function writeState(el: HTMLElement, s: FollowLogsState): void {
  el.__followLogs = s
}

function distanceFromBottom(el: HTMLElement): number {
  return el.scrollHeight - el.scrollTop - el.clientHeight
}

function scrollToBottom(el: HTMLElement): void {
  const s = readState(el)
  s.programmatic = true
  el.scrollTop = el.scrollHeight
  // Clear the guard on the next frame, after the async scroll event has fired.
  requestAnimationFrame(() => {
    const cur = readState(el)
    cur.programmatic = false
    writeState(el, cur)
  })
  writeState(el, s)
}

function onScroll(e: Event): void {
  const el = e.currentTarget as HTMLElement
  const s = readState(el)
  if (s.programmatic) return // ignore scrolls we triggered ourselves
  const d = distanceFromBottom(el)
  if (s.pinned && d > FOLLOW_UNPIN_THRESHOLD) s.pinned = false
  else if (!s.pinned && d <= FOLLOW_PIN_THRESHOLD) s.pinned = true
  writeState(el, s)
}

export const vFollowLogs: Directive<HTMLElement> = {
  mounted(el: HTMLElement) {
    const s: FollowLogsState = { pinned: true, programmatic: false, raf: null, ro: null }
    writeState(el, s)
    el.addEventListener('scroll', onScroll, { passive: true })

    // Re-assert bottom on container resize (window resize, font load, flex change).
    if (typeof ResizeObserver !== 'undefined') {
      s.ro = new ResizeObserver(() => {
        const cur = readState(el)
        if (cur.pinned) scrollToBottom(el)
      })
      s.ro.observe(el)
    }

    // Scroll to end now, then again after paint (layout may not be final at mount),
    // and once more on the next frame to cover late v-for content / font swap.
    scrollToBottom(el)
    s.raf = requestAnimationFrame(() => {
      if (readState(el).pinned) scrollToBottom(el)
    })
  },
  updated(el: HTMLElement) {
    if (readState(el).pinned) scrollToBottom(el)
  },
  unmounted(el: HTMLElement) {
    const s = readState(el)
    el.removeEventListener('scroll', onScroll)
    if (s.raf !== null) cancelAnimationFrame(s.raf)
    s.ro?.disconnect()
    el.__followLogs = undefined
  },
}

// Augment HTMLElement for the per-element state field (module-private).
declare module 'vue' {
  interface ComponentCustomProperties {}
}
interface HTMLElement {
  __followLogs?: FollowLogsState
}
```

Notes on conventions:
- No `+` string concatenation; template literals only (TS equivalent of the Go
  `fmt.Sprintf` rule for the JS layer).
- No new runtime dependencies. `ResizeObserver` is guarded for older browsers.
- The directive is a named export `vFollowLogs` so `<script setup>` can import it
  and auto-register it as `v-follow-logs` (Vue 3 convention: a binding named
  `vFollowLogs` is exposed as `v-follow-logs`).

### 4.2 `PipelineView.vue` template change (subspan logs)

Replace `PipelineView.vue:97-117` so the subspan logs render inside a single
scrollable `v-follow-logs` container (preserving the per-subspan indentation via
padding on each `.subspan-block`, but moving the log lines into the scroll
container). Concretely, wrap the `.subspans` content:

```html
<div class="subspans">
  <div v-if="step.subSpans.length === 0" class="empty">No sub-spans</div>
  <div v-else v-follow-logs class="logs subspan-logs">
    <template v-for="s in step.subSpans" :key="s.node.span_id">
      <div class="subspan-block" :style="{ paddingLeft: (12 + s.depth * 16) + 'px' }">
        <div class="subspan">
          <span :class="['dot', `dot-${s.node.status}`]"></span>
          <span class="subspan-name">{{ s.node.name }}</span>
          <span class="subspan-duration">{{ formatDuration(liveSpanDuration(s.node)) }}</span>
        </div>
        <template v-for="(log, i) in logsForSubtree(s.node)" :key="`ss-${i}`">
          <div v-if="logText(log.line) !== null" class="log-line subspan-log">
            <span class="log-ts">{{ formatTime(log.timestamp) }}</span>
            <span class="log-msg">{{ logText(log.line) }}</span>
          </div>
        </template>
      </div>
    </template>
  </div>
</div>
```

This puts **all** subspan logs for the step into one scroll container that
auto-follows, which matches the user's mental model ("the step's logs"). The
existing direct-span container at `89-96` is kept (it handles the rare case where
the step span itself carries logs); when both are present they each follow
independently.

### 4.3 `PipelineView.vue` script change

- Remove `FOLLOW_LOG_THRESHOLD`, `followLogsOnScroll`, and the inline
  `vFollowLogs` directive (`PipelineView.vue:223-256`).
- Add `import { vFollowLogs } from '@/directives/followLogs'` near the top of
  `<script setup>` (after the existing imports, ~`161-164`). In `<script setup>`,
  an imported binding named `vFollowLogs` is automatically available as
  `v-follow-logs` in the template — no `directives: { ... }` registration needed.

### 4.4 CSS additions (`PipelineView.vue` `<style scoped>`)

Add (alongside the existing `.logs` rule at `914-922`):

```css
.subspan-logs {
  max-height: 500px;
  overflow-y: auto;
}
```

The existing `.subspan-log { padding-left: 24px; }` (`930-932`) is preserved so
log lines stay indented under their subspan header inside the new container.

## 5. Frontend behavior spec (detailed)

1. **Scroll-to-end on view open.** When a `v-follow-logs` container mounts (step
   expanded, or first log arrives making `v-if` true), `mounted` calls
   `scrollToBottom` synchronously, again in `requestAnimationFrame`, and the
   `ResizeObserver` re-asserts if layout shifts during the same frame. The view
   is at the end before the user sees it.
2. **Stick-to-bottom while pinned.** `updated` fires after every reactive patch
   that appends log lines (Vue 3 guarantees children are patched before
   `updated`). If `pinned` is true, `scrollToBottom` runs. New lines keep the
   view at the end.
3. **Unpin on user scroll-up.** `onScroll` (passive) computes
   `distanceFromBottom`. While `pinned` and the user scrolls so that distance
   exceeds `FOLLOW_UNPIN_THRESHOLD` (24px), `pinned` becomes false. Subsequent
   `updated` calls do not scroll.
4. **Re-pin on scroll-to-bottom (hysteresis).** While unpinned and the user
   scrolls so that distance is ≤ `FOLLOW_PIN_THRESHOLD` (4px), `pinned` becomes
   true again. The next `updated` (next log arrival) resumes following. The
   4px/24px gap prevents flap from sub-pixel jitter.
5. **Programmatic-scroll guard.** `scrollToBottom` sets `programmatic = true`
   before assigning `scrollTop` and clears it on the next frame. `onScroll`
   ignores events while `programmatic` is true, so the directive's own scrolls
   never change the pinned state.
6. **Rapid bursts.** The SSE path already debounces refetches to 300ms
   (`PipelineView.vue:344-349`) and each refetch replaces `logs.value` wholesale,
   so `updated` fires once per burst (not per line). `scrollToBottom` is O(1)
   (single `scrollTop` assignment). No per-line scheduling is needed.
7. **Container resize.** `ResizeObserver` watches the container; if `pinned`,
   it re-asserts bottom on height changes (window resize, font load, parent
   flex changes). `unmounted` disconnects the observer.
8. **DOM/SSE specifics.** The directive is element-scoped (state on
   `el.__followLogs`), so the three `v-follow-logs` sites (services `42`, step
   direct `89`, step subspans new, unmatched `126`) are fully independent. The
   `scroll` listener is `{ passive: true }` to avoid blocking scroll jank.
9. **Element re-creation.** Collapsing/re-expanding a step unmounts the
   container (`unmounted` cleans up) and remounts it (`mounted` re-pins and
   scrolls to end). `recomputeSteps` (`633-641`) preserves `step.expanded`
   across refetches via `:key="step.span.span_id"`, so the container is **not**
   remounted on every SSE refetch — only on explicit collapse/expand. This
   preserves the user's scroll/pin state during streaming.

## 6. Edge cases

- **Empty logs.** The `v-if` guards (`logsForSpan(...).length > 0` at `89`; the
  new `v-else` for subspans) mean the container is not created when there is
  nothing to show; the "No logs for this step" / "No sub-spans" empty states
  render instead. No scroll needed.
- **Logs arrive before view opens (buffer replay).** `onMounted` → `loadLogs`
  fetches the full 24h window up front; the container mounts with all history
  already present and `mounted` scrolls to the end. SSE only triggers refetches
  of the same window. No special replay path required.
- **Tab switch / visibility change.** `onVisibilityChange` (`333-335`) already
  refreshes `now` for duration ticking. Background tabs throttle `setInterval`
  but do not pause SSE; on return, the next `logs_update` refetch fires
  `updated`, which scrolls to bottom if still pinned. If the user had pinned
  and switched away, they return to a bottom-pinned view. No extra work needed.
- **Very high log rates.** Debounce (300ms) + full-window refetch means
  `updated` fires at most ~3×/s. `scrollToBottom` is a single property write;
  the browser handles coalescing. `passive` scroll listeners avoid jank. The
  1000-line Loki cap (`logs.go:17`) bounds DOM size.
- **Element re-creation.** Covered in §5.9: collapse/expand remounts cleanly;
  refetches do not remount (stable `:key`).
- **Scroll jank avoidance.** `passive` listeners; no layout thrash (we only
  read `scrollHeight`/`scrollTop`/`clientHeight` and write `scrollTop` once per
  update); `ResizeObserver` callback only writes when `pinned`.
- **Direct-span logs + subspan logs both present.** Two independent
  `v-follow-logs` containers, each follows independently. Acceptable: the
  direct-span container is short (rare in Dagger) and sits above the subspan
  container.
- **Reduced-motion / accessibility.** No animation is introduced; scrolling is
  instantaneous. No `prefers-reduced-motion` handling needed.

## 7. Error handling and validation

- **Directive robustness.** `ResizeObserver` existence is guarded
  (`typeof ResizeObserver !== 'undefined'`); older browsers degrade to
  mount/updated-only scrolling (still correct, just no resize re-assert).
- **No exceptions thrown from `onScroll`** — all reads are on `HTMLElement`
  properties; the `programmatic` guard prevents feedback loops.
- **Type safety.** `__followLogs` is typed via interface augmentation; the
  directive is `Directive<HTMLElement>`. `vue-tsc --noEmit` (existing
  `typecheck` script, `package.json:10`) must pass.
- **No backend validation** — no API contract change. `fetchTraceLogs`
  (`client.ts:187-191`) already tolerates `data?.entries ?? []`.

## 8. Test plan

### 8.1 Unit tests (frontend, optional but recommended)

Add `ui/src/directives/followLogs.test.ts` using Vitest + jsdom (dev-only deps).
Table-driven, stdlib-style assertions (no testify equivalent in JS; use
Vitest's `expect`). Cases:

| Case | Setup | Expected |
|------|-------|----------|
| `mounted` scrolls to end | container `max-height:100px`, 1000px content | `scrollTop === scrollHeight - clientHeight` |
| `updated` follows when pinned | append child, call `updated` | `scrollTop` at bottom |
| `updated` does NOT follow when unpinned | set `__followLogs.pinned=false`, append, `updated` | `scrollTop` unchanged |
| unpin on scroll-up | pinned, dispatch scroll with `scrollTop` 50px above bottom | `pinned === false` |
| re-pin near bottom (hysteresis) | unpinned, scroll to within 4px | `pinned === true` |
| no re-pin in the 4–24px gap | unpinned, scroll to 15px from bottom | `pinned === false` |
| programmatic scroll ignored | `scrollToBottom` then dispatch scroll | `pinned` unchanged |
| `unmounted` cleans up | mount then unmount | `__followLogs === undefined`, no listener (scroll dispatch does not throw) |
| ResizeObserver re-asserts | pinned, trigger RO callback | `scrollTop` at bottom |

Run: `cd ui && npm test`.

### 8.2 Go tests

**None required.** No Go files change. Existing handler tests
(`internal/handler/*_test.go`) and the SSE broadcast tests
(`internal/handler/otel_broadcast_test.go`,
`internal/repository/telemetry_test.go`) are unaffected. Run `go test ./...`
to confirm no regression (the `//go:embed all:ui-dist` requires `ui/dist` to
exist; it already does and is regenerated by the Docker build).

### 8.3 Integration tests (`tests/integration/`)

No new integration test is required for a pure-CSS/JS scroll behavior (the
integration suite proves the Dagger client API contract, not browser UX). The
existing trace/logs endpoints remain covered. If a smoke test is desired, add a
manual step to the runbook instead (§8.4).

### 8.4 Manual verification (mandatory per AGENTS.local.md §6)

After deploying to the `home` cluster (`dagger-cache-test` release):

1. Open `https://dagger.home.webcenter.fr/pipelines`, pick a **running** pipeline.
2. Expand a step whose logs live on subspans (the common case — badge shows
   "N logs"). Confirm the subspan log view opens **scrolled to the end**.
3. Keep the step open; confirm new log lines keep the view pinned to the bottom
   as they stream in.
4. Scroll up ~30px; confirm the view stops following (unpin).
5. Scroll back to the very bottom; confirm following resumes on the next log
   arrival (re-pin).
6. Collapse and re-expand the step; confirm it re-opens scrolled to the end.
7. Resize the browser window while pinned; confirm the view stays at the
   bottom.
8. Repeat 2–7 for the "Services" log view and the "Unmatched logs" view to
   confirm the hardened directive did not regress them.
9. Switch tabs away for 30s during a run, return; confirm the view is at the
   bottom (pinned) and resumes following.

## 9. Documentation updates (per AGENTS.md)

- **`docs/README.md`** — in the "Log viewer" bullet (`~1117-1123`), add:
  > Each log container auto-scrolls to the end when opened and sticks to the
  > bottom while new lines stream in; scrolling up unpins, scrolling back to the
  > bottom re-pins (with a small hysteresis so jitter does not flap the state).
- **`docs/design/ADR-020-log-autofollow-ux.md`** — new ADR: context (subspan logs
  had no scroll container; directive lacked hysteresis/resize/programmatic
  guard), decision (single `v-follow-logs` scroll container per log region,
  hysteresis 4px/24px, `ResizeObserver`, programmatic-scroll guard), alternatives
  (per-subspan containers — rejected: fragmented UX; CSS `scroll-align` — not
  widely supported for dynamic content), consequences.
- **`docs/design/index.md`** — append the ADR-020 row to the table.
- **`config/config.app.yaml.sample`** — **no change** (no new config keys).

## 10. Step-by-step implementation checklist (incremental verification)

1. **Create the directive module.**
   - `ui/src/directives/followLogs.ts` per §4.1.
2. **Wire it into the component.**
   - `ui/src/pipeline/PipelineView.vue`: add the import; remove the inline
     directive block (`223-256`); confirm `v-follow-logs` still resolves.
   - `cd ui && npm run typecheck` → must pass.
3. **Restructure the subspan log template.**
   - Apply the §4.2 template change (wrap subspan logs in
     `<div v-follow-logs class="logs subspan-logs">`).
   - Add the `.subspan-logs` CSS (§4.4).
   - `cd ui && npm run typecheck` → must pass.
4. **(Optional) Add unit tests.**
   - Add `vitest`, `jsdom`, `@vue/test-utils` to `ui/package.json` devDependencies;
     add `"test": "vitest run"` script.
   - `ui/src/directives/followLogs.test.ts` per §8.1.
   - `cd ui && npm test` → all cases green.
5. **Build the UI.**
   - `cd ui && npm run build` → regenerates `ui/dist` (consumed by
     `//go:embed all:ui-dist` in `internal/handler/ui.go`).
6. **Verify the Go build still embeds.**
   - `go build ./...` → succeeds (embed picks up the new `ui/dist`).
   - `go test ./...` → no regressions (no Go changes expected to fail).
7. **Update docs.**
   - `docs/README.md` log-viewer bullet; create `ADR-020-log-autofollow-ux.md`;
     append to `docs/design/index.md`.
8. **Build the container image (includes the UI).**
   - `docker build -t docker.io/disaster/dagger-kubernetes:dev .`
9. **Push.**
   - `docker push docker.io/disaster/dagger-kubernetes:dev`
10. **Capture live Helm values (do not skip — see AGENTS.local.md §4.3).**
    - `helm --kubeconfig /home/user/.kube/home get values dagger-cache-test -n dagger-cache-test -o yaml > /tmp/dagger-cache-test.values.yaml`
11. **Upgrade the release (mandatory `raft.replicas=1`).**
    - `helm --kubeconfig /home/user/.kube/home upgrade --install dagger-cache-test ./deploy/helm/dagger-kubernetes -n dagger-cache-test -f /tmp/dagger-cache-test.values.yaml --set supervisor.config.raft.replicas=1 --set supervisor.image.tag=dev --set supervisor.image.pullPolicy=Always --set supervisor.image.repository=docker.io/disaster/dagger-kubernetes`
12. **Force rollout (dev tag is mutable).**
    - `kubectl --kubeconfig /home/user/.kube/home -n dagger-cache-test rollout restart statefulset/dagger-cache-test-dagger-kubernetes`
    - `kubectl --kubeconfig /home/user/.kube/home -n dagger-cache-test rollout status statefulset/dagger-cache-test-dagger-kubernetes --timeout=300s`
13. **Manual verification** on `https://dagger.home.webcenter.fr` per §8.4
    (agent verifies, then human confirms per AGENTS.local.md §6).

## 11. Open questions / out of scope

- **Per-subspan vs single step container.** This plan puts all subspan logs for a
  step in one scroll container (matches "the step's logs" mental model). If the
  team prefers per-subspan scroll containers (one per `.subspan-block`), that is a
  UX call — flagged for the human reviewer during §8.4. The directive supports
  either; only the template wrapping changes.
- **Frontend test runner introduction.** Adding Vitest is a dev-only dependency
  addition. AGENTS.md says "no new dependencies unless required and justified."
  The justification is that the scroll math is the exact locus of this bug and is
  unsafe to leave untested. If the team declines, drop §4 step 4 and §8.1 and
  rely on §8.4 manual verification only.
- **No backend / config / API changes** — explicitly out of scope.
