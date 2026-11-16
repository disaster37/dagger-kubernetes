<template>
  <div class="mx-auto max-w-2xl">
    <h1 class="mb-2 text-2xl font-semibold capitalize">{{ provider }} integration</h1>
    <p class="mb-6 text-[13px] text-muted">Connect {{ org }} to a {{ provider }} repository.</p>
    <UCard>
      <form class="space-y-4" @submit.prevent="save">
        <UFormField label="Repository (owner/repo)">
          <UInput v-model="repo" class="w-full" />
        </UFormField>
        <UFormField label="API base URL (optional)">
          <UInput v-model="baseUrl" class="w-full" placeholder="https://api.github.com" />
        </UFormField>
        <UFormField label="Access token">
          <UInput v-model="token" type="password" class="w-full" />
        </UFormField>
        <UFormField label="Webhook secret">
          <UInput v-model="secret" type="password" class="w-full" />
        </UFormField>
        <UAlert v-if="error" color="error" variant="soft" :description="error" />
        <UAlert v-if="done" color="success" variant="soft" description="Integration saved and webhook registered." />
        <UButton type="submit" :loading="loading">Save integration</UButton>
      </form>
    </UCard>
  </div>
</template>

<script setup lang="ts">
import api from '~/api/client'

const route = useRoute()
const org = computed(() => String(route.params.org))
const provider = computed(() => String(route.params.provider))
const repo = ref('')
const baseUrl = ref('')
const token = ref('')
const secret = ref('')
const loading = ref(false)
const error = ref('')
const done = ref(false)

async function save() {
  loading.value = true
  error.value = ''
  done.value = false
  try {
    await api.post(`/api/v1/orgs/${org.value}/integrations`, {
      provider: provider.value,
      repo: repo.value,
      base_url: baseUrl.value,
      token: token.value,
      webhook_secret: secret.value,
    })
    done.value = true
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to save integration'
  } finally {
    loading.value = false
  }
}
</script>
