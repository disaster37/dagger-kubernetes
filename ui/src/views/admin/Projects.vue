<template>
  <div>
    <h1 class="page-title">Projects</h1>

    <div class="card" style="margin-bottom: 16px;">
      <h3>Create project</h3>
      <form @submit.prevent="createProject" style="display: flex; gap: 8px; margin-top: 12px; flex-wrap: wrap;">
        <input v-model="newProject.name" placeholder="repo slug, e.g. github.com/acme/api" style="flex: 1; min-width: 250px; padding: 6px 10px; background: #0d1117; border: 1px solid #30363d; border-radius: 6px; color: #c9d1d9;" />
        <select v-model="newProject.group_id" style="padding: 6px 10px; background: #0d1117; border: 1px solid #30363d; border-radius: 6px; color: #c9d1d9;">
          <option value="">Unassigned</option>
          <option v-for="g in groups" :key="g.id" :value="g.id">{{ g.name }}</option>
        </select>
        <button type="submit" class="btn btn-primary">Create</button>
      </form>
      <p v-if="createError" style="color: #f85149; font-size: 13px; margin-top: 8px;">{{ createError }}</p>
    </div>

    <div class="card">
      <table>
        <thead>
          <tr><th>Name</th><th>Group</th><th>Created</th><th>Actions</th></tr>
        </thead>
        <tbody>
          <tr v-for="p in projects" :key="p.id">
            <td><code>{{ p.name }}</code></td>
            <td>
              <select :value="p.group_id" @change="assign(p, ($event.target as HTMLSelectElement).value)" style="padding: 4px 8px; background: #0d1117; border: 1px solid #30363d; border-radius: 4px; color: #c9d1d9;">
                <option value="">Unassigned</option>
                <option v-for="g in groups" :key="g.id" :value="g.id">{{ g.name }}</option>
              </select>
            </td>
            <td>{{ p.created_at }}</td>
            <td><button class="btn btn-danger" @click="deleteProject(p)">Delete</button></td>
          </tr>
        </tbody>
      </table>
      <p v-if="!projects.length" style="padding: 24px; text-align: center; color: #8b949e;">
        No projects yet. Projects are created automatically when CI runs a trace, or you can pre-create them here.
      </p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { listProjects, createProject as apiCreateProject, updateProject, deleteProject as apiDeleteProject, listGroups } from '@/api/client'
import type { Project, Group } from '@/api/types'

const projects = ref<Project[]>([])
const groups = ref<Group[]>([])
const newProject = ref({ name: '', group_id: '' })
const createError = ref('')

onMounted(load)

async function load() {
  projects.value = await listProjects()
  groups.value = await listGroups()
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

async function createProject() {
  createError.value = ''
  try {
    await apiCreateProject(newProject.value.name, newProject.value.group_id)
    newProject.value = { name: '', group_id: '' }
    await load()
  } catch (e: any) {
    createError.value = e.response?.data?.message || 'Failed to create project'
  }
}

function assign(p: Project, groupId: string) {
  run(() => updateProject(p.id, groupId), 'Failed to assign project')
}

function deleteProject(p: Project) {
  if (!confirm(`Delete project ${p.name}?`)) return
  run(() => apiDeleteProject(p.id), 'Failed to delete project')
}
</script>

<style scoped>
.btn-danger { background: #da3633; color: #fff; }
</style>
