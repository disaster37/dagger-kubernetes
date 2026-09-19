<template>
  <div>
    <h1 class="mb-6 text-2xl font-semibold">Projects</h1>

    <UCard class="mb-4">
      <h3 class="font-semibold">Create project</h3>
      <UForm class="mt-3 flex flex-wrap gap-2" @submit="createProject">
        <UInput v-model="newProject.name" placeholder="repo slug, e.g. github.com/acme/api" class="min-w-[250px] flex-1" />
        <USelect v-model="newProject.group_id" :items="groupOptions" />
        <UButton type="submit" color="primary">Create</UButton>
      </UForm>
      <UAlert v-if="createError" class="mt-2" color="error" variant="soft" :description="createError" />
    </UCard>

    <UCard>
      <UTable :data="projects" :columns="columns" empty="No projects yet. Projects are created automatically when CI runs a trace, or you can pre-create them here.">
        <template #name-cell="{ row }">
          <code>{{ row.original.name }}</code>
        </template>
        <template #group_id-cell="{ row }">
          <USelect
            :model-value="row.original.group_id"
            :items="groupOptions"
            @update:model-value="(v) => assign(row.original, String(v))"
          />
        </template>
        <template #actions-cell="{ row }">
          <UButton size="xs" color="error" variant="soft" @click="askDeleteProject(row.original)">Delete</UButton>
        </template>
      </UTable>
    </UCard>

    <UModal v-model:open="confirmOpen" title="Delete project">
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
import { listProjects, createProject as apiCreateProject, updateProject, deleteProject as apiDeleteProject, listGroups } from '~/api/client'
import type { Project, Group } from '~/api/types'

const toast = useToast()
const projects = ref<Project[]>([])
const groups = ref<Group[]>([])
const newProject = ref({ name: '', group_id: '' })
const createError = ref('')

const confirmOpen = ref(false)
const confirmMessage = ref('')
let confirmAction: (() => void) | null = null

const columns = [
  { id: 'name', accessorKey: 'name', header: 'Name' },
  { id: 'group_id', accessorKey: 'group_id', header: 'Group' },
  { id: 'created_at', accessorKey: 'created_at', header: 'Created' },
  { id: 'actions', header: 'Actions' },
]

const groupOptions = computed(() => [
  { label: 'Unassigned', value: '' },
  ...groups.value.map((g) => ({ label: g.name, value: g.id })),
])

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
    toast.add({ title: e.response?.data?.message || failureMessage, color: 'error' })
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

function askDeleteProject(p: Project) {
  confirmMessage.value = `Delete project ${p.name}?`
  confirmAction = () => run(() => apiDeleteProject(p.id), 'Failed to delete project')
  confirmOpen.value = true
}

function runConfirmed() {
  confirmOpen.value = false
  confirmAction?.()
  confirmAction = null
}
</script>
