<template>
  <div>
    <h1 class="page-title">Pipelines</h1>

    <div v-if="auth.isAdmin" class="card" style="margin-bottom: 16px;">
      <label style="font-size: 14px; margin-right: 8px;">Filter by group:</label>
      <select v-model="groupFilter" @change="load" style="padding: 4px 10px; background: #0d1117; border: 1px solid #30363d; border-radius: 6px; color: #c9d1d9;">
        <option value="">All</option>
        <option value="unassigned">Unassigned</option>
        <option v-for="g in groups" :key="g.id" :value="g.id">{{ g.name }}</option>
      </select>
    </div>

    <div class="card">
      <table>
        <thead>
          <tr>
            <th>Pipeline</th>
            <th>Status</th>
            <th>Version</th>
            <th>Duration</th>
            <th>CI</th>
            <th>Group</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="trace in traces" :key="trace.trace_id">
            <td>
              <div class="pipeline-identity">{{ identity(trace) }}</div>
              <code class="pipeline-trace-id">{{ trace.trace_id }}</code>
            </td>
            <td>
              <span :class="['badge', `badge-${trace.status}`]">{{ trace.status }}</span>
            </td>
            <td>{{ trace.version || '-' }}</td>
            <td>{{ formatDuration(trace.duration_ms) }}</td>
            <td>{{ ciLabel(trace.ci_provider) }}</td>
            <td>{{ trace.group_name || '-' }}</td>
            <td>
              <router-link :to="`/pipelines/${trace.trace_id}`" class="btn">View DAG</router-link>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-if="traces.length === 0" style="padding: 24px; text-align: center; color: #8b949e;">
        No pipelines yet. Run <code>dagger call</code> with DAGGER_CLOUD_URL set to this server.
      </p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { fetchTraces, listGroups } from '@/api/client'
import { useAuthStore } from '@/stores/auth'
import type { TraceRow, Group } from '@/api/types'

const auth = useAuthStore()
const traces = ref<TraceRow[]>([])
const groups = ref<Group[]>([])
const groupFilter = ref('')

onMounted(async () => {
  if (auth.isAdmin) {
    try {
      groups.value = await listGroups()
    } catch { /* ignore */ }
  }
  await load()
})

async function load() {
  try {
    traces.value = await fetchTraces(groupFilter.value || undefined)
  } catch (e) {
    console.error('Failed to fetch traces', e)
  }
}

function formatDuration(ms: number): string {
  if (!ms) return '-'
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(1)}s`
  const m = Math.floor(s / 60)
  return `${m}m ${(s % 60).toFixed(0)}s`
}

// Derive a short, human-friendly name from the repo/project identity:
//   https://github.com/org/repo.git -> org/repo
//   github.com/org/repo            -> org/repo
//   /path/to/folder                -> folder
//   nogit-proj                     -> nogit-proj
function shortDisplayName(v: string): string {
  let s = (v || '').trim()
  if (!s) return ''
  s = s.replace(/^[a-z][a-z0-9+.-]*:\/\//i, '') // strip scheme (https://, git://, ...)
  const scp = s.match(/^[^@\s]+@[^:\s]+:(.+)$/)  // scp-style git@host:org/repo
  if (scp) s = scp[1]
  s = s.replace(/\.git$/, '')                    // strip .git suffix
  s = s.replace(/[@#].*$/, '')                   // strip @ref / #ref
  s = s.replace(/\/+$/, '')                      // strip trailing slashes
  const seg = s.split('/').filter(Boolean)
  if (seg.length === 0) return ''
  // host/org/repo -> org/repo
  if (seg.length >= 3 && seg[0].includes('.')) {
    return `${seg[seg.length - 2]}/${seg[seg.length - 1]}`
  }
  // filesystem path or bare name -> basename
  return seg[seg.length - 1]
}

function identity(trace: TraceRow): string {
  const name = shortDisplayName(trace.ci_repo || trace.project_name)
  const user = trace.username ? `@${trace.username}` : ''
  if (user && name) return `${user} · ${name}`
  if (user) return user
  if (name) return name
  return '-'
}

// The CLI reports "dagger.io/ci" as "true"/"false" (whether a CI is detected),
// not a provider name. Render a human-friendly label for the CI column.
function ciLabel(ci: string): string {
  if (!ci || ci === 'false') return '-'
  if (ci === 'true') return 'CI'
  return ci
}
</script>

<style scoped>
.pipeline-identity {
  font-weight: 600;
  color: #f0f6fc;
}

.pipeline-trace-id {
  display: block;
  margin-top: 2px;
  font-size: 11px;
  color: #8b949e;
}
</style>
