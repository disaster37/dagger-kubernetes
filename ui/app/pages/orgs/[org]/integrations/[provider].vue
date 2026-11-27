<template>
  <div class="mx-auto max-w-4xl space-y-4">
    <div>
      <h1 class="mb-2 text-2xl font-semibold capitalize">{{ provider }} integration</h1>
      <p class="text-[13px] text-muted">Connect {{ org }} to a {{ provider }} repository.</p>
    </div>
    <UAlert v-if="error" color="error" variant="soft" :description="error" />
    <UAlert v-if="ownersError" color="warning" variant="soft" :description="ownersError" />
    <UAlert v-if="notice" color="success" variant="soft" :description="notice" />

    <template v-if="provider === 'github'">
      <UCard>
        <template #header>
          <div class="flex items-center justify-between gap-2">
            <h2 class="font-semibold">Your GitHub identity</h2>
            <UBadge v-if="connection?.connected" color="success" variant="soft">connected</UBadge>
          </div>
        </template>
        <div v-if="connection?.connected" class="flex flex-wrap items-center justify-between gap-3 text-[13px]">
          <div>
            <p class="font-semibold">@{{ connection.connection?.github_login }}</p>
            <p class="text-muted">{{ connection.connection?.base_url }}</p>
            <div class="mt-1 flex flex-wrap gap-1">
              <UBadge v-for="scope in connection.connection?.scopes ?? []" :key="scope" color="neutral" variant="soft">{{ scope }}</UBadge>
            </div>
          </div>
          <UButton size="xs" color="error" variant="soft" @click="disconnectIdentity">Disconnect</UButton>
        </div>
        <div v-else class="space-y-4">
          <UAlert
            v-if="!connection?.oauth_enabled && !connection?.device_flow_enabled"
            color="warning"
            variant="soft"
            description="No GitHub sign-in is configured on this deployment. Use a personal access token to link repositories. An administrator can enable one-click linking with a GitHub OAuth App client_id."
          />
          <div class="flex flex-wrap gap-2">
            <UButton v-if="connection?.oauth_enabled" :loading="oauthStarting" @click="startOAuth">Connect via GitHub OAuth</UButton>
            <UButton v-if="connection?.device_flow_enabled" variant="soft" :loading="deviceStarting" @click="startDeviceFlow">
              Connect via Device Code
            </UButton>
          </div>
          <form class="flex flex-wrap items-end gap-2" @submit.prevent="connectPAT">
            <UFormField label="Personal access token" class="min-w-64 flex-1">
              <UInput v-model="patToken" type="password" class="w-full" autocomplete="off" />
            </UFormField>
            <UButton type="submit" variant="soft" :loading="patLoading">Link with PAT</UButton>
          </form>
        </div>
      </UCard>

      <UCard>
        <template #header><h2 class="font-semibold">Connected organization accounts</h2></template>
        <div v-for="acct in accounts" :key="acct.id" class="mb-2 flex flex-wrap items-center justify-between gap-2 rounded border border-default p-2">
          <div class="text-[13px]">
            <p class="font-semibold">@{{ acct.owner }}</p>
            <p class="text-muted">{{ acct.owner_type }} · {{ acct.base_url }}</p>
          </div>
          <UButton size="xs" color="error" variant="soft" @click="disconnectAccount(acct)">Disconnect</UButton>
        </div>
        <UEmpty v-if="!accounts.length" icon="i-lucide-users" title="No organization accounts" description="Connect a GitHub owner above." />
        <form class="mt-4 flex flex-wrap items-end gap-2" @submit.prevent="connectAccount">
          <UFormField label="GitHub owner (user or org)" class="min-w-56 flex-1">
            <UInput v-model="accountOwner" class="w-full" placeholder="acme" />
          </UFormField>
          <UFormField label="PAT (optional when linked above)" class="min-w-56 flex-1">
            <UInput v-model="accountToken" type="password" class="w-full" autocomplete="off" />
          </UFormField>
          <UButton type="submit" :loading="accountLoading">Connect owner</UButton>
        </form>
        <div v-if="owners.length" class="mt-2 flex flex-wrap gap-1">
          <UButton v-for="owner in owners" :key="owner.login" size="xs" variant="soft" @click="accountOwner = owner.login">
            @{{ owner.login }}
          </UButton>
        </div>
      </UCard>

      <UCard>
        <template #header><h2 class="font-semibold">Repositories & check triggers</h2></template>
        <div class="mb-3 flex flex-wrap items-end gap-2">
          <UFormField label="Account" class="min-w-48">
            <select v-model="selectedAccountID" class="rounded border border-default bg-default px-2 py-1 text-[13px]" @change="loadRepositories">
              <option value="">Select an account</option>
              <option v-for="acct in accounts" :key="acct.id" :value="acct.id">@{{ acct.owner }}</option>
            </select>
          </UFormField>
          <UFormField label="Filter owner" class="min-w-40"><UInput v-model="ownerFilter" class="w-full" /></UFormField>
          <UFormField label="Search" class="min-w-48"><UInput v-model="search" class="w-full" /></UFormField>
          <UButton variant="soft" :loading="reposLoading" @click="loadRepositories">Search</UButton>
        </div>
        <div v-for="repo in repositories" :key="repo.repository" class="mb-2 rounded border border-default p-2 text-[13px]">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <div>
              <p class="font-semibold">{{ repo.full_name }}</p>
              <div class="mt-1 flex flex-wrap gap-1">
                <UBadge v-if="repo.private" color="neutral" variant="soft">private</UBadge>
                <UBadge v-if="repo.archived" color="warning" variant="soft">archived</UBadge>
                <UBadge v-if="!repo.admin_perm" color="warning" variant="soft">missing repo admin</UBadge>
                <UBadge v-if="repo.selected" color="success" variant="soft">claimed</UBadge>
                <UBadge v-else-if="repo.claimed_by_org_name" color="error" variant="soft">claimed by {{ repo.claimed_by_org_name }}</UBadge>
              </div>
            </div>
            <div class="flex gap-1">
              <UButton v-if="!repo.selected && repo.eligible" size="xs" :loading="repoAction === repo.repository" @click="enableRepo(repo)">
                Enable
              </UButton>
              <UButton v-if="repo.selected" size="xs" color="error" variant="soft" @click="releaseRepo(repo)">Release</UButton>
            </div>
          </div>
          <div v-if="repo.selected" class="mt-2 flex flex-wrap gap-4">
            <UCheckbox
              :model-value="triggers[repo.repository]?.defaultBranch ?? true"
              label="Default branch"
              @update:model-value="(v: boolean | 'indeterminate') => toggleRepo(repo, 'defaultBranch', v === true)"
            />
            <UCheckbox
              :model-value="triggers[repo.repository]?.pullRequest ?? true"
              label="Pull requests"
              @update:model-value="(v: boolean | 'indeterminate') => toggleRepo(repo, 'pullRequest', v === true)"
            />
            <UCheckbox
              :model-value="triggers[repo.repository]?.branchPush ?? false"
              label="Branch pushes"
              @update:model-value="(v: boolean | 'indeterminate') => toggleRepo(repo, 'branchPush', v === true)"
            />
          </div>
        </div>
        <UEmpty v-if="!reposLoading && !repositories.length" icon="i-lucide-git-branch" title="No repositories" description="Select an account and search." />
      </UCard>
    </template>

    <UCard>
      <template #header><h2 class="font-semibold">Existing per-repo integrations</h2></template>
      <div v-for="src in sources" :key="src.id" class="mb-2 flex flex-wrap items-center justify-between gap-2 rounded border border-default p-2">
        <div class="text-[13px]">
          <p class="font-semibold">{{ src.repo }}</p>
          <p class="text-muted">webhook: <code>/v1/webhooks/{{ src.provider }}/{{ src.id }}</code></p>
          <p v-if="src.app_id" class="text-muted">app id: {{ src.app_id }} · installation: {{ src.installation_id }}</p>
        </div>
        <UButton size="xs" color="error" variant="soft" @click="remove(src.id)">Delete</UButton>
      </div>
      <UEmpty v-if="!sources.length" icon="i-lucide-plug" title="No integrations" description="Add one below." />
    </UCard>

    <UCard>
      <template #header><h2 class="font-semibold">Add manual integration</h2></template>
      <form class="space-y-4" @submit.prevent="save">
        <UFormField label="Repository (owner/repo)"><UInput v-model="repo" class="w-full" /></UFormField>
        <UFormField label="API base URL (optional)"><UInput v-model="baseUrl" class="w-full" placeholder="https://api.github.com" /></UFormField>
        <UFormField label="Access token"><UInput v-model="token" type="password" class="w-full" autocomplete="off" /></UFormField>
        <UFormField label="Webhook secret" help="Auto-generated if empty."><UInput v-model="secret" type="password" class="w-full" placeholder="auto-generated" /></UFormField>
        <div v-if="provider === 'github'" class="grid gap-3 md:grid-cols-2">
          <UFormField label="GitHub App ID (optional)"><UInput v-model="appID" class="w-full" /></UFormField>
          <UFormField label="Installation ID (optional)"><UInput v-model="installationID" class="w-full" /></UFormField>
        </div>
        <UButton type="submit" :loading="loading">Save integration</UButton>
      </form>
    </UCard>

    <UModal v-model:open="deviceModalOpen" title="Authorize GitHub device" @update:open="onDeviceModalOpen">
      <template #body>
        <div v-if="device" class="space-y-4 text-[13px]">
          <p class="text-muted">Enter this code on GitHub to authorize {{ org }}:</p>
          <div class="flex flex-wrap items-center gap-2">
            <code class="select-all rounded border border-default bg-elevated px-4 py-3 font-mono text-2xl font-semibold tracking-[0.3em]">{{ device.user_code }}</code>
            <UButton size="sm" variant="soft" icon="i-lucide-copy" @click="copyCode">Copy</UButton>
          </div>
          <p v-if="copyState" class="text-muted">{{ copyState }}</p>
          <UButton :to="device.verification_uri" target="_blank" rel="noopener noreferrer" variant="soft" icon="i-lucide-external-link">
            Open {{ device.verification_uri }}
          </UButton>
          <p v-if="deviceExpired" class="text-error">Code expired — start again.</p>
          <p v-else class="flex items-center gap-2 text-muted">
            <UIcon name="i-lucide-loader-circle" class="size-4 animate-spin" />
            <span>{{ deviceStatus || 'Waiting for authorization…' }}</span>
            <span class="tabular-nums">({{ countdownLabel }})</span>
          </p>
        </div>
        <div v-else class="space-y-4 text-[13px]">
          <p class="text-muted">Device authorization is unavailable on this deployment.</p>
          <div class="flex justify-end">
            <UButton color="neutral" variant="outline" @click="cancelDeviceFlow">Close</UButton>
          </div>
        </div>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2">
          <UButton v-if="deviceExpired" @click="startDeviceFlow">Start again</UButton>
          <UButton color="neutral" variant="outline" @click="cancelDeviceFlow">Cancel</UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>

<script setup lang="ts">
import {
  connectGitHubPAT,
  createForgeSource,
  createOrgGitHubAccount,
  deleteForgeSource,
  deleteOrgGitHubAccount,
  deleteOrgGitHubRepoClaim,
  disconnectGitHub,
  enableOrgGitHubSources,
  fetchOrgRepoSettings,
  getGitHubConnection,
  listForgeSources,
  listGitHubOwners,
  listOrgGitHubAccounts,
  listOrgGitHubRepositories,
  pollGitHubDeviceFlow,
  startGitHubDeviceFlow,
  startOrgGitHubOAuth,
  updateOrgGitHubRepoSettings,
} from '~/api/client'
import type {
  ForgeSource,
  GitHubAccountSource,
  GitHubConnectionState,
  GitHubDeviceFlowStart,
  GitHubOwner,
  SourceRepositoryItem,
} from '~/api/types'

const route = useRoute()
const org = computed(() => String(route.params.org))
const provider = computed(() => String(route.params.provider))
const sources = ref<ForgeSource[]>([])
const connection = ref<GitHubConnectionState | null>(null)
const accounts = ref<GitHubAccountSource[]>([])
const owners = ref<GitHubOwner[]>([])
const repositories = ref<SourceRepositoryItem[]>([])
const triggers = reactive<Record<string, { defaultBranch: boolean; pullRequest: boolean; branchPush: boolean }>>({})

const patToken = ref('')
const patLoading = ref(false)
const oauthStarting = ref(false)
const device = ref<GitHubDeviceFlowStart | null>(null)
const deviceStarting = ref(false)
const deviceModalOpen = ref(false)
const deviceExpired = ref(false)
const deviceStatus = ref('')
const deviceRemaining = ref(0)
const copyState = ref('')
const accountOwner = ref('')
const accountToken = ref('')
const accountLoading = ref(false)
const selectedAccountID = ref('')
const ownerFilter = ref('')
const search = ref('')
const reposLoading = ref(false)
const repoAction = ref('')

const repo = ref('')
const baseUrl = ref('')
const token = ref('')
const secret = ref('')
const appID = ref('')
const installationID = ref('')
const loading = ref(false)
const error = ref('')
const ownersError = ref('')
const notice = ref('')

let pollTimer: ReturnType<typeof setTimeout> | null = null
let expiryTimer: ReturnType<typeof setInterval> | null = null

const countdownLabel = computed(() => {
  const total = Math.max(0, deviceRemaining.value)
  const minutes = Math.floor(total / 60)
  const seconds = total % 60
  return `${minutes}:${String(seconds).padStart(2, '0')}`
})

function repoTail(repo: string): string {
  const parts = repo.toLowerCase().split('/')
  return parts.slice(-2).join('/')
}

function stopPolling() {
  if (pollTimer) {
    clearTimeout(pollTimer)
    pollTimer = null
  }
}

function stopExpiryCountdown() {
  if (expiryTimer) {
    clearInterval(expiryTimer)
    expiryTimer = null
  }
}

function updateRemaining() {
  const expiresAt = device.value ? Date.parse(device.value.expires_at) : Number.NaN
  deviceRemaining.value = Number.isFinite(expiresAt) ? Math.max(0, Math.ceil((expiresAt - Date.now()) / 1000)) : 0
  if (deviceRemaining.value <= 0) {
    deviceExpired.value = true
    deviceStatus.value = ''
    stopPolling()
    stopExpiryCountdown()
  }
}

function startExpiryCountdown() {
  stopExpiryCountdown()
  updateRemaining()
  if (!deviceExpired.value) expiryTimer = setInterval(updateRemaining, 1000)
}

async function load() {
  error.value = ''
  ownersError.value = ''
  try {
    sources.value = (await listForgeSources(org.value)).filter(s => s.provider === provider.value)
    if (provider.value !== 'github') return
    connection.value = await getGitHubConnection()
    accounts.value = await listOrgGitHubAccounts(org.value)
    owners.value = []
    try {
      owners.value = await listGitHubOwners()
    } catch {
      ownersError.value = 'Link a GitHub account to browse repositories.'
    }
    if (selectedAccountID.value && !accounts.value.some(a => a.id === selectedAccountID.value)) selectedAccountID.value = ''
    if (!selectedAccountID.value && accounts.value.length) selectedAccountID.value = accounts.value[0]!.id
    if (selectedAccountID.value) await loadRepositories()
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to load integrations'
  }
}

async function loadRepositories() {
  if (!selectedAccountID.value) {
    repositories.value = []
    return
  }
  reposLoading.value = true
  try {
    const [repos, settings] = await Promise.all([
      listOrgGitHubRepositories(org.value, {
        account_id: selectedAccountID.value,
        owner: ownerFilter.value,
        q: search.value,
      }),
      fetchOrgRepoSettings(org.value).catch(() => []),
    ])
    repositories.value = repos
    for (const item of repositories.value) {
      const match = settings.find(s => repoTail(s.repo) === item.full_name.toLowerCase())
      if (item.selected) {
        triggers[item.repository] = {
          defaultBranch: match?.effectiveRunChecksOnDefaultBranch ?? true,
          pullRequest: match?.effectiveRunChecksOnPullRequest ?? true,
          branchPush: match?.effectiveRunChecksOnBranchPush ?? false,
        }
      }
    }
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to list repositories'
  } finally {
    reposLoading.value = false
  }
}

async function connectPAT() {
  if (!patToken.value) return
  patLoading.value = true
  error.value = ''
  try {
    await connectGitHubPAT(patToken.value)
    patToken.value = ''
    notice.value = 'GitHub account linked.'
    await load()
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to link GitHub token'
  } finally {
    patLoading.value = false
  }
}

async function startOAuth() {
  oauthStarting.value = true
  error.value = ''
  try {
    const redirect = `${window.location.origin}/github/callback`
    const url = await startOrgGitHubOAuth(org.value, redirect)
    window.location.href = url
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to start GitHub OAuth'
    oauthStarting.value = false
  }
}

async function startDeviceFlow() {
  deviceStarting.value = true
  error.value = ''
  copyState.value = ''
  deviceStatus.value = ''
  deviceExpired.value = false
  device.value = null
  deviceModalOpen.value = false
  try {
    const started = await startGitHubDeviceFlow(org.value)
    if (!started?.user_code || !started?.verification_uri) {
      error.value = 'GitHub returned an incomplete device authorization response'
      return
    }
    device.value = started
    deviceStatus.value = 'Waiting for authorization…'
    deviceModalOpen.value = true
    startExpiryCountdown()
    schedulePoll(started.interval)
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to start device flow'
    device.value = null
    deviceModalOpen.value = false
  } finally {
    deviceStarting.value = false
  }
}

function schedulePoll(seconds: number) {
  stopPolling()
  pollTimer = setTimeout(pollDevice, Math.max(seconds, 5) * 1000)
}

async function pollDevice() {
  if (!device.value || deviceExpired.value) return
  try {
    const resp = await pollGitHubDeviceFlow(device.value.session_id)
    if (resp.status === 'complete') {
      stopPolling()
      stopExpiryCountdown()
      device.value = null
      deviceModalOpen.value = false
      deviceStatus.value = ''
      if (resp.result?.target_org_slug) {
        await navigateTo(`/orgs/${resp.result.target_org_slug}/integrations/github?connected=1`)
        return
      }
      notice.value = `Connected to GitHub as @${resp.result?.connection.github_login ?? ''}.`
      await load()
      return
    }
    deviceStatus.value = resp.status === 'slow_down' ? 'GitHub asked us to slow down; retrying…' : 'Waiting for authorization…'
    schedulePoll(resp.interval ?? device.value.interval)
  } catch (e: any) {
    stopPolling()
    stopExpiryCountdown()
    device.value = null
    deviceModalOpen.value = false
    deviceStatus.value = ''
    error.value = e?.response?.data?.message ?? 'GitHub device authorization failed'
  }
}

async function copyCode() {
  const code = device.value?.user_code ?? ''
  if (!code) return
  copyState.value = ''
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(code)
    } else {
      fallbackCopy(code)
    }
    copyState.value = 'Copied.'
  } catch {
    try {
      fallbackCopy(code)
      copyState.value = 'Copied.'
    } catch {
      copyState.value = 'Copy failed — select the code and copy it manually.'
    }
  }
}

function fallbackCopy(text: string) {
  const el = document.createElement('textarea')
  el.value = text
  el.setAttribute('readonly', '')
  el.style.position = 'fixed'
  el.style.opacity = '0'
  document.body.appendChild(el)
  el.select()
  try {
    if (!document.execCommand('copy')) throw new Error('copy failed')
  } finally {
    document.body.removeChild(el)
  }
}

function cancelDeviceFlow() {
  stopPolling()
  stopExpiryCountdown()
  device.value = null
  deviceModalOpen.value = false
  deviceStatus.value = ''
  copyState.value = ''
}

function onDeviceModalOpen(open: boolean) {
  if (!open) cancelDeviceFlow()
}

async function disconnectIdentity() {
  try {
    await disconnectGitHub()
    notice.value = 'GitHub identity disconnected.'
    await load()
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to disconnect GitHub identity'
  }
}

async function connectAccount() {
  if (!accountOwner.value) return
  accountLoading.value = true
  error.value = ''
  try {
    await createOrgGitHubAccount(org.value, {
      owner: accountOwner.value,
      token: accountToken.value || undefined,
    })
    accountOwner.value = ''
    accountToken.value = ''
    notice.value = 'GitHub owner connected to the organization.'
    await load()
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to connect GitHub owner'
  } finally {
    accountLoading.value = false
  }
}

async function disconnectAccount(acct: GitHubAccountSource) {
  if (!window.confirm(`Disconnect @${acct.owner} and release its repository claims?`)) return
  try {
    await deleteOrgGitHubAccount(org.value, acct.id, true)
    await load()
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to disconnect GitHub owner'
  }
}

async function enableRepo(item: SourceRepositoryItem) {
  repoAction.value = item.repository
  error.value = ''
  try {
    await enableOrgGitHubSources(org.value, selectedAccountID.value, [{
      repo: item.repository,
      run_checks_on_default_branch: true,
      run_checks_on_pull_request: true,
      run_checks_on_branch_push: false,
      is_public: !item.private,
    }])
    triggers[item.repository] = { defaultBranch: true, pullRequest: true, branchPush: false }
    notice.value = `Checks enabled for ${item.full_name}.`
    await loadRepositories()
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to enable checks'
  } finally {
    repoAction.value = ''
  }
}

async function releaseRepo(item: SourceRepositoryItem) {
  const [owner, name] = item.full_name.split('/')
  if (!owner || !name) return
  repoAction.value = item.repository
  try {
    await deleteOrgGitHubRepoClaim(org.value, owner, name)
    delete triggers[item.repository]
    await loadRepositories()
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to release repository'
  } finally {
    repoAction.value = ''
  }
}

async function toggleRepo(item: SourceRepositoryItem, key: 'defaultBranch' | 'pullRequest' | 'branchPush', value: boolean) {
  const [owner, name] = item.full_name.split('/')
  if (!owner || !name) return
  const next = triggers[item.repository] ?? { defaultBranch: true, pullRequest: true, branchPush: false }
  next[key] = value
  triggers[item.repository] = next
  try {
    await updateOrgGitHubRepoSettings(org.value, owner, name, {
      run_checks_on_default_branch: next.defaultBranch,
      run_checks_on_pull_request: next.pullRequest,
      run_checks_on_branch_push: next.branchPush,
    })
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to update check triggers'
  }
}

async function save() {
  loading.value = true
  error.value = ''
  try {
    await createForgeSource(org.value, {
      provider: provider.value,
      repo: repo.value,
      base_url: baseUrl.value,
      token: token.value,
      webhook_secret: secret.value,
      app_id: appID.value || undefined,
      installation_id: installationID.value || undefined,
    })
    notice.value = 'Integration saved and webhook registered.'
    repo.value = ''
    token.value = ''
    secret.value = ''
    await load()
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to save integration'
  } finally {
    loading.value = false
  }
}

async function remove(id: string) {
  await deleteForgeSource(org.value, id)
  await load()
}

onMounted(load)
onBeforeUnmount(() => {
  // Null device so an in-flight poll that resolves after unmount cannot
  // reschedule itself and keep polling the device endpoint post-navigation.
  cancelDeviceFlow()
})
</script>
