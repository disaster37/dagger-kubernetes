<template>
  <div>
    <h1 class="mb-6 text-2xl font-semibold">Settings</h1>

    <UCard class="mb-4">
      <h3 class="font-semibold">Profile</h3>
      <UTable class="mt-3" :data="profileRows" :columns="profileColumns" />
    </UCard>

    <UCard class="mb-4">
      <h3 class="font-semibold">My Groups</h3>
      <ul v-if="auth.groups.length" class="mt-2 ml-6 list-disc">
        <li v-for="g in auth.groups" :key="g.id">{{ g.name }}</li>
      </ul>
      <p v-else class="mt-2 text-muted">You are not a member of any group.</p>
      <div v-if="auth.user?.oauth_groups?.length" class="mt-3">
        <h3 class="font-semibold">OAuth groups (upstream)</h3>
        <UBadge
          v-for="og in auth.user.oauth_groups"
          :key="og"
          class="mr-1"
          color="neutral"
          variant="soft"
        >{{ og }}</UBadge>
      </div>
    </UCard>

    <UCard class="mb-4">
      <h3 class="font-semibold">API Token</h3>
      <p class="mb-3 text-[13px] text-muted">
        Use this token as <code>DAGGER_CLOUD_TOKEN</code> in CI. It is shown once; store it securely.
      </p>
      <div v-if="tokenMeta">
        <UTable :data="tokenRows" :columns="tokenColumns" />
        <div v-if="plaintext" class="my-3">
          <code class="block rounded-md bg-elevated p-2 break-all">{{ plaintext }}</code>
        </div>
        <div class="mt-3 flex gap-2">
          <UButton color="neutral" variant="outline" @click="askRegenerate">Regenerate</UButton>
          <UButton color="error" variant="soft" @click="askRevoke">Revoke</UButton>
        </div>
      </div>
      <div v-else>
        <UButton color="primary" @click="create">Generate token</UButton>
        <div v-if="plaintext" class="mt-3">
          <code class="block rounded-md bg-elevated p-2 break-all">{{ plaintext }}</code>
        </div>
      </div>
      <UAlert v-if="tokenError" class="mt-2" color="error" variant="soft" :description="tokenError" />
    </UCard>

    <UCard class="mb-4">
      <h3 class="font-semibold">Change Password</h3>
      <UForm class="mt-3" @submit="handleChangePassword">
        <div class="mb-2">
          <UInput v-model="currentPw" type="password" placeholder="Current password" class="w-full" />
        </div>
        <div class="mb-2">
          <UInput v-model="newPw" type="password" placeholder="New password (min 8)" class="w-full" />
        </div>
        <UButton type="submit" color="primary">Change password</UButton>
        <UAlert v-if="pwError" class="mt-2" color="error" variant="soft" :description="pwError" />
        <UAlert v-if="pwOk" class="mt-2" color="success" variant="soft" description="Password changed." />
      </UForm>
    </UCard>

    <UCard v-if="auth.isAdmin">
      <h3 class="font-semibold">Admin</h3>
      <p class="mt-2">
        <ULink to="/admin/users">Manage users</ULink> ·
        <ULink to="/admin/groups">Manage groups</ULink> ·
        <ULink to="/admin/projects">Manage projects</ULink>
      </p>
    </UCard>

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
import { getMyToken, createMyToken, regenerateMyToken, revokeMyToken, changePassword } from '~/api/client'
import type { TokenMeta } from '~/api/types'

const auth = useAuthStore()
const tokenMeta = ref<TokenMeta | null>(null)
const plaintext = ref('')
const tokenError = ref('')
const currentPw = ref('')
const newPw = ref('')
const pwError = ref('')
const pwOk = ref(false)

const confirmOpen = ref(false)
const confirmTitle = ref('')
const confirmMessage = ref('')
let confirmAction: (() => void) | null = null

const profileColumns = [
  { id: 'label', accessorKey: 'label', header: '' },
  { id: 'value', accessorKey: 'value', header: '' },
]
const profileRows = computed(() => [
  { label: 'Username', value: auth.user?.username ?? '' },
  { label: 'Role', value: auth.user?.role ?? '' },
  { label: 'OAuth provider', value: auth.user?.oauth_provider || '—' },
])

const tokenColumns = [
  { id: 'label', accessorKey: 'label', header: '' },
  { id: 'value', accessorKey: 'value', header: '' },
]
const tokenRows = computed(() => [
  { label: 'Prefix', value: `${tokenMeta.value?.prefix ?? ''}…` },
  { label: 'Created', value: tokenMeta.value?.created_at ?? '' },
  { label: 'Last used', value: tokenMeta.value?.last_used_at || 'never' },
])

onMounted(async () => {
  try {
    tokenMeta.value = await getMyToken()
  } catch {
    tokenMeta.value = null
  }
})

// issueToken runs create/regenerate, stores the one-time plaintext, and
// refreshes the masked metadata.
async function issueToken(issue: () => Promise<{ token: string }>, failureMessage: string) {
  tokenError.value = ''
  try {
    const res = await issue()
    plaintext.value = res.token
    tokenMeta.value = await getMyToken()
  } catch (e: any) {
    tokenError.value = e.response?.data?.message || failureMessage
  }
}

function create() {
  issueToken(createMyToken, 'Failed to generate token')
}

function askRegenerate() {
  confirmTitle.value = 'Regenerate token'
  confirmMessage.value = 'Regenerating invalidates your current CI token immediately. Continue?'
  confirmAction = () => issueToken(regenerateMyToken, 'Failed to regenerate')
  confirmOpen.value = true
}

function askRevoke() {
  confirmTitle.value = 'Revoke token'
  confirmMessage.value = 'Revoke your API token? CI using it will stop working.'
  confirmAction = () => revoke()
  confirmOpen.value = true
}

function runConfirmed() {
  confirmOpen.value = false
  confirmAction?.()
  confirmAction = null
}

async function revoke() {
  tokenError.value = ''
  try {
    await revokeMyToken()
    tokenMeta.value = null
    plaintext.value = ''
  } catch (e: any) {
    tokenError.value = e.response?.data?.message || 'Failed to revoke'
  }
}

async function handleChangePassword() {
  pwError.value = ''
  pwOk.value = false
  if (newPw.value.length < 8) {
    pwError.value = 'New password must be at least 8 characters'
    return
  }
  try {
    await changePassword(currentPw.value, newPw.value)
    pwOk.value = true
    currentPw.value = ''
    newPw.value = ''
  } catch (e: any) {
    pwError.value = e.response?.data?.message || 'Failed to change password'
  }
}
</script>
