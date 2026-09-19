# Issue #23 — Migrate frontend to Nuxt 4 + Nuxt UI v4

**Status:** Implementation-ready
**Target:** `github.com/disaster/dagger-kubernetes` (working tree at `/projects/dagger-cache`)
**Issue:** https://github.com/disaster37/dagger-kubernetes/issues/23

---

## 0. Executive summary of decisions

| Decision | Choice | Rationale |
|---|---|---|
| Framework | **Nuxt 4** (`ssr: false`, static SPA) | Go binary embeds static files only; no Node server in production. |
| Build command | `nuxt generate` → `.output/public` | Pure SPA; emits `index.html` + `200.html`/`404.html` fallbacks. |
| Output copied to Go embed | `.output/public/*` → `internal/handler/ui-dist/` | Replaces `ui/dist` → `ui-dist`. |
| Asset directory | `buildAssetsDir: '/assets/'` | Keeps `internal/handler/ui.go` + `ui_test.go` **unchanged** (hashed assets stay under `assets/`, already `immutable`). |
| Nuxt UI | **v4** (latest stable 4.x, Tailwind v4, Reka UI) | User decision; v4 is the current stable line for Nuxt 4. |
| Nuxt UI adoption | **Full component adoption** (user decision) | Swap custom elements for Nuxt UI v4 components; keep behavior identical. Bespoke data widgets (span tree, live log tail, hand-rolled SVG metrics chart per ADR-037) are rebuilt *from* Nuxt UI primitives; only residual layout CSS where no primitive exists. |
| Stores | `@pinia/nuxt` + `pinia` v3 (setup-store API unchanged) | Idiomatic Nuxt pinia integration; auto-installs pinia. |
| HTTP | Keep `axios` + existing 401-refresh interceptor | Preserves auth flow exactly; no `ofetch`/`$fetch` rewrite. |
| API base URL | Keep `baseURL: '/'` (same-origin) | UI + API are served by the same Go process/ingress; no `useRuntimeConfig` needed. |

**Go code changes: NONE** (asset-dir decision preserves `ui.go`/`ui_test.go` verbatim). Build/Dagger/Docker/docs are the only non-`ui/` touchpoints.

---

## 1. Target versions

Install-time resolution is authoritative; these are the expected latest-stable majors/minors at plan time (2026-09-18), confirmed via Nuxt/Nuxt UI docs:

| Package | Version | Notes |
|---|---|---|
| `nuxt` | `^4.1.3` (Nuxt 4 stable) | Requires Node `20.19+` / `22.12+` (Node 18 EOL). |
| `@nuxt/ui` | `^4.0.1` (Nuxt UI **v4** stable) | Tailwind CSS **v4**, Reka UI; built for Nuxt 4. Latest stable v4 minor confirmed = `4.0.1`. |
| `tailwindcss` | `^4` (v4.x) | Direct dep required by Nuxt UI v4 install. |
| `@pinia/nuxt` | `^0.11.0` | Pulls `pinia` v3. |
| `pinia` | `^3.0.0` | Explicit: stores import `defineStore` from `'pinia'`. |
| `axios` | `^1.7.9` (unchanged) | Keep. |
| `@iconify-json/lucide` | `^1.2.0` | Icon set for `UIcon` (`i-lucide-*`); Nuxt UI v4 uses `@nuxt/icon` and needs a collection installed. |
| `vue-tsc` (dev) | `^3.0.0` | Required by `nuxt typecheck`. |
| `typescript` (dev) | `^5.9.0` | Required by `nuxt typecheck`. |

**Removed deps:** `vue`, `vue-router` (Nuxt manages both), `@vitejs/plugin-vue`, `vite`.

> No `@tailwindcss/vite` or `@nuxtjs/color-mode` is needed: the `@nuxt/ui` v4 module wires Tailwind v4 and its own `useColorMode()` (Reka/`@vueuse`-based) automatically.

---

## 2. Rendering / build mode

- `ssr: false` (SPA, client-side routing with history mode — matches the current `createWebHistory()`).
- `"build": "nuxt generate"` produces a static SPA in **`.output/public/`** containing:
  - `index.html` (root shell),
  - `200.html` + `404.html` (SPA fallbacks — **not** consumed by the Go handler; see below),
  - hashed JS/CSS under `assets/` (because of `buildAssetsDir: '/assets/'`).
- **Go handler interaction:** `internal/handler/ui.go` `serveUIPath` (lines 35–45) falls back to `index.html` for extension-less paths and serves files verbatim otherwise. Nuxt's `index.html` is the same SPA shell as `200.html`, so the existing fallback already serves deep links (`/pipelines/<id>`, `/image-cache`, …) correctly. **No Go change.** `200.html`/`404.html` are emitted but unused (the handler ignores them); leave them in the embedded output (harmless).
- Prerender safety: with `ssr: false`, `nuxt generate` does **not** execute page/component setup code server-side — it only emits the static shell — so `window`/`document`/`EventSource`/`ResizeObserver`/`setInterval` access is never hit during the build. No `import.meta.client`/`onMounted` guards are required for the *build*; existing client-only code is untouched.

---

## 3. Asset directory decision

**Chosen: `buildAssetsDir: '/assets/'`.** All hashed build assets emit under `/assets/…`, which `ui.go:63` (`strings.HasPrefix(rel, "assets/")`) already serves with `Cache-Control: public, max-age=31536000, immutable`, and `ui_test.go` already asserts. **`internal/handler/ui.go` and `internal/handler/ui_test.go` are NOT modified.**

Rejected alternative (set Go to also treat `_nuxt/` as immutable) would touch Go + tests and add lint/dead-symbol risk for zero correctness gain — not worth it.

---

## 4. File-by-file migration map

### 4.1 New/rewritten root files (in `ui/`)

| Old | New / action |
|---|---|
| `ui/package.json` | Rewrite (deps + scripts; see §9.1). |
| `ui/package-lock.json` | Regenerate via `npm install`. |
| `ui/index.html` | **Delete** (title/meta moved to `nuxt.config.ts` `app.head`). |
| `ui/vite.config.ts` | **Delete** (replaced by `nuxt.config.ts`; dev proxy moves to `nitro.devProxy`). |
| `ui/tsconfig.json` | Replace with minimal `{ "extends": "./.nuxt/tsconfig.json" }`. |
| `ui/tsconfig.node.json` | **Delete**. |
| `ui/src/main.ts` | **Delete** (Nuxt entry is `app.vue`; pinia via `@pinia/nuxt`). |
| — | **Create `ui/nuxt.config.ts`** (see §4.2). |
| — | **Create `ui/app.config.ts`** (theme colors, see §5.1). |
| — | **Create `ui/app/plugins/color-mode.ts`** (dark default, see §5.1). |

### 4.2 `ui/nuxt.config.ts`

```ts
export default defineNuxtConfig({
  compatibilityDate: '2026-09-18',
  devtools: { enabled: false },
  modules: ['@nuxt/ui', '@pinia/nuxt'],
  css: ['~/assets/css/main.css'],
  ssr: false,
  buildAssetsDir: '/assets/',          // hashed assets under /assets/ (keeps Go handler unchanged)
  app: {
    head: {
      title: 'Dagger Kubernetes - Pipeline View',
      htmlAttrs: { lang: 'en' },
      meta: [{ name: 'viewport', content: 'width=device-width, initial-scale=1.0' }],
    },
  },
  nitro: {
    devProxy: {
      '/v1':   { target: 'http://localhost:8080', changeOrigin: true },
      '/api':  { target: 'http://localhost:8080', changeOrigin: true },
      '/auth': { target: 'http://localhost:8080', changeOrigin: true },
      '/ws':   { target: 'ws://localhost:8080', ws: true },   // vestigial (UI uses SSE, not ws); keep for dev parity
    },
  },
})
```

> Nuxt UI v4 does **not** take a `colorMode` key in `nuxt.config.ts`; dark default is set via `useColorMode()` in a plugin (§5.1). No `ui.theme.prefix` is needed (no Tailwind prefix).

### 4.3 `ui/app/assets/css/main.css` (was `ui/src/style.css`)

Tailwind v4 + Nuxt UI v4 entry, plus residual bespoke CSS that has no Nuxt UI primitive (log monospace, `<mark>` highlight, metric SVG, status dots):

```css
@import "tailwindcss";
@import "@nuxt/ui";

/* residual bespoke CSS retained only where Nuxt UI has no equivalent:
   - .logs / .log-line / .log-ts / .log-msg / .log-step / mark (live log tail)
   - .metric-* (hand-rolled SVG chart, ADR-037)
   - .status-dot (10px health dot)
   (copy these rules verbatim from ui/src/style.css) */
```

All other `style.css` rules (`.card`, `.btn`, `.badge`, table reset, navbar, etc.) are **dropped** — replaced by Nuxt UI v4 components.

### 4.4 Pages (file-based routes)

Route table (derived from `ui/src/router/index.ts` lines 6–21):

| Path | New page file | Page meta | Source file |
|---|---|---|---|
| `/` (→ redirect `/pipelines`) | `app/pages/index.vue` | `definePageMeta({ redirect: '/pipelines' })` | router `{ path: '/', redirect: '/pipelines' }` |
| `/pipelines` | `app/pages/pipelines/index.vue` | — | `views/Pipelines.vue` |
| `/pipelines/:id` | `app/pages/pipelines/[id].vue` | — | `pipeline/PipelineView.vue` (param name stays `id`) |
| `/history` | `app/pages/history.vue` | — | `history/History.vue` |
| `/fleet` | `app/pages/fleet.vue` | — | `fleet/Runners.vue` |
| `/services` | `app/pages/services.vue` | — | `views/Services.vue` |
| `/settings` | `app/pages/settings.vue` | — | `views/Settings.vue` |
| `/connect` | `app/pages/connect.vue` | — | `views/Connect.vue` |
| `/auth/login` | `app/pages/auth/login.vue` | `public` | `auth/Login.vue` |
| `/auth/callback` | `app/pages/auth/callback.vue` | `public` | `auth/Callback.vue` |
| `/admin/users` | `app/pages/admin/users.vue` | `admin` | `views/admin/Users.vue` |
| `/admin/groups` | `app/pages/admin/groups.vue` | `admin` | `views/admin/Groups.vue` |
| `/admin/projects` | `app/pages/admin/projects.vue` | `admin` | `views/admin/Projects.vue` |
| `/image-cache` | `app/pages/image-cache.vue` | `admin` | `imagecache/ImageCache.vue` |

### 4.5 Shell, layout, components, stores, utils, plugins

| Old | New |
|---|---|
| `src/App.vue` (navbar + `<router-view/>` + status watcher + logout) | `app/layouts/default.vue` (navbar + `<slot/>` + same watcher/logout) |
| — (new) | `app/app.vue`: `<UApp><NuxtLayout><NuxtPage/></NuxtLayout></UApp>` |
| `src/components/StatusIndicator.vue` | `app/components/StatusIndicator.vue` (auto-imported) |
| `src/pipeline/MetricChart.vue` | `app/components/MetricChart.vue` |
| `src/pipeline/StepTree.vue` | `app/components/StepTree.vue` |
| `src/pipeline/LogPanel.vue` | `app/components/LogPanel.vue` |
| `src/pipeline/spanTree.ts` | `app/utils/spanTree.ts` (unchanged exports) |
| `src/directives/followLogs.ts` | `app/utils/followLogs.ts` (helpers + `vFollowLogs`) + `app/plugins/follow-logs.ts` (registers `v-follow-logs` globally) |
| `src/stores/auth.ts` | `app/stores/auth.ts` (unchanged) |
| `src/stores/status.ts` | `app/stores/status.ts` (unchanged) |
| `src/api/client.ts` | `app/api/client.ts` (unchanged) |
| `src/api/types.ts` | `app/api/types.ts` (unchanged) |
| `src/router/index.ts` | **Delete** (file-based routes + `app/middleware/auth.global.ts`) |
| — (new) | `app/middleware/auth.global.ts` (see §7.1) |
| — (new) | `app/types/route-meta.d.ts` (PageMeta augmentation) |
| — (new) | `app/plugins/color-mode.ts` (dark default, §5.1) |

Import-path rewrite: every `@/…` import becomes `~/…` (Nuxt aliases `~` and `@` both resolve to `app/`).

### 4.6 `nuxt` auto-imports (remove explicit imports where safe)

- Components in `app/components/` are auto-imported → remove `import StatusIndicator/MetricChart/StepTree/LogPanel` lines.
- `app/utils/*` exports are auto-imported, but keep explicit `~/utils/spanTree` and `~/utils/followLogs` imports to avoid name collisions and keep types crisp.
- `app/stores/*` via `@pinia/nuxt` are auto-imported, but keep explicit `~/stores/auth`/`~/stores/status` (esp. in `app/api/client.ts`, which runs outside Nuxt context — the interceptor must keep the explicit `import { useAuthStore } from '~/stores/auth'`).

---

## 5. Nuxt UI v4 adoption (full component adoption)

### 5.1 Theme + color mode

`ui/app.config.ts` — semantic color aliases (v4 shape, unchanged from v3):

```ts
export default defineAppConfig({
  ui: {
    colors: {
      primary: 'blue',
      neutral: 'slate',
    },
  },
})
```

`ui/app/plugins/color-mode.ts` — force dark (v4 uses `useColorMode()`, not `colorMode.preference` in nuxt.config):

```ts
export default defineNuxtPlugin(() => {
  const colorMode = useColorMode()
  colorMode.preference = 'dark'
})
```

> If the installed `@nuxt/ui` v4 still honors `ui.colorMode.preference` in `app.config.ts`, that is an acceptable alternative; the plugin above is the unambiguous fallback and is recommended.

Semantic status mapping preserved 1:1 (existing `status-*` colors → Nuxt UI v4 `color` tokens):

| Current | Nuxt UI v4 |
|---|---|
| `badge-success` / `status-ok` (green) | `success` |
| `badge-failed` / `status-down` (red) | `error` |
| `badge-running` (blue) | `info` |
| `status-degraded` (amber) | `warning` |
| `status-unknown` / `badge-muted` / `badge-neutral` (grey) | `neutral` |

### 5.2 Global shell

| Current | Nuxt UI v4 |
|---|---|
| `<div id="app">` root | `<UApp>` (in `app.vue`) — v4 wrapper, replaces the old `UModals`/`USlideovers`/`UNotifications` globals |
| `<nav class="navbar">` + `.nav-links` `<router-link>` | `<UHeader>` with `left` (logo `ULink`), `center` (`UNavigationMenu orientation="horizontal" :items="..."`), `right` (StatusIndicator + user) |
| Logo `<router-link class="logo">` | `<ULink to="/" class="font-bold">` |
| Nav `<router-link>` + `router-link-active` | `UNavigationMenu :items` with `orientation="horizontal"` (v4 uses `items`, not `links`; active handling via `v-model`/`default-value` or per-item `active`) |
| `StatusIndicator` dot + label | `UButton :to="/services" variant="ghost" size="sm"` wrapping a `UBadge` (color = status mapping above); keep the tiny 10px dot as a residual span |
| User `<span>` + `.role-badge` + Logout `<button>` | `UBadge` (role) + `UDropdownMenu :items` (logout entry) or `UButton` logout |
| `<main class="content">` | `<UMain>` with a Tailwind `max-w-[1400px] mx-auto p-6` container |

### 5.3 Per-view mapping

**Pipelines** (`app/pages/pipelines/index.vue`)
- group `<select>` → `USelect :items` (`v-model`, `@change="load"`); v4 uses `items` (not `options`).
- `<table>` → `UTable` (`:columns`, `:rows`); status cell → `UBadge :color`; "View DAG" → `UButton size="xs" :to="\`/pipelines/${trace.trace_id}\`"`.
- empty `<p>` → `UEmpty` or `UAlert color="neutral"`.

**Services** (`app/pages/services.vue`)
- loading `.loading-state/.spinner` → `UIcon name="i-lucide-loader-circle" class="animate-spin"` centered.
- `.error-banner` → `UAlert color="error"` + `UButton` retry.
- Overall card → `UCard` + `UBadge`.
- services `<table>` → `UTable`; state cell → `UBadge`/`UIcon` + status text.

**Connect** (`app/pages/connect.vue`)
- version `<select>` → `USelect :items`; "Show token plaintext" `<input type=checkbox>` → `UCheckbox`.
- env `<table>` → `UTable`; `<code>` cells → `UCode`; secret values keep the residual `.secret-value`/`.env-value` inline-code styling.
- copy buttons → `UButton`; `copied` → `UBadge color="success"`.
- snippets `<pre class="snippet">` → `UCard` + residual `<pre>` (keep monospace/scroll styling).

**Settings** (`app/pages/settings.vue`)
- profile/groups/token tables → `UTable`/`UCard`; groups badges → `UBadge color="neutral"`.
- token `Generate/Regenerate/Revoke` → `UButton color="primary|neutral|error"`.
- change-password `<form>` + `<input>` → `UForm` + `UInput type="password"` + `UButton type="submit" color="primary"`; inline errors → `UAlert color="error"`; success → `UAlert color="success"`.

**admin/Users** (`app/pages/admin/users.vue`)
- create form inputs → `UInput` + `USelect :items` (role) + `UButton`.
- table → `UTable`; role `<select>` → `USelect :items`; group badges → `UBadge`; token `<code>` → `UCode`.
- "Groups" modal → `UModal v-model:open` + `UCheckbox` list; `Reset PW` `prompt()` → `UModal v-model:open` + `UInput type="password"` (preserve min-8 validation); `Revoke token`/`Delete` `confirm()` → `UAlertDialog v-model:open` (or `UModal` with Cancel/Confirm), preserving the exact confirm strings and the `disabled` on self-delete.

**admin/Groups** (`app/pages/admin/groups.vue`)
- create form → `UInput` (name/pattern/max sessions) + `UCheckbox` (agent available) + `UButton`.
- table → `UTable`; agent `<input type=checkbox>` → `UCheckbox`.
- Members modal → `UModal v-model:open` + `UCheckbox` list + `UButton` Save/Cancel; Delete `confirm()` → `UAlertDialog v-model:open`.

**admin/Projects** (`app/pages/admin/projects.vue`)
- create form → `UInput` + `USelect :items` (group) + `UButton`; table → `UTable`; group `<select>` → `USelect :items`; Delete `confirm()` → `UAlertDialog v-model:open`.

**Image cache** (`app/pages/image-cache.vue`)
- loading/error/empty → `UIcon` spinner / `UAlert` / `UEmpty`.
- mirror `<div class="card">` → `UCard`; reachable/backend badges → `UBadge color="success|error|neutral"`.
- repository/tag `<table>` → `UTable` with a `UCheckbox` selection column; "Prune selected"/"Prune all" → `UButton` (`color="error"` for prune-all); prune results → `UAlert`/`UCard` summary; `window.confirm` → `UAlertDialog v-model:open`.

**Fleet / Runners** (`app/pages/fleet.vue`)
- version `<div class="card">` → `UCard`; ready/down badge → `UBadge color="success|error"`; pod table → `UTable`; "Purge local cache" → `UButton` (admin-gated); purge results → `UAlert`; `window.confirm` → `UAlertDialog v-model:open`.

**History** (`app/pages/history.vue`)
- stats/GC `<table>` → `UTable`/`UCard`; GC on/off → `UBadge color="success|error"`; admin purge `<input>` → `UInput`; "Purge trace"/"Purge all" → `UButton color="error"`; `confirm()` → `UAlertDialog v-model:open`; purge message → `UAlert`.

**Login** (`app/pages/auth/login.vue`)
- `group_required` info `<div>` → `UAlert color="info"`; OAuth error `<p>` → `UAlert color="error"`.
- form → `UForm` + `UInput` (username/password, `autocomplete` preserved) + `UButton color="primary" block` (`:loading` → `loading` prop).
- GitHub/OIDC `<a class="btn">` → `UButton block :to="/api/v1/auth/oauth/{github,oidc}/login?redirect=…"` (or keep `<ULink>`; these are same-origin backend navigation, not SPA routes).

**Callback** (`app/pages/auth/callback.vue`)
- "Authenticating…" → `UIcon name="i-lucide-loader-circle" class="animate-spin"` + text.

**Pipeline view** (`app/pages/pipelines/[id].vue` + `app/components/{StepTree,LogPanel,MetricChart}.vue`)
- header "← Back" → `UButton :to="/pipelines" variant="ghost"`; status → `UBadge`; version/user chips → `UBadge variant="soft"`; duration → residual text.
- metrics card → `UCard` + `MetricChart` (keep custom SVG — ADR-037 decision, no charting dep).
- services card → `UCard`; collapsible service rows → `UAccordion` (or `UCollapsible`) with residual `.logs` block + `v-follow-logs`.
- steps card → `UCard` + `StepTree`.
- details `<table>` → `UTable`.
- `StepTree`: breadcrumb → `UBreadcrumb :items :separator-icon="i-lucide-chevron-right"` (v4 renames `links`→`items`, `divider`→`separator-icon`, `#divider`→`#separator`); focus header → `UButton`/`UIcon`; step rows/chevrons → `UAccordion`/`UCollapsible` built from `UButton`/`UIcon`/`UBadge`; count badges → `UBadge color="info"`; "hidden" → `UBadge color="neutral"`.
- `LogPanel`: search → `UInput` (`@input` debounce preserved); mode toggle → `UButtonGroup` (contains/regex); "Load more"/"↓ N new logs" → `UButton`; error → `UAlert`; log lines + `<mark>` highlight → residual (no primitive).

**Preserve exactly** (no redesign): every polling interval, debounce, `disposed` guard, SSE `connectLiveTrace`, `searchRefreshKey`, `loadSeq` generation, `pinned`/`newCount` autofollow state machine, `safeRedirect`/`redirectTarget` CWE-601 guards, `confirm`/`prompt` strings, and all API calls.

### 5.4 v3→v4 breaking changes to apply (authoritative)

| Area | v3 | v4 |
|---|---|---|
| Wrapper | `UModals`/`USlideovers`/`UNotifications` | Single `<UApp>` (we already wrap in UApp) |
| `USelect` | `:options` | `:items` |
| `UNavigationMenu` / `UHorizontalNavigation` | `:links` | `:items` |
| `UBreadcrumb` | `:links`, `divider`, `#divider` slot | `:items`, `separator-icon`, `#separator` slot |
| `UButton` | `:padded="false"`, `truncate` | `square` (drop `padded`/`truncate`) |
| `app.config.ts` overrides | `default: {…}`, flat keys | `slots: {…}` + `defaultVariants: {…}` (we use only `ui.colors`, so no override migration needed) |
| Color mode | `colorMode.preference` (config) | `useColorMode()` composable / `definePageMeta({ colorMode })` |
| `UCarousel` (unused) | `indicators` | `dots` |

---

## 6. Data structures & function signatures (exact)

### 6.1 `app/middleware/auth.global.ts`

```ts
export default defineNuxtRouteMiddleware(async (to) => {
  const auth = useAuthStore()
  if (to.meta.public) return
  if (!auth.user) await auth.loadUser()                 // bootstrap session via httpOnly cookie (/me)
  if (!auth.isAuthenticated) {
    return navigateTo({ path: '/auth/login', query: { redirect: to.fullPath } })
  }
  if (to.meta.admin && !auth.isAdmin) return navigateTo('/pipelines')
})
```

### 6.2 `app/types/route-meta.d.ts`

```ts
declare module 'nuxt/schema' {
  interface PageMeta {
    public?: boolean
    admin?: boolean
  }
}
```

### 6.3 `app/plugins/follow-logs.ts`

```ts
import { vFollowLogs } from '~/utils/followLogs'
export default defineNuxtPlugin((nuxtApp) => {
  nuxtApp.vueApp.directive('follow-logs', vFollowLogs)
})
```

### 6.4 `app/utils/followLogs.ts`

Move verbatim from `ui/src/directives/followLogs.ts`; keep exports and augmentation:

```ts
export const FOLLOW_PIN_THRESHOLD = 4
export const FOLLOW_UNPIN_THRESHOLD = 24
export interface FollowLogsState { pinned: boolean; programmatic: boolean; raf: number | null; ro: ResizeObserver | null }
export function followLogsPinned(el: HTMLElement | null): boolean
export function followLogsPin(el: HTMLElement | null): void
export const vFollowLogs: Directive<HTMLElement>
declare global { interface HTMLElement { __followLogs?: FollowLogsState } }
```

### 6.5 `app/utils/spanTree.ts`, `app/api/{client,types}.ts`, `app/stores/{auth,status}.ts`

Unchanged exports (move verbatim, only rewrite `@/` → `~/`):

- `spanTree.ts`: `INTERNAL_SPAN_PREFIXES`, `INTERNAL_SPAN_EXACT`, `isInternalSpanName`, `attrBool`, `isInternalSpan`, `isHiddenSpan`, `isTransparentSpan`, `visibleChildren`, `computeRowOwners`, `formatCount`, `entryKey`, `maxEntryTimestampNanos`, `flattenVisibleChildren`, `findSpanByID`, `flattenVisible`, `spanStartMs`, `spanEndMs`, `subtreeDuration`, `liveSpanDuration`, `formatDuration`, `logText`, `highlightSegments`, `DisplaySpan`, `HighlightSegment`.
- `client.ts`: `api` default export + `fetchProviders`, `fetchMe`, `loginRequest`, `refreshRequest`, `logoutRequest`, `changePassword`, `listUsers`, `createUser`, `updateUser`, `deleteUser`, `resetPassword`, `setUserGroups`, `getUserTokenMeta`, `revokeUserToken`, `listGroups`, `createGroup`, `updateGroup`, `deleteGroup`, `getGroupMembers`, `setGroupMembers`, `listProjects`, `createProject`, `updateProject`, `deleteProject`, `getMyToken`, `createMyToken`, `regenerateMyToken`, `revokeMyToken`, `fetchTraces`, `fetchTrace`, `fetchTraceLogs`, `fetchTraceMetrics`, `fetchTraceSearch`, `fetchFleetInfo`, `purgeEngineCache`, `fetchHistoryInfo`, `purgeHistory`, `purgeAllHistory`, `fetchPlatformStatus`, `fetchImageCacheInfo`, `pruneImageCache`, `pruneAllImageCache`, `fetchConnectEnv`, `connectLiveTrace`.
- `types.ts`: all interfaces unchanged.
- `auth.ts`: `useAuthStore` returning `{ user, isAuthenticated, isAdmin, groups, login, loadUser, refreshSession, logout }`.
- `status.ts`: `useStatusStore` returning `{ state, services, lastError, loading, refresh, start, stop }`.

No Go function signatures change.

---

## 7. Auth / SSE specifics

- **Same-origin API + httpOnly cookies:** `api = axios.create({ baseURL: '/', timeout: 30000, withCredentials: true })` stays. `useRuntimeConfig` is **not** required (UI and API share the ingress origin). No `NUXT_PUBLIC_API_BASE` env is introduced; if a future non-root base is ever needed, add `runtimeConfig.public.apiBase` then.
- **Route guard:** `app/middleware/auth.global.ts` (§6.1) replaces `router.beforeEach` (router/index.ts lines 24–40) with identical logic, including the `loadUser()` bootstrap on first navigation and the CWE-601-safe `redirect` query.
- **OIDC/GitHub login:** unchanged — `Login.vue` renders `ULink`/`UButton` to `/api/v1/auth/oauth/{github,oidc}/login?redirect=…` (server sets session cookies and redirects to `/auth/callback`). `redirectTarget` keeps the internal-absolute-path validation (Login.vue lines 85–91).
- **Callback:** `app/pages/auth/callback.vue` keeps `safeRedirect` + `onMounted → auth.loadUser() → router.push(redirect)` or `/auth/login?error=oauth`. Read `route.query.error` (Login shows `errorCode === 'oauth'`/`'group_required'` states).
- **401 interceptor:** unchanged (`client.ts` lines 36–64): refresh-once guard `_retried`, skip refresh for `/api/v1/auth/login|refresh`, then `auth.logout()` + `window.location.href = '/auth/login'`. The `typeof window !== 'undefined'` guard stays (harmless).
- **SSE:** `connectLiveTrace(id): EventSource` uses `new EventSource('/api/v1/traces/${id}/live')` — same-origin, sends the session cookie. Client-only (no server rendering with `ssr: false`), so no `window`/`document`/`EventSource` access occurs during prerender.
- **followLogs:** global `v-follow-logs` directive via plugin; `followLogsPinned`/`followLogsPin` imported from `~/utils/followLogs`. Remove the now-redundant `import { vFollowLogs }` from PipelineView.
- **Status polling lifecycle:** move the `watch(auth.isAuthenticated)` + `onUnmounted(status.stop)` block from App.vue into `app/layouts/default.vue` unchanged.
- **Dark mode:** `app/plugins/color-mode.ts` sets `useColorMode().preference = 'dark'` (v4 has no `colorMode.preference` config key).

---

## 8. Edge cases, error handling, validation

- **Missing/empty API base URL:** not applicable (same-origin `'/'`). If a request ever uses an absolute URL, it stays server-computed.
- **401 handling:** single-retry refresh guard preserved; a second 401 on the retried request surfaces the error (no refresh loop — CWE-400).
- **Callback error query params:** `?error=oauth` → `/auth/login?error=oauth` shows the OAuth error; `?error=group_required` shows the "Access Restricted" card.
- **SSE reconnect:** `EventSource` auto-reconnects; the 5s poll fallback in PipelineView remains for resilience; `onUnmounted` closes `eventSource` and clears timers/debounces.
- **Prerender of dynamic routes:** `nuxt generate` with `ssr:false` does not prerender `/pipelines/:id` (no payload); deep links resolve client-side via the Go `index.html` fallback.
- **Trailing slashes / deep links:** extension-less paths → `index.html` (ui.go:37); Nuxt router history mode handles `/pipelines`, `/image-cache`, etc. Verify `/pipelines/<id>` and `/image-cache` render and that a hard refresh on a deep link returns the SPA shell (not 404).
- **Cache headers:** hashed assets under `assets/` → `immutable`; `index.html`/extension-less → `no-cache` (unchanged; confirmed by `ui_test.go`).
- **Lockfile consistency:** commit a regenerated `ui/package-lock.json`; Docker/Dagger use `npm ci` (fails loudly on mismatch).
- **Node version:** `node:22-alpine` satisfies Nuxt 4's Node 20.19+/22.12+ floor.
- **Build determinism:** content-hashed assets; `npm ci` + committed lockfile → reproducible output.
- **`ui-dist` committed bundle:** regenerate `internal/handler/ui-dist/` (delete old `index.html` + `assets/*`, copy `.output/public/*`) and commit it so `go build`/`dagger call build` succeed without a UI build step.
- **Vestigial `/ws` dev proxy:** keep for dev parity; the production UI never calls it.
- **Nuxt UI v4 icon handling:** `@nuxt/icon` needs an icon collection installed (`@iconify-json/lucide`); verify `i-lucide-*` icons resolve at build time, or add the collection if `nuxt generate`/`nuxt typecheck` flags unknown icons.
- **Color-mode plugin timing:** with `ssr:false` the plugin runs client-side only; no hydration mismatch to guard.

---

## 9. Command sequence

### 9.1 Branch + deps

```bash
git checkout main && git pull
git checkout -b fix/issue-23-nuxt4-nuxt-ui-migration
cd ui
# rewrite package.json (deps/scripts) first, then:
npm install nuxt@latest @nuxt/ui@latest tailwindcss@latest @pinia/nuxt@latest pinia@latest axios@^1.7.9 @iconify-json/lucide@latest
npm install -D vue-tsc@latest typescript@latest
# verify package.json now matches §1 (nuxt ^4, @nuxt/ui ^4.0.x, tailwindcss ^4, etc.)
```

Expected: `package-lock.json` rewritten with lockfileVersion 3; no peer-dep errors; `@nuxt/ui` resolves to `^4.0.1`.

### 9.2 Typecheck + build (UI)

```bash
cd ui
npm run typecheck    # nuxt typecheck — generates .nuxt/tsconfig.json then vue-tsc
npm run build        # nuxt generate — emits .output/public
ls .output/public    # expect: index.html, 200.html, 404.html, assets/*.{js,css}
```

Failure triage: typecheck errors point at stale `@/` imports or missing page-meta types; `nuxt generate` failure is usually a v4 prop rename (check §5.4 — `items` vs `options`/`links`, `separator-icon` vs `divider`, `square` vs `padded`), an unknown `i-lucide-*` icon, or a `colorMode` config key that should be the plugin.

### 9.3 Regenerate embedded bundle

```bash
rm -rf internal/handler/ui-dist/assets internal/handler/ui-dist/index.html
cp -R ui/.output/public/. internal/handler/ui-dist/
git add internal/handler/ui-dist
```

### 9.4 Go + full CI gate

```bash
go build ./... && go vet ./... && go test ./...
# Full gate (Docker daemon available):
dagger call -m ./dagger --src . ci export --path out
# Minimum when no daemon: the above + dagger call -m ./dagger --src . lint
```

Expected: lint/vet/test green; `ui` function builds via `nuxt generate` in node:22-alpine; `docker` smoke (`-h`) passes.

### 9.5 Docker build smoke (local)

```bash
docker build -t docker.io/disaster/dagger-kubernetes:dev .
```

Expected: ui-builder stage `npm ci` + `npm run build` succeed; go-builder `go:embed all:ui-dist` resolves the new `assets/`.

### 9.6 Cluster redeploy + validation (AGENTS.local.md §4–§5 — mandatory)

```bash
export KUBECONFIG=/home/user/.kube/home
docker build -t docker.io/disaster/dagger-kubernetes:dev .
docker push docker.io/disaster/dagger-kubernetes:dev
helm --kubeconfig /home/user/.kube/home get values dagger-kubernetes-test -n dagger-kubernetes-test -o yaml > /tmp/dagger-kubernetes-test.values.yaml
# (confirm captured values still carry raft.clusterDomain + dns.nameserver per AGENTS.local.md §4.3)
helm --kubeconfig /home/user/.kube/home upgrade --install dagger-kubernetes-test ./deploy/helm/dagger-kubernetes \
  --namespace dagger-kubernetes-test -f /tmp/dagger-kubernetes-test.values.yaml \
  --set supervisor.image.tag=dev --set supervisor.image.pullPolicy=Always \
  --set supervisor.image.repository=docker.io/disaster/dagger-kubernetes
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test rollout restart statefulset/dagger-kubernetes-test-dagger-kubernetes
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test rollout status statefulset/dagger-kubernetes-test-dagger-kubernetes --timeout=300s
```

Agent verification (must pass): pods `Running`/`Ready`; `curl -sk https://localhost:8080/healthz` + `/readyz` = 200 (port-forward per §5.1); authed `/api/v1/status` + `/api/v1/fleet` 200; supervisor logs free of fatal/panic/`unknown raft command kind`.

Human verification (must pass before done): login at `https://dagger.home.webcenter.fr`; header status dot; navigate Pipelines, History, Runners, Services, Settings, Connect, Image cache, admin Users/Groups/Projects, and a live `/pipelines/<id>` (step tree, log search, SSE live updates, autofollow, metrics charts) against real cluster data.

---

## 10. Docs updates

1. **`DAGGER.md`**
   - Function table `ui` row (line 15): update "Upstream has no UI support" rationale to "Local Nuxt 4 + Nuxt UI v4 build".
   - Individual-functions `ui` row (line 52): change return from `dist/` directory → `.output/public/` directory.
   - Add a troubleshooting note: Nuxt static SPA (`ssr:false`), `buildAssetsDir: '/assets/'` (keeps Go immutable-cache path), node:22-alpine floor.
2. **`docs/README.md`**
   - Line 1655: "embedded Vue 3 SPA (packaged in `ui-dist/` via `//go:embed`)" → "embedded Nuxt 4 + Nuxt UI v4 SPA (static, `ssr: false`, packaged in `ui-dist/` via `//go:embed`)".
   - Line 1721–1724: cache-header sentence stays valid (`/assets/*` immutable) — no change needed beyond the stack name.
   - Line 2198 Development: `cd ui && npm install && npm run build` stays valid (add note that build output is `.output/public`).
3. **New ADR `docs/design/ADR-039-frontend-nuxt4-nuxt-ui.md`** (frontend stack is an architectural decision; ADR-001 covers only Go). Document: Vue 3 + Vite SPA → Nuxt 4 (`ssr:false`) + Nuxt UI v4 + Tailwind v4; static `nuxt generate` → `.output/public`; `buildAssetsDir:'/assets/'` (no Go change); `@pinia/nuxt`; axios retained; full component adoption with residual bespoke CSS for the span tree/log tail/hand-rolled metrics chart.
   - Add ADR-039 row to `docs/design/index.md`.
4. **`config/config.app.yaml.sample`:** **no change** (no config key is introduced or modified; UI stays same-origin).
5. **`AGENTS.local.md`:** no release-name/image-tag/endpoint/value change; the deploying agent appends a §7 revision entry after the redeploy per the existing convention (§6.4).

---

## 11. Acceptance criteria / Definition of Done

- [ ] `ui/` builds: `npm run typecheck` and `npm run build` green; `.output/public` contains `index.html` + `assets/*` (no `_nuxt/`).
- [ ] `@nuxt/ui` resolves to `^4.0.x`; `UApp` wrapper present; `useColorMode()` dark default active.
- [ ] `internal/handler/ui-dist/` regenerated + committed (old `assets/*`/`index.html` removed).
- [ ] `go build ./... && go vet ./... && go test ./...` green; `dagger call -m ./dagger --src . ci export --path out` green (lint + race tests + UI + binaries + Docker smoke + helm matrix).
- [ ] `Dockerfile` builds; embedded UI serves at `/` and deep links (`/pipelines/<id>`) return the SPA shell.
- [ ] Every route from §4.4 renders with auth guard + admin gating preserved; login → callback → redirect works for internal and OAuth providers.
- [ ] SSE live updates, 5s poll fallback, log search paging, autofollow pin/unpin, metric charts, image-cache prune confirmations all behave as before.
- [ ] Cache headers unchanged (`assets/*` immutable, shell `no-cache`) — `ui_test.go` still passes without edits.
- [ ] Docs updated (DAGGER.md, docs/README.md, ADR-039 + index.md).
- [ ] Cluster redeploy (§9.6): agent checks pass AND human confirms the live UI.
- [ ] PR opened from `fix/issue-23-nuxt4-nuxt-ui-migration` → `main`, titled `fix: migrate front to Nuxt 4 + Nuxt UI v4 (fixes #23)`, body referencing issue #23 with the decision summary.

---

## 12. Risks & rollback

| Risk | Mitigation |
|---|---|
| Embedded asset path break (`.output/public` vs `dist`) | Dockerfile + `dagger/main.go` Ui() both updated; smoke-test the built image's `/` and `/pipelines/<id>`. |
| Cache-header regression (`_nuxt` vs `assets`) | `buildAssetsDir: '/assets/'` keeps Go untouched; `ui_test.go` unchanged and still green. |
| CI node image / Nuxt Node floor | node:22-alpine already satisfies Nuxt 4; full `ci` gate run before merge. |
| Lockfile/`npm ci` mismatch | Commit regenerated `package-lock.json`; Docker/Dagger use `npm ci`. |
| Visual regression (full component adoption) | Human verification on live cluster (§9.6); rollback = revert branch + `helm rollback dagger-kubernetes-test` (to the pre-migration revision). |
| Nuxt UI v4 API drift (prop renames from v3) | Follow §5.4 mapping (`items`/`separator-icon`/`square`/`UApp`/`useColorMode`); typecheck catches most renames. |
| Missing icon collection (`@nuxt/icon`) | `@iconify-json/lucide` installed; build/typecheck fail loudly on unknown `i-lucide-*` icons. |
| `go:embed` fails if `ui-dist` emptied | Regenerate + commit `internal/handler/ui-dist/` before the Go build. |

**Rollback:** revert/close the PR and `helm rollback dagger-kubernetes-test` to the prior revision (the `dev` tag is mutable; a fresh `docker build` from `main` restores the Vue 3 bundle).

---

## Open questions / assumptions for confirmation

1. **Dark theme fidelity:** full adoption means the UI adopts Nuxt UI v4's dark theme (blue primary, slate neutral) rather than the exact GitHub-dark hexes. Confirm this is acceptable, or provide a target palette to encode in `app.config.ts` `ui.colors`.
2. **`@iconify-json/lucide`** assumed as the icon set (spinners/chevrons via `UIcon`). Confirm no bespoke icon set is preferred.
3. **Native `confirm()`/`alert()`/`prompt()`** are replaced by `UAlertDialog`/`UModal` (with identical guard strings) as part of full adoption — confirm this is in scope (vs keeping native dialogs).
