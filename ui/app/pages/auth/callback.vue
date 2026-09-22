<template>
  <div class="mx-auto mt-20 max-w-[400px] text-center">
    <UIcon name="i-lucide-loader-circle" class="size-8 animate-spin text-primary" />
    <p class="mt-3">Authenticating…</p>
  </div>
</template>

<script setup lang="ts">
import { useAuthStore } from '~/stores/auth'

definePageMeta({ public: true })

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

// safeRedirect mirrors the backend safeRedirectPath: the query redirect is
// attacker-influenceable (anyone can link to /auth/callback?redirect=...), so
// only internal absolute paths are accepted (CWE-601).
function safeRedirect(path: string | null): string {
  if (!path || !path.startsWith('/') || path.startsWith('//') || path.includes('\\')) {
    return '/pipelines'
  }
  return path
}

onMounted(async () => {
  // The server set the session cookies and redirected here with ?redirect=<path>;
  // no tokens are in the URL. loadUser() reads /me via the cookie.
  await auth.loadUser()
  if (auth.isAuthenticated) {
    router.push(safeRedirect(route.query.redirect as string | null))
  } else {
    router.push('/auth/login?error=oauth')
  }
})
</script>
