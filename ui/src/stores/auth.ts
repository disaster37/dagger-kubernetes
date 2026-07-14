import { defineStore } from 'pinia'
import { ref, computed } from 'vue'

export const useAuthStore = defineStore('auth', () => {
  const token = ref<string | null>(localStorage.getItem('dagger_cache_token'))
  const refreshToken = ref<string | null>(localStorage.getItem('dagger_cache_refresh_token'))
  const username = ref<string>('')
  const role = ref<string>('viewer')

  const isAuthenticated = computed(() => !!token.value)
  const isAdmin = computed(() => role.value === 'admin')

  function login(accessToken: string, refresh: string, user: string, userRole: string) {
    token.value = accessToken
    refreshToken.value = refresh
    username.value = user
    role.value = userRole
    localStorage.setItem('dagger_cache_token', accessToken)
    localStorage.setItem('dagger_cache_refresh_token', refresh)
  }

  function logout() {
    token.value = null
    refreshToken.value = null
    username.value = ''
    role.value = 'viewer'
    localStorage.removeItem('dagger_cache_token')
    localStorage.removeItem('dagger_cache_refresh_token')
  }

  return { token, refreshToken, username, role, isAuthenticated, isAdmin, login, logout }
})
