# ADR-044: Pipelines overview updates live via a sentinel-topic SSE stream

- **Status:** accepted
- **Date:** 2026-09-26
- **Deciders:** dagger-kubernetes maintainers
- **Resolves:** issue #41 ("fix - dynamic view of pipelines overview")

## Context

The `/pipelines` overview page (the list of ALL pipelines) was not dynamic for
new pipelines. It fetched once on mount and kept a 10s poll **only while some
row was `running`**. When nothing was running — the steady state of an idle
platform — no poll fired, so a brand-new pipeline never appeared until the user
manually reloaded the page. Status transitions of running rows were picked up
only by that conditional poll, adding up to 10s of lag even when a live
mechanism existed for the per-trace view.

The per-trace pipeline view already streams: `GET /api/v1/traces/:id/live` is
an SSE endpoint that subscribes to the `repository.LiveHub` and receives
`trace_update`/`logs_update` re-fetch hints whenever OTLP ingest touches that
trace (ADR-003, ADR-041). The overview needed the same immediacy at the
list level.

## Decision

### 1. Reuse the per-trace `LiveHub` with a sentinel subscription key

The hub's key is an arbitrary string (`clients map[string]map[*LiveClient]bool`),
currently a trace ID. A fixed sentinel constant, `domain.PipelinesTopic =
"__pipelines__"`, turns the same hub into a list-level fan-out with **zero
structural change**: per-trace keys are hex Dagger trace IDs, so the non-hex
literal cannot collide.

New endpoint: **`GET /api/v1/traces/live`** → `handleTracesListLive` — the
per-trace handler minus the trace lookup: authenticate via
`requireAuthWithQueryFallback` (EventSource cannot set headers, so the
`?token=` fallback applies here too), set the SSE headers, subscribe on
`domain.PipelinesTopic`, block on `ctx.Done()`/`client.Done()`, unsubscribe.

There is deliberately **no trace-level authorization** on this route: the
stream carries no data — only `{"type":"pipelines_update"}` — and the client's
subsequent `GET /api/v1/traces` is identity-scoped server-side, so the stream
leaks nothing beyond "something changed". Static-before-param route resolution
in Hertz makes `/api/v1/traces/live` coexist with `/api/v1/traces/:traceID`
(the same precedent as `/api/v1/cli/versions/latest` vs `/api/v1/cli/:version`).

### 2. Event-driven broadcast at the two choke points (no periodic diff)

`{"type":"pipelines_update"}` is broadcast to the sentinel topic from the two
places that already know the list changed:

1. **`broadcastOTelUpdate("traces", body)`** — every OTLP trace batch implies a
   new pipeline or a status/duration change. One broadcast per batch (not per
   trace ID). `logs`/`metrics` ingest do not touch the list and do not
   broadcast.
2. **`PipelineLifecycle.markFailed`** — disconnect detection and the stale
   sweeper mark a trace `failed` outside OTLP ingest; the overview must reflect
   that immediately too (alongside the existing per-trace `trace_update`).

The payload is a stateless "re-fetch" hint: the client re-runs
`GET /api/v1/traces` with its current filters (debounced ~300ms so a burst
collapses into one request). No timer, no store walk, no diffing.

### 3. Routing stays leader-pinned for free

`leaderPinnedRoute` already pins every path with prefix `/api/v1/traces/` and
suffix `/live` (ADR-041), so `/api/v1/traces/live` is forwarded to the Raft
leader by followers without a rule change. OTLP ingest (POST) and the
`markFailed` Raft write are leader-side as well, so producers and subscribers
live on the same pod — consistent with ADR-041's "events produced only on the
leader". Followers stream the pinned SSE hop through the existing incremental
forward proxy (`TestLeaderForwardStreamsSSE`).

### 4. Connection lifecycle reuses the hub unchanged

`writePump` keeps the stream healthy (30s `WriteKeepAlive` under nginx's
`proxy_read_timeout`), `Broadcast` is non-blocking (a full 256-buffer skips the
slow subscriber, never the producer), and `Unsubscribe` deletes the topic map
entry and closes `Send`, so no client or goroutine leaks. The `EventSource`
auto-reconnects; a burst after reconnect debounces to one re-fetch.

## Alternatives rejected

- **Aggressive unconditional polling** (e.g. always-on 2–5s poll): exactly the
  failure mode being fixed (the poll existed but only while a row was running),
  and a fixed aggressive poll wastes load across the whole user base for the
  rare "new pipeline appeared" event. The 10s conditional poll is **kept** as a
  resilience fallback, mirroring the detail view's 5s fallback.
- **WebSocket**: bidirectional capability is unneeded (server → client hint
  only); AGENTS.md mandates Hertz-native SSE for server push.
- **Per-topic hub or new pub/sub component**: would duplicate the tested
  subscribe/broadcast/writePump machinery for one extra key. The sentinel key
  gets the same semantics for free.
- **Pushing the full list in the event**: duplicates data the client already
  knows how to fetch, breaks identity scoping at the stream layer, and bloats
  events for no benefit — the list endpoint is cheap and always scoped.

## Consequences

- `/pipelines` shows new pipelines and status changes without a manual reload;
  `onUnmounted` closes the `EventSource` and clears the debounce timer.
- Two new broadcast points must stay in sync with list semantics: any future
  writer that mutates `trace_meta` list-visible fields (status, ownership,
  timestamps) should broadcast to `domain.PipelinesTopic` as well.
- `LiveHub.keepAlive` (default 30s) is a per-hub field so the `repository`
  tests can shorten the keep-alive cadence before the first Subscribe without
  touching shared package state; production behavior is unchanged.
- The integration test `TestPipelinesOverviewSSE` pins the contract:
  authenticated SSE stream + OTLP ingest → `pipelines_update` event + the
  re-fetch target reflects the new trace.
