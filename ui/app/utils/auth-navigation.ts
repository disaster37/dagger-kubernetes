// auth-navigation.ts mirrors the backend safeRedirectPath / error-hint
// contract so the SPA never navigates to an attacker-controlled or
// authentication destination and always renders a stable message.

const FALLBACK = '/pipelines'

// safeAuthRedirect validates a post-login SPA redirect target. Only internal
// absolute paths are allowed; schemes, hosts, protocol-relative URLs,
// backslashes, encoded controls and authentication paths all fall back to
// /pipelines (CWE-601).
export function safeAuthRedirect(raw: unknown): string {
  if (typeof raw !== 'string' || raw === '') return FALLBACK
  for (const ch of raw) {
    const c = ch.codePointAt(0) ?? 0
    if (c < 0x20 || c === 0x7f) return FALLBACK
  }
  if (raw.includes('\\')) return FALLBACK
  if (!raw.startsWith('/') || raw.startsWith('//')) return FALLBACK

  let url: URL
  try {
    url = new URL(raw, 'http://placeholder.invalid')
  } catch {
    return FALLBACK
  }
  // A raw target that resolves to a different origin/authority is rejected.
  if (url.origin !== 'http://placeholder.invalid') return FALLBACK
  const path = url.pathname
  if (path === '' || !path.startsWith('/') || path.startsWith('//') || path.includes('\\')) return FALLBACK
  for (const ch of path) {
    const c = ch.codePointAt(0) ?? 0
    if (c < 0x20 || c === 0x7f) return FALLBACK
  }
  if (path === '/auth' || path.startsWith('/auth/') || path.startsWith('/api/v1/auth/')) return FALLBACK
  return `${path}${url.search}${url.hash}`
}

// authLoginTarget builds the fixed login-error URL for a whitelisted code.
export function authLoginTarget(code: string, redirect: unknown): string {
  const target = safeAuthRedirect(redirect)
  return `/auth/login?error=${encodeURIComponent(code)}&redirect=${encodeURIComponent(target)}`
}

const MESSAGES: Record<string, string> = {
  group_required: 'Your account was authenticated, but you are not a member of any authorized group. Please contact your administrator to request access.',
  session: 'Your session could not be restored. Please sign in again.',
  oauth: 'OAuth login failed. Please try again.',
  account_disabled: 'Access to this account is disabled. Please contact an administrator.',
  auth_conflict: 'Another sign-in or account update changed this session. Please try again.',
}

// authErrorMessage maps a query error code (possibly an array or unknown) to a
// stable user-facing message, or null when there is no error. Unknown non-empty
// codes render the generic OAuth failure.
export function authErrorMessage(code: unknown): string | null {
  if (Array.isArray(code)) code = code[0]
  if (typeof code !== 'string' || code === '') return null
  return MESSAGES[code] ?? MESSAGES.oauth!
}
