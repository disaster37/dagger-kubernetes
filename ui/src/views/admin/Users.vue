<template>
  <div>
    <h1 class="page-title">Users</h1>

    <div class="card" style="margin-bottom: 16px;">
      <h3>Create user</h3>
      <form @submit.prevent="createUser" style="display: flex; gap: 8px; margin-top: 12px; flex-wrap: wrap;">
        <input v-model="newUser.username" placeholder="username" style="padding: 6px 10px; background: #0d1117; border: 1px solid #30363d; border-radius: 6px; color: #c9d1d9;" />
        <input v-model="newUser.password" type="password" placeholder="password" style="padding: 6px 10px; background: #0d1117; border: 1px solid #30363d; border-radius: 6px; color: #c9d1d9;" />
        <select v-model="newUser.role" style="padding: 6px 10px; background: #0d1117; border: 1px solid #30363d; border-radius: 6px; color: #c9d1d9;">
          <option value="user">user</option>
          <option value="admin">admin</option>
        </select>
        <button type="submit" class="btn btn-primary">Create</button>
      </form>
      <p v-if="createError" style="color: #f85149; font-size: 13px; margin-top: 8px;">{{ createError }}</p>
    </div>

    <div class="card">
      <table>
        <thead>
          <tr>
            <th>Username</th><th>Role</th><th>Groups</th><th>Token</th><th>Created</th><th>Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="u in users" :key="u.id">
            <td>{{ u.username }}</td>
            <td>
              <select :value="u.role" @change="updateRole(u, ($event.target as HTMLSelectElement).value)" style="padding: 2px 6px; background: #0d1117; border: 1px solid #30363d; border-radius: 4px; color: #c9d1d9;">
                <option value="user">user</option>
                <option value="admin">admin</option>
              </select>
            </td>
            <td>
              <span v-for="g in u.groups" :key="g.id" class="badge" style="margin-right: 4px;">{{ g.name }}</span>
              <span v-if="!u.groups.length">—</span>
            </td>
            <td><code v-if="u.token">{{ u.token.prefix }}…</code><span v-else>—</span></td>
            <td>{{ u.created_at }}</td>
            <td>
              <button class="btn" @click="manageGroups(u)">Groups</button>
              <button class="btn" @click="resetPw(u)">Reset PW</button>
              <button class="btn" v-if="u.token" @click="revokeToken(u)">Revoke token</button>
              <button class="btn btn-danger" @click="deleteUser(u)" :disabled="u.id === auth.user?.id">Delete</button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <div v-if="groupModalUser" class="modal-overlay" @click.self="groupModalUser = null">
      <div class="card modal">
        <h3>Groups for {{ groupModalUser.username }}</h3>
        <div style="margin-top: 12px; max-height: 300px; overflow-y: auto;">
          <label v-for="g in allGroups" :key="g.id" style="display: block; padding: 4px 0;">
            <input type="checkbox" :value="g.id" v-model="groupModalSelected" /> {{ g.name }}
          </label>
        </div>
        <div style="margin-top: 12px;">
          <button class="btn btn-primary" @click="saveGroups">Save</button>
          <button class="btn" @click="groupModalUser = null">Cancel</button>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useAuthStore } from '@/stores/auth'
import { listUsers, createUser as apiCreateUser, updateUser, deleteUser as apiDeleteUser, resetPassword, setUserGroups, listGroups, revokeUserToken } from '@/api/client'
import type { UserRow, Group } from '@/api/types'

const auth = useAuthStore()
const users = ref<UserRow[]>([])
const allGroups = ref<Group[]>([])
const newUser = ref({ username: '', password: '', role: 'user' })
const createError = ref('')
const groupModalUser = ref<UserRow | null>(null)
const groupModalSelected = ref<string[]>([])

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
    alert(e.response?.data?.message || failureMessage)
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

function deleteUser(u: UserRow) {
  if (!confirm(`Delete user ${u.username}?`)) return
  run(() => apiDeleteUser(u.id), 'Failed to delete user')
}

async function resetPw(u: UserRow) {
  const pw = prompt(`New password for ${u.username} (min 8 chars):`)
  if (!pw) return
  try {
    await resetPassword(u.id, pw)
    alert('Password reset')
  } catch (e: any) {
    alert(e.response?.data?.message || 'Failed to reset password')
  }
}

function revokeToken(u: UserRow) {
  if (!confirm(`Revoke API token for ${u.username}?`)) return
  run(() => revokeUserToken(u.id), 'Failed to revoke token')
}

function manageGroups(u: UserRow) {
  groupModalUser.value = u
  groupModalSelected.value = u.groups.map(g => g.id)
}

async function saveGroups() {
  if (!groupModalUser.value) return
  const id = groupModalUser.value.id
  if (await run(() => setUserGroups(id, groupModalSelected.value), 'Failed to save groups')) {
    groupModalUser.value = null
  }
}
</script>

<style scoped>
.modal-overlay {
  position: fixed; top: 0; left: 0; right: 0; bottom: 0;
  background: rgba(0,0,0,0.6); display: flex; align-items: center; justify-content: center;
  z-index: 100;
}
.modal { max-width: 400px; width: 90%; }
.btn-danger { background: #da3633; color: #fff; }
</style>
