<template>
  <div class="mx-auto mt-20 max-w-[400px]">
    <!-- group_required info card — shown above the login form so it is immediately visible -->
    <UAlert
      v-if="errorCode === 'group_required'"
      class="mb-5"
      color="info"
      variant="soft"
      icon="i-lucide-lock"
      title="Access Restricted"
      description="Your account was authenticated successfully, but you are not a member of any authorized group. Please contact your administrator to request access."
    />

    <UCard>
      <h2 class="mb-4 text-xl font-semibold">Login</h2>
      <UAlert
        v-if="errorCode === 'oauth'"
        class="mb-3"
        color="error"
        variant="soft"
        description="OAuth login failed. Please try again."
      />
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
      <div v-if="providers.oauth_github" class="mt-4 text-center">
        <USeparator class="mb-4" />
        <!-- `external` forces a full-page navigation: these are backend OAuth
             endpoints that redirect to the provider, not SPA routes. Without it
             NuxtLink would intercept the click and the auth middleware would
             bounce back to /auth/login. -->
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
import type { Providers } from '~/api/types'

definePageMeta({ public: true })

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
