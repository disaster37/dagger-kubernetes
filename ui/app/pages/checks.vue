<template>
  <div>
    <h1 class="mb-6 text-2xl font-semibold">Cloud Checks</h1>
    <UAlert v-if="error" color="error" variant="soft" :description="error" class="mb-4" />
    <UCard>
      <div class="mb-4 flex flex-wrap items-end gap-3">
        <UFormField label="Organization">
          <USelect v-model="org" :items="orgOptions" @update:model-value="load" />
        </UFormField>
        <UFormField label="Status">
          <USelect v-model="status" :items="statuses" @update:model-value="load" />
        </UFormField>
        <UFormField label="Repository">
          <UInput v-model="repo" placeholder="acme/repo" @keyup.enter="load" />
        </UFormField>
        <UButton @click="load">Refresh</UButton>
      </div>
      <div class="overflow-x-auto">
        <table class="w-full text-left text-[13px]">
          <thead class="text-muted">
            <tr>
              <th class="py-2">Commit</th>
              <th>Event</th>
              <th>Repo</th>
              <th>Status</th>
              <th>Checks</th>
              <th />
            </tr>
          </thead>
          <tbody>
            <tr v-for="run in runs" :key="run.id" class="border-t border-default">
              <td class="py-2"><code>{{ (run.sha || '').slice(0, 8) }}</code></td>
              <td>
                {{ run.event }}<span v-if="run.pr_number"> #{{ run.pr_number }}</span>
                <UBadge v-if="run.is_fork" color="warning" variant="soft" size="xs" class="ml-1">fork</UBadge>
              </td>
              <td>{{ run.repo }}</td>
              <td><UBadge :color="statusColor(run.status)" variant="soft">{{ run.status }}</UBadge></td>
              <td>
                <div class="flex flex-wrap gap-1">
                  <NuxtLink
                    v-for="check in run.checks || []"
                    :key="check.name"
                    :to="check.trace_id ? `/pipelines/${check.trace_id}` : '#'"
                    class="rounded bg-elevated px-1.5 py-0.5"
                  >
                    {{ check.name }}: {{ check.state }}
                  </NuxtLink>
                </div>
              </td>
              <td class="text-right">
                <div v-if="run.status === 'waiting_approval'" class="flex items-center justify-end gap-2">
                  <UCheckbox v-model="includeSecrets" label="Include secrets" />
                  <UButton size="xs" @click="approve(run.id)">Approve Fork PR</UButton>
                </div>
                <UButton v-else size="xs" variant="soft" @click="rerun(run.id)">Rerun</UButton>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <UEmpty v-if="!runs.length" icon="i-lucide-check-circle" title="No check runs" description="Webhooks from configured forge integrations appear here." />
    </UCard>
  </div>
</template>

<script setup lang="ts">
import { approveForkCheckRun, listOrgChecks, listOrgs, rerunCheckRun } from '~/api/client'
import type { CheckRunRecord, Org } from '~/api/types'

const orgs = ref<Org[]>([])
const org = ref('default')
const status = ref('all')
const repo = ref('')
const runs = ref<CheckRunRecord[]>([])
const error = ref('')
const includeSecrets = ref(false)
const statuses = ['all', 'waiting_approval', 'running', 'success', 'failed']
const orgOptions = computed(() => orgs.value.map(o => ({ label: o.name, value: o.id })))

function statusColor(s: string) {
  if (s === 'success') return 'success'
  if (s === 'failed') return 'error'
  if (s === 'waiting_approval') return 'warning'
  return 'neutral'
}

async function load() {
  error.value = ''
  try {
    runs.value = await listOrgChecks(org.value, {
      status: status.value === 'all' ? undefined : status.value,
      repo: repo.value || undefined,
    })
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to load check runs'
  }
}
async function approve(id: string) {
  await approveForkCheckRun(id, includeSecrets.value)
  includeSecrets.value = false
  await load()
}
async function rerun(id: string) {
  await rerunCheckRun(id)
  await load()
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
