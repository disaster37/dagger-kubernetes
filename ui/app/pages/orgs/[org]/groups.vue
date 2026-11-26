<template>
  <div>
    <OrgSubnav />
    <h1 class="mb-6 text-2xl font-semibold">Groups</h1>

    <UCard class="mb-4">
      <h3 class="font-semibold">Create group</h3>
      <UForm class="mt-3 flex flex-wrap items-center gap-2" @submit="createGroup">
        <UInput v-model="newGroup.name" placeholder="name" />
        <UInput v-model.number="newGroup.max_runner_sessions" type="number" placeholder="max sessions (0=∞)" class="w-[140px]" />
        <UCheckbox v-model="newGroup.agent_available" label="agent available" />
        <UInput v-model="newGroup.auto_assign_pattern" placeholder="auto-assign regex (optional)" class="min-w-[200px] flex-1" />
        <UButton type="submit" color="primary">Create</UButton>
      </UForm>
      <UAlert v-if="createError" class="mt-2" color="error" variant="soft" :description="createError" />
    </UCard>

    <UCard>
      <div v-if="!loaded" class="py-8 text-center text-muted">…</div>
      <UTable v-else :data="groups" :columns="columns">
        <template #agent_available-cell="{ row }">
          <UCheckbox
            :model-value="row.original.agent_available"
            @update:model-value="toggleAgent(row.original)"
          />
        </template>
        <template #auto_assign_pattern-cell="{ row }">
          <code v-if="row.original.auto_assign_pattern">{{ row.original.auto_assign_pattern }}</code>
          <span v-else>—</span>
        </template>
        <template #max_runner_sessions-cell="{ row }">
          {{ row.original.active_sessions }} / {{ row.original.max_runner_sessions === 0 ? '∞' : row.original.max_runner_sessions }}
        </template>
        <template #actions-cell="{ row }">
          <div class="flex gap-1">
            <UButton size="xs" color="neutral" variant="outline" @click="editMembers(row.original)">Members</UButton>
            <UButton size="xs" color="error" variant="soft" @click="askDeleteGroup(row.original)">Delete</UButton>
          </div>
        </template>
      </UTable>
    </UCard>

    <UModal v-model:open="membersModalOpen" :title="`Members of ${membersModalGroup?.name ?? ''}`">
      <template #body>
        <UAlert v-if="membersError" class="mb-2" color="error" variant="soft" :description="membersError" />
        <div class="max-h-[300px] overflow-y-auto">
          <UCheckboxGroup
            v-model="membersModalSelected"
            :items="membersModalItems"
            class="block"
          />
        </div>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2">
          <UButton color="primary" @click="saveMembers">Save</UButton>
          <UButton color="neutral" variant="outline" @click="membersModalOpen = false">Cancel</UButton>
        </div>
      </template>
    </UModal>

    <UModal v-model:open="confirmOpen" title="Delete group">
      <template #body>
        <p>{{ confirmMessage }}</p>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2">
          <UButton color="neutral" variant="outline" @click="confirmOpen = false">Cancel</UButton>
          <UButton color="error" @click="runConfirmed">Confirm</UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>

<script setup lang="ts">
import { listGroups, createGroup as apiCreateGroup, deleteGroup as apiDeleteGroup, updateGroup, setGroupMembers, listUsers, getGroupMembers } from '~/api/client'
import type { Group, UserRow } from '~/api/types'
import { refreshAfterWrite } from '~/utils/refresh'

const toast = useToast()
const orgStore = useOrgStore()
const groups = ref<Group[]>([])
const allUsers = ref<UserRow[]>([])
const loaded = ref(false)
const newGroup = ref({ name: '', max_runner_sessions: 0, agent_available: true, auto_assign_pattern: '' })
const createError = ref('')
const membersModalOpen = ref(false)
const membersModalGroup = ref<Group | null>(null)
const membersModalSelected = ref<string[]>([])
const membersModalItems = computed(() => allUsers.value.map(u => ({ label: u.username, value: u.id })))
const membersError = ref('')

const confirmOpen = ref(false)
const confirmMessage = ref('')
let confirmAction: (() => void) | null = null

const columns = [
  { id: 'name', accessorKey: 'name', header: 'Name' },
  { id: 'member_count', accessorKey: 'member_count', header: 'Members' },
  { id: 'max_runner_sessions', accessorKey: 'max_runner_sessions', header: 'Active / Max' },
  { id: 'agent_available', accessorKey: 'agent_available', header: 'Agent' },
  { id: 'auto_assign_pattern', accessorKey: 'auto_assign_pattern', header: 'Pattern' },
  { id: 'actions', header: 'Actions' },
]

onMounted(() => { orgStore.syncFromRoute(useRoute()); void load() })

// load fetches groups + users. Background refreshes pass silent=true and
// rethrow so refreshAfterWrite can log a persistent failure without painting an
// alert over an already-rendered table.
async function load(silent = false) {
  try {
    groups.value = await listGroups()
    allUsers.value = await listUsers()
    loaded.value = true
  } catch (e: any) {
    if (silent) throw e
    toast.add({ title: 'Failed to load groups', color: 'error' })
  }
}

// upsertGroup replaces the row for id in place (or appends it), so a mutation
// result is visible before the next (possibly stale) read converges.
function upsertGroup(g: Group) {
  const idx = groups.value.findIndex(x => x.id === g.id)
  const existing = idx >= 0 ? groups.value[idx] : undefined
  if (existing) groups.value.splice(idx, 1, { ...existing, ...g })
  else groups.value.push(g)
}

async function createGroup() {
  createError.value = ''
  try {
    const created = await apiCreateGroup(newGroup.value)
    upsertGroup(created)
    newGroup.value = { name: '', max_runner_sessions: 0, agent_available: true, auto_assign_pattern: '' }
    void refreshAfterWrite(() => load(true))
  } catch (e: any) {
    createError.value = e.response?.data?.message || 'Failed to create group'
  }
}

async function toggleAgent(g: Group) {
  try {
    const updated = await updateGroup(g.id, { ...g, agent_available: !g.agent_available })
    upsertGroup(updated)
    void refreshAfterWrite(() => load(true))
  } catch (e: any) {
    toast.add({ title: e.response?.data?.message || 'Failed to update group', color: 'error' })
  }
}

function askDeleteGroup(g: Group) {
  confirmMessage.value = `Delete group ${g.name}? Projects in this group become unassigned.`
  confirmAction = () => void deleteGroup(g)
  confirmOpen.value = true
}

async function deleteGroup(g: Group) {
  const previous = groups.value
  groups.value = groups.value.filter(x => x.id !== g.id)
  try {
    await apiDeleteGroup(g.id)
    void refreshAfterWrite(() => load(true))
  } catch (e: any) {
    groups.value = previous
    toast.add({ title: e.response?.data?.message || 'Failed to delete group', color: 'error' })
  }
}

function runConfirmed() {
  confirmOpen.value = false
  confirmAction?.()
  confirmAction = null
}

function editMembers(g: Group) {
  membersModalGroup.value = g
  membersError.value = ''
  membersModalOpen.value = true
  getGroupMembers(g.id)
    .then(ms => {
      membersModalSelected.value = ms.map(u => u.id)
    })
    .catch((e: any) => {
      membersError.value = e.response?.data?.message || 'Failed to load members'
    })
}

async function saveMembers() {
  const group = membersModalGroup.value
  if (!group) return
  const selected = [...membersModalSelected.value]
  try {
    await setGroupMembers(group.id, selected)
    upsertGroup({ ...group, member_count: selected.length })
    membersModalOpen.value = false
    void refreshAfterWrite(() => load(true))
  } catch (e: any) {
    membersError.value = e.response?.data?.message || 'Failed to save members'
  }
}
</script>
