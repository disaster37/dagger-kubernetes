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

export interface SpanNode {
  span_id: string
  parent_span_id: string
  trace_id: string
  name: string
  status: string
  start_time: string
  duration_ns: number
  duration_ms: number
  attributes: Record<string, string>
  children: SpanNode[]
}

export interface TraceDetail {
  trace_id: string
  root_span: SpanNode | null
  status: string
  start_time: string
  duration_ns: number
  duration_ms: number
  version: string
  ci_provider?: string
  ci_repo?: string
  user_id?: string
  username?: string
}

export interface TraceLogEntry {
  timestamp: string
  line: string
  span_id?: string
}

// Frontend-only view model derived from span + logs; not part of any API contract.
export interface ServiceInfo {
  span: SpanNode
  /** true while the up span has no end time (status === 'running') */
  running: boolean
  /** exposed host:port URL from the "tunnel started" log attributes, if present */
  url: string | null
  /** exposed port number, if present */
  port: number | null
  /** protocol string e.g. "tcp", if present */
  protocol: string | null
  /** tunnel description, if present */
  description: string | null
  /** all logs for the service subtree, sorted ascending by timestamp */
  logs: TraceLogEntry[]
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

export interface CacheVersionRef {
  version: string
  tag: string
  ref: string
  size: number
  layer_count: number
  digest: string
  protected: boolean
  last_used_at?: string
}

export interface GCRunSummary {
  started_at: string
  finished_at: string
  purged_tags: number
  freed_bytes: number
  skipped: number
  errors: number
  message?: string
}

export interface GCRules {
  enabled: boolean
  max_age: string
  schedule: string
  min_refs_to_keep: number
  protect_active_versions: boolean
  last_run_at?: string
  last_run_summary?: GCRunSummary
  next_run_at?: string
}

export interface CacheInfo {
  backend: string
  registry: string
  running: boolean
  reachable: boolean
  total_size: number
  object_count: number
  versions: CacheVersionRef[]
  hit_rate: number | null
  hit_count: number
  miss_count: number
  collected_at: string
  message?: string
  gc: GCRules
}

export interface PurgeRequest {
  version: string
  tag?: string
}

export interface PurgeResult {
  purged: number
  freed_bytes: number
  versions: string[]
  already_purged: number
  message?: string
}

export type ServiceState = 'ok' | 'degraded' | 'down' | 'unknown'

export interface ServiceStatus {
  name: string
  category: string
  state: ServiceState
  message?: string
  configured: boolean
  checked_at: string
}

export interface PlatformStatus {
  state: ServiceState
  services: ServiceStatus[]
  checked_at: string
}

export interface ConnectEnvVar {
  name: string
  value: string
  required: boolean
  secret: boolean
  description: string
}

export interface ConnectTokenMeta {
  exists: boolean
  prefix: string
  recoverable: boolean
}

export interface ConnectEnvSnapshot {
  server_url: string
  data_hostname: string
  cache_backend: string
  version_floor: string
  allowed_versions: string[]
  selected_version?: string
  token: ConnectTokenMeta
  env_vars: ConnectEnvVar[]
}
