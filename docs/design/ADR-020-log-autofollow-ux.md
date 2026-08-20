# ADR-020: Log auto-follow UX — pinned-to-bottom log containers with hysteresis

- **Status:** accepted
- **Date:** 2026-08-19
- **Deciders:** dagger-cache maintainers

## Context

The live pipeline viewer (`/pipelines/:id`, `ui/src/pipeline/PipelineView.vue`)
renders a step's log output in two regions: logs attached **directly** to the
step span (rare in Dagger) and logs attached to the step's **leaf sub-spans**
(the common case — the step's "N logs" badge counts the whole subtree).

The sub-span logs were rendered **inline** inside each `.subspan-block`, with no
`overflow-y: auto` container and no follow directive, so:

- Opening a step's log view did not scroll to the end; the user had to scroll
  down manually.
- While pinned at the bottom, incoming log lines did not keep the view at the
  end (no stick-to-bottom).

The existing inline `v-follow-logs` directive (a single 8px threshold) had
further weaknesses: no hysteresis (1px jitter near the bottom flaps pin state),
programmatic scrolls re-entered the scroll handler (racing user scrolls), no
resize/layout-shift re-assert, and a single rAF re-assert that could miss
same-frame content. The directive was also duplicated across three template
sites with no shared, testable module.

The backend needs no changes: `onMounted` already fetches the full 24h log
window up front, and SSE only triggers refetches of that same window.

## Decision

1. **Single `v-follow-logs` scroll container per log region.** All of a step's
   sub-span logs render inside one scrollable `<div v-follow-logs class="logs">`
   (per-subspan indentation preserved via `paddingLeft` on each
   `.subspan-block`). This matches the user's mental model of "the step's logs".
   The direct-span container and the services/unmatched containers keep
   following independently.

2. **Extract and harden the directive** (`ui/src/directives/followLogs.ts`,
   exported as `vFollowLogs`, no runtime dependencies):

   - **Hysteresis:** `FOLLOW_PIN_THRESHOLD = 4px` re-pins when the user scrolls
     back near the bottom; `FOLLOW_UNPIN_THRESHOLD = 24px` unpins once the user
     is >24px from the bottom. Unpin > pin so sub-pixel jitter does not flap the
     state.
   - **Programmatic-scroll guard:** `scrollToBottom` sets `programmatic = true`
     before assigning `scrollTop` and clears it on the next frame; the passive
     `scroll` handler ignores events it triggered itself, so the directive's own
     scrolls never change the pinned state.
   - **Resize/layout-shift handling:** a guarded `ResizeObserver` re-asserts the
     bottom when the container resizes while pinned (window resize, font load,
     parent flex change).
   - **Mount re-assert:** `mounted` scrolls synchronously, then again on the
     next frame to cover late `v-for` content / font swap.
   - **Per-element state:** state lives on `el.__followLogs`, so multiple
     directive instances stay independent; `unmounted` removes the listener,
     cancels the rAF, disconnects the observer, and clears the state.

3. **Element re-creation semantics.** Collapse/expand unmounts and remounts the
   container (clean re-pin + scroll-to-end). Refetches preserve `step.expanded`
   via stable `:key="step.span.span_id"`, so the container is **not** remounted
   on every SSE refetch — the user's scroll/pin state survives streaming.

## Alternatives considered

- **Per-subspan scroll containers** (one per `.subspan-block`): rejected — the
  user thinks of "the step's logs", not N separate scroll areas; fragmented UX
  and N× the follow state.
- **CSS `scroll-snap` / `scroll-align` to bottom:** not widely supported for
  dynamic, streaming content; unreliable across browsers.
- **Keep the inline directive and only wrap the template:** insufficient — the
  inline directive lacked hysteresis, the programmatic guard, and resize
  handling, all of which are the locus of the flakiness.

## Consequences

- Opening a step's log view lands at the end immediately; streaming lines keep it
  pinned until the user scrolls up (>24px), and it re-pins when they scroll back
  to within 4px of the bottom.
- The directive is now a single shared module, element-scoped and side-effect
  clean (listeners/observers/rAF all torn down on unmount).
- No backend, config, or API changes; `config/config.app.yaml.sample` unchanged.
- Older browsers without `ResizeObserver` degrade gracefully to mount/updated
  scrolling only (still correct, no resize re-assert).
- The frontend has no test runner today; scroll math coverage is deferred to
  manual verification (see the runbook in AGENTS.local.md §6).
