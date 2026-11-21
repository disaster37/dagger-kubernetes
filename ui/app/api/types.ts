export type Role = 'admin' | 'user'

export interface GroupSummary { id: string; org_id?: string; name: string }

export interface OrgSummary {
  id: string
  name: string
  display_name: string
  role: 'admin' | 'org_admin' | 'member'
}

export interface QuotaThreshold { soft: number; hard: number }

export interface OrgQuotas {
  max_groups: QuotaThreshold
  max_projects: QuotaThreshold
  max_members: QuotaThreshold
  max_concurrent_sessions: QuotaThreshold
  max_engines: QuotaThreshold
  max_cache_bytes: QuotaThreshold
  max_pipeline_retention: number
}

export interface OrgQuotaUsage {
  groups: number
  projects: number
  members: number
  concurrent_sessions: number
  engines: number
  cache_bytes: number
  effective: OrgQuotas
}

export interface AuthUser {
  id: string
  username: string
  role: Role
  groups: GroupSummary[]
  orgs: OrgSummary[]
  oauth_provider?: string
  oauth_groups?: string[]
}

export interface Group extends GroupSummary {
  org_id: string
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
  oauth_groups?: string[]
  groups: GroupSummary[]
  created_at: string
  disabled_at?: string
  deleted_at?: string
  deactivated_at?: string
  token?: TokenMeta | null
}

export interface Project {
  id: string
  org_id?: string
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

export interface TraceFilterParams {
  groupId?: string // group id, or the 'unassigned' sentinel
  ciRepo?: string // substring
  user?: string // substring
  org?: string // org slug, or 'all' for admin cross-org view
}

export interface ExecEnvVar {
  name: string
  value?: string
  is_secret?: boolean
}

export interface ExecInfo {
  command: string
  args?: string[]
  cwd?: string
  user?: string
  env?: ExecEnvVar[]
  exit_code?: number | null
  kind?: string
}

export interface SpanEvent {
  name: string
  time_unix_nano?: number
  attributes?: Record<string, string>
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
  events?: SpanEvent[]
  exec?: ExecInfo | null
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

// --- Subtree-scoped log search (GET /api/v1/traces/:id/search) ---

export type LogSearchMode = 'contains' | 'regex'

export interface LogSearchRequest {
  span_id?: string
  q?: string
  mode?: LogSearchMode
  limit?: number
  cursor?: number
}

export interface LogSearchPage {
  entries: TraceLogEntry[]
  next?: number
  counts?: Record<string, number> // span_id -> matching log count (first page only)
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

// --- Trace-scoped engine resource metrics ---

export interface MetricPoint {
  t: number
  v: number
}

export interface MetricSeries {
  name: string
  label: string
  unit: string
  points: MetricPoint[]
}

export interface TraceMetrics {
  trace_id: string
  start_time: string
  end_time: string
  step_seconds: number
  series: MetricSeries[]
}

export interface FleetStorage {
  used_bytes: number
  capacity_bytes: number
  percent: number
}

export interface FleetMetrics {
  version: string
  start_time: string
  end_time: string
  step_seconds: number
  series: MetricSeries[]
  storage?: FleetStorage | null
}

export interface Providers {
  internal: boolean
  oauth_github: boolean
  oauth_oidc: boolean
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
  org?: string
  org_slug?: string
  replicas: number
  readyReplicas: number
  ordinals: FleetReplica[]
}

export interface EnginePodPurgeResult {
  pod_name: string
  ordinal: number
  pruned: boolean
  error?: string
}

export interface EnginePVCDeleteResult {
  pvc_name: string
  ordinal: number
  deleted: boolean
  error?: string
}

export interface EngineCachePurgeResult {
  version: string
  state: 'running' | 'completed' | 'failed'
  started_at: string
  finished_at?: string
  replicas: number
  pods: EnginePodPurgeResult[]
  pvcs?: EnginePVCDeleteResult[]
  message?: string
}

export interface HistoryGCRunSummary {
  started_at: string
  finished_at: string
  purged_traces: number
  skipped_running: number
  logs_deleted: number
  metrics_deleted: number
  telemetry_errors: number
  errors: number
  message?: string
}

export interface HistoryGCRules {
  enabled: boolean
  max_age: string
  schedule: string
  last_run_at?: string
  last_run_summary?: HistoryGCRunSummary
  next_run_at?: string
}

export interface HistoryInfo {
  trace_count: number
  oldest_updated_at?: string
  collected_at: string
  gc: HistoryGCRules
}

export interface HistoryPurgeRequest {
  trace_id?: string
}

export interface HistoryPurgeResult {
  purged_traces: number
  logs_deleted: number
  metrics_deleted: number
  trace_ids: string[]
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
  version_floor: string
  allowed_versions: string[]
  selected_version?: string
  token: ConnectTokenMeta
  env_vars: ConnectEnvVar[]
}

// --- Image cache (local Zot mirrors, admin) ---

export interface ImageCacheTag {
  tag: string
  digest: string
  size_bytes: number
  layer_count: number
}

export interface ImageCacheRepository {
  repository: string
  tags: ImageCacheTag[]
}

export interface ImageCacheMirrorInfo {
  id: string
  host: string
  upstream: string
  backend: string
  reachable: boolean
  repositories: ImageCacheRepository[]
  error?: string
}

export interface ImageCacheInfo {
  mirrors: ImageCacheMirrorInfo[]
  collected_at: string
}

export interface ImageCachePruneRef {
  repository: string
  tag?: string
  digest?: string
}

export interface ImageCachePruneItem {
  repository: string
  tag?: string
  digest?: string
  pruned: boolean
  error?: string
}

export interface ImageCachePruneResult {
  mirror_id: string
  items: ImageCachePruneItem[]
  pruned: number
  errors: number
  message?: string
}

export interface ImageCachePruneAllItem {
  repository: string
  tag?: string
  digest?: string
  pruned: boolean
  error?: string
}

export interface ImageCachePruneAllMirror {
  mirror_id: string
  manifests_pruned: number
  repositories_processed: number
  errors: number
  failed?: ImageCachePruneAllItem[]
  message?: string
  error?: string
}

export interface ImageCachePruneAllResult {
  mirrors: ImageCachePruneAllMirror[]
  message?: string
}

// --- Cloud parity (orgs, environments, checks, workflows, artifacts, cache) ---
export interface Org {
  id: string
  name: string
  display_name: string
  features: string[]
  quotas?: OrgQuotas
  created_at: string
}

export interface OrgMemberRow {
  org_id: string
  user_id: string
  username?: string
  role: 'org_admin' | 'member'
  groups?: GroupSummary[]
  created_at?: string
}

export interface OrgMember {
  org_id: string
  user_id: string
  role: 'org_admin' | 'member'
}

export interface OrgTokenMeta extends TokenMeta {
  exists?: boolean
}

export interface SecretMeta {
  id: string
  scope: 'org' | 'project' | 'environment'
  scope_id: string
  name: string
  created_at: string
  updated_at: string
}

export interface ProtectedEnvironment {
  id: string
  org_id: string
  name: string
  approver_group_ids: string[]
  prevent_self_approval: boolean
  approval_timeout: number
  created_at: string
  updated_at: string
}

export interface ForgeSource {
  id: string
  org_id: string
  project_id?: string
  provider: 'github' | 'gitlab' | 'gitea'
  base_url: string
  repo: string
  app_id?: string
  installation_id?: string
  created_at: string
  updated_at: string
}

export interface CheckItem {
  name: string
  state: 'pending' | 'success' | 'failure' | 'error'
  trace_id?: string
  summary?: string
  updated_at: string
}

export interface CheckRunRecord {
  id: string
  org_id: string
  project_id: string
  source_id: string
  provider: string
  repo: string
  event: string
  sha: string
  ref: string
  default_branch?: string
  target_branch?: string
  tag?: string
  pr_number?: number
  is_fork: boolean
  is_release: boolean
  status: 'waiting_approval' | 'running' | 'success' | 'failed' | 'canceled'
  include_secrets: boolean
  approved_by?: string
  approved_at?: string
  checks?: CheckItem[]
  error?: string
  created_at: string
  updated_at: string
}

export interface WorkflowInput {
  type: 'string' | 'boolean' | 'number' | 'choice' | 'artifact'
  description?: string
  required?: boolean
  default?: unknown
  options?: string[]
}

export interface WorkflowDef {
  id: string
  org_id: string
  project_id: string
  repo: string
  default_ref: string
  default_sha: string
  workflow: {
    name: string
    on: Record<string, unknown>
    after_checks: boolean
    jobs: Record<string, unknown>
    inputs?: Record<string, WorkflowInput>
  }
  updated_at: string
}

export interface WorkflowJobRun {
  name: string
  status: string
  trace_id?: string
  outputs?: Record<string, string>
  artifacts?: string[]
  environment?: string
  waiting_since?: string
  approval_deadline?: string
  approved_by?: string
  approved_at?: string
  rejected_by?: string
  triggered_run_id?: string
  error?: string
}

export interface WorkflowRun {
  id: string
  workflow: string
  org_id: string
  project_id: string
  sha: string
  ref: string
  status: string
  number: number
  jobs: Record<string, WorkflowJobRun>
  git: Record<string, unknown>
  inputs?: Record<string, unknown>
  triggered_by?: string
  created_at: string
  updated_at: string
}

export interface RemoteCacheStats {
  enabled: boolean
  namespace?: string
  entries?: number
  bytes?: number
  soft_limit_bytes?: number
  hard_limit_bytes?: number
}
