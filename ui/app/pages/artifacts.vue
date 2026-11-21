<template>
  <div>
    <h1 class="mb-6 text-2xl font-semibold">Artifacts</h1>
    <UAlert v-if="error" color="error" variant="soft" :description="error" class="mb-4" />
    <UCard>
      <div class="mb-4 flex flex-wrap items-end gap-3">
        <UFormField label="Organization">
          <USelect v-model="org" :items="orgOptions" @update:model-value="load" />
        </UFormField>
        <UFormField label="Project"><UInput v-model="project" placeholder="optional" /></UFormField>
        <UFormField label="Run"><UInput v-model="run" placeholder="optional" /></UFormField>
        <UButton @click="load">Load</UButton>
      </div>
      <table class="w-full text-left text-[13px]">
        <thead class="text-muted"><tr><th class="py-2">Artifact</th><th>Run</th><th /></tr></thead>
        <tbody>
          <tr v-for="key in keys" :key="key" class="border-t border-default">
            <td class="py-2"><code>{{ artifactName(key) }}</code></td>
            <td>{{ artifactRun(key) }}</td>
            <td class="text-right"><UButton size="xs" @click="download(key)">Download</UButton></td>
          </tr>
        </tbody>
      </table>
      <UEmpty v-if="!keys.length" icon="i-lucide-package" title="No artifacts" description="Workflow jobs upload declared artifacts here." />
    </UCard>
  </div>
</template>

<script setup lang="ts">
import { downloadArtifactBlob, listArtifacts, listOrgs } from '~/api/client'
import type { Org } from '~/api/types'

const orgs = ref<Org[]>([])
const org = ref('default')
const project = ref('')
const run = ref('')
const keys = ref<string[]>([])
const error = ref('')
const orgOptions = computed(() => orgs.value.map(o => ({ label: o.name, value: o.id })))

function artifactName(key: string) {
  return key.split('/').at(-1) ?? key
}
function artifactRun(key: string) {
  const parts = key.split('/')
  return parts.length >= 4 ? parts[3] : ''
}
async function load() {
  error.value = ''
  try {
    keys.value = await listArtifacts({ org: org.value, project: project.value || undefined, run: run.value || undefined })
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to load artifacts'
  }
}
async function download(key: string) {
  const blob = await downloadArtifactBlob(key)
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = artifactName(key)
  a.click()
  URL.revokeObjectURL(url)
}

onMounted(async () => {
  try {
    orgs.value = await listOrgs()
    if (orgs.value.length) org.value = orgs.value[0]?.id ?? 'default'
  } catch {
    orgs.value = []
  }
  await load()
})
</script>
