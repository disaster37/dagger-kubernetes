<template>
  <div>
    <h1 class="mb-6 text-2xl font-semibold">Users</h1>

    <UCard class="mb-4">
      <h3 class="font-semibold">Create user</h3>
      <UForm class="mt-3 flex flex-wrap gap-2" @submit="createUser">
        <UInput v-model="newUser.username" placeholder="username" />
        <UInput v-model="newUser.password" type="password" placeholder="password" />
        <USelect v-model="newUser.role" :items="roleOptions" />
        <UButton type="submit" color="primary">Create</UButton>
      </UForm>
      <UAlert v-if="createError" class="mt-2" color="error" variant="soft" :description="createError" />
    </UCard>

    <UCard>
      <UTable :data="users" :columns="columns">
        <template #status-cell="{ row }">
          <UBadge :color="statusColor(row.original)" variant="soft">{{ statusLabel(row.original) }}</UBadge>
        </template>
        <template #role-cell="{ row }">
          <USelect
            :model-value="row.original.role"
            :items="roleOptions"
            :disabled="!!row.original.deleted_at || !!row.original.disabled_at"
            @update:model-value="(v) => updateRole(row.original, String(v))"
          />
        </template>
        <template #groups-cell="{ row }">
          <UBadge
            v-for="g in row.original.groups"
            :key="g.id"
            class="mr-1"
            color="neutral"
            variant="soft"
          >{{ g.name }}</UBadge>
          <span v-if="!row.original.groups.length">—</span>
        </template>
        <template #oauth_groups-cell="{ row }">
          <UBadge
            v-for="og in row.original.oauth_groups"
            :key="og"
            class="mr-1"
            color="neutral"
            variant="soft"
          >{{ og }}</UBadge>
          <span v-if="!row.original.oauth_groups?.length">—</span>
        </template>
        <template #token-cell="{ row }">
          <code v-if="row.original.token">{{ row.original.token.prefix }}…</code>
          <span v-else>—</span>
        </template>
        <template #actions-cell="{ row }">
          <div class="flex flex-wrap gap-1">
            <!-- A tombstone only supports Restore: credentials and memberships
                 were removed and identity/username stay reserved. -->
            <template v-if="row.original.deleted_at">
              <UButton size="xs" color="primary" variant="soft" @click="askRestore(row.original)">Restore</UButton>
            </template>
            <template v-else>
              <UButton
                size="xs"
                color="neutral"
                variant="outline"
                :disabled="!!row.original.disabled_at"
                @click="manageGroups(row.original)"
              >Groups</UButton>
              <UButton
                size="xs"
                color="neutral"
                variant="outline"
                :disabled="!!row.original.disabled_at"
                @click="openResetPw(row.original)"
              >Reset PW</UButton>
              <UButton
                v-if="row.original.token"
                size="xs"
                color="neutral"
                variant="outline"
                @click="askRevokeToken(row.original)"
              >Revoke token</UButton>
              <UButton
                v-if="row.original.disabled_at"
                size="xs"
                color="primary"
                variant="soft"
                @click="askEnable(row.original)"
              >Enable</UButton>
              <UButton
                v-else
                size="xs"
                color="warning"
                variant="soft"
                :disabled="row.original.id === auth.user?.id"
                @click="askDisable(row.original)"
              >Disable</UButton>
              <UButton
                size="xs"
                color="error"
                variant="soft"
                :disabled="row.original.id === auth.user?.id"
                @click="askDeleteUser(row.original)"
              >Delete</UButton>
            </template>
          </div>
        </template>
      </UTable>
    </UCard>

    <UModal v-model:open="groupModalOpen" :title="`Groups for ${groupModalUser?.username ?? ''}`">
      <template #body>
        <div class="max-h-[300px] overflow-y-auto">
          <UCheckbox
            v-for="g in allGroups"
            :key="g.id"
            v-model="groupModalSelected"
            :value="g.id"
            :label="g.name"
            class="block py-1"
          />
        </div>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2">
          <UButton color="primary" @click="saveGroups">Save</UButton>
          <UButton color="neutral" variant="outline" @click="groupModalOpen = false">Cancel</UButton>
        </div>
      </template>
    </UModal>

    <UModal v-model:open="resetPwOpen" :title="`Reset password for ${resetPwUser?.username ?? ''}`">
      <template #body>
        <UInput v-model="resetPwValue" type="password" placeholder="New password (min 8 chars)" class="w-full" />
      </template>
      <template #footer>
        <div class="flex justify-end gap-2">
          <UButton color="primary" @click="confirmResetPw">Reset</UButton>
          <UButton color="neutral" variant="outline" @click="resetPwOpen = false">Cancel</UButton>
        </div>
      </template>
    </UModal>

    <UModal v-model:open="confirmOpen" :title="confirmTitle">
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
import { useAuthStore } from '~/stores/auth'
import { listUsers, createUser as apiCreateUser, updateUser, deleteUser as apiDeleteUser, resetPassword, setUserGroups, listGroups, revokeUserToken, setUserState } from '~/api/client'
import type { UserRow, Group } from '~/api/types'

const auth = useAuthStore()
const toast = useToast()
const users = ref<UserRow[]>([])
const allGroups = ref<Group[]>([])
const newUser = ref({ username: '', password: '', role: 'user' })
const createError = ref('')
const groupModalOpen = ref(false)
const groupModalUser = ref<UserRow | null>(null)
const groupModalSelected = ref<string[]>([])
const resetPwOpen = ref(false)
const resetPwUser = ref<UserRow | null>(null)
const resetPwValue = ref('')

const confirmOpen = ref(false)
const confirmTitle = ref('')
const confirmMessage = ref('')
let confirmAction: (() => void) | null = null

const roleOptions = [
  { label: 'user', value: 'user' },
  { label: 'admin', value: 'admin' },
]

// statusLabel/statusColor derive the account status: Deleted > Disabled >
// IdP-revoked > Active.
function statusLabel(u: UserRow): string {
  if (u.deleted_at) return 'Deleted'
  if (u.disabled_at) return 'Disabled'
  if (u.deactivated_at) return 'IdP revoked'
  return 'Active'
}
function statusColor(u: UserRow): 'error' | 'warning' | 'success' {
  if (u.deleted_at) return 'error'
  if (u.disabled_at || u.deactivated_at) return 'warning'
  return 'success'
}

const columns = [
  { id: 'username', accessorKey: 'username', header: 'Username' },
  { id: 'status', header: 'Status' },
  { id: 'role', accessorKey: 'role', header: 'Role' },
  { id: 'groups', accessorKey: 'groups', header: 'Groups' },
  { id: 'oauth_groups', accessorKey: 'oauth_groups', header: 'OAuth groups' },
  { id: 'token', accessorKey: 'token', header: 'Token' },
  { id: 'created_at', accessorKey: 'created_at', header: 'Created' },
  { id: 'actions', header: 'Actions' },
]

onMounted(load)

async function load() {
  users.value = await listUsers()
  allGroups.value = await listGroups()
}

// run performs a mutation, refreshes the lists, and alerts on failure.
// Returns true on success so callers can react (e.g. close a modal).
async function run(action: () => Promise<unknown>, failureMessage: string): Promise<boolean> {
  try {
    await action()
    await load()
    return true
  } catch (e: any) {
    toast.add({ title: e.response?.data?.message || failureMessage, color: 'error' })
    return false
  }
}

async function createUser() {
  createError.value = ''
  try {
    await apiCreateUser(newUser.value.username, newUser.value.password, newUser.value.role)
    newUser.value = { username: '', password: '', role: 'user' }
    await load()
  } catch (e: any) {
    createError.value = e.response?.data?.message || 'Failed to create user'
  }
}

function updateRole(u: UserRow, role: string) {
  run(() => updateUser(u.id, role), 'Failed to update role')
}

function askDeleteUser(u: UserRow) {
  confirmTitle.value = 'Delete user'
  confirmMessage.value = `Delete user ${u.username}? Their username and identity stay reserved, credentials and group memberships/tokens are removed, and outstanding sessions are invalidated. The account can later be restored (disabled, credentialless).`
  confirmAction = () => run(() => apiDeleteUser(u.id), 'Failed to delete user')
  confirmOpen.value = true
}

function askDisable(u: UserRow) {
  confirmTitle.value = 'Disable user'
  confirmMessage.value = `Disable ${u.username}? Their API token is revoked and existing sessions are invalidated. They cannot sign in again until enabled.`
  confirmAction = () => run(() => setUserState(u.id, 'disable'), 'Failed to disable user')
  confirmOpen.value = true
}

function askEnable(u: UserRow) {
  confirmTitle.value = 'Enable user'
  confirmMessage.value = `Enable ${u.username}? The account becomes usable again with its existing credentials; a fresh API token can be generated.`
  confirmAction = () => run(() => setUserState(u.id, 'enable'), 'Failed to enable user')
  confirmOpen.value = true
}

function askRestore(u: UserRow) {
  confirmTitle.value = 'Restore user'
  confirmMessage.value = `Restore ${u.username}? The identity and username stay reserved. The account is restored disabled and credentialless: enable it and set a new password (or sign in again with the Identity Provider) to use it.`
  confirmAction = () => run(() => setUserState(u.id, 'restore'), 'Failed to restore user')
  confirmOpen.value = true
}

function openResetPw(u: UserRow) {
  resetPwUser.value = u
  resetPwValue.value = ''
  resetPwOpen.value = true
}

async function confirmResetPw() {
  const u = resetPwUser.value
  if (!u || !resetPwValue.value) return
  try {
    await resetPassword(u.id, resetPwValue.value)
    resetPwOpen.value = false
    toast.add({ title: 'Password reset', color: 'success' })
  } catch (e: any) {
    toast.add({ title: e.response?.data?.message || 'Failed to reset password', color: 'error' })
  }
}

function askRevokeToken(u: UserRow) {
  confirmTitle.value = 'Revoke token'
  confirmMessage.value = `Revoke API token for ${u.username}?`
  confirmAction = () => run(() => revokeUserToken(u.id), 'Failed to revoke token')
  confirmOpen.value = true
}

function runConfirmed() {
  confirmOpen.value = false
  confirmAction?.()
  confirmAction = null
}

function manageGroups(u: UserRow) {
  groupModalUser.value = u
  groupModalSelected.value = u.groups.map(g => g.id)
  groupModalOpen.value = true
}

async function saveGroups() {
  if (!groupModalUser.value) return
  const id = groupModalUser.value.id
  if (await run(() => setUserGroups(id, groupModalSelected.value), 'Failed to save groups')) {
    groupModalOpen.value = false
  }
}
</script>
