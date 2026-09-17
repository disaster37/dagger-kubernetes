# ADR-038: Pipeline tree drill-down, zoom, and subtree-scoped log search

- **Status:** accepted
- **Date:** 2026-09-17
- **Deciders:** dagger-kubernetes maintainers

## Context

The pipeline view (`/pipelines/<id>`, `ui/src/pipeline/PipelineView.vue`) rendered
Dagger spans as a flat, two-level list: "steps" (top-level spans) with sub-spans
flattened inline (indented by depth). There was no way to navigate the Dagger
code/function call hierarchy as a *tree*, no way to "zoom" into a level and
descend to leaves, and no log search. The whole-trace log fetch was capped at
1000 lines, so a search could never reach beyond that window.

Issue #21 asks for: a tree that mirrors the Dagger function nesting; at the top
level the main levels with **all** logs of their sub-levels aggregated; **zoom /
drill-down** into a level (breadcrumb navigation) down to leaf steps; and
**keyword + regex log search** scoped to the currently displayed level (node +
descendants), paging beyond the 1000-line cap.

## Decision

### 1. The backend already serves the full span tree — no tree-building change

`GET /api/v1/traces/:id` returns `domain.TraceInfo.RootSpan` with
`SpanNode.Children`/`ParentSpanID`, built by
`repository.SpanTreeReconstructor.reconstruct`. The feature is therefore a UI
drill-down plus one new server-side search endpoint.

### 2. Server-side, subtree-scoped search endpoint

`GET /api/v1/traces/:traceID/search` resolves the focused node's descendant span
IDs from the reconstructed tree, applies keyword/regex matching in Go, and pages
the results.

**Why server-side.** The search scope is a *subtree* (node + all descendants),
which Loki's `span_id` label cannot express without walking the span tree — so
the supervisor (which owns the reconstructed tree) is the right place. Text
matching stays in Go `regexp`/`strings` (RE2-safe) rather than LogQL line
filters, avoiding LogQL escaping pitfalls and reusing the existing `LogsClient`
Loki fetch.

**Scoping rule (`service.LevelSpanIDs`).** The focused node's subtree is walked
and non-internal span IDs are collected. Internal spans
(`dagger.io/ui.internal` or the name rules from `ci_steps.go`) are excluded, but
their non-internal descendants are kept — mirroring the UI's log-ownership
rules. `focus == ""` or `focus == root.SpanID` selects the whole trace (returns
`nil`, meaning "no span filter"). A focus not present in the tree yields
`found=false` → HTTP 404. The walk is depth-capped at `ciMaxAbsoluteDepth`
(1024) to bound recursion (CWE-674).

**Request.** `GET /api/v1/traces/:traceID/search?span_id=<base64>&q=<text>&mode=contains|regex&limit=<n>&cursor=<unixns>`.
All query params are optional. `mode` defaults to `contains`; anything else is
400. `limit` defaults to 500 and is capped at 2000. The window defaults to the
last 24h.

**Response.** `{"entries":[{"timestamp":"...","line":"...","span_id":"..."}],"next":<unixns>,"counts":{"<span_id>":<n>}}`.
`next` is omitted (0) when Loki is exhausted. `counts` maps each span ID to the
number of matching logs in the scanned window and is returned **only on the
first page** (`cursor == 0`); load-more pages omit it. The client rolls the raw
per-span counts up to the visible tree rows (see §4).

**RE2 safety.** Go's `regexp` is RE2 (linear-time, no catastrophic
backtracking), so the server is authoritative and safe. An invalid pattern is
reported as `domain.ErrInvalidRegex` and mapped to HTTP 400. The client
pre-validates `new RegExp(query)` and shows an inline error without calling the
API; the client-side regex only re-runs over the already-limited (≤ page size)
server results, so a pathological JS regex is a low-risk, best-effort display
path.

### 3. Cursor (not offset) pagination

Loki returns forward-sorted entries; a timestamp cursor is the natural,
stateless pager. `SearchTraceLogs` queries Loki forward in batches of
`logSearchBatch` (1000) raw entries, sorts each batch ascending (the per-stream
append is not globally sorted), filters by span set + text, and returns a
`next` cursor of `lastExaminedTimestamp + 1ns`. It stops when the page is full,
when Loki returns a short batch (exhausted), or after `logSearchMaxScan` (5000)
raw entries scanned per call (CWE-400).

On the first page (`IncludeCounts`) the scan keeps going past a full page — up
to the same `logSearchMaxScan` bound — to count matching logs per span, while
still returning only `limit` entries and anchoring the cursor at the last
*returned* entry. Load-more pages keep the old fast path (stop as soon as the
page fills) and omit `counts`.

**Boundary limitation (documented).** If more than `limit` entries share the
exact final nanosecond returned in a page, the overflow beyond the page is
picked up by the next cursor only at `lastExaminedTimestamp+1ns`, so
same-nanosecond surplus is dropped. In practice Dagger log lines carry distinct
Loki timestamps; this is the accepted, stateless pager trade-off.

### 4. UI: drill-down tree + breadcrumb + aggregated log panel

- `ui/src/pipeline/spanTree.ts` holds the shared, pure span-visibility helpers
  (`attrBool`, `isInternalSpanName`, `isInternalSpan`, `isHiddenSpan`,
  `isTransparentSpan`, `visibleChildren`, `flattenVisible`) plus `logText` and
  `highlightSegments`, so `PipelineView` (services/ownership) and `StepTree`
  share one truth.
- `StepTree.vue` keeps `focusPath` (breadcrumb root→focus) and an `expanded`
  set. The list is `visibleChildren(focus)`; a chevron toggles in-place
  expansion, a name click zooms in (pushes onto `focusPath`). Breadcrumb crumbs
  zoom back out.
- `LogPanel.vue` is presentational: it renders the aggregated log page for the
  current focus, a debounced search box, a contains/regex toggle, match count,
  and a "Load more" button. It fetches `/search` with `span_id = focus.span_id`
  (omitted for the root = whole trace).
- **Log → step.** Each log line carries a clickable step badge resolved from
  `entry.span_id` via `computeRowOwners(focus)` (the same internal/transparent
  ownership rules as the tree). Clicking a badge scrolls to and briefly flashes
  the owning row. Logs that cannot be attributed (internal span, empty/unknown
  span, span outside the focus subtree) render a muted "unattributed" badge.
- **Step → logs.** The first search page returns per-span matching counts; the
  client rolls them up to the visible rows and renders a count badge per row.
  Clicking a count badge zooms into that step (the breadcrumb zooms back out).
  Counts are query-aware and consistent with the panel (same server-side match).
- **XSS.** Log text is rendered via `{{ }}` interpolation only; highlight uses
  `<mark>` + text segments, never `v-html`. Badge text is likewise interpolated.

## Alternatives considered

- **Pure client-side search + in-memory paging.** Rejected: capped at the
  1000-line whole-trace fetch, cannot search beyond it.
- **Push span/text filters into LogQL** (`span_id=~"hex1|hex2|..."` +
  `|=`/`|~`). Rejected: label-regex escaping, span-ID base64→hex round-trip, and
  URL length limits for large subtrees; supervisor-side filtering is simpler and
  reuses existing code.
- **Keep the old Steps card alongside the new tree.** Rejected: one cohesive
  tree view avoids duplicated log rendering.
- **Offset-based pagination.** Rejected: Loki has no stable global offset; a
  timestamp cursor is the only reliable forward pager.

## Consequences

- The pipeline view mirrors the Dagger function nesting and can drill to leaves.
- Logs and steps are linked in both directions: log lines carry a step badge
  that scrolls to the owning row, and step rows carry a matching-log count badge
  that zooms in. Un-attributable logs are labeled "unattributed" inline; the
  separate "Unmatched / general logs" card is gone.
- Search is server-side and pages beyond the 1000-line cap; the Services card
  still uses the whole-trace fetch (unchanged).
- Search matches the raw Loki JSON line (not the UI's `logText`-decoded body);
  JSON escaping (`\n`, quotes) can cause rare contains/regex misses. Accepted
  for v1.
- Live search during streaming is kept simple (refresh on `logs_update`/poll);
  a result may lag a few seconds behind a just-ingested line.

## Open questions

- Whether to also page the Services card through `/search` (out of scope for
  #21). The former Unmatched card was removed: un-attributable logs now render
  inline in the panel with an "unattributed" badge, so there is nothing left to
  page separately.
- Whether to match the decoded log body instead of the raw JSON line (would
  require decoding server-side).