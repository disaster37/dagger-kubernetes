<template>
  <div class="mx-auto mt-20 max-w-[400px]">
    <!-- One stable alert for every callback/redirect error hint. -->
    <UAlert
      v-if="errorMessage"
      class="mb-5"
      :color="errorCode === 'group_required' ? 'info' : 'error'"
      variant="soft"
      :icon="errorCode === 'group_required' ? 'i-lucide-lock' : 'i-lucide-circle-alert'"
      :title="errorCode === 'group_required' ? 'Access Restricted' : 'Sign-in problem'"
      :description="errorMessage"
    />

    <div class="mb-6 flex justify-center">
      <AppLogo class="size-16 text-primary" />
    </div>

    <UCard>
      <h2 class="mb-4 text-xl font-semibold">Login</h2>
      <UForm @submit="handleLogin">
        <div class="mb-3">
          <label class="mb-1 block text-sm">Username</label>
          <UInput
            v-model="username"
            type="text"
            autocomplete="username"
            placeholder="username"
            class="w-full"
          />
        </div>
        <div class="mb-4">
          <label class="mb-1 block text-sm">Password</label>
          <UInput
            v-model="password"
            type="password"
            autocomplete="current-password"
            placeholder="password"
            class="w-full"
          />
        </div>
        <UButton type="submit" color="primary" block :loading="loading">
          {{ loading ? 'Signing in…' : 'Sign in' }}
        </UButton>
      </UForm>
      <UAlert
        v-if="error"
        class="mt-3"
        color="error"
        variant="soft"
        :description="error"
      />
      <UAlert
        v-if="providersError"
        class="mt-3"
        color="warning"
        variant="soft"
        :description="providersError"
      />
      <div v-if="providers.oauth_github" class="mt-4 text-center">
        <USeparator class="mb-4" />
        <!-- `external` forces a full-page navigation: these are backend OAuth
             endpoints that redirect to the provider, not SPA routes. -->
        <UButton :to="githubLoginUrl" external color="neutral" variant="outline" block>
          Login with GitHub
        </UButton>
      </div>
      <div v-if="providers.oauth_oidc" class="mt-4 text-center">
        <USeparator class="mb-4" />
        <UButton :to="oidcLoginUrl" external color="neutral" variant="outline" block>
          Login with OIDC
        </UButton>
      </div>
    </UCard>
  </div>
</template>

<script setup lang="ts">
import { useAuthStore } from '~/stores/auth'
import { fetchProviders } from '~/api/client'
import { authErrorMessage, safeAuthRedirect } from '~/utils/auth-navigation'
import type { Providers } from '~/api/types'

definePageMeta({ public: true })

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

const username = ref('')
const password = ref('')
const error = ref('')
const providersError = ref('')
const loading = ref(false)
const providers = ref<Providers>({ internal: true, oauth_github: false, oauth_oidc: false })

const errorCode = computed(() => {
  const raw = route.query.error
  return Array.isArray(raw) ? String(raw[0] ?? '') : (typeof raw === 'string' ? raw : '')
})
const errorMessage = computed(() => authErrorMessage(errorCode.value))
// The redirect query param is attacker-influenceable (login links); only
// internal absolute paths are accepted (CWE-601). The backend re-validates for
// the OAuth flow.
const redirectTarget = computed(() => safeAuthRedirect(route.query.redirect))
const githubLoginUrl = computed(() => `/api/v1/auth/oauth/github/login?redirect=${encodeURIComponent(redirectTarget.value)}`)
const oidcLoginUrl = computed(() => `/api/v1/auth/oauth/oidc/login?redirect=${encodeURIComponent(redirectTarget.value)}`)

onMounted(async () => {
  try {
    providers.value = await fetchProviders()
  } catch {
    // Keep internal auth visible; surface the discovery failure on screen.
    providersError.value = 'Could not load sign-in providers. Internal sign-in may still work.'
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
    await router.push(redirectTarget.value)
  } catch (e: any) {
    // A credential failure is generic; a transport/internal failure must not be
    // misreported as bad credentials and must stay on the form.
    error.value = e?.response
      ? 'Invalid username or password'
      : 'Sign-in is temporarily unavailable. Please try again.'
  } finally {
    loading.value = false
  }
}
</script>
