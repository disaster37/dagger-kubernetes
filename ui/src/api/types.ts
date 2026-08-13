export type Role = 'admin' | 'user'

export interface GroupSummary { id: string; name: string }

export interface AuthUser {
  id: string
  username: string
  role: Role
  groups: GroupSummary[]
  oauth_provider?: string
}

export interface Group extends GroupSummary {
  description: string
  max_runner_sessions: number
  agent_available: boolean
  auto_assign_pattern: string
  member_count?: number
  active_sessions?: number
  created_at: string
}

export interface TokenMeta {
  id: string
  prefix: string
  created_at: string
  last_used_at: string | null
}

export interface UserRow {
  id: string
  username: string
  role: Role
  oauth_provider?: string
  groups: GroupSummary[]
  created_at: string
  token?: TokenMeta | null
}

export interface Project {
  id: string
  name: string
  group_id: string
  group_name?: string
  created_at: string
}

export interface TraceRow {
  trace_id: string
  status: string
  version: string
  duration_ms: number
  ci_provider: string
  ci_repo: string
  project_name: string
  group_id: string
  group_name: string
  username: string
  started_at: string
}

export interface Providers {
  internal: boolean
  oauth_github: boolean
}

export interface LoginResponse {
  access_token: string
  refresh_token: string
  user: AuthUser
}

export interface RefreshResponse {
  access_token: string
  refresh_token: string
}

export interface FleetReplica {
  name: string
  ordinal: number
  version: string
  podIP: string
  ready: boolean
  startedAt: string
  pinnedSessions: number
}

export interface FleetInfo {
  version: string
  stsName: string
  replicas: number
  readyReplicas: number
  ordinals: FleetReplica[]
}
