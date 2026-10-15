<template>
  <div class="mx-auto mt-20 max-w-[400px] text-center">
    <UIcon name="i-lucide-loader-circle" class="size-8 animate-spin text-primary" />
    <p class="mt-3">Authenticating…</p>
  </div>
</template>

<script setup lang="ts">
import { useAuthStore } from '~/stores/auth'
import { authLoginTarget, safeAuthRedirect } from '~/utils/auth-navigation'

definePageMeta({ public: true })

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

onMounted(async () => {
  // The server set the session cookies and redirected here with ?redirect=<path>;
  // no tokens are in the URL. callback=true uses recovery mode 'none' and a new
  // auth epoch so the fresh callback credentials are never replaced by an old
  // refresh flow.
  const ok = await auth.loadUser({ callback: true })
  if (ok) {
    // The redirect query is attacker-influenceable; only internal paths are
    // accepted (CWE-601). Mirror the backend validation.
    await router.replace(safeAuthRedirect(route.query.redirect))
    return
  }
  // The callback itself may have succeeded but the session could not be
  // restored. Show a stable, explicit error instead of spinning forever.
  await router.replace(authLoginTarget('session', route.query.redirect))
})
</script>
