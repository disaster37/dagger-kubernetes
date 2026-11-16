<template>
  <div>
    <h1 class="mb-6 text-2xl font-semibold">Artifacts</h1>
    <UAlert v-if="error" color="error" variant="soft" :description="error" />
    <UCard>
      <form class="flex flex-wrap items-end gap-3" @submit.prevent="load">
        <UFormField label="Organization"><UInput v-model="org" /></UFormField>
        <UFormField label="Project"><UInput v-model="project" /></UFormField>
        <UFormField label="Run"><UInput v-model="run" /></UFormField>
        <UButton type="submit">Load</UButton>
      </form>
      <ul class="mt-4 space-y-1 text-[13px]">
        <li v-for="key in keys" :key="key"><code>{{ key }}</code></li>
      </ul>
    </UCard>
  </div>
</template>

<script setup lang="ts">
import api from '~/api/client'

const org = ref('default')
const project = ref('')
const run = ref('')
const keys = ref<string[]>([])
const error = ref('')

async function load() {
  error.value = ''
  try {
    const { data } = await api.get('/api/v1/artifacts', { params: { org: org.value, project: project.value, run: run.value } })
    keys.value = data
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to load artifacts'
  }
}
</script>
