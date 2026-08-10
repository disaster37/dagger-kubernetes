import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { fetchMe, loginRequest, refreshRequest } from '@/api/client'
import type { AuthUser } from '@/api/types'

export const useAuthStore = defineStore('auth', () => {
  const token = ref<string | null>(localStorage.getItem('dagger_cache_token'))
  const refreshToken = ref<string | null>(localStorage.getItem('dagger_cache_refresh_token'))
  const user = ref<AuthUser | null>(null)

  const isAuthenticated = computed(() => !!token.value)
  const isAdmin = computed(() => user.value?.role === 'admin')
  const groups = computed(() => user.value?.groups ?? [])

  let refreshInFlight: Promise<boolean> | null = null

  function setTokens(access: string, refresh: string) {
    token.value = access
    refreshToken.value = refresh
    localStorage.setItem('dagger_cache_token', access)
    localStorage.setItem('dagger_cache_refresh_token', refresh)
  }

  async function login(username: string, password: string): Promise<void> {
    const res = await loginRequest(username, password)
    setTokens(res.access_token, res.refresh_token)
    user.value = res.user
  }

  async function loadUser(): Promise<void> {
    try {
      user.value = await fetchMe()
    } catch {
      logout()
    }
  }

  async function refreshSession(): Promise<boolean> {
    if (refreshInFlight) return refreshInFlight
    if (!refreshToken.value) {
      logout()
      return false
    }
    refreshInFlight = (async () => {
      try {
        const res = await refreshRequest(refreshToken.value!)
        setTokens(res.access_token, res.refresh_token)
        return true
      } catch {
        logout()
        return false
      } finally {
        refreshInFlight = null
      }
    })()
    return refreshInFlight
  }

  function logout(): void {
    token.value = null
    refreshToken.value = null
    user.value = null
    localStorage.removeItem('dagger_cache_token')
    localStorage.removeItem('dagger_cache_refresh_token')
  }

  return { token, refreshToken, user, isAuthenticated, isAdmin, groups, login, setTokens, loadUser, refreshSession, logout }
})
