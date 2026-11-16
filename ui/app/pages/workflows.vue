<template>
  <div>
    <h1 class="mb-6 text-2xl font-semibold">Workflows</h1>
    <UAlert v-if="error" color="error" variant="soft" :description="error" class="mb-4" />
    <div class="mb-4 flex flex-wrap items-end gap-3">
      <UFormField label="Organization">
        <USelect v-model="org" :items="orgOptions" @update:model-value="load" />
      </UFormField>
      <UButton @click="load">Refresh</UButton>
      <UButton icon="i-lucide-play" @click="showDispatch = true">Dispatch workflow</UButton>
    </div>

    <UCard class="mb-4">
      <template #header><h2 class="font-semibold">Approval queue</h2></template>
      <div v-for="item in approvals" :key="`${item.run.id}/${item.job.name}`" class="mb-2 flex flex-wrap items-center justify-between gap-2 rounded border border-default p-2">
        <div>
          <p class="font-semibold">{{ item.run.workflow }} #{{ item.run.number }} · {{ item.job.name }}</p>
          <p class="text-[13px] text-muted">
            environment: <UBadge color="warning" variant="soft" size="xs">{{ item.job.environment }}</UBadge>
            <span v-if="item.job.approval_deadline"> · deadline {{ new Date(item.job.approval_deadline).toLocaleString() }}</span>
          </p>
        </div>
        <div class="flex gap-2">
          <UButton size="xs" @click="approve(item.run.id, item.job.name)">Approve</UButton>
          <UButton size="xs" color="error" variant="soft" @click="reject(item.run.id, item.job.name)">Reject</UButton>
        </div>
      </div>
      <UEmpty v-if="!approvals.length" icon="i-lucide-shield-check" title="No pending approvals" description="Protected environment gates appear here." />
    </UCard>

    <UCard>
      <template #header><h2 class="font-semibold">Runs</h2></template>
      <div v-for="run in runs" :key="run.id" class="mb-2 rounded border border-default p-2">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <div>
            <p class="font-semibold">{{ run.workflow }} #{{ run.number }}</p>
            <p class="text-[13px] text-muted">{{ run.sha.slice(0, 8) }} · {{ run.ref }} · {{ run.status }}</p>
          </div>
          <UButton size="xs" variant="soft" @click="toggle(run.id)">{{ expanded[run.id] ? 'Hide' : 'Jobs' }}</UButton>
        </div>
        <div v-if="expanded[run.id]" class="mt-2 space-y-1 text-[13px]">
          <div v-for="job in Object.values(run.jobs)" :key="job.name" class="rounded bg-elevated p-2">
            <div class="flex flex-wrap items-center gap-2">
              <span class="font-semibold">{{ job.name }}</span>
              <UBadge :color="job.status === 'success' ? 'success' : job.status === 'failed' ? 'error' : 'neutral'" variant="soft" size="xs">{{ job.status }}</UBadge>
              <NuxtLink v-if="job.trace_id" :to="`/pipelines/${job.trace_id}`" class="underline">pipeline</NuxtLink>
              <NuxtLink v-if="job.triggered_run_id" :to="`/workflows?run=${job.triggered_run_id}`" class="underline">triggered run</NuxtLink>
              <span v-if="job.approved_by" class="text-muted">approved by {{ job.approved_by }}</span>
            </div>
            <pre v-if="job.outputs && Object.keys(job.outputs).length" class="mt-1 whitespace-pre-wrap">{{ JSON.stringify(job.outputs, null, 2) }}</pre>
            <p v-if="job.error" class="text-error">{{ job.error }}</p>
          </div>
        </div>
      </div>
      <UEmpty v-if="!runs.length" icon="i-lucide-workflow" title="No workflow runs" description="Workflows run after checks succeed or on manual dispatch." />
    </UCard>

    <UModal v-model:open="showDispatch">
      <UCard>
        <template #header><h2 class="font-semibold">Dispatch workflow</h2></template>
        <div class="space-y-3">
          <USelect v-model="dispatchProject" :items="projectOptions" placeholder="Project" />
          <USelect v-model="dispatchWorkflowName" :items="workflowOptions" placeholder="Workflow" />
          <UInput v-model="dispatchRef" placeholder="refs/heads/main" />
          <div v-for="(input, name) in dispatchInputs" :key="name" class="space-y-1">
            <p class="text-[13px] font-semibold">{{ name }} <span v-if="input.required" class="text-error">*</span></p>
            <USelect v-if="input.type === 'choice'" :model-value="String(dispatchValues[name] ?? '')" :items="(input.options || []) as string[]" @update:model-value="v => dispatchValues[name] = v" />
            <USwitch v-else-if="input.type === 'boolean'" v-model="dispatchValues[name]" />
            <UInput v-else :model-value="String(dispatchValues[name] ?? '')" :type="input.type === 'number' ? 'number' : 'text'" @update:model-value="v => dispatchValues[name] = v" />
          </div>
          <UButton :loading="dispatching" @click="dispatch">Run</UButton>
        </div>
      </UCard>
    </UModal>
  </div>
</template>

<script setup lang="ts">
import { approveWorkflowJob, dispatchWorkflow, listOrgWorkflowRuns, listOrgWorkflows, listOrgs, rejectWorkflowJob } from '~/api/client'
import type { Org, WorkflowDef, WorkflowInput, WorkflowRun } from '~/api/types'

const orgs = ref<Org[]>([])
const org = ref('default')
const runs = ref<WorkflowRun[]>([])
const defs = ref<WorkflowDef[]>([])
const error = ref('')
const expanded = ref<Record<string, boolean>>({})
const showDispatch = ref(false)
const dispatching = ref(false)
const dispatchProject = ref('')
const dispatchWorkflowName = ref('')
const dispatchRef = ref('refs/heads/main')
const dispatchValues = ref<Record<string, unknown>>({})
const orgOptions = computed(() => orgs.value.map(o => ({ label: o.name, value: o.id })))
const projectOptions = computed(() => [...new Set(defs.value.map(d => d.project_id))])
const workflowOptions = computed(() => defs.value.filter(d => d.project_id === dispatchProject.value).map(d => d.workflow.name))
const dispatchInputs = computed<Record<string, WorkflowInput>>(() => {
  const def = defs.value.find(d => d.project_id === dispatchProject.value && d.workflow.name === dispatchWorkflowName.value)
  return def?.workflow.inputs || {}
})
const approvals = computed(() => {
  const out: { run: WorkflowRun; job: WorkflowRun['jobs'][string] }[] = []
  for (const run of runs.value) {
    for (const job of Object.values(run.jobs)) {
      if (job.status === 'waiting_approval') out.push({ run, job })
    }
  }
  return out
})

async function load() {
  error.value = ''
  try {
    const [r, d] = await Promise.all([listOrgWorkflowRuns(org.value), listOrgWorkflows(org.value)])
    runs.value = r
    defs.value = d
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to load workflows'
  }
}
function toggle(id: string) {
  expanded.value[id] = !expanded.value[id]
}
async function approve(runId: string, job: string) {
  await approveWorkflowJob(runId, job)
  await load()
}
async function reject(runId: string, job: string) {
  await rejectWorkflowJob(runId, job, 'rejected from UI')
  await load()
}
async function dispatch() {
  dispatching.value = true
  try {
    await dispatchWorkflow(org.value, {
      project_id: dispatchProject.value,
      workflow: dispatchWorkflowName.value,
      ref: dispatchRef.value,
      inputs: dispatchValues.value,
    })
    showDispatch.value = false
    await load()
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to dispatch workflow'
  } finally {
    dispatching.value = false
  }
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
