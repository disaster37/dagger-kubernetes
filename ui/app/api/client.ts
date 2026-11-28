import axios from 'axios'
import { installAuthInterceptors, type AuthRecoveryMode, type AuthRequestConfig } from '~/api/auth-interceptor'
import { useAuthStore } from '~/stores/auth'
import { useOrgStore } from '~/stores/org'
import type {
  AuthUser,
  CheckRunRecord,
  ConnectEnvSnapshot,
  EngineCachePurgeResult,
  FleetInfo,
  FleetMetrics,
  Group,
  GroupSummary,
  HistoryInfo,
  HistoryPurgeRequest,
  HistoryPurgeResult,
  ImageCacheInfo,
  ImageCachePruneAllResult,
  ImageCachePruneRef,
  ImageCachePruneResult,
  ForgeSource,
  GitHubAccountSource,
  GitHubConnectionState,
  GitHubDeviceFlowStart,
  GitHubOAuthCompleteResult,
  GitHubOwner,
  OrgRepoSettings,
  RepoSetting,
  SourceRepositoryItem,
  LogSearchPage,
  LogSearchRequest,
  Org,
  OrgMember,
  OrgMemberRow,
  OrgQuotaUsage,
  OrgQuotas,
  OrgSummary,
  OrgTokenMeta,
  PlatformStatus,
  Project,
  ProtectedEnvironment,
  RemoteCacheStats,
  SecretMeta,
  Providers,
  TokenMeta,
  TraceDetail,
  TraceFilterParams,
  TraceLogEntry,
  TraceMetrics,
  TraceRow,
  UserRow,
  WorkflowDef,
  WorkflowRun,
} from '~/api/types'

const api = axios.create({
  baseURL: '/',
  timeout: 30000,
  withCredentials: true,
})

// The isolated interceptor owns 401 recovery: one refresh + one retry, then a
// single current-epoch navigation for protected APIs. Auth endpoints return to
// their explicit caller (no recursion, no swallowed error).
installAuthInterceptors(
  api,
  () => {
    const auth = useAuthStore()
    return {
      get authEpoch() { return auth.authEpoch },
      refreshSession: () => auth.refreshSession(),
      clearLocalSession: () => auth.clearLocalSession(),
    }
  },
  () => (typeof window !== 'undefined' ? window.location : null),
)

// Inject the active organization into every request unless the caller set it
// explicitly, or the request is an explicit `?org=all` cross-org admin view
// (which must not send X-Dagger-Org). The store is resolved lazily so this
// module can load before Pinia in tests/builds.
api.interceptors.request.use((config) => {
  try {
    const org = useOrgStore()
    const params = (config.params ?? {}) as Record<string, unknown>
    if (org.activeSlug && !config.headers['X-Dagger-Org'] && params.org !== 'all') {
      config.headers['X-Dagger-Org'] = org.activeSlug
    }
  } catch {
    // Pinia not initialized (SSR/build): skip.
  }
  return config
})

// Surface soft-quota warnings and make 429 messages actionable.
api.interceptors.response.use(
  (response) => {
    const warning = response.headers['x-dagger-quota-warning']
    if (warning && typeof window !== 'undefined') {
      useToast().add({ title: 'Organization quota warning', description: String(warning), color: 'warning' })
    }
    return response
  },
  (error) => {
    if (error?.response?.status === 429 && typeof window !== 'undefined') {
      useToast().add({
        title: 'Organization quota exceeded',
        description: String(error.response.data?.message ?? 'Organization quota exceeded'),
        color: 'error',
      })
    }
    return Promise.reject(error)
  },
)

export default api

// --- Auth ---
export async function fetchProviders(): Promise<Providers> {
  const { data } = await api.get('/api/v1/auth/providers')
  return data
}
export async function fetchMe(recovery: AuthRecoveryMode = 'local'): Promise<AuthUser> {
  const { data } = await api.get('/api/v1/auth/me', { authRecovery: recovery } as AuthRequestConfig)
  return data
}
export async function loginRequest(username: string, password: string): Promise<AuthUser> {
  const { data } = await api.post('/api/v1/auth/login', { username, password })
  return data
}
export async function refreshRequest(): Promise<void> {
  await api.post('/api/v1/auth/refresh')
}
export async function logoutRequest(): Promise<void> {
  await api.post('/api/v1/auth/logout')
}
export async function changePassword(currentPassword: string, newPassword: string): Promise<void> {
  await api.put('/api/v1/auth/password', { current_password: currentPassword, new_password: newPassword })
}

// --- Users (admin) ---
export async function listUsers(): Promise<UserRow[]> {
  const { data } = await api.get('/api/v1/users')
  return (data as UserRow[] | null) ?? []
}
export async function createUser(username: string, password: string, role: string): Promise<UserRow> {
  const { data } = await api.post('/api/v1/users', { username, password, role })
  return data
}
export async function updateUser(id: string, role: string): Promise<UserRow> {
  const { data } = await api.put(`/api/v1/users/${id}`, { role })
  return data
}
export async function deleteUser(id: string): Promise<void> {
  await api.delete(`/api/v1/users/${id}`)
}
export async function setUserState(id: string, action: 'disable' | 'enable' | 'restore'): Promise<UserRow> {
  const { data } = await api.post(`/api/v1/users/${id}/state`, { action })
  return data
}
export async function resetPassword(id: string, password: string): Promise<void> {
  await api.put(`/api/v1/users/${id}/password`, { password })
}
export async function setUserGroups(id: string, groupIds: string[]): Promise<GroupSummary[]> {
  const { data } = await api.put(`/api/v1/users/${id}/groups`, { group_ids: groupIds })
  return data
}
export async function getUserTokenMeta(id: string): Promise<TokenMeta> {
  const { data } = await api.get(`/api/v1/users/${id}/token`)
  return data
}
export async function revokeUserToken(id: string): Promise<void> {
  await api.delete(`/api/v1/users/${id}/token`)
}

// --- Groups (admin) ---
function activeOrgPath(): string {
  const org = useOrgStore()
  if (!org.activeSlug) throw new Error('no active organization')
  return `/api/v1/orgs/${encodeURIComponent(org.activeSlug)}`
}

export async function listGroups(): Promise<Group[]> {
  const { data } = await api.get(`${activeOrgPath()}/groups`)
  return (data as Group[] | null) ?? []
}
export async function createGroup(payload: Partial<Group>): Promise<Group> {
  const { data } = await api.post(`${activeOrgPath()}/groups`, payload)
  return data
}
export async function updateGroup(id: string, payload: Partial<Group>): Promise<Group> {
  const { data } = await api.put(`${activeOrgPath()}/groups/${id}`, payload)
  return data
}
export async function deleteGroup(id: string): Promise<void> {
  await api.delete(`${activeOrgPath()}/groups/${id}`)
}
export async function getGroupMembers(id: string): Promise<UserRow[]> {
  const { data } = await api.get(`${activeOrgPath()}/groups/${id}/members`)
  return (data as UserRow[] | null) ?? []
}
export async function setGroupMembers(id: string, userIds: string[]): Promise<void> {
  await api.put(`${activeOrgPath()}/groups/${id}/members`, { user_ids: userIds })
}

// --- Projects (admin) ---
export async function listProjects(): Promise<Project[]> {
  const { data } = await api.get(`${activeOrgPath()}/projects`)
  return (data as Project[] | null) ?? []
}
export async function createProject(name: string, groupId: string): Promise<Project> {
  const { data } = await api.post(`${activeOrgPath()}/projects`, { name, group_id: groupId })
  return data
}
export async function updateProject(id: string, groupId: string): Promise<Project> {
  const { data } = await api.put(`${activeOrgPath()}/projects/${id}`, { group_id: groupId })
  return data
}
export async function deleteProject(id: string): Promise<void> {
  await api.delete(`${activeOrgPath()}/projects/${id}`)
}

// --- Self-service tokens ---
export async function getMyToken(): Promise<TokenMeta> {
  const { data } = await api.get('/api/v1/tokens/me')
  return data
}
export async function createMyToken(): Promise<{ token: string }> {
  const { data } = await api.post('/api/v1/tokens/me')
  return data
}
export async function regenerateMyToken(): Promise<{ token: string }> {
  const { data } = await api.put('/api/v1/tokens/me/regenerate')
  return data
}
export async function revokeMyToken(): Promise<void> {
  await api.delete('/api/v1/tokens/me')
}

// --- Traces / fleet / cache ---
export async function fetchTraces(filter: TraceFilterParams = {}): Promise<TraceRow[]> {
  const params: Record<string, string> = {}
  if (filter.groupId) params.group_id = filter.groupId
  if (filter.ciRepo) params.ci_repo = filter.ciRepo
  if (filter.user) params.user = filter.user
  if (filter.org) params.org = filter.org
  const { data } = await api.get('/api/v1/traces', { params })
  return (data as TraceRow[] | null) ?? []
}
export async function fetchTrace(id: string): Promise<TraceDetail> {
  const { data } = await api.get(`/api/v1/traces/${id}`)
  return data
}
export async function fetchTraceLogs(id: string): Promise<TraceLogEntry[]> {
  const { data } = await api.get(`/api/v1/traces/${id}/logs`)
  const entries = data?.entries ?? []
  return entries as TraceLogEntry[]
}
export async function fetchTraceMetrics(id: string): Promise<TraceMetrics> {
  const { data } = await api.get(`/api/v1/traces/${id}/metrics`)
  return data as TraceMetrics
}
export async function fetchTraceSearch(id: string, params: LogSearchRequest): Promise<LogSearchPage> {
  const { data } = await api.get(`/api/v1/traces/${id}/search`, { params })
  const page = (data as LogSearchPage | null) ?? ({} as LogSearchPage)
  // The server may omit/null `entries` on an empty page; normalise so callers
  // can always iterate the array. `counts` is only present on the first page.
  return { entries: page.entries ?? [], next: page.next, counts: page.counts ?? {} }
}
export async function fetchFleetInfo(org?: string): Promise<FleetInfo[]> {
  const params: Record<string, string> = {}
  if (org) params.org = org
  const { data } = await api.get('/api/v1/fleet', { params })
  return (data as FleetInfo[] | null) ?? []
}
export async function purgeEngineCache(version: string, orgSlug?: string): Promise<EngineCachePurgeResult> {
  const params: Record<string, string> = {}
  if (orgSlug) params.org = orgSlug
  const headers: Record<string, string> = {}
  if (orgSlug) headers['X-Dagger-Org'] = orgSlug
  const { data } = await api.post(`/api/v1/fleet/${encodeURIComponent(version)}/purge-cache`, null, { params, headers })
  return data as EngineCachePurgeResult
}
export async function fetchFleetMetrics(version: string, orgSlug?: string): Promise<FleetMetrics | null> {
  const params: Record<string, string> = {}
  if (orgSlug) params.org = orgSlug
  const headers: Record<string, string> = {}
  if (orgSlug) headers['X-Dagger-Org'] = orgSlug
  const { data } = await api.get(`/api/v1/fleet/${encodeURIComponent(version)}/metrics`, { params, headers })
  return (data as FleetMetrics | null) ?? null
}
export async function fetchHistoryInfo(): Promise<HistoryInfo> {
  const { data } = await api.get('/api/v1/history')
  return data as HistoryInfo
}
export async function purgeHistory(payload: HistoryPurgeRequest): Promise<HistoryPurgeResult> {
  const { data } = await api.post('/api/v1/history/purge', payload)
  return data as HistoryPurgeResult
}
export async function purgeAllHistory(): Promise<HistoryPurgeResult> {
  const { data } = await api.post('/api/v1/history/purge-all')
  return data as HistoryPurgeResult
}
export async function fetchPlatformStatus(): Promise<PlatformStatus> {
  const { data } = await api.get('/api/v1/status')
  return data as PlatformStatus
}

// --- Image cache (local Zot mirrors, admin) ---
export async function fetchImageCacheInfo(): Promise<ImageCacheInfo> {
  // Every mirror walk is capped by its own 30s budget; allow more than the
  // 30s instance default so a slow mirror does not abort the listing.
  const { data } = await api.get('/api/v1/image-cache', { timeout: 60000 })
  return data as ImageCacheInfo
}
export async function pruneImageCache(mirrorId: string, refs: ImageCachePruneRef[]): Promise<ImageCachePruneResult> {
  // The server-side prune budget is 30s; 60s avoids racing it at the default.
  const { data } = await api.post('/api/v1/image-cache/prune', { mirror_id: mirrorId, refs }, { timeout: 60000 })
  return data as ImageCachePruneResult
}
export async function pruneAllImageCache(mirrorId?: string): Promise<ImageCachePruneAllResult> {
  // The server-side prune-all budget is 10 minutes; the 30s default would
  // abort the UI long before the server finishes.
  const { data } = await api.post('/api/v1/image-cache/prune-all', { mirror_id: mirrorId ?? '' }, { timeout: 660000 })
  return data as ImageCachePruneAllResult
}

// --- Connect-env snapshot ---
export async function fetchConnectEnv(version?: string, reveal?: boolean, tokenType?: string): Promise<ConnectEnvSnapshot> {
  const params: Record<string, string> = {}
  if (version) params.version = version
  if (reveal) params.reveal = 'true'
  if (tokenType) params.token_type = tokenType
  const { data } = await api.get('/api/v1/connect/env', { params })
  return data as ConnectEnvSnapshot
}

// SSE live trace stream (same-origin EventSource sends the session cookie).
export function connectLiveTrace(id: string): EventSource {
  return new EventSource(`/api/v1/traces/${id}/live`)
}

// SSE pipeline-list change stream (same-origin EventSource sends the session
// cookie; the supervisor also accepts ?token= via query fallback).
export function connectLivePipelines(): EventSource {
  return new EventSource('/api/v1/traces/live')
}

// --- Cloud parity: orgs, environments, secrets, integrations ---
export async function listOrgs(): Promise<Org[]> {
  const { data } = await api.get('/api/v1/orgs')
  return (data as Org[] | null) ?? []
}
export async function createOrg(payload: { name: string; display_name?: string; quotas?: Partial<OrgQuotas> }): Promise<Org> {
  const { data } = await api.post('/api/v1/orgs', payload)
  return data
}
export async function getOrg(org: string): Promise<Org> {
  const { data } = await api.get(`/api/v1/orgs/${encodeURIComponent(org)}`)
  return data
}
export async function updateOrg(org: string, payload: { display_name?: string; features?: string[] }): Promise<Org> {
  const { data } = await api.put(`/api/v1/orgs/${encodeURIComponent(org)}`, payload)
  return data
}
export async function deleteOrg(org: string, force = false): Promise<void> {
  await api.delete(`/api/v1/orgs/${encodeURIComponent(org)}`, { params: { force } })
}
export async function getOrgQuotas(org: string): Promise<OrgQuotaUsage> {
  const { data } = await api.get(`/api/v1/orgs/${encodeURIComponent(org)}/quotas`)
  return data
}
export async function updateOrgQuotas(org: string, quotas: OrgQuotas): Promise<OrgQuotaUsage> {
  const { data } = await api.put(`/api/v1/orgs/${encodeURIComponent(org)}/quotas`, quotas)
  return data
}
export async function listOrgMemberRows(org: string): Promise<OrgMemberRow[]> {
  const { data } = await api.get(`/api/v1/orgs/${encodeURIComponent(org)}/members`)
  return (data as OrgMemberRow[] | null) ?? []
}
export async function setOrgMemberGroups(org: string, userId: string, groupIds: string[]): Promise<GroupSummary[]> {
  const { data } = await api.put(`/api/v1/orgs/${encodeURIComponent(org)}/members/${userId}/groups`, { group_ids: groupIds })
  return (data as GroupSummary[] | null) ?? []
}
export async function listOrgMembers(org: string): Promise<OrgMember[]> {
  const { data } = await api.get(`/api/v1/orgs/${org}/members`)
  return (data as OrgMember[] | null) ?? []
}
export async function setOrgMember(org: string, userId: string, role: string): Promise<OrgMemberRow> {
  const { data } = await api.put(`/api/v1/orgs/${org}/members`, { user_id: userId, role })
  return data as OrgMemberRow
}
export async function removeOrgMember(org: string, userId: string): Promise<void> {
  await api.delete(`/api/v1/orgs/${org}/members/${userId}`)
}
export async function getOrgToken(org: string): Promise<OrgTokenMeta> {
  const { data } = await api.get(`/api/v1/orgs/${org}/token`)
  return data
}
export async function generateOrgToken(org: string): Promise<{ token: string; meta: TokenMeta }> {
  const { data } = await api.post(`/api/v1/orgs/${org}/token`)
  return data
}
export async function revokeOrgToken(org: string): Promise<void> {
  await api.delete(`/api/v1/orgs/${org}/token`)
}
export async function listOrgSecrets(org: string): Promise<SecretMeta[]> {
  const { data } = await api.get(`/api/v1/orgs/${org}/secrets`)
  return (data as SecretMeta[] | null) ?? []
}
export async function putOrgSecret(org: string, name: string, value: string): Promise<SecretMeta> {
  const { data } = await api.put(`/api/v1/orgs/${org}/secrets`, { name, value })
  return data
}
export async function deleteOrgSecret(org: string, name: string): Promise<void> {
  await api.delete(`/api/v1/orgs/${org}/secrets/${name}`)
}
export async function listProjectSecrets(projectId: string): Promise<SecretMeta[]> {
  const { data } = await api.get(`${activeOrgPath()}/projects/${projectId}/secrets`)
  return (data as SecretMeta[] | null) ?? []
}
export async function putProjectSecret(projectId: string, name: string, value: string): Promise<SecretMeta> {
  const { data } = await api.put(`${activeOrgPath()}/projects/${projectId}/secrets`, { name, value })
  return data
}
export async function deleteProjectSecret(projectId: string, name: string): Promise<void> {
  await api.delete(`${activeOrgPath()}/projects/${projectId}/secrets/${name}`)
}
export async function listEnvironments(org: string): Promise<ProtectedEnvironment[]> {
  const { data } = await api.get(`/api/v1/orgs/${org}/environments`)
  return (data as ProtectedEnvironment[] | null) ?? []
}
export async function putEnvironment(org: string, env: string, payload: { approver_group_ids: string[]; prevent_self_approval: boolean; approval_timeout: string }): Promise<ProtectedEnvironment> {
  const { data } = await api.put(`/api/v1/orgs/${org}/environments/${env}`, payload)
  return data
}
export async function deleteEnvironment(org: string, env: string): Promise<void> {
  await api.delete(`/api/v1/orgs/${org}/environments/${env}`)
}
export async function listEnvSecrets(org: string, env: string): Promise<SecretMeta[]> {
  const { data } = await api.get(`/api/v1/orgs/${org}/environments/${env}/secrets`)
  return (data as SecretMeta[] | null) ?? []
}
export async function putEnvSecret(org: string, env: string, name: string, value: string): Promise<SecretMeta> {
  const { data } = await api.put(`/api/v1/orgs/${org}/environments/${env}/secrets`, { name, value })
  return data
}
export async function deleteEnvSecret(org: string, env: string, name: string): Promise<void> {
  await api.delete(`/api/v1/orgs/${org}/environments/${env}/secrets/${name}`)
}
export async function listForgeSources(org: string): Promise<ForgeSource[]> {
  const { data } = await api.get(`/api/v1/orgs/${org}/integrations`)
  return (data as ForgeSource[] | null) ?? []
}
export async function createForgeSource(org: string, payload: Record<string, unknown>): Promise<ForgeSource> {
  const { data } = await api.post(`/api/v1/orgs/${org}/integrations`, payload)
  return data
}
export async function deleteForgeSource(org: string, sourceId: string): Promise<void> {
  await api.delete(`/api/v1/orgs/${org}/integrations/${sourceId}`)
}

// --- GitHub credential linking & repo claims ---
// Every GitHub integration call opts into 'local' auth recovery: a 401 may
// trigger one silent refresh but never clears the session or navigates. A
// missing/unlinked GitHub account is a business state (409), and the caller
// must always observe the rejection instead of being logged out.
export async function getGitHubConnection(): Promise<GitHubConnectionState> {
  const { data } = await api.get('/api/v1/github/connection', { authRecovery: 'local' } as AuthRequestConfig)
  return data
}
export async function disconnectGitHub(): Promise<void> {
  await api.delete('/api/v1/github/connection', { authRecovery: 'local' } as AuthRequestConfig)
}
export async function completeGitHubOAuth(code: string, state: string): Promise<GitHubOAuthCompleteResult> {
  const { data } = await api.post('/api/v1/github/oauth/complete', { code, state }, { authRecovery: 'local' } as AuthRequestConfig)
  return data
}
export async function connectGitHubPAT(token: string, baseUrl = ''): Promise<{ connection: GitHubConnectionState['connection']; available_owners: GitHubOwner[] }> {
  const { data } = await api.post('/api/v1/github/pat', { token, base_url: baseUrl }, { authRecovery: 'local' } as AuthRequestConfig)
  return data
}
export async function startGitHubDeviceFlow(org = ''): Promise<GitHubDeviceFlowStart> {
  const { data } = await api.post('/api/v1/github/device/code', { org }, { authRecovery: 'local' } as AuthRequestConfig)
  return data
}
export async function pollGitHubDeviceFlow(sessionId: string): Promise<{ status: 'pending' | 'slow_down' | 'complete'; interval?: number; result?: GitHubOAuthCompleteResult }> {
  const { data } = await api.post('/api/v1/github/device/poll', { session_id: sessionId }, { authRecovery: 'local' } as AuthRequestConfig)
  return data
}
export async function listGitHubOwners(): Promise<GitHubOwner[]> {
  const { data } = await api.get('/api/v1/github/owners', { authRecovery: 'local' } as AuthRequestConfig)
  return (data?.owners as GitHubOwner[] | null) ?? []
}
export async function startOrgGitHubOAuth(org: string, redirectUri: string): Promise<string> {
  const { data } = await api.post(`/api/v1/orgs/${org}/github/oauth/start`, { redirect_uri: redirectUri }, { authRecovery: 'local' } as AuthRequestConfig)
  return data.authorize_url
}
export async function listOrgGitHubAccounts(org: string): Promise<GitHubAccountSource[]> {
  const { data } = await api.get(`/api/v1/orgs/${org}/github/accounts`, { authRecovery: 'local' } as AuthRequestConfig)
  return (data as GitHubAccountSource[] | null) ?? []
}
export async function createOrgGitHubAccount(org: string, payload: { owner: string; base_url?: string; token?: string }): Promise<GitHubAccountSource> {
  const { data } = await api.post(`/api/v1/orgs/${org}/github/accounts`, payload, { authRecovery: 'local' } as AuthRequestConfig)
  return data
}
export async function deleteOrgGitHubAccount(org: string, accountId: string, cascade = true): Promise<void> {
  await api.delete(`/api/v1/orgs/${org}/github/accounts/${accountId}`, { params: { cascade }, authRecovery: 'local' } as AuthRequestConfig)
}
export async function fetchOrgRepoSettings(org: string): Promise<OrgRepoSettings[]> {
  const { data } = await api.post('/query', {
    query: `query($org: String!) { org(name: $org) { repoSettings { repo isPublic effectiveRunChecksOnDefaultBranch effectiveRunChecksOnPullRequest effectiveRunChecksOnBranchPush } } }`,
    variables: { org },
  }, { authRecovery: 'local' } as AuthRequestConfig)
  return (data?.data?.org?.repoSettings as OrgRepoSettings[] | null) ?? []
}
export async function listOrgGitHubRepositories(org: string, params: { account_id?: string; owner?: string; q?: string } = {}): Promise<SourceRepositoryItem[]> {
  const { data } = await api.get(`/api/v1/orgs/${org}/github/repositories`, { params, authRecovery: 'local' } as AuthRequestConfig)
  return (data?.repositories as SourceRepositoryItem[] | null) ?? []
}
export async function enableOrgGitHubSources(org: string, accountId: string, repos: Array<{ repo: string; run_checks_on_default_branch?: boolean; run_checks_on_pull_request?: boolean; run_checks_on_branch_push?: boolean; is_public?: boolean }>): Promise<{ updated: number }> {
  const { data } = await api.post(`/api/v1/orgs/${org}/github/sources`, { account_id: accountId, repos }, { authRecovery: 'local' } as AuthRequestConfig)
  return data
}
export async function updateOrgGitHubRepoSettings(org: string, owner: string, repo: string, payload: { run_checks_on_default_branch?: boolean; run_checks_on_pull_request?: boolean; run_checks_on_branch_push?: boolean; is_public?: boolean }): Promise<RepoSetting> {
  const { data } = await api.put(`/api/v1/orgs/${org}/github/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/settings`, payload, { authRecovery: 'local' } as AuthRequestConfig)
  return data
}
export async function deleteOrgGitHubRepoClaim(org: string, owner: string, repo: string): Promise<void> {
  await api.delete(`/api/v1/orgs/${org}/github/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/claim`, { authRecovery: 'local' } as AuthRequestConfig)
}

// --- Cloud Checks ---
export async function listOrgChecks(org: string, params: { status?: string; repo?: string; limit?: number } = {}): Promise<CheckRunRecord[]> {
  const { data } = await api.get(`/api/v1/orgs/${org}/checks`, { params })
  return (data as CheckRunRecord[] | null) ?? []
}
export async function approveForkCheckRun(id: string, includeSecrets = false): Promise<CheckRunRecord> {
  const { data } = await api.post(`/api/v1/checks/runs/${id}/approve`, { include_secrets: includeSecrets })
  return data
}
export async function rerunCheckRun(id: string, filters: { tests?: string[]; files?: string[]; checks?: string[] } = {}): Promise<CheckRunRecord> {
  const { data } = await api.post(`/api/v1/checks/runs/${id}/rerun`, filters)
  return data
}

// --- Workflows ---
export async function listOrgWorkflows(org: string, project?: string): Promise<WorkflowDef[]> {
  const { data } = await api.get(`/api/v1/orgs/${org}/workflows`, { params: { project } })
  return (data as WorkflowDef[] | null) ?? []
}
export async function dispatchWorkflow(org: string, payload: { project_id: string; workflow: string; ref?: string; inputs?: Record<string, unknown> }): Promise<WorkflowRun> {
  const { data } = await api.post(`/api/v1/orgs/${org}/workflows/dispatch`, payload)
  return data
}
export async function listOrgWorkflowRuns(org: string, params: { project?: string; status?: string; limit?: number } = {}): Promise<WorkflowRun[]> {
  const { data } = await api.get(`/api/v1/orgs/${org}/workflows/runs`, { params })
  return (data as WorkflowRun[] | null) ?? []
}
export async function getWorkflowRun(runId: string): Promise<WorkflowRun> {
  const { data } = await api.get(`/api/v1/workflows/runs/${runId}`)
  return data
}
export async function approveWorkflowJob(runId: string, job: string): Promise<WorkflowRun> {
  const { data } = await api.post(`/api/v1/workflows/runs/${runId}/approve/${job}`)
  return data
}
export async function rejectWorkflowJob(runId: string, job: string, reason = ''): Promise<WorkflowRun> {
  const { data } = await api.post(`/api/v1/workflows/runs/${runId}/reject/${job}`, { reason })
  return data
}

// --- Artifacts ---
export async function listArtifacts(params: { org: string; project?: string; run?: string }): Promise<string[]> {
  const { data } = await api.get('/api/v1/artifacts', { params })
  return (data as string[] | null) ?? []
}
export async function downloadArtifactBlob(key: string): Promise<Blob> {
  const { data } = await api.get('/api/v1/artifacts/download', { params: { key }, responseType: 'blob' })
  return data
}

// --- Remote cache ---
export async function fetchRemoteCacheStats(namespace?: string): Promise<RemoteCacheStats | null> {
  const { data } = await api.get('/api/v1/remote-cache', { params: { namespace } })
  return (data as RemoteCacheStats | null) ?? null
}
export async function purgeRemoteCache(namespace: string): Promise<void> {
  await api.post('/api/v1/remote-cache/purge', null, { params: { namespace } })
}
