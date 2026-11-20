<template>
  <div>
    <OrgSubnav />
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
      <div v-if="!loaded" class="py-8 text-center text-muted">…</div>
      <UTable v-else :data="projects" :columns="columns" empty="No projects yet. Projects are created automatically when CI runs a trace, or you can pre-create them here.">
        <template #name-cell="{ row }">
          <code>{{ row.original.name }}</code>
        </template>
        <template #group_id-cell="{ row }">
          <USelect
            :model-value="row.original.group_id || '__unassigned__'"
            :items="groupOptions"
            @update:model-value="(v) => assign(row.original, String(v))"
          />
        </template>
        <template #actions-cell="{ row }">
          <div class="flex gap-2">
            <UButton size="xs" variant="soft" @click="openSecrets(row.original)">Secrets</UButton>
            <UButton size="xs" color="error" variant="soft" @click="askDeleteProject(row.original)">Delete</UButton>
          </div>
        </template>
      </UTable>
    </UCard>

    <UModal v-model:open="secretsOpen" :title="`Secrets · ${secretsProject?.name || ''}`">
      <template #body>
        <div class="space-y-3">
          <div class="flex flex-wrap items-end gap-2">
            <UInput v-model="secretName" placeholder="NAME" />
            <UInput v-model="secretValue" type="password" placeholder="value" />
            <UButton size="xs" @click="saveSecret">Save</UButton>
          </div>
          <ul class="space-y-1 text-[13px]">
            <li v-for="s in projectSecrets" :key="s.id" class="flex items-center justify-between">
              <code>{{ s.name }}</code>
              <UButton size="xs" color="error" variant="soft" @click="removeSecret(s.name)">Delete</UButton>
            </li>
          </ul>
        </div>
      </template>
    </UModal>

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
import { listProjects, createProject as apiCreateProject, updateProject, deleteProject as apiDeleteProject, listGroups, listProjectSecrets, putProjectSecret, deleteProjectSecret } from '~/api/client'
import type { Project, Group, SecretMeta } from '~/api/types'
import { refreshAfterWrite } from '~/utils/refresh'

const toast = useToast()
const orgStore = useOrgStore()
const projects = ref<Project[]>([])
const groups = ref<Group[]>([])
const loaded = ref(false)
const newProject = ref({ name: '', group_id: '__unassigned__' })
const createError = ref('')

const secretsOpen = ref(false)
const secretsProject = ref<Project | null>(null)
const projectSecrets = ref<SecretMeta[]>([])
const secretName = ref('')
const secretValue = ref('')

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
  { label: 'Unassigned', value: '__unassigned__' },
  ...groups.value.map((g) => ({ label: g.name, value: g.id })),
])

onMounted(() => { orgStore.syncFromRoute(useRoute()); void load() })

// load fetches projects + groups. Background refreshes pass silent=true and
// rethrow so refreshAfterWrite can log a persistent failure without painting an
// alert over an already-rendered table.
async function load(silent = false) {
  try {
    projects.value = await listProjects()
    groups.value = await listGroups()
    loaded.value = true
  } catch (e: any) {
    if (silent) throw e
    toast.add({ title: 'Failed to load projects', color: 'error' })
  }
}

// upsertProject replaces the row for id in place (or appends it), so a mutation
// result is visible before the next (possibly stale) read converges.
function upsertProject(p: Project) {
  const idx = projects.value.findIndex(x => x.id === p.id)
  const existing = idx >= 0 ? projects.value[idx] : undefined
  if (existing) projects.value.splice(idx, 1, { ...existing, ...p })
  else projects.value.push(p)
}

async function createProject() {
  createError.value = ''
  try {
    const created = await apiCreateProject(
      newProject.value.name,
      newProject.value.group_id === '__unassigned__' ? '' : newProject.value.group_id,
    )
    upsertProject(created)
    newProject.value = { name: '', group_id: '__unassigned__' }
    void refreshAfterWrite(() => load(true))
  } catch (e: any) {
    createError.value = e.response?.data?.message || 'Failed to create project'
  }
}

async function assign(p: Project, groupId: string) {
  try {
    const updated = await updateProject(p.id, groupId === '__unassigned__' ? '' : groupId)
    upsertProject(updated)
    void refreshAfterWrite(() => load(true))
  } catch (e: any) {
    toast.add({ title: e.response?.data?.message || 'Failed to assign project', color: 'error' })
  }
}

function askDeleteProject(p: Project) {
  confirmMessage.value = `Delete project ${p.name}?`
  confirmAction = () => void deleteProject(p)
  confirmOpen.value = true
}

async function deleteProject(p: Project) {
  const previous = projects.value
  projects.value = projects.value.filter(x => x.id !== p.id)
  try {
    await apiDeleteProject(p.id)
    void refreshAfterWrite(() => load(true))
  } catch (e: any) {
    projects.value = previous
    toast.add({ title: e.response?.data?.message || 'Failed to delete project', color: 'error' })
  }
}

async function openSecrets(p: Project) {
  secretsProject.value = p
  projectSecrets.value = await listProjectSecrets(p.id)
  secretsOpen.value = true
}
function reloadSecrets(p: Project) {
  return listProjectSecrets(p.id).then((rows) => { projectSecrets.value = rows })
}
async function saveSecret() {
  const project = secretsProject.value
  if (!project || !secretName.value) return
  const name = secretName.value
  const value = secretValue.value
  secretName.value = ''
  secretValue.value = ''
  await putProjectSecret(project.id, name, value)
  void refreshAfterWrite(() => reloadSecrets(project))
}
async function removeSecret(name: string) {
  const project = secretsProject.value
  if (!project) return
  const previous = projectSecrets.value
  projectSecrets.value = projectSecrets.value.filter(s => s.name !== name)
  try {
    await deleteProjectSecret(project.id, name)
    void refreshAfterWrite(() => reloadSecrets(project))
  } catch (e: any) {
    projectSecrets.value = previous
    toast.add({ title: e.response?.data?.message || 'Failed to delete secret', color: 'error' })
  }
}

function runConfirmed() {
  confirmOpen.value = false
  confirmAction?.()
  confirmAction = null
}
</script>
