<template>
  <div style="max-width: 400px; margin: 80px auto;">
    <div class="card">
      <h2 style="margin-bottom: 16px;">Login</h2>
      <p v-if="errorQuery" style="color: #f85149; font-size: 13px; margin-bottom: 12px;">
        OAuth login failed. Please try again.
      </p>
      <form @submit.prevent="handleLogin">
        <div style="margin-bottom: 12px;">
          <label style="display: block; margin-bottom: 4px; font-size: 14px;">Username</label>
          <input
            v-model="username"
            type="text"
            autocomplete="username"
            placeholder="username"
            style="width: 100%; padding: 8px 12px; background: #0d1117; border: 1px solid #30363d; border-radius: 6px; color: #c9d1d9; font-size: 14px;"
          />
        </div>
        <div style="margin-bottom: 16px;">
          <label style="display: block; margin-bottom: 4px; font-size: 14px;">Password</label>
          <input
            v-model="password"
            type="password"
            autocomplete="current-password"
            placeholder="password"
            style="width: 100%; padding: 8px 12px; background: #0d1117; border: 1px solid #30363d; border-radius: 6px; color: #c9d1d9; font-size: 14px;"
          />
        </div>
        <button type="submit" class="btn btn-primary" style="width: 100%;" :disabled="loading">
          {{ loading ? 'Signing in…' : 'Sign in' }}
        </button>
      </form>
      <p v-if="error" style="color: #f85149; font-size: 13px; margin-top: 12px;">{{ error }}</p>
      <div v-if="providers.oauth_github" style="margin-top: 16px; text-align: center;">
        <hr style="border-color: #30363d; margin-bottom: 16px;" />
        <a :href="githubLoginUrl" class="btn" style="display: inline-block; padding: 8px 16px;">
          Login with GitHub
        </a>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { fetchProviders } from '@/api/client'
import type { Providers } from '@/api/types'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

const username = ref('')
const password = ref('')
const error = ref('')
const loading = ref(false)
const providers = ref<Providers>({ internal: true, oauth_github: false })

const errorQuery = computed(() => route.query.error === 'oauth')
const githubLoginUrl = computed(() => `/api/v1/auth/oauth/github/login?redirect=${encodeURIComponent(redirectTarget.value)}`)
// The redirect query param is attacker-influenceable (login links); only
// internal absolute paths are accepted (CWE-601). The backend re-validates
// for the OAuth flow.
const redirectTarget = computed(() => {
  const raw = route.query.redirect as string | undefined
  if (!raw || !raw.startsWith('/') || raw.startsWith('//') || raw.includes('\\')) {
    return '/pipelines'
  }
  return raw
})

onMounted(async () => {
  try {
    providers.value = await fetchProviders()
  } catch {
    // ignore — defaults keep internal auth visible
  }
})

async function handleLogin() {
  if (!username.value || !password.value) {
    error.value = 'Username and password are required'
    return
  }
  error.value = ''
  loading.value = true
  try {
    await auth.login(username.value, password.value)
    router.push(redirectTarget.value)
  } catch {
    error.value = 'Invalid username or password'
  } finally {
    loading.value = false
  }
}
</script>
