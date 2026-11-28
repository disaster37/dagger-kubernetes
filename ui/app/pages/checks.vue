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
              <th v-for="key in visibleFilterColumns" :key="key">{{ key }}</th>
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
              <td class="py-1">
                <div class="space-y-1">
                  <div v-for="check in run.checks || []" :key="check.name">
                    <div class="flex flex-wrap items-center gap-1">
                      <NuxtLink
                        :to="check.trace_id ? `/pipelines/${check.trace_id}` : '#'"
                        class="rounded bg-elevated px-1.5 py-0.5"
                      >
                        {{ check.name }}
                      </NuxtLink>
                      <UBadge :color="checkColor(check.state)" variant="soft" size="xs">{{ check.state }}</UBadge>
                      <UButton
                        v-if="check.tests && check.tests.length"
                        size="xs"
                        variant="ghost"
                        @click="toggleTests(run.id, check.name)"
                      >
                        {{ isTestsOpen(run.id, check.name) ? 'Hide' : `${check.tests.length} tests` }}
                      </UButton>
                      <UBadge v-if="testFailures(check)" color="error" variant="soft" size="xs">
                        {{ testFailures(check) }} failed
                      </UBadge>
                    </div>
                    <div v-if="isTestsOpen(run.id, check.name) && check.tests" class="mt-1 ml-2">
                      <table class="w-full text-left text-[12px]">
                        <tbody>
                          <tr
                            v-for="test in check.tests"
                            :key="test.span_id || test.name"
                            class="border-t border-default"
                          >
                            <td class="py-1 pr-2"><code>{{ test.name }}</code></td>
                            <td class="pr-2 text-muted">{{ test.suite }}</td>
                            <td class="pr-2 text-muted">{{ test.file }}</td>
                            <td class="pr-2 text-muted">{{ test.duration_ms ? `${test.duration_ms} ms` : '' }}</td>
                            <td class="pr-2">
                              <UBadge :color="testColor(test.status)" variant="soft" size="xs">{{ test.status }}</UBadge>
                            </td>
                            <td class="text-right">
                              <UButton size="xs" variant="soft" @click="rerunTest(run.id, test.name)">Re-run</UButton>
                            </td>
                          </tr>
                        </tbody>
                      </table>
                    </div>
                  </div>
                  <details v-if="overflowFilterColumns(run).length" class="text-[12px] text-muted">
                    <summary>{{ overflowFilterColumns(run).length }} more filter columns</summary>
                    <div v-for="key in overflowFilterColumns(run)" :key="key" class="ml-2">
                      <span class="text-muted">{{ key }}:</span> {{ filterCell(run, key) }}
                    </div>
                  </details>
                </div>
              </td>
              <td v-for="key in visibleFilterColumns" :key="key" class="py-1 text-muted">{{ filterCell(run, key) }}</td>
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
import type { CheckItem, CheckRunRecord, Org } from '~/api/types'

// MAX_FILTER_COLUMNS caps the typed-artifact columns shown in the table; any
// further keys collapse into the per-run details popover so a wide v1.0
// `dagger check -l --all` table never breaks the layout.
const MAX_FILTER_COLUMNS = 4

const orgs = ref<Org[]>([])
const org = ref('default')
const status = ref('all')
const repo = ref('')
const runs = ref<CheckRunRecord[]>([])
const error = ref('')
const includeSecrets = ref(false)
const openTests = ref<Record<string, boolean>>({})
const statuses = ['all', 'waiting_approval', 'running', 'success', 'failed']
const orgOptions = computed(() => orgs.value.map(o => ({ label: o.name, value: o.id })))

function filterKeysFor(run: CheckRunRecord): string[] {
  const keys = new Set<string>()
  for (const check of run.checks || []) {
    for (const key of Object.keys(check.filters || {})) keys.add(key)
  }
  return [...keys].sort()
}
const filterColumns = computed(() => {
  const keys = new Set<string>()
  for (const run of runs.value) {
    for (const key of filterKeysFor(run)) keys.add(key)
  }
  return [...keys].sort()
})
const visibleFilterColumns = computed(() => filterColumns.value.slice(0, MAX_FILTER_COLUMNS))
function overflowFilterColumns(run: CheckRunRecord): string[] {
  return filterKeysFor(run).filter(key => !visibleFilterColumns.value.includes(key))
}
function filterCell(run: CheckRunRecord, key: string): string {
  return (run.checks || [])
    .filter(check => check.filters?.[key])
    .map(check => `${check.name}: ${check.filters?.[key]}`)
    .join(' · ')
}

function statusColor(s: string) {
  if (s === 'success') return 'success'
  if (s === 'failed') return 'error'
  if (s === 'waiting_approval') return 'warning'
  return 'neutral'
}
function checkColor(s: string) {
  if (s === 'success') return 'success'
  if (s === 'failure' || s === 'error') return 'error'
  return 'neutral'
}
function testColor(s: string) {
  if (s === 'passed') return 'success'
  if (s === 'failed') return 'error'
  return 'neutral'
}
function testFailures(check: CheckItem): number {
  return (check.tests || []).filter(test => test.status === 'failed').length
}

function testsKey(runID: string, checkName: string) {
  return `${runID}/${checkName}`
}
function isTestsOpen(runID: string, checkName: string) {
  return openTests.value[testsKey(runID, checkName)] === true
}
function toggleTests(runID: string, checkName: string) {
  const key = testsKey(runID, checkName)
  openTests.value[key] = !openTests.value[key]
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
async function rerunTest(id: string, testName: string) {
  await rerunCheckRun(id, { tests: [testName] })
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
