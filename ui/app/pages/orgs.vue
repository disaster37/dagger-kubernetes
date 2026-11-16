<template>
  <div>
    <div class="mb-6 flex items-center justify-between">
      <h1 class="text-2xl font-semibold">Organizations</h1>
      <UButton icon="i-lucide-plus" @click="showCreate = true">Create org</UButton>
    </div>
    <UAlert v-if="error" color="error" variant="soft" :description="error" class="mb-4" />

    <div class="mb-4 flex flex-wrap gap-2">
      <UButton
        v-for="o in orgs"
        :key="o.id"
        :color="selected === o.id ? 'primary' : 'neutral'"
        :variant="selected === o.id ? 'solid' : 'soft'"
        @click="selectOrg(o.id)"
      >
        {{ o.display_name || o.name }}
      </UButton>
    </div>

    <div v-if="selected" class="space-y-4">
      <UCard>
        <template #header><h2 class="font-semibold">Members</h2></template>
        <div class="mb-3 flex flex-wrap items-end gap-2">
          <UInput v-model="memberUserID" placeholder="user id" />
          <USelect v-model="memberRole" :items="['member', 'org_admin']" />
          <UButton @click="addMember">Add / update</UButton>
        </div>
        <ul class="space-y-1 text-[13px]">
          <li v-for="m in members" :key="m.user_id" class="flex items-center justify-between">
            <span><code>{{ m.user_id }}</code> · {{ m.role }}</span>
            <UButton size="xs" color="error" variant="soft" @click="removeMember(m.user_id)">Remove</UButton>
          </li>
        </ul>
      </UCard>

      <UCard>
        <template #header><h2 class="font-semibold">Org token</h2></template>
        <p class="text-[13px] text-muted">Scoped <code>dag_&lt;org&gt;_</code> token used by CI, checks, and workflows.</p>
        <div class="mt-3 flex gap-2">
          <UButton @click="rotateToken">Generate / rotate</UButton>
          <UButton color="error" variant="soft" @click="revokeToken">Revoke</UButton>
        </div>
        <UAlert v-if="newToken" color="warning" variant="soft" class="mt-3" :description="`Copy this token now: ${newToken}`" />
      </UCard>

      <UCard>
        <template #header><h2 class="font-semibold">Org secrets</h2></template>
        <div class="mb-3 flex flex-wrap items-end gap-2">
          <UInput v-model="secretName" placeholder="NAME" />
          <UInput v-model="secretValue" type="password" placeholder="value" />
          <UButton @click="saveSecret">Save</UButton>
        </div>
        <ul class="space-y-1 text-[13px]">
          <li v-for="s in secrets" :key="s.id" class="flex items-center justify-between">
            <code>{{ s.name }}</code>
            <UButton size="xs" color="error" variant="soft" @click="removeSecret(s.name)">Delete</UButton>
          </li>
        </ul>
      </UCard>

      <UCard>
        <template #header><h2 class="font-semibold">Protected environments</h2></template>
        <div class="mb-3 grid gap-2 md:grid-cols-4">
          <UInput v-model="envName" placeholder="prod" />
          <USelect v-model="envGroups" :items="groupOptions" multiple placeholder="approver groups" />
          <UInput v-model="envTimeout" placeholder="24h" />
          <div class="flex items-center gap-2">
            <USwitch v-model="envPreventSelf" />
            <span class="text-[13px]">Prevent self-approval</span>
          </div>
        </div>
        <UButton class="mb-3" @click="saveEnvironment">Save environment</UButton>
        <div v-for="env in environments" :key="env.id" class="mb-3 rounded border border-default p-3">
          <div class="flex items-center justify-between">
            <div>
              <p class="font-semibold">{{ env.name }}</p>
              <p class="text-[13px] text-muted">
                groups: {{ env.approver_group_ids.join(', ') || 'none' }} · timeout: {{ env.approval_timeout }}ns
              </p>
            </div>
            <div class="flex gap-2">
              <UButton size="xs" variant="soft" @click="editEnvironment(env)">Edit</UButton>
              <UButton size="xs" color="error" variant="soft" @click="removeEnvironment(env.name)">Delete</UButton>
            </div>
          </div>
          <div class="mt-2 flex flex-wrap items-end gap-2">
            <UInput v-model="envSecretName" placeholder="NAME" />
            <UInput v-model="envSecretValue" type="password" placeholder="value" />
            <UButton size="xs" @click="saveEnvSecret(env.name)">Add secret</UButton>
          </div>
          <ul class="mt-2 space-y-1 text-[13px]">
            <li v-for="s in envSecrets[env.name] || []" :key="s.id" class="flex items-center justify-between">
              <code>{{ s.name }}</code>
              <UButton size="xs" color="error" variant="soft" @click="removeEnvSecret(env.name, s.name)">Delete</UButton>
            </li>
          </ul>
        </div>
      </UCard>

      <UCard>
        <template #header><h2 class="font-semibold">Git integrations</h2></template>
        <div class="mb-3 flex flex-wrap gap-2">
          <UButton v-for="p in ['github', 'gitlab', 'gitea']" :key="p" :to="`/${selected}/integrations/${p}`" variant="soft">
            {{ p }}
          </UButton>
        </div>
        <ul class="space-y-1 text-[13px]">
          <li v-for="src in sources" :key="src.id" class="flex items-center justify-between">
            <span>{{ src.provider }} · {{ src.repo }} · <code>/v1/webhooks/{{ src.provider }}/{{ src.id }}</code></span>
            <UButton size="xs" color="error" variant="soft" @click="removeSource(src.id)">Delete</UButton>
          </li>
        </ul>
      </UCard>
    </div>

    <UModal v-model:open="showCreate">
      <UCard>
        <template #header><h2 class="font-semibold">Create organization</h2></template>
        <div class="space-y-3">
          <UInput v-model="newOrgName" placeholder="acme" />
          <UInput v-model="newOrgDisplay" placeholder="Acme Inc" />
          <UButton :loading="creating" @click="createOrg">Create</UButton>
        </div>
      </UCard>
    </UModal>
  </div>
</template>

<script setup lang="ts">
import api, {
  createOrg as createOrgRequest,
  deleteEnvSecret,
  deleteEnvironment,
  deleteForgeSource,
  deleteOrgSecret,
  generateOrgToken,
  listEnvSecrets,
  listEnvironments,
  listForgeSources,
  listOrgMembers,
  listOrgSecrets,
  listOrgs,
  putEnvSecret,
  putEnvironment,
  putOrgSecret,
  removeOrgMember,
  revokeOrgToken,
  setOrgMember,
} from '~/api/client'
import type { ForgeSource, Org, OrgMember, ProtectedEnvironment, SecretMeta } from '~/api/types'

const orgs = ref<Org[]>([])
const selected = ref('')
const members = ref<OrgMember[]>([])
const secrets = ref<SecretMeta[]>([])
const environments = ref<ProtectedEnvironment[]>([])
const envSecrets = ref<Record<string, SecretMeta[]>>({})
const sources = ref<ForgeSource[]>([])
const groups = ref<{ id: string; name: string }[]>([])
const error = ref('')
const showCreate = ref(false)
const creating = ref(false)
const newOrgName = ref('')
const newOrgDisplay = ref('')
const newToken = ref('')
const memberUserID = ref('')
const memberRole = ref('member')
const secretName = ref('')
const secretValue = ref('')
const envName = ref('')
const envGroups = ref<string[]>([])
const envTimeout = ref('24h')
const envPreventSelf = ref(true)
const envSecretName = ref('')
const envSecretValue = ref('')

const groupOptions = computed(() => groups.value.map(g => ({ label: g.name, value: g.id })))

async function loadOrgs() {
  try {
    orgs.value = await listOrgs()
    if (!selected.value && orgs.value.length) await selectOrg(orgs.value[0]?.id ?? '')
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to load organizations'
  }
}

async function selectOrg(id: string) {
  selected.value = id
  error.value = ''
  try {
    const [m, s, envs, srcs] = await Promise.all([
      listOrgMembers(id), listOrgSecrets(id), listEnvironments(id), listForgeSources(id),
    ])
    members.value = m
    secrets.value = s
    environments.value = envs
    sources.value = srcs
    const nested = await Promise.all(envs.map(env => listEnvSecrets(id, env.name)))
    envSecrets.value = Object.fromEntries(envs.map((env, i) => [env.name, nested[i] ?? []]))
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to load organization'
  }
}

async function createOrg() {
  creating.value = true
  try {
    const org = await createOrgRequest({ name: newOrgName.value, display_name: newOrgDisplay.value })
    showCreate.value = false
    newOrgName.value = ''
    newOrgDisplay.value = ''
    await loadOrgs()
    await selectOrg(org.id)
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to create organization'
  } finally {
    creating.value = false
  }
}

async function addMember() {
  if (!memberUserID.value) return
  await setOrgMember(selected.value, memberUserID.value, memberRole.value)
  memberUserID.value = ''
  await selectOrg(selected.value)
}
async function removeMember(userID: string) {
  await removeOrgMember(selected.value, userID)
  await selectOrg(selected.value)
}
async function rotateToken() {
  const res = await generateOrgToken(selected.value)
  newToken.value = res.token
}
async function revokeToken() {
  await revokeOrgToken(selected.value)
  newToken.value = ''
}
async function saveSecret() {
  if (!secretName.value) return
  await putOrgSecret(selected.value, secretName.value, secretValue.value)
  secretName.value = ''
  secretValue.value = ''
  await selectOrg(selected.value)
}
async function removeSecret(name: string) {
  await deleteOrgSecret(selected.value, name)
  await selectOrg(selected.value)
}
async function saveEnvironment() {
  if (!envName.value) return
  await putEnvironment(selected.value, envName.value, {
    approver_group_ids: envGroups.value,
    prevent_self_approval: envPreventSelf.value,
    approval_timeout: envTimeout.value,
  })
  envName.value = ''
  await selectOrg(selected.value)
}
function editEnvironment(env: ProtectedEnvironment) {
  envName.value = env.name
  envGroups.value = [...env.approver_group_ids]
  envPreventSelf.value = env.prevent_self_approval
  envTimeout.value = `${Math.round(env.approval_timeout / 1e9)}s`
}
async function removeEnvironment(name: string) {
  await deleteEnvironment(selected.value, name)
  await selectOrg(selected.value)
}
async function saveEnvSecret(env: string) {
  if (!envSecretName.value) return
  await putEnvSecret(selected.value, env, envSecretName.value, envSecretValue.value)
  envSecretName.value = ''
  envSecretValue.value = ''
  await selectOrg(selected.value)
}
async function removeEnvSecret(env: string, name: string) {
  await deleteEnvSecret(selected.value, env, name)
  await selectOrg(selected.value)
}
async function removeSource(id: string) {
  await deleteForgeSource(selected.value, id)
  await selectOrg(selected.value)
}

onMounted(async () => {
  await loadOrgs()
  try {
    const { data } = await api.get('/api/v1/groups')
    groups.value = data
  } catch {
    groups.value = []
  }
})
</script>
