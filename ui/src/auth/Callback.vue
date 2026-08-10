<template>
  <div style="max-width: 400px; margin: 80px auto; text-align: center;">
    <p>Authenticating…</p>
  </div>
</template>

<script setup lang="ts">
import { onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'

const router = useRouter()
const auth = useAuthStore()

// safeRedirect mirrors the backend safeRedirectPath: the fragment is
// attacker-influenceable (anyone can link to /auth/callback#...), so only
// internal absolute paths are accepted (CWE-601).
function safeRedirect(path: string | null): string {
  if (!path || !path.startsWith('/') || path.startsWith('//') || path.includes('\\')) {
    return '/pipelines'
  }
  return path
}

onMounted(async () => {
  const params = new URLSearchParams(window.location.hash.slice(1))
  const accessToken = params.get('access_token')
  const refreshToken = params.get('refresh_token')
  const redirect = safeRedirect(params.get('redirect'))

  if (accessToken && refreshToken) {
    auth.setTokens(accessToken, refreshToken)
    await auth.fetchMe()
    // Strip the fragment so the token is not left in the URL/history.
    history.replaceState(null, '', '/auth/callback')
    router.push(redirect)
  } else {
    router.push('/auth/login?error=oauth')
  }
})
</script>
