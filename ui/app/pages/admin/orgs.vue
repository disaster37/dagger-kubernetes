<template>
  <div>
    <div class="mb-6 flex items-center justify-between">
      <h1 class="text-2xl font-semibold">Organizations & Quotas</h1>
      <UButton icon="i-lucide-plus" @click="showCreate = true">Create organization</UButton>
    </div>
    <UAlert v-if="error" color="error" variant="soft" class="mb-4" :description="error" />

    <UCard>
      <UTable :data="orgs" :columns="columns">
        <template #name-cell="{ row }">
          <div>
            <p class="font-medium">{{ row.original.display_name || row.original.name }}</p>
            <code class="text-xs text-muted">{{ row.original.name }} · {{ row.original.id }}</code>
          </div>
        </template>
        <template #actions-cell="{ row }">
          <div class="flex gap-1">
            <UButton size="xs" variant="outline" @click="openEdit(row.original)">Edit</UButton>
            <UButton size="xs" variant="outline" @click="openQuotas(row.original)">Quotas</UButton>
            <UButton
              size="xs"
              color="error"
              variant="soft"
              :disabled="row.original.name === 'default'"
              @click="askDelete(row.original)"
            >
              Delete
            </UButton>
          </div>
        </template>
      </UTable>
    </UCard>

    <UModal v-model:open="showCreate" title="Create organization">
      <template #body>
        <div class="space-y-3">
          <UInput v-model="newOrg.name" placeholder="slug (immutable, lowercase)" />
          <UInput v-model="newOrg.display_name" placeholder="Display name" />
          <UButton :loading="saving" @click="create">Create</UButton>
        </div>
      </template>
    </UModal>

    <UModal v-model:open="editOpen" title="Edit organization">
      <template #body>
        <div class="space-y-3">
          <p class="text-sm text-muted">Slug <code>{{ editing?.name }}</code> is immutable.</p>
          <UInput v-model="editDisplay" placeholder="Display name" />
          <UButton :loading="saving" @click="saveEdit">Save</UButton>
        </div>
      </template>
    </UModal>

    <UModal v-model:open="quotaOpen" :title="`Quotas for ${editing?.name}`">
      <template #body>
        <div class="space-y-3">
          <div v-for="q in quotaFields" :key="q.key" class="grid grid-cols-3 items-center gap-2">
            <span class="text-sm">{{ q.label }}</span>
            <UInput v-model.number="quotaForm[q.key]!.soft" type="number" placeholder="soft" />
            <UInput v-model.number="quotaForm[q.key]!.hard" type="number" placeholder="hard" />
          </div>
          <UButton :loading="saving" @click="saveQuotas">Save quotas</UButton>
        </div>
      </template>
    </UModal>

    <UModal v-model:open="deleteOpen" title="Delete organization">
      <template #body>
        <p class="text-sm">Delete <strong>{{ editing?.name }}</strong>? This cascades all groups, projects, traces, fleets, and cached objects.</p>
        <UButton class="mt-3" color="error" :loading="saving" @click="confirmDelete">Delete organization</UButton>
      </template>
    </UModal>
  </div>
</template>

<script setup lang="ts">
import { createOrg, deleteOrg, getOrgQuotas, listOrgs, updateOrg, updateOrgQuotas } from '~/api/client'
import type { Org, OrgQuotas, QuotaThreshold } from '~/api/types'

definePageMeta({ admin: true })

const orgs = ref<Org[]>([])
const error = ref('')
const saving = ref(false)
const showCreate = ref(false)
const editOpen = ref(false)
const quotaOpen = ref(false)
const deleteOpen = ref(false)
const editing = ref<Org | null>(null)
const editDisplay = ref('')
const newOrg = reactive({ name: '', display_name: '' })

const quotaFields = [
  { key: 'max_groups', label: 'Groups' },
  { key: 'max_projects', label: 'Projects' },
  { key: 'max_members', label: 'Members' },
  { key: 'max_concurrent_sessions', label: 'Sessions' },
  { key: 'max_engines', label: 'Engines' },
  { key: 'max_cache_bytes', label: 'Cache bytes' },
] as const

const quotaForm = reactive<Record<string, QuotaThreshold>>({})
for (const f of quotaFields) quotaForm[f.key] = { soft: 0, hard: 0 }

const columns = [
  { accessorKey: 'name', header: 'Organization' },
  { accessorKey: 'actions', header: '' },
]

async function load() {
  error.value = ''
  try {
    orgs.value = await listOrgs()
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to load organizations'
  }
}

async function create() {
  saving.value = true
  try {
    await createOrg({ name: newOrg.name, display_name: newOrg.display_name })
    showCreate.value = false
    newOrg.name = ''
    newOrg.display_name = ''
    await load()
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to create organization'
  } finally {
    saving.value = false
  }
}

function openEdit(o: Org) {
  editing.value = o
  editDisplay.value = o.display_name
  editOpen.value = true
}
async function saveEdit() {
  if (!editing.value) return
  saving.value = true
  try {
    await updateOrg(editing.value.name, { display_name: editDisplay.value })
    editOpen.value = false
    await load()
  } finally {
    saving.value = false
  }
}

async function openQuotas(o: Org) {
  editing.value = o
  const usage = await getOrgQuotas(o.name)
  for (const f of quotaFields) {
    const eff = usage.effective[f.key]
    quotaForm[f.key] = { soft: eff.soft, hard: eff.hard }
  }
  quotaOpen.value = true
}
async function saveQuotas() {
  if (!editing.value) return
  saving.value = true
  try {
    const payload = { ...quotaForm, max_pipeline_retention: 0 } as unknown as OrgQuotas
    await updateOrgQuotas(editing.value.name, payload)
    quotaOpen.value = false
  } finally {
    saving.value = false
  }
}

function askDelete(o: Org) {
  editing.value = o
  deleteOpen.value = true
}
async function confirmDelete() {
  if (!editing.value) return
  saving.value = true
  try {
    await deleteOrg(editing.value.name, true)
    deleteOpen.value = false
    await load()
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>
