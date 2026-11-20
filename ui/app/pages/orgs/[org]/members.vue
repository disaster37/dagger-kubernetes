<template>
  <div>
    <OrgSubnav />
    <UCard>
      <template #header>
        <div class="flex flex-wrap items-center justify-between gap-2">
          <h1 class="text-xl font-semibold">Members</h1>
          <div v-if="canAdmin" class="flex flex-wrap items-end gap-2">
            <UInput v-model="newUsername" placeholder="username" />
            <USelect v-model="newRole" :items="['member', 'org_admin']" />
            <UButton :loading="saving" @click="addMember">Add member</UButton>
          </div>
        </div>
      </template>
      <UAlert v-if="error" color="error" variant="soft" class="mb-3" :description="error" />
      <div v-if="!loaded" class="py-8 text-center text-muted">…</div>
      <UTable v-else :data="members" :columns="columns">
        <template #username-cell="{ row }">
          <span>{{ row.original.username || row.original.user_id }}</span>
        </template>
        <template #groups-cell="{ row }">
          <span class="text-xs">{{ (row.original.groups ?? []).map(g => g.name).join(', ') || '—' }}</span>
        </template>
        <template #role-cell="{ row }">
          <USelect
            v-if="canAdmin"
            :model-value="row.original.role"
            :items="['member', 'org_admin']"
            size="xs"
            @update:model-value="(v: string) => changeRole(row.original.user_id, v)"
          />
          <UBadge v-else variant="subtle">{{ row.original.role }}</UBadge>
        </template>
        <template #actions-cell="{ row }">
          <div v-if="canAdmin" class="flex gap-1">
            <UButton size="xs" variant="outline" @click="editGroups(row.original)">Groups</UButton>
            <UButton size="xs" color="error" variant="soft" @click="removeMember(row.original.user_id)">Remove</UButton>
          </div>
        </template>
      </UTable>
    </UCard>

    <UModal v-model:open="groupsOpen" :title="`Groups for ${selectedUser}`">
      <template #body>
        <UCheckboxGroup v-model="selectedGroups" :items="groupItems" class="block" />
        <UButton class="mt-3" @click="saveGroups">Save</UButton>
      </template>
    </UModal>
  </div>
</template>

<script setup lang="ts">
import { listGroups, listOrgMemberRows, removeOrgMember, setOrgMember, setOrgMemberGroups } from '~/api/client'
import type { Group, OrgMemberRow } from '~/api/types'
import { refreshAfterWrite } from '~/utils/refresh'

const route = useRoute()
const auth = useAuthStore()
const orgStore = useOrgStore()

const members = ref<OrgMemberRow[]>([])
const groups = ref<Group[]>([])
const error = ref('')
const loaded = ref(false)
const saving = ref(false)
const newUsername = ref('')
const newRole = ref('member')
const groupsOpen = ref(false)
const selectedUser = ref('')
const selectedGroups = ref<string[]>([])

const columns = [
  { accessorKey: 'username', header: 'User' },
  { accessorKey: 'role', header: 'Role' },
  { accessorKey: 'groups', header: 'Groups' },
  { accessorKey: 'actions', header: '' },
]
const groupItems = computed(() => groups.value.map(g => ({ label: g.name, value: g.id })))
const canAdmin = computed(() => auth.isAdmin || orgStore.activeOrg?.role === 'org_admin')

// load fetches the rows; background refreshes pass silent=true so a transient
// follower read does not flash an error over an already-rendered table.
async function load(silent = false) {
  if (!silent) error.value = ''
  try {
    members.value = await listOrgMemberRows(orgStore.activeSlug)
    groups.value = await listGroups()
    loaded.value = true
  } catch (e: any) {
    if (!silent) error.value = e?.response?.data?.message ?? 'Failed to load members'
    else throw e
  }
}

// upsertMember replaces the row for user_id in place (or appends it), so a
// mutation result is visible before the next (possibly stale) read converges.
function upsertMember(row: OrgMemberRow) {
  const idx = members.value.findIndex(m => m.user_id === row.user_id)
  const existing = idx >= 0 ? members.value[idx] : undefined
  if (existing) members.value.splice(idx, 1, { ...existing, ...row })
  else members.value.push(row)
}

async function addMember() {
  if (!newUsername.value) return
  saving.value = true
  try {
    const row = await setOrgMember(orgStore.activeSlug, newUsername.value, newRole.value)
    upsertMember(row)
    newUsername.value = ''
    void refreshAfterWrite(() => load(true))
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to add member'
  } finally {
    saving.value = false
  }
}
async function changeRole(userId: string, role: string) {
  try {
    const row = await setOrgMember(orgStore.activeSlug, userId, role)
    upsertMember(row)
    void refreshAfterWrite(() => load(true))
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to change role'
    void refreshAfterWrite(() => load(true))
  }
}
async function removeMember(userId: string) {
  const previous = members.value
  members.value = members.value.filter(m => m.user_id !== userId)
  try {
    await removeOrgMember(orgStore.activeSlug, userId)
    void refreshAfterWrite(() => load(true))
  } catch (e: any) {
    members.value = previous
    error.value = e?.response?.data?.message ?? 'Failed to remove member'
  }
}
function editGroups(member: OrgMemberRow) {
  selectedUser.value = member.user_id
  selectedGroups.value = (member.groups ?? []).map(g => g.id)
  groupsOpen.value = true
}
async function saveGroups() {
  try {
    const saved = await setOrgMemberGroups(orgStore.activeSlug, selectedUser.value, selectedGroups.value)
    const idx = members.value.findIndex(m => m.user_id === selectedUser.value)
    const existing = idx >= 0 ? members.value[idx] : undefined
    if (existing) {
      // The PUT returns the authoritative org-scoped group summaries.
      members.value.splice(idx, 1, { ...existing, groups: saved })
    }
    groupsOpen.value = false
    void refreshAfterWrite(() => load(true))
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to save groups'
  }
}

onMounted(() => { orgStore.syncFromRoute(route); void load() })
</script>
