<template>
  <div>
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
      <UTable :data="groups" :columns="columns">
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
          <UCheckbox
            v-for="u in allUsers"
            :key="u.id"
            v-model="membersModalSelected"
            :value="u.id"
            :label="u.username"
            class="block py-1"
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

const toast = useToast()
const groups = ref<Group[]>([])
const allUsers = ref<UserRow[]>([])
const newGroup = ref({ name: '', max_runner_sessions: 0, agent_available: true, auto_assign_pattern: '' })
const createError = ref('')
const membersModalOpen = ref(false)
const membersModalGroup = ref<Group | null>(null)
const membersModalSelected = ref<string[]>([])
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

onMounted(load)

async function load() {
  groups.value = await listGroups()
  allUsers.value = await listUsers()
}

// run performs a mutation, refreshes the lists, and alerts on failure.
async function run(action: () => Promise<unknown>, failureMessage: string) {
  try {
    await action()
    await load()
  } catch (e: any) {
    toast.add({ title: e.response?.data?.message || failureMessage, color: 'error' })
  }
}

async function createGroup() {
  createError.value = ''
  try {
    await apiCreateGroup(newGroup.value)
    newGroup.value = { name: '', max_runner_sessions: 0, agent_available: true, auto_assign_pattern: '' }
    await load()
  } catch (e: any) {
    createError.value = e.response?.data?.message || 'Failed to create group'
  }
}

function toggleAgent(g: Group) {
  run(() => updateGroup(g.id, { ...g, agent_available: !g.agent_available }), 'Failed to update group')
}

function askDeleteGroup(g: Group) {
  confirmMessage.value = `Delete group ${g.name}? Projects in this group become unassigned.`
  confirmAction = () => run(() => apiDeleteGroup(g.id), 'Failed to delete group')
  confirmOpen.value = true
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
  if (!membersModalGroup.value) return
  try {
    await setGroupMembers(membersModalGroup.value.id, membersModalSelected.value)
    membersModalOpen.value = false
    await load()
  } catch (e: any) {
    membersError.value = e.response?.data?.message || 'Failed to save members'
  }
}
</script>
