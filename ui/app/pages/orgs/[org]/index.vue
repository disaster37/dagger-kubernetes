<template>
  <div>
    <OrgSubnav />
    <UAlert v-if="org.routeForbidden" color="error" variant="soft" title="Access denied" description="You do not have access to this organization." class="mb-4">
      <template #actions>
        <UButton :to="org.activeSlug ? `/orgs/${org.activeSlug}` : '/pipelines'">Go to my organization</UButton>
      </template>
    </UAlert>

    <div v-else-if="current" class="space-y-4">
      <UCard>
        <template #header>
          <div class="flex items-center justify-between">
            <h2 class="font-semibold">{{ current.display_name || current.name }}</h2>
            <UBadge variant="subtle">{{ current.id }}</UBadge>
          </div>
        </template>
        <div class="grid gap-4 md:grid-cols-2">
          <div>
            <p class="text-xs text-muted">Slug (immutable)</p>
            <p class="font-mono text-sm">{{ current.name }}</p>
          </div>
          <div>
            <p class="text-xs text-muted">Storage ID (immutable)</p>
            <p class="font-mono text-sm">{{ current.id }}</p>
          </div>
        </div>
        <div v-if="canAdmin" class="mt-4 flex flex-wrap items-end gap-2">
          <UInput v-model="displayName" placeholder="Display name" />
          <UButton :loading="saving" @click="saveDisplayName">Save display name</UButton>
        </div>
        <p v-if="error" class="mt-3 text-sm text-error">{{ error }}</p>
      </UCard>

      <UCard>
        <template #header><h2 class="font-semibold">Quota usage</h2></template>
        <div v-if="usage" class="grid gap-3 md:grid-cols-3">
          <div v-for="row in usageRows" :key="row.label">
            <div class="flex justify-between text-xs">
              <span>{{ row.label }}</span>
              <span>{{ row.used }} / {{ row.limit || '∞' }}</span>
            </div>
            <UProgress :model-value="row.percent" size="sm" />
          </div>
        </div>
      </UCard>

      <UCard>
        <template #header><h2 class="font-semibold">Organization token</h2></template>
        <p class="text-sm text-muted">Scoped <code>dag_&lt;slug&gt;_</code> token for CI, checks, and workflows.</p>
        <div class="mt-3 flex gap-2">
          <UButton v-if="canAdmin" @click="rotateToken">Generate / rotate</UButton>
          <UButton v-if="canAdmin" color="error" variant="soft" @click="revokeToken">Revoke</UButton>
        </div>
        <UAlert v-if="newToken" color="warning" variant="soft" class="mt-3" :description="`Copy this token now: ${newToken}`" />
      </UCard>

      <UCard>
        <template #header><h2 class="font-semibold">Organization secrets</h2></template>
        <div v-if="canAdmin" class="mb-3 flex flex-wrap items-end gap-2">
          <UInput v-model="secretName" placeholder="NAME" />
          <UInput v-model="secretValue" type="password" placeholder="value" />
          <UButton @click="saveSecret">Save</UButton>
        </div>
        <ul class="space-y-1 text-sm">
          <li v-for="s in secrets" :key="s.id" class="flex items-center justify-between">
            <code>{{ s.name }}</code>
            <UButton v-if="canAdmin" size="xs" color="error" variant="soft" @click="removeSecret(s.name)">Delete</UButton>
          </li>
        </ul>
      </UCard>

      <UCard>
        <template #header><h2 class="font-semibold">Integrations</h2></template>
        <div class="flex flex-wrap gap-2">
          <UButton v-for="p in ['github', 'gitlab', 'gitea']" :key="p" :to="`/orgs/${current.name}/integrations/${p}`" variant="soft">
            {{ p }}
          </UButton>
        </div>
      </UCard>
    </div>
  </div>
</template>

<script setup lang="ts">
import { deleteOrgSecret, generateOrgToken, getOrg, getOrgQuotas, listOrgSecrets, putOrgSecret, revokeOrgToken, updateOrg } from '~/api/client'
import type { Org, OrgQuotaUsage, SecretMeta } from '~/api/types'
import { useOrgStore } from '~/stores/org'

const route = useRoute()
const auth = useAuthStore()
const org = useOrgStore()

const current = ref<Org | null>(null)
const usage = ref<OrgQuotaUsage | null>(null)
const secrets = ref<SecretMeta[]>([])
const displayName = ref('')
const saving = ref(false)
const error = ref('')
const newToken = ref('')
const secretName = ref('')
const secretValue = ref('')

const slug = computed(() => String(route.params.org ?? ''))
const canAdmin = computed(() => auth.isAdmin || org.activeOrg?.role === 'org_admin')

const usageRows = computed(() => {
  if (!usage.value) return []
  const e = usage.value.effective
  const row = (label: string, used: number, limit: number) => ({
    label,
    used,
    limit,
    percent: limit > 0 ? Math.min(100, Math.round((used / limit) * 100)) : 0,
  })
  return [
    row('Groups', usage.value.groups, e.max_groups.hard),
    row('Projects', usage.value.projects, e.max_projects.hard),
    row('Members', usage.value.members, e.max_members.hard),
    row('Sessions', usage.value.concurrent_sessions, e.max_concurrent_sessions.hard),
    row('Engines', usage.value.engines, e.max_engines.hard),
    row('Cache bytes', usage.value.cache_bytes, e.max_cache_bytes.hard),
  ]
})

async function load() {
  error.value = ''
  try {
    current.value = await getOrg(slug.value)
    displayName.value = current.value.display_name
    usage.value = await getOrgQuotas(slug.value)
    secrets.value = await listOrgSecrets(slug.value)
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to load organization'
  }
}

async function saveDisplayName() {
  saving.value = true
  try {
    current.value = await updateOrg(slug.value, { display_name: displayName.value })
    await auth.loadUser()
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to update organization'
  } finally {
    saving.value = false
  }
}

async function rotateToken() {
  newToken.value = (await generateOrgToken(slug.value)).token
}
async function revokeToken() {
  await revokeOrgToken(slug.value)
  newToken.value = ''
}
async function saveSecret() {
  if (!secretName.value) return
  await putOrgSecret(slug.value, secretName.value, secretValue.value)
  secretName.value = ''
  secretValue.value = ''
  secrets.value = await listOrgSecrets(slug.value)
}
async function removeSecret(name: string) {
  await deleteOrgSecret(slug.value, name)
  secrets.value = await listOrgSecrets(slug.value)
}

watch(() => route.params.org, () => { org.syncFromRoute(route); load() }, { immediate: true })
</script>
