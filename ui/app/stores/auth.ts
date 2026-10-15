import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { fetchMe, loginRequest, logoutRequest, refreshRequest } from '~/api/client'
import type { AuthUser } from '~/api/types'

export const useAuthStore = defineStore('auth', () => {
  const user = ref<AuthUser | null>(null)
  // Auth state is derived from the server (httpOnly cookie): /me success.
  const isAuthenticated = ref(false)
  // authEpoch invalidates stale async completions: login/callback restoration
  // and clearLocalSession/logout advance it. A completion from an older epoch
  // must never set/clear the newer identity or trigger navigation.
  const authEpoch = ref(0)

  const isAdmin = computed(() => user.value?.role === 'admin')
  const groups = computed(() => user.value?.groups ?? [])

  let refreshInFlight: Promise<boolean> | null = null
  let refreshInFlightEpoch = -1

  function clearLocalSession(): void {
    authEpoch.value++
    refreshInFlight = null
    refreshInFlightEpoch = -1
    user.value = null
    isAuthenticated.value = false
  }

  async function login(username: string, password: string): Promise<void> {
    authEpoch.value++
    const epoch = authEpoch.value
    const u = await loginRequest(username, password)
    if (epoch !== authEpoch.value) return
    user.value = u
    isAuthenticated.value = true
  }

  // loadUser restores the session from the cookie. `callback: true` is used by
  // the OAuth callback: it increments the epoch before starting and uses the
  // 'none' recovery mode so fresh callback credentials are never replaced by
  // an old refresh flow. Returns true on success.
  async function loadUser(options?: { callback?: boolean }): Promise<boolean> {
    if (options?.callback) authEpoch.value++
    const epoch = authEpoch.value
    try {
      const u = await fetchMe(options?.callback ? 'none' : 'local')
      if (epoch !== authEpoch.value) return isAuthenticated.value
      user.value = u
      isAuthenticated.value = true
      return true
    } catch {
      if (epoch !== authEpoch.value) return isAuthenticated.value
      user.value = null
      isAuthenticated.value = false
      return false
    }
  }

  async function refreshSession(): Promise<boolean> {
    const epoch = authEpoch.value
    if (refreshInFlight && refreshInFlightEpoch === epoch) return refreshInFlight
    refreshInFlightEpoch = epoch
    refreshInFlight = (async () => {
      try {
        await refreshRequest()
        return true
      } catch {
        return false
      } finally {
        if (refreshInFlightEpoch === epoch) {
          refreshInFlight = null
          refreshInFlightEpoch = -1
        }
      }
    })()
    return refreshInFlight
  }

  // logout is explicit only: clear local state immediately, then do one
  // un-intercepted logout request and surface its failure to the caller (never
  // claim server revocation succeeded). No refresh or navigation recursion.
  async function logout(): Promise<void> {
    clearLocalSession()
    await logoutRequest()
  }

  return { user, isAuthenticated, authEpoch, isAdmin, groups, login, loadUser, refreshSession, clearLocalSession, logout }
})
