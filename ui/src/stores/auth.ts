import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { fetchMe, loginRequest, logoutRequest, refreshRequest } from '@/api/client'
import type { AuthUser } from '@/api/types'

export const useAuthStore = defineStore('auth', () => {
  const user = ref<AuthUser | null>(null)
  // Auth state is derived from the server (httpOnly cookie): /me success.
  const isAuthenticated = ref(false)

  const isAdmin = computed(() => user.value?.role === 'admin')
  const groups = computed(() => user.value?.groups ?? [])

  let refreshInFlight: Promise<boolean> | null = null

  async function login(username: string, password: string): Promise<void> {
    const u = await loginRequest(username, password)
    user.value = u
    isAuthenticated.value = true
  }

  async function loadUser(): Promise<void> {
    try {
      user.value = await fetchMe()
      isAuthenticated.value = true
    } catch {
      user.value = null
      isAuthenticated.value = false
    }
  }

  async function refreshSession(): Promise<boolean> {
    if (refreshInFlight) return refreshInFlight
    refreshInFlight = (async () => {
      try {
        await refreshRequest()
        return true
      } catch {
        return false
      } finally {
        refreshInFlight = null
      }
    })()
    return refreshInFlight
  }

  async function logout(): Promise<void> {
    try {
      await logoutRequest()
    } catch {
      // ignore — best-effort server cookie clear
    } finally {
      user.value = null
      isAuthenticated.value = false
    }
  }

  return { user, isAuthenticated, isAdmin, groups, login, loadUser, refreshSession, logout }
})
