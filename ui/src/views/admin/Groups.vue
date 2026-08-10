<template>
  <div>
    <h1 class="page-title">Groups</h1>

    <div class="card" style="margin-bottom: 16px;">
      <h3>Create group</h3>
      <form @submit.prevent="createGroup" style="display: flex; gap: 8px; margin-top: 12px; flex-wrap: wrap;">
        <input v-model="newGroup.name" placeholder="name" style="padding: 6px 10px; background: #0d1117; border: 1px solid #30363d; border-radius: 6px; color: #c9d1d9;" />
        <input v-model.number="newGroup.max_runner_sessions" type="number" placeholder="max sessions (0=∞)" style="width: 140px; padding: 6px 10px; background: #0d1117; border: 1px solid #30363d; border-radius: 6px; color: #c9d1d9;" />
        <label style="display: flex; align-items: center; gap: 4px;"><input type="checkbox" v-model="newGroup.agent_available" /> agent available</label>
        <input v-model="newGroup.auto_assign_pattern" placeholder="auto-assign regex (optional)" style="flex: 1; min-width: 200px; padding: 6px 10px; background: #0d1117; border: 1px solid #30363d; border-radius: 6px; color: #c9d1d9;" />
        <button type="submit" class="btn btn-primary">Create</button>
      </form>
      <p v-if="createError" style="color: #f85149; font-size: 13px; margin-top: 8px;">{{ createError }}</p>
    </div>

    <div class="card">
      <table>
        <thead>
          <tr>
            <th>Name</th><th>Members</th><th>Active / Max</th><th>Agent</th><th>Pattern</th><th>Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="g in groups" :key="g.id">
            <td>{{ g.name }}</td>
            <td>{{ g.member_count }}</td>
            <td>{{ g.active_sessions }} / {{ g.max_runner_sessions === 0 ? '∞' : g.max_runner_sessions }}</td>
            <td>
              <input type="checkbox" :checked="g.agent_available" @change="toggleAgent(g)" />
            </td>
            <td><code v-if="g.auto_assign_pattern">{{ g.auto_assign_pattern }}</code><span v-else>—</span></td>
            <td>
              <button class="btn" @click="editMembers(g)">Members</button>
              <button class="btn btn-danger" @click="deleteGroup(g)">Delete</button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <div v-if="membersModalGroup" class="modal-overlay" @click.self="membersModalGroup = null">
      <div class="card modal">
        <h3>Members of {{ membersModalGroup.name }}</h3>
        <p v-if="membersError" style="color: #f85149; font-size: 13px;">{{ membersError }}</p>
        <div style="margin-top: 12px; max-height: 300px; overflow-y: auto;">
          <label v-for="u in allUsers" :key="u.id" style="display: block; padding: 4px 0;">
            <input type="checkbox" :value="u.id" v-model="membersModalSelected" /> {{ u.username }}
          </label>
        </div>
        <div style="margin-top: 12px;">
          <button class="btn btn-primary" @click="saveMembers">Save</button>
          <button class="btn" @click="membersModalGroup = null">Cancel</button>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { listGroups, createGroup as apiCreateGroup, deleteGroup as apiDeleteGroup, updateGroup, setGroupMembers, listUsers, getGroupMembers } from '@/api/client'
import type { Group, UserRow } from '@/api/types'

const groups = ref<Group[]>([])
const allUsers = ref<UserRow[]>([])
const newGroup = ref({ name: '', max_runner_sessions: 0, agent_available: true, auto_assign_pattern: '' })
const createError = ref('')
const membersModalGroup = ref<Group | null>(null)
const membersModalSelected = ref<string[]>([])
const membersError = ref('')

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
    alert(e.response?.data?.message || failureMessage)
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

function deleteGroup(g: Group) {
  if (!confirm(`Delete group ${g.name}? Projects in this group become unassigned.`)) return
  run(() => apiDeleteGroup(g.id), 'Failed to delete group')
}

function editMembers(g: Group) {
  membersModalGroup.value = g
  membersError.value = ''
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
    membersModalGroup.value = null
    await load()
  } catch (e: any) {
    membersError.value = e.response?.data?.message || 'Failed to save members'
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
