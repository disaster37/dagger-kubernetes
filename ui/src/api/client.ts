import axios, { type AxiosError, type AxiosRequestConfig } from 'axios'
import { useAuthStore } from '@/stores/auth'
import type {
  AuthUser,
  CacheInfo,
  ConnectEnvSnapshot,
  FleetInfo,
  Group,
  GroupSummary,
  HistoryInfo,
  HistoryPurgeRequest,
  HistoryPurgeResult,
  LoginResponse,
  PlatformStatus,
  Project,
  Providers,
  PurgeRequest,
  PurgeResult,
  RefreshResponse,
  TokenMeta,
  TraceDetail,
  TraceLogEntry,
  TraceRow,
  UserRow,
} from '@/api/types'

const api = axios.create({
  baseURL: '/',
  timeout: 30000,
})

api.interceptors.request.use((config) => {
  const auth = useAuthStore()
  if (auth.token) {
    config.headers.Authorization = `Bearer ${auth.token}`
  }
  return config
})

api.interceptors.response.use(
  (response) => response,
  async (error: AxiosError) => {
    if (error.response?.status === 401) {
      const auth = useAuthStore()
      const url = error.config?.url ?? ''
      const isAuthEndpoint = url.includes('/api/v1/auth/login') || url.includes('/api/v1/auth/refresh')
      if (!isAuthEndpoint && auth.refreshToken) {
        const ok = await auth.refreshSession()
        if (ok && error.config) {
          // Retry the original request once with the new token.
          const retryConfig: AxiosRequestConfig = { ...error.config }
          retryConfig.headers = { ...error.config.headers, Authorization: `Bearer ${auth.token}` }
          return api.request(retryConfig)
        }
      }
      auth.logout()
      if (typeof window !== 'undefined') {
        window.location.href = '/auth/login'
      }
    }
    return Promise.reject(error)
  }
)

export default api

// --- Auth ---
export async function fetchProviders(): Promise<Providers> {
  const { data } = await api.get('/api/v1/auth/providers')
  return data
}
export async function fetchMe(): Promise<AuthUser> {
  const { data } = await api.get('/api/v1/auth/me')
  return data
}
export async function loginRequest(username: string, password: string): Promise<LoginResponse> {
  const { data } = await api.post('/api/v1/auth/login', { username, password })
  return data
}
export async function refreshRequest(refreshToken: string): Promise<RefreshResponse> {
  const { data } = await api.post('/api/v1/auth/refresh', { refresh_token: refreshToken })
  return data
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
export async function fetchTraces(groupId?: string): Promise<TraceRow[]> {
  const params = groupId !== undefined ? { group_id: groupId } : {}
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
export async function fetchFleetInfo(): Promise<FleetInfo[]> {
  const { data } = await api.get('/api/v1/fleet')
  return (data as FleetInfo[] | null) ?? []
}
export async function fetchCacheInfo(): Promise<CacheInfo> {
  const { data } = await api.get('/api/v1/cache')
  return data as CacheInfo
}
export async function purgeCache(payload: PurgeRequest): Promise<PurgeResult> {
  const { data } = await api.post('/api/v1/cache/purge', payload)
  return data as PurgeResult
}
export async function purgeAllCache(): Promise<PurgeResult> {
  const { data } = await api.post('/api/v1/cache/purge-all')
  return data as PurgeResult
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

// --- Connect-env snapshot ---
export async function fetchConnectEnv(version?: string, reveal?: boolean): Promise<ConnectEnvSnapshot> {
  const params: Record<string, string> = {}
  if (version) params.version = version
  if (reveal) params.reveal = 'true'
  const { data } = await api.get('/api/v1/connect/env', { params })
  return data as ConnectEnvSnapshot
}

// SSE live trace stream (EventSource cannot set headers; use ?token= query param).
export function connectLiveTrace(id: string): EventSource {
  const auth = useAuthStore()
  const token = encodeURIComponent(auth.token ?? '')
  return new EventSource(`/api/v1/traces/${id}/live?token=${token}`)
}
