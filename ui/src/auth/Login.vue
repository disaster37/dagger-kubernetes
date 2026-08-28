<template>
  <div style="max-width: 400px; margin: 80px auto;">
    <!-- group_required info card — shown above the login form so it is immediately visible -->
    <div v-if="errorCode === 'group_required'" style="background: #1a2332; border: 1px solid #58a6ff; border-radius: 8px; padding: 20px 24px; margin-bottom: 20px;">
      <div style="display: flex; align-items: flex-start; gap: 12px;">
        <span style="font-size: 24px; line-height: 1.4;">🔒</span>
        <div>
          <div style="font-weight: 600; font-size: 15px; color: #58a6ff; margin-bottom: 6px;">Access Restricted</div>
          <p style="color: #c9d1d9; font-size: 13px; line-height: 1.6; margin: 0;">
            Your account was authenticated successfully, but you are not a member of any authorized group. Please contact your administrator to request access.
          </p>
        </div>
      </div>
    </div>
    <div class="card">
      <h2 style="margin-bottom: 16px;">Login</h2>
      <p v-if="errorCode === 'oauth'" style="color: #f85149; font-size: 13px; margin-bottom: 12px;">
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
      <div v-if="providers.oauth_oidc" style="margin-top: 16px; text-align: center;">
        <hr style="border-color: #30363d; margin-bottom: 16px;" />
        <a :href="oidcLoginUrl" class="btn" style="display: inline-block; padding: 8px 16px;">
          Login with OIDC
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
const providers = ref<Providers>({ internal: true, oauth_github: false, oauth_oidc: false })

const errorCode = computed(() => (route.query.error as string) || null)
const githubLoginUrl = computed(() => `/api/v1/auth/oauth/github/login?redirect=${encodeURIComponent(redirectTarget.value)}`)
const oidcLoginUrl = computed(() => `/api/v1/auth/oauth/oidc/login?redirect=${encodeURIComponent(redirectTarget.value)}`)
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
