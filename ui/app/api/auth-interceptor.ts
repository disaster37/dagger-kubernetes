import type { AxiosError, AxiosInstance, AxiosRequestConfig, InternalAxiosRequestConfig } from 'axios'
import { authLoginTarget, safeAuthRedirect } from '~/utils/auth-navigation'

// AuthRecoveryMode selects how a 401 is handled:
//   - 'default': at most one refresh + one retry, then clear the local session
//     and navigate once to the login error screen (protected APIs).
//   - 'local':   may refresh once but never navigates or logs out; the caller
//     observes the rejection (used by /me).
//   - 'none':    rejects immediately (used by the OAuth callback so fresh
//     callback credentials are never replaced by an old refresh flow).
export type AuthRecoveryMode = 'default' | 'local' | 'none'

export type AuthRequestConfig = AxiosRequestConfig & {
  authRecovery?: AuthRecoveryMode
  _retried?: boolean
  _authEpoch?: number
}

// AuthState is the slice of the auth store the interceptor needs. It is
// injected so the interceptor can be exercised without Nuxt/Pinia globals.
export interface AuthState {
  authEpoch: number
  refreshSession(): Promise<boolean>
  clearLocalSession(): void
}

// BrowserLocation is the minimal location surface (window.location).
export interface BrowserLocation {
  pathname: string
  search: string
  hash: string
  assign(url: string): void
}

const AUTH_PREFIX = '/api/v1/auth/'

function requestPath(url: string | undefined): string {
  if (!url) return ''
  let p = url
  const q = p.indexOf('?')
  if (q >= 0) p = p.slice(0, q)
  const h = p.indexOf('#')
  if (h >= 0) p = p.slice(0, h)
  return p
}

function isAuthPath(path: string): boolean {
  return path === '/api/v1/auth' || path.startsWith(AUTH_PREFIX)
}

// navigatedEpoch guards against multiple concurrent terminal 401s each
// triggering a navigation in the same epoch.
let navigatedEpoch = -1

// installAuthInterceptors wires epoch-aware recovery onto api. Request
// configs capture the current authEpoch exactly once (never on a retry).
export function installAuthInterceptors(
  api: AxiosInstance,
  getAuth: () => AuthState,
  getLocation: () => BrowserLocation | null,
): void {
  api.interceptors.request.use((config) => {
    const cfg = config as InternalAxiosRequestConfig & AuthRequestConfig
    if (cfg._authEpoch === undefined) {
      cfg._authEpoch = getAuth().authEpoch
    }
    return cfg
  })

  api.interceptors.response.use(
    (response) => response,
    async (error: AxiosError) => {
      const cfg = error.config as (AuthRequestConfig & { url?: string }) | undefined
      if (!cfg || error.response?.status !== 401) {
        return Promise.reject(error)
      }
      const path = requestPath(cfg.url)
      const isMe = path === '/api/v1/auth/me'
      // Every auth endpoint except /me bypasses recovery and navigation so a
      // failed login/refresh/logout returns to its explicit caller.
      if (isAuthPath(path) && !isMe) {
        return Promise.reject(error)
      }
      const mode: AuthRecoveryMode = cfg.authRecovery ?? 'default'
      if (mode === 'none') {
        return Promise.reject(error)
      }

      const auth = getAuth()
      const epoch = cfg._authEpoch
      const currentEpoch = epoch === undefined || epoch === auth.authEpoch

      if (!cfg._retried && currentEpoch) {
        const ok = await auth.refreshSession()
        // A refresh that started in an older epoch must not retry with the
        // new epoch's credentials.
        if (ok && epoch === auth.authEpoch) {
          cfg._retried = true
          return api.request(cfg as AxiosRequestConfig)
        }
      }

      if (mode === 'local') {
        // Never navigate or log out from the /me recovery path.
        return Promise.reject(error)
      }

      // Once per captured epoch: clearLocalSession advances authEpoch, so the
      // guard must compare against the decision epoch, not the post-clear one.
      const decisionEpoch = epoch === undefined ? auth.authEpoch : epoch
      if (currentEpoch && navigatedEpoch !== decisionEpoch) {
        navigatedEpoch = decisionEpoch
        auth.clearLocalSession()
        const loc = getLocation()
        if (loc) {
          const target = safeAuthRedirect(`${loc.pathname}${loc.search}${loc.hash}`)
          loc.assign(authLoginTarget('session', target))
        }
      }
      return Promise.reject(error)
    },
  )
}
