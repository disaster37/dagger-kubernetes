<template>
  <div class="mx-auto max-w-3xl">
    <h1 class="mb-2 text-2xl font-semibold capitalize">{{ provider }} integration</h1>
    <p class="mb-6 text-[13px] text-muted">Connect {{ org }} to a {{ provider }} repository.</p>
    <UAlert v-if="error" color="error" variant="soft" :description="error" class="mb-4" />
    <UCard class="mb-4">
      <template #header><h2 class="font-semibold">Existing integrations</h2></template>
      <div v-for="src in sources" :key="src.id" class="mb-2 flex flex-wrap items-center justify-between gap-2 rounded border border-default p-2">
        <div class="text-[13px]">
          <p class="font-semibold">{{ src.repo }}</p>
          <p class="text-muted">webhook: <code>/v1/webhooks/{{ src.provider }}/{{ src.id }}</code></p>
          <p v-if="src.app_id" class="text-muted">app id: {{ src.app_id }} · installation: {{ src.installation_id }}</p>
        </div>
        <UButton size="xs" color="error" variant="soft" @click="remove(src.id)">Delete</UButton>
      </div>
      <UEmpty v-if="!sources.length" icon="i-lucide-plug" title="No integrations" description="Add one below." />
    </UCard>
    <UCard>
      <template #header><h2 class="font-semibold">Add integration</h2></template>
      <form class="space-y-4" @submit.prevent="save">
        <UFormField label="Repository (owner/repo)"><UInput v-model="repo" class="w-full" /></UFormField>
        <UFormField label="API base URL (optional)"><UInput v-model="baseUrl" class="w-full" placeholder="https://api.github.com" /></UFormField>
        <UFormField label="Access token"><UInput v-model="token" type="password" class="w-full" /></UFormField>
        <UFormField label="Webhook secret" help="Auto-generated if empty."><UInput v-model="secret" type="password" class="w-full" placeholder="auto-generated" /></UFormField>
        <div v-if="provider === 'github'" class="grid gap-3 md:grid-cols-2">
          <UFormField label="GitHub App ID (optional)"><UInput v-model="appID" class="w-full" /></UFormField>
          <UFormField label="Installation ID (optional)"><UInput v-model="installationID" class="w-full" /></UFormField>
        </div>
        <UAlert v-if="done" color="success" variant="soft" description="Integration saved and webhook registered." />
        <UButton type="submit" :loading="loading">Save integration</UButton>
      </form>
    </UCard>
  </div>
</template>

<script setup lang="ts">
import { createForgeSource, deleteForgeSource, listForgeSources } from '~/api/client'
import type { ForgeSource } from '~/api/types'

const route = useRoute()
const org = computed(() => String(route.params.org))
const provider = computed(() => String(route.params.provider))
const sources = ref<ForgeSource[]>([])
const repo = ref('')
const baseUrl = ref('')
const token = ref('')
const secret = ref('')
const appID = ref('')
const installationID = ref('')
const loading = ref(false)
const error = ref('')
const done = ref(false)

async function load() {
  try {
    sources.value = (await listForgeSources(org.value)).filter(s => s.provider === provider.value)
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to load integrations'
  }
}
async function save() {
  loading.value = true
  error.value = ''
  done.value = false
  try {
    await createForgeSource(org.value, {
      provider: provider.value,
      repo: repo.value,
      base_url: baseUrl.value,
      token: token.value,
      webhook_secret: secret.value,
      app_id: appID.value || undefined,
      installation_id: installationID.value || undefined,
    })
    done.value = true
    repo.value = ''
    token.value = ''
    secret.value = ''
    await load()
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to save integration'
  } finally {
    loading.value = false
  }
}
async function remove(id: string) {
  await deleteForgeSource(org.value, id)
  await load()
}

onMounted(load)
</script>
