# ADR-040: Frontend is Nuxt 4 + Nuxt UI v4 (static SPA)

- **Status:** accepted
- **Date:** 2026-09-18
- **Deciders:** dagger-kubernetes maintainers

## Context

The pipeline UI was a hand-rolled Vue 3 + Vite SPA (`ui/src/`) with a custom
GitHub-dark stylesheet, `vue-router`, and `pinia`. It was built with
`vite build` into `ui/dist/`, copied into `internal/handler/ui-dist/`, and
embedded into the Go binary via `//go:embed all:ui-dist` (ADR-001 covers only
the Go stack; the frontend stack was never recorded).

Issue #23 asked to migrate the frontend to **Nuxt 4** and **Nuxt UI v4**. The
constraints that shaped the decision:

- The Go binary embeds static files only — there is no Node server in
  production, so the UI must remain a pure static SPA.
- `internal/handler/ui.go` serves content-hashed files under `assets/` with
  `Cache-Control: public, max-age=31536000, immutable` and everything else
  (including the `index.html` shell for extension-less deep links) with
  `no-cache`. `internal/handler/ui_test.go` asserts this behavior.
- The UI and API share the same origin (same ingress), so the axios client's
  `baseURL: '/'` and httpOnly-cookie auth must be preserved exactly.

## Decision

### 1. Nuxt 4, static SPA (`ssr: false`)

The UI is a Nuxt 4 app under `ui/app/` with `ssr: false`. `npm run build` runs
`nuxt generate`, emitting a static SPA into **`ui/.output/public/`**
(`index.html`, `200.html`, `404.html`, hashed `assets/*`). The Dockerfile and
the Dagger `ui` function copy/return `.output/public/` instead of `dist/`.

### 2. `app.buildAssetsDir: '/assets/'` — no Go change

Nuxt's default hashed-asset directory is `/_nuxt/`. Pinning
`app.buildAssetsDir` to `/assets/` keeps every hashed asset under the path the
Go handler already treats as immutable, so `internal/handler/ui.go` and
`internal/handler/ui_test.go` are **unchanged**. The `index.html` shell is the
same SPA shell as `200.html`, so the existing extension-less fallback already
serves deep links (`/pipelines/<id>`, `/image-cache`, …).

### 3. Nuxt UI v4 + Tailwind v4, full component adoption

`@nuxt/ui` v4 (Tailwind v4, Reka UI) is the component layer. The global shell
uses `UApp`/`UHeader`/`UNavigationMenu`/`UMain`; views use `UCard`, `UTable`,
`UButton`, `UBadge`, `USelect`, `UInput`, `UCheckbox`, `UForm`, `UModal`,
`UAlert`, `UEmpty`, `UAccordion`/`UCollapsible`, `UBreadcrumb`, `UIcon`, and
`UDropdownMenu`. Semantic status colors map 1:1 (`success`/`error`/`info`/
`warning`/`neutral`).

Residual bespoke CSS is retained only where Nuxt UI has no equivalent: the live
log tail (`.logs`/`.log-line`/`mark`), the hand-rolled SVG metrics chart
(ADR-037), status dots, the span-tree layout, and the connect-env inline code
and snippets.

### 4. `@pinia/nuxt` + pinia v3; axios retained

Stores keep the setup-store API and are auto-imported via `@pinia/nuxt`. The
axios client (including the single-retry 401 refresh interceptor) is moved
verbatim; `baseURL: '/'` and `withCredentials: true` are unchanged. No
`useRuntimeConfig`/`ofetch` rewrite.

### 5. Auth guard and SSE preserved

`vue-router`'s `beforeEach` guard becomes `app/middleware/auth.global.ts` with
identical logic (public routes, `/me` bootstrap, admin gating, CWE-601-safe
`redirect` query). The SSE live-trace stream (`connectLiveTrace`), the 5s poll
fallback, log-search paging, and the autofollow pin/unpin state machine are
preserved.

## Consequences

- **Positive:** idiomatic Nuxt routing/auto-imports, a maintained component
  library, Tailwind v4, and a smaller bespoke stylesheet. The Go embed contract
  and cache headers are untouched.
- **Negative / risks:** the UI now depends on the Nuxt/Nuxt UI release train
  and Node `20.19+`/`22.12+` (the module uses `node:22-alpine`). The visual
  theme adopts Nuxt UI's dark palette (blue primary, slate neutral) rather than
  the previous GitHub-dark hexes. `nuxt generate` also emits per-route
  prerendered shells (e.g. `pipelines/index.html`) that the Go handler does not
  use; they are harmless.
- **Rollback:** revert the branch and `helm rollback dagger-kubernetes-test` to
  the prior revision; a fresh image build from `main` restores the Vue 3 bundle.

## Alternatives considered

- **Keep Vue 3 + Vite, add Nuxt UI manually:** rejected — Nuxt UI v4 is built
  for Nuxt and the issue explicitly requested the Nuxt 4 migration.
- **`ssr: true` / hybrid rendering:** rejected — the Go binary embeds static
  files only; there is no Node runtime in production.
- **Change the Go handler to treat `_nuxt/` as immutable:** rejected — it would
  touch Go code and tests for zero correctness gain versus pinning
  `buildAssetsDir`.
- **Replace axios with `$fetch`/`ofetch`:** rejected — the existing 401
  refresh-once interceptor and cookie flow are subtle and already correct.
