import axios from 'axios'
import { installAuthInterceptors, type AuthRecoveryMode, type AuthRequestConfig } from '~/api/auth-interceptor'
import { useAuthStore } from '~/stores/auth'
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
  LogSearchPage,
  LogSearchRequest,
  Org,
  OrgMember,
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
  return data
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
export async function listGroups(): Promise<Group[]> {
  const { data } = await api.get('/api/v1/groups')
  return data
}
export async function createGroup(payload: Partial<Group>): Promise<Group> {
  const { data } = await api.post('/api/v1/groups', payload)
  return data
}
export async function updateGroup(id: string, payload: Partial<Group>): Promise<Group> {
  const { data } = await api.put(`/api/v1/groups/${id}`, payload)
  return data
}
export async function deleteGroup(id: string): Promise<void> {
  await api.delete(`/api/v1/groups/${id}`)
}
export async function getGroupMembers(id: string): Promise<UserRow[]> {
  const { data } = await api.get(`/api/v1/groups/${id}/members`)
  return data
}
export async function setGroupMembers(id: string, userIds: string[]): Promise<void> {
  await api.put(`/api/v1/groups/${id}/members`, { user_ids: userIds })
}

// --- Projects (admin) ---
export async function listProjects(): Promise<Project[]> {
  const { data } = await api.get('/api/v1/projects')
  return data
}
export async function createProject(name: string, groupId: string): Promise<Project> {
  const { data } = await api.post('/api/v1/projects', { name, group_id: groupId })
  return data
}
export async function updateProject(id: string, groupId: string): Promise<Project> {
  const { data } = await api.put(`/api/v1/projects/${id}`, { group_id: groupId })
  return data
}
export async function deleteProject(id: string): Promise<void> {
  await api.delete(`/api/v1/projects/${id}`)
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
  const { data } = await api.get('/api/v1/traces', { params })
  return data
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
  const page = data as LogSearchPage
  // The server may omit/null `entries` on an empty page; normalise so callers
  // can always iterate the array. `counts` is only present on the first page.
  return { entries: page.entries ?? [], next: page.next, counts: page.counts ?? {} }
}
export async function fetchFleetInfo(): Promise<FleetInfo[]> {
  const { data } = await api.get('/api/v1/fleet')
  return (data as FleetInfo[] | null) ?? []
}
export async function purgeEngineCache(version: string): Promise<EngineCachePurgeResult> {
  const { data } = await api.post(`/api/v1/fleet/${encodeURIComponent(version)}/purge-cache`)
  return data as EngineCachePurgeResult
}
export async function fetchFleetMetrics(version: string): Promise<FleetMetrics> {
  const { data } = await api.get(`/api/v1/fleet/${encodeURIComponent(version)}/metrics`)
  return data as FleetMetrics
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
export async function fetchConnectEnv(version?: string, reveal?: boolean): Promise<ConnectEnvSnapshot> {
  const params: Record<string, string> = {}
  if (version) params.version = version
  if (reveal) params.reveal = 'true'
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
  return data
}
export async function createOrg(payload: { name: string; display_name?: string }): Promise<Org> {
  const { data } = await api.post('/api/v1/orgs', payload)
  return data
}
export async function listOrgMembers(org: string): Promise<OrgMember[]> {
  const { data } = await api.get(`/api/v1/orgs/${org}/members`)
  return data
}
export async function setOrgMember(org: string, userId: string, role: string): Promise<void> {
  await api.put(`/api/v1/orgs/${org}/members`, { user_id: userId, role })
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
  return data
}
export async function putOrgSecret(org: string, name: string, value: string): Promise<SecretMeta> {
  const { data } = await api.put(`/api/v1/orgs/${org}/secrets`, { name, value })
  return data
}
export async function deleteOrgSecret(org: string, name: string): Promise<void> {
  await api.delete(`/api/v1/orgs/${org}/secrets/${name}`)
}
export async function listProjectSecrets(projectId: string): Promise<SecretMeta[]> {
  const { data } = await api.get(`/api/v1/projects/${projectId}/secrets`)
  return data
}
export async function putProjectSecret(projectId: string, name: string, value: string): Promise<SecretMeta> {
  const { data } = await api.put(`/api/v1/projects/${projectId}/secrets`, { name, value })
  return data
}
export async function deleteProjectSecret(projectId: string, name: string): Promise<void> {
  await api.delete(`/api/v1/projects/${projectId}/secrets/${name}`)
}
export async function listEnvironments(org: string): Promise<ProtectedEnvironment[]> {
  const { data } = await api.get(`/api/v1/orgs/${org}/environments`)
  return data
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
  return data
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
  return data
}
export async function createForgeSource(org: string, payload: Record<string, unknown>): Promise<ForgeSource> {
  const { data } = await api.post(`/api/v1/orgs/${org}/integrations`, payload)
  return data
}
export async function deleteForgeSource(org: string, sourceId: string): Promise<void> {
  await api.delete(`/api/v1/orgs/${org}/integrations/${sourceId}`)
}

// --- Cloud Checks ---
export async function listOrgChecks(org: string, params: { status?: string; repo?: string; limit?: number } = {}): Promise<CheckRunRecord[]> {
  const { data } = await api.get(`/api/v1/orgs/${org}/checks`, { params })
  return data
}
export async function approveForkCheckRun(id: string, includeSecrets = false): Promise<CheckRunRecord> {
  const { data } = await api.post(`/api/v1/checks/runs/${id}/approve`, { include_secrets: includeSecrets })
  return data
}
export async function rerunCheckRun(id: string): Promise<CheckRunRecord> {
  const { data } = await api.post(`/api/v1/checks/runs/${id}/rerun`)
  return data
}

// --- Workflows ---
export async function listOrgWorkflows(org: string, project?: string): Promise<WorkflowDef[]> {
  const { data } = await api.get(`/api/v1/orgs/${org}/workflows`, { params: { project } })
  return data
}
export async function dispatchWorkflow(org: string, payload: { project_id: string; workflow: string; ref?: string; inputs?: Record<string, unknown> }): Promise<WorkflowRun> {
  const { data } = await api.post(`/api/v1/orgs/${org}/workflows/dispatch`, payload)
  return data
}
export async function listOrgWorkflowRuns(org: string, params: { project?: string; status?: string; limit?: number } = {}): Promise<WorkflowRun[]> {
  const { data } = await api.get(`/api/v1/orgs/${org}/workflows/runs`, { params })
  return data
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
  return data
}
export async function downloadArtifactBlob(key: string): Promise<Blob> {
  const { data } = await api.get('/api/v1/artifacts/download', { params: { key }, responseType: 'blob' })
  return data
}

// --- Remote cache ---
export async function fetchRemoteCacheStats(namespace?: string): Promise<RemoteCacheStats> {
  const { data } = await api.get('/api/v1/remote-cache', { params: { namespace } })
  return data
}
export async function purgeRemoteCache(namespace: string): Promise<void> {
  await api.post('/api/v1/remote-cache/purge', null, { params: { namespace } })
}
