<template>
  <div>
    <h1 class="page-title">Settings</h1>

    <div class="card">
      <h3>Profile</h3>
      <table style="margin-top: 12px;">
        <tbody>
          <tr><td>Username</td><td>{{ auth.user?.username }}</td></tr>
          <tr><td>Role</td><td>{{ auth.user?.role }}</td></tr>
          <tr><td>OAuth provider</td><td>{{ auth.user?.oauth_provider || '—' }}</td></tr>
        </tbody>
      </table>
    </div>

    <div class="card">
      <h3>My Groups</h3>
      <ul v-if="auth.groups.length" style="padding-left: 24px; margin-top: 8px;">
        <li v-for="g in auth.groups" :key="g.id">{{ g.name }}</li>
      </ul>
      <p v-else style="color: #8b949e; margin-top: 8px;">You are not a member of any group.</p>
    </div>

    <div class="card">
      <h3>API Token</h3>
      <p style="font-size: 13px; color: #8b949e; margin-bottom: 12px;">
        Use this token as <code>DAGGER_CLOUD_TOKEN</code> in CI. It is shown once; store it securely.
      </p>
      <div v-if="tokenMeta">
        <table>
          <tbody>
            <tr><td>Prefix</td><td><code>{{ tokenMeta.prefix }}…</code></td></tr>
            <tr><td>Created</td><td>{{ tokenMeta.created_at }}</td></tr>
            <tr><td>Last used</td><td>{{ tokenMeta.last_used_at || 'never' }}</td></tr>
          </tbody>
        </table>
        <div v-if="plaintext" style="margin: 12px 0;">
          <code style="display: block; padding: 8px; background: #161b22; border-radius: 6px; word-break: break-all;">{{ plaintext }}</code>
        </div>
        <div style="margin-top: 12px;">
          <button class="btn" @click="regenerate">Regenerate</button>
          <button class="btn btn-danger" style="margin-left: 8px;" @click="revoke">Revoke</button>
        </div>
      </div>
      <div v-else>
        <button class="btn btn-primary" @click="create">Generate token</button>
        <div v-if="plaintext" style="margin-top: 12px;">
          <code style="display: block; padding: 8px; background: #161b22; border-radius: 6px; word-break: break-all;">{{ plaintext }}</code>
        </div>
      </div>
      <p v-if="tokenError" style="color: #f85149; font-size: 13px; margin-top: 8px;">{{ tokenError }}</p>
    </div>

    <div class="card">
      <h3>Change Password</h3>
      <form @submit.prevent="handleChangePassword" style="margin-top: 12px;">
        <div style="margin-bottom: 8px;">
          <input v-model="currentPw" type="password" placeholder="Current password" style="width: 100%; padding: 8px 12px; background: #0d1117; border: 1px solid #30363d; border-radius: 6px; color: #c9d1d9;" />
        </div>
        <div style="margin-bottom: 8px;">
          <input v-model="newPw" type="password" placeholder="New password (min 8)" style="width: 100%; padding: 8px 12px; background: #0d1117; border: 1px solid #30363d; border-radius: 6px; color: #c9d1d9;" />
        </div>
        <button type="submit" class="btn btn-primary">Change password</button>
        <p v-if="pwError" style="color: #f85149; font-size: 13px; margin-top: 8px;">{{ pwError }}</p>
        <p v-if="pwOk" style="color: #3fb950; font-size: 13px; margin-top: 8px;">Password changed.</p>
      </form>
    </div>

    <div v-if="auth.isAdmin" class="card">
      <h3>Admin</h3>
      <p style="margin-top: 8px;">
        <router-link to="/admin/users">Manage users</router-link> ·
        <router-link to="/admin/groups">Manage groups</router-link> ·
        <router-link to="/admin/projects">Manage projects</router-link>
      </p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useAuthStore } from '@/stores/auth'
import { getMyToken, createMyToken, regenerateMyToken, revokeMyToken, changePassword } from '@/api/client'
import type { TokenMeta } from '@/api/types'

const auth = useAuthStore()
const tokenMeta = ref<TokenMeta | null>(null)
const plaintext = ref('')
const tokenError = ref('')
const currentPw = ref('')
const newPw = ref('')
const pwError = ref('')
const pwOk = ref(false)

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

function regenerate() {
  if (!confirm('Regenerating invalidates your current CI token immediately. Continue?')) return
  issueToken(regenerateMyToken, 'Failed to regenerate')
}

async function revoke() {
  if (!confirm('Revoke your API token? CI using it will stop working.')) return
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
