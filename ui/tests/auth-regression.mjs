// auth-regression.mjs — Node stdlib harness for the auth recovery logic.
//
// It transpiles and imports the ACTUAL ui/app/utils/auth-navigation.ts and
// ui/app/api/auth-interceptor.ts (never a copy) and drives them with a real
// Axios instance and a custom adapter. Run with:
//   node --experimental-vm-modules tests/auth-regression.mjs
//
// The store/SFC SSR cases require the full Nuxt toolchain and are covered by
// the Go integration wrapper + human browser checks; this harness proves the
// interceptor/navigation contracts that caused the re-login loop.

import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { dirname, join } from 'node:path'
import ts from 'typescript'
import axios from 'axios'

const here = dirname(fileURLToPath(import.meta.url))
const uiDir = join(here, '..')
const harnessDir = join(uiDir, 'node_modules', '.auth-harness')

let failures = 0
function check(name, cond, detail = '') {
  if (cond) {
    console.log(`ok   - ${name}`)
  } else {
    failures++
    console.log(`FAIL - ${name}${detail ? `: ${detail}` : ''}`)
  }
}

function transpile(srcPath, rewrite) {
  let src = readFileSync(srcPath, 'utf8')
  if (rewrite) src = rewrite(src)
  return ts.transpileModule(src, {
    compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
  }).outputText
}

rmSync(harnessDir, { recursive: true, force: true })
mkdirSync(harnessDir, { recursive: true })

const navJs = transpile(join(uiDir, 'app', 'utils', 'auth-navigation.ts'))
writeFileSync(join(harnessDir, 'auth-navigation.mjs'), navJs)

const interceptorJs = transpile(
  join(uiDir, 'app', 'api', 'auth-interceptor.ts'),
  (s) => s.replace("from '~/utils/auth-navigation'", "from './auth-navigation.mjs'"),
)
writeFileSync(join(harnessDir, 'auth-interceptor.mjs'), interceptorJs)

const nav = await import(pathToFileURL(join(harnessDir, 'auth-navigation.mjs')).href)

// --- safeAuthRedirect parity (CWE-601) -------------------------------------
for (const [input, want] of [
  ['', '/pipelines'],
  [null, '/pipelines'],
  ['/pipelines', '/pipelines'],
  ['/admin/users?x=1', '/admin/users?x=1'],
  ['//evil.com', '/pipelines'],
  ['///evil.com', '/pipelines'],
  ['/\\evil.com', '/pipelines'],
  ['https://evil.com', '/pipelines'],
  ['evil.com', '/pipelines'],
  ['/auth/login', '/pipelines'],
  ['/auth/callback?redirect=x', '/pipelines'],
  ['/api/v1/auth/me', '/pipelines'],
  [`/pipelines${String.fromCharCode(0)}`, '/pipelines'],
]) {
  check(`safeAuthRedirect(${JSON.stringify(input)})`, nav.safeAuthRedirect(input) === want, `got ${nav.safeAuthRedirect(input)}`)
}

// --- authErrorMessage -------------------------------------------------------
for (const [input, want] of [
  ['session', nav.authErrorMessage('session')],
  ['account_disabled', 'Access to this account is disabled. Please contact an administrator.'],
  ['auth_conflict', 'Another sign-in or account update changed this session. Please try again.'],
  ['unknown_code', nav.authErrorMessage('oauth')],
  [['session', 'other'], nav.authErrorMessage('session')],
  [{}, null],
  ['', null],
  [undefined, null],
]) {
  check(`authErrorMessage(${JSON.stringify(input)})`, nav.authErrorMessage(input) === want, `got ${JSON.stringify(nav.authErrorMessage(input))}`)
}

// --- interceptor behaviours -------------------------------------------------
function fakeAxiosError(config, status) {
  return new axios.AxiosError(
    `request failed with status ${status}`,
    'ERR_BAD_REQUEST',
    config,
    null,
    { data: { message: 'unauthorized' }, status, statusText: '', headers: {}, config },
  )
}

async function freshInterceptor() {
  const url = pathToFileURL(join(harnessDir, 'auth-interceptor.mjs')).href + `?t=${Math.random()}`
  return import(url)
}

function makeApi(handler) {
  const api = axios.create({ baseURL: '/', adapter: handler })
  return api
}

// 1. Protected 401 -> one refresh + one retry succeeds, no navigation.
{
  const { installAuthInterceptors } = await freshInterceptor()
  let attempts = 0
  const api = makeApi(async (config) => {
    attempts++
    if (attempts === 1) throw fakeAxiosError(config, 401)
    return { data: { ok: true }, status: 200, statusText: 'OK', headers: {}, config }
  })
  const navCalls = []
  let refreshed = 0
  const auth = { authEpoch: 0, async refreshSession() { refreshed++; return true }, clearLocalSession() { throw new Error('must not clear') } }
  installAuthInterceptors(api, () => auth, () => ({ pathname: '/x', search: '', hash: '', assign: (u) => navCalls.push(u) }))
  const res = await api.get('/api/v1/status')
  check('protected 401 refreshes once and retries', res.status === 200 && attempts === 2 && refreshed === 1)
  check('successful retry does not navigate', navCalls.length === 0)
}

// 2. Terminal 401 -> clearLocalSession + exactly one navigation with target.
{
  const { installAuthInterceptors } = await freshInterceptor()
  const api = makeApi(async (config) => { throw fakeAxiosError(config, 401) })
  const navCalls = []
  let cleared = 0
  const auth = { authEpoch: 0, async refreshSession() { return false }, clearLocalSession() { cleared++; this.authEpoch++ } }
  installAuthInterceptors(api, () => auth, () => ({ pathname: '/admin/users', search: '?a=1', hash: '', assign: (u) => navCalls.push(u) }))
  await api.get('/api/v1/users').catch(() => {})
  check('terminal 401 clears local session once', cleared === 1, `cleared=${cleared}`)
  check('terminal 401 navigates to session error with safe target',
    navCalls.length === 1 && navCalls[0] === `/auth/login?error=${encodeURIComponent('session')}&redirect=${encodeURIComponent('/admin/users?a=1')}`,
    JSON.stringify(navCalls))
}

// 3. Auth endpoints bypass recovery: no refresh, no navigation, no recursion.
{
  const { installAuthInterceptors } = await freshInterceptor()
  const api = makeApi(async (config) => { throw fakeAxiosError(config, 401) })
  let refreshed = 0
  const navCalls = []
  const auth = { authEpoch: 0, async refreshSession() { refreshed++; return true }, clearLocalSession() { throw new Error('must not clear') } }
  installAuthInterceptors(api, () => auth, () => ({ pathname: '/', search: '', hash: '', assign: (u) => navCalls.push(u) }))
  await api.post('/api/v1/auth/login').catch(() => {})
  await api.post('/api/v1/auth/refresh').catch(() => {})
  await api.post('/api/v1/auth/logout').catch(() => {})
  check('auth login/refresh/logout 401 never refresh or navigate', refreshed === 0 && navCalls.length === 0)
}

// 4. local mode (/me) may refresh once but never navigates or clears.
{
  const { installAuthInterceptors } = await freshInterceptor()
  const api = makeApi(async (config) => { throw fakeAxiosError(config, 401) })
  let refreshed = 0
  const navCalls = []
  const auth = { authEpoch: 0, async refreshSession() { refreshed++; return false }, clearLocalSession() { throw new Error('must not clear') } }
  installAuthInterceptors(api, () => auth, () => ({ pathname: '/', search: '', hash: '', assign: (u) => navCalls.push(u) }))
  await api.get('/api/v1/auth/me', { authRecovery: 'local' }).catch(() => {})
  check('local mode refreshes but never navigates', refreshed === 1 && navCalls.length === 0)
}

// 5. none mode rejects immediately without refresh.
{
  const { installAuthInterceptors } = await freshInterceptor()
  const api = makeApi(async (config) => { throw fakeAxiosError(config, 401) })
  let refreshed = 0
  const auth = { authEpoch: 0, async refreshSession() { refreshed++; return true }, clearLocalSession() {} }
  installAuthInterceptors(api, () => auth, () => null)
  await api.get('/api/v1/auth/me', { authRecovery: 'none' }).catch(() => {})
  check('none mode rejects without refresh', refreshed === 0)
}

// 6. Multiple concurrent terminal 401s navigate once per epoch.
{
  const { installAuthInterceptors } = await freshInterceptor()
  let release
  const gate = new Promise((r) => { release = r })
  const api = makeApi(async (config) => { await gate; throw fakeAxiosError(config, 401) })
  const navCalls = []
  let cleared = 0
  const auth = { authEpoch: 0, async refreshSession() { return false }, clearLocalSession() { cleared++; this.authEpoch++ } }
  installAuthInterceptors(api, () => auth, () => ({ pathname: '/p', search: '', hash: '', assign: (u) => navCalls.push(u) }))
  const ps = [api.get('/api/v1/a').catch(() => {}), api.get('/api/v1/b').catch(() => {}), api.get('/api/v1/c').catch(() => {})]
  release()
  await Promise.all(ps)
  check('concurrent terminal 401s navigate exactly once', navCalls.length === 1, `${navCalls.length} navigations`)
}

// 7. Stale-epoch 401 must not clear or navigate the newer session.
{
  const { installAuthInterceptors } = await freshInterceptor()
  let release
  const gate = new Promise((r) => { release = r })
  const api = makeApi(async (config) => { await gate; throw fakeAxiosError(config, 401) })
  const navCalls = []
  let cleared = 0
  const auth = { authEpoch: 0, async refreshSession() { return false }, clearLocalSession() { cleared++; this.authEpoch++ } }
  installAuthInterceptors(api, () => auth, () => ({ pathname: '/p', search: '', hash: '', assign: (u) => navCalls.push(u) }))
  const p = api.get('/api/v1/x')
  await new Promise((r) => setTimeout(r, 0)) // let the request interceptor capture epoch 0
  auth.authEpoch++ // newer epoch arrives while the request is in flight
  release()
  await p.catch(() => {})
  check('stale-epoch 401 does not clear or navigate', cleared === 0 && navCalls.length === 0, `cleared=${cleared} nav=${navCalls.length}`)
}

rmSync(harnessDir, { recursive: true, force: true })

if (failures > 0) {
  console.error(`\n${failures} harness assertion(s) failed`)
  process.exit(1)
}
console.log('\nall auth regression assertions passed')
