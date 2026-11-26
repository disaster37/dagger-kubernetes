<template>
  <div class="mx-auto max-w-xl">
    <UCard v-if="loading">
      <div class="flex items-center gap-3 py-8">
        <UIcon name="i-lucide-loader-circle" class="size-5 animate-spin" />
        <span class="text-[13px]">Completing GitHub connection…</span>
      </div>
    </UCard>

    <UCard v-else-if="mismatch">
      <template #header><h1 class="font-semibold">Wrong GitHub account session</h1></template>
      <UAlert color="warning" variant="soft" :description="mismatchMessage" class="mb-4" />
      <p class="mb-4 text-[13px] text-muted">
        This authorization was left untouched. Signing in as the original user lets you complete it.
      </p>
      <UButton color="primary" :loading="signingOut" @click="switchUser">
        Sign out and continue as {{ mismatch.expected_username }}
      </UButton>
    </UCard>

    <UCard v-else-if="error">
      <template #header><h1 class="font-semibold">GitHub connection failed</h1></template>
      <UAlert color="error" variant="soft" :description="error" class="mb-4" />
      <UButton to="/pipelines" variant="soft">Back to pipelines</UButton>
    </UCard>

    <UCard v-else-if="result">
      <template #header><h1 class="font-semibold">Connected to GitHub</h1></template>
      <div class="space-y-4 text-[13px]">
        <p>
          Connected as
          <UBadge color="success" variant="soft">@{{ result.connection.github_login }}</UBadge>
        </p>
        <p v-if="result.auto_connected_org" class="text-muted">
          Linked to {{ result.auto_connected_org.owner }} for organization
          <span class="font-semibold">{{ result.auto_connected_org.org_id }}</span>.
        </p>
        <UAlert
          color="info"
          variant="soft"
          description="You can now return to your terminal and run `dagger cloud checks on`."
        />
        <div class="flex flex-wrap gap-2">
          <UButton v-if="targetOrg" :to="`/orgs/${targetOrg}/integrations/github?connected=1`">
            Select repositories & configure triggers →
          </UButton>
          <UButton to="/pipelines" variant="soft">Back to pipelines</UButton>
        </div>
      </div>
    </UCard>
  </div>
</template>

<script setup lang="ts">
import { authLoginTarget } from '~/utils/auth-navigation'
import { completeGitHubOAuth } from '~/api/client'
import type { GitHubOAuthCompleteResult } from '~/api/types'

interface StateMismatch {
  code: 'state_user_mismatch'
  expected_username: string
  current_username: string
}

const route = useRoute()
const auth = useAuthStore()
const result = ref<GitHubOAuthCompleteResult | null>(null)
const mismatch = ref<StateMismatch | null>(null)
const error = ref('')
const loading = ref(true)
const signingOut = ref(false)
const started = ref(false)

const mismatchMessage = computed(() =>
  mismatch.value
    ? `This GitHub authorization was started by ${mismatch.value.expected_username}. You are signed in as ${mismatch.value.current_username}.`
    : '',
)
const targetOrg = computed(() => result.value?.target_org_slug || result.value?.admin_orgs?.[0]?.name || '')

function safeErrorText(): string {
  if (route.query.error) {
    return 'GitHub authorization was canceled or denied.'
  }
  return 'The GitHub connection could not be completed. Please try again.'
}

async function complete() {
  const code = typeof route.query.code === 'string' ? route.query.code : ''
  const state = typeof route.query.state === 'string' ? route.query.state : ''
  if (!code || !state) {
    error.value = safeErrorText()
    loading.value = false
    return
  }
  try {
    const data = await completeGitHubOAuth(code, state)
    result.value = data
    if (data.target_org_slug) {
      await navigateTo(`/orgs/${data.target_org_slug}/integrations/github?connected=1`)
    }
  } catch (e: any) {
    const data = e?.response?.data
    if (data?.code === 'state_user_mismatch') {
      mismatch.value = {
        code: 'state_user_mismatch',
        expected_username: String(data.expected_username ?? ''),
        current_username: String(data.current_username ?? ''),
      }
    } else {
      error.value = safeErrorText()
    }
  } finally {
    loading.value = false
  }
}

async function switchUser() {
  signingOut.value = true
  try {
    await auth.logout()
  } finally {
    signingOut.value = false
    await navigateTo(authLoginTarget('session', route.fullPath))
  }
}

onMounted(async () => {
  if (started.value) return
  started.value = true
  await complete()
})
</script>
