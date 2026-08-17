<template>
  <div>
    <h1 class="page-title">MagicCache</h1>

    <div v-if="loading" class="loading-state">
      <div class="spinner"></div>
      <p>Loading cache stats...</p>
    </div>

    <div v-else-if="error" class="error-banner">
      <p>{{ error }}</p>
      <button class="btn" @click="load">Retry</button>
    </div>

    <div v-else-if="!info.running" class="error-banner">
      <p>Cache not running. {{ info.message || '' }}</p>
      <button class="btn" @click="load">Retry</button>
    </div>

    <div v-else>
      <div class="card">
        <h3>Cache Configuration</h3>
        <table>
          <tbody>
            <tr><td>Backend</td><td>{{ info.backend }}</td></tr>
            <tr><td>Registry</td><td><code>{{ info.registry }}</code></td></tr>
            <tr><td>Status</td><td><span class="badge badge-success">running</span></td></tr>
            <tr><td>Total size</td><td>{{ formatBytes(info.total_size) }}</td></tr>
            <tr><td>Objects (layers)</td><td>{{ info.object_count < 0 ? 'unknown' : info.object_count }}</td></tr>
            <tr><td>Hit rate</td><td>{{ hitRateLabel }}</td></tr>
            <tr><td>Collected</td><td>{{ formatTime(info.collected_at) }}</td></tr>
          </tbody>
        </table>
        <p v-if="info.message" style="color: #8b949e; font-size: 13px; margin-top: 12px;">{{ info.message }}</p>
      </div>

      <div class="card">
        <h3>Cache Versions</h3>
        <p v-if="info.versions.length === 0" style="color: #8b949e; margin-top: 8px;">No version refs discovered.</p>
        <table v-else style="margin-top: 12px;">
          <thead>
            <tr>
              <th>Version</th>
              <th>Tag</th>
              <th>Size</th>
              <th>Layers</th>
              <th>Protected</th>
              <th v-if="auth.isAdmin">Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="v in info.versions" :key="v.ref">
              <td>{{ v.version }}</td>
              <td><code>{{ v.tag }}</code></td>
              <td>{{ formatBytes(v.size) }}</td>
              <td>{{ v.layer_count < 0 ? 'unknown' : v.layer_count }}</td>
              <td>
                <span :class="['badge', v.protected ? 'badge-success' : 'badge-running']">
                  {{ v.protected ? 'protected' : 'unprotected' }}
                </span>
              </td>
              <td v-if="auth.isAdmin">
                <button class="btn btn-danger" @click="purgeVersion(v.version, v.tag)">Purge</button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <div class="card">
        <h3>Auto-clean (GC)</h3>
        <p style="margin-top: 8px; font-size: 13px; color: #8b949e;">
          Auto-clean is <strong :class="info.gc.enabled ? 'status-ok' : 'status-down'">{{ info.gc.enabled ? 'ON' : 'OFF' }}</strong>.
          {{ gcSummary }}
        </p>
        <table style="margin-top: 12px;">
          <tbody>
            <tr><td>Max age</td><td>{{ info.gc.max_age }}</td></tr>
            <tr><td>Schedule</td><td>{{ info.gc.schedule }}</td></tr>
            <tr><td>Keep (most recent per minor)</td><td>{{ info.gc.min_refs_to_keep }}</td></tr>
            <tr><td>Protect active versions</td><td>{{ info.gc.protect_active_versions ? 'yes' : 'no' }}</td></tr>
            <tr v-if="info.gc.last_run_at"><td>Last run</td><td>{{ formatTime(info.gc.last_run_at) }}</td></tr>
            <tr v-if="info.gc.next_run_at"><td>Next run (est.)</td><td>{{ formatTime(info.gc.next_run_at) }}</td></tr>
          </tbody>
        </table>
        <div v-if="info.gc.last_run_summary" style="margin-top: 12px;">
          <p style="font-size: 13px; color: #8b949e;">
            Last run purged {{ info.gc.last_run_summary.purged_tags }} tag(s),
            freed {{ formatBytes(info.gc.last_run_summary.freed_bytes) }},
            skipped {{ info.gc.last_run_summary.skipped }}, errors {{ info.gc.last_run_summary.errors }}.
          </p>
        </div>
      </div>

      <div v-if="auth.isAdmin" class="card">
        <h3>Admin</h3>
        <p style="font-size: 13px; color: #8b949e; margin: 8px 0;">
          Purge removes cache blobs from the OCI registry. The registry must have delete enabled.
        </p>
        <button class="btn btn-danger" @click="purgeAll">Purge all cache tags</button>
        <p v-if="purgeMessage" style="margin-top: 12px; font-size: 13px;">{{ purgeMessage }}</p>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { fetchCacheInfo, purgeCache, purgeAllCache } from '@/api/client'
import { useAuthStore } from '@/stores/auth'
import type { CacheInfo } from '@/api/types'

const auth = useAuthStore()

const info = ref<CacheInfo>(emptyCache())
const loading = ref(true)
const error = ref<string | null>(null)
const purgeMessage = ref('')

function emptyCache(): CacheInfo {
  return {
    backend: '-',
    registry: '-',
    running: false,
    reachable: false,
    total_size: -1,
    object_count: -1,
    versions: [],
    hit_rate: null,
    hit_count: 0,
    miss_count: 0,
    collected_at: '',
    gc: { enabled: false, max_age: '', schedule: '', min_refs_to_keep: 0, protect_active_versions: false },
  }
}

const hitRateLabel = computed(() => {
  if (info.value.hit_rate == null) return 'no data'
  return `${(info.value.hit_rate * 100).toFixed(1)}% (${info.value.hit_count} hits / ${info.value.miss_count} misses)`
})

const gcSummary = computed(() => {
  const gc = info.value.gc
  if (!gc.enabled) return 'Configured via cache.gc.* (disabled by default).'
  return `Tags older than ${gc.max_age} are purged every ${gc.schedule}.`
})

async function load(): Promise<void> {
  loading.value = true
  error.value = null
  try {
    info.value = await fetchCacheInfo()
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'Failed to load cache info'
  } finally {
    loading.value = false
  }
}

async function purgeVersion(version: string, tag: string): Promise<void> {
  if (!confirm(`Purge cache for ${version} (tag ${tag})?`)) return
  purgeMessage.value = ''
  try {
    const res = await purgeCache({ version, tag })
    purgeMessage.value = res.purged > 0
      ? `Purged ${res.purged} tag(s), freed ${formatBytes(res.freed_bytes)}.`
      : res.message || 'Nothing to purge.'
    await load()
  } catch (e: any) {
    purgeMessage.value = e.response?.data?.message || 'Purge failed'
  }
}

async function purgeAll(): Promise<void> {
  if (!confirm('Purge ALL cache tags? This removes every cache blob in the registry.')) return
  purgeMessage.value = ''
  try {
    const res = await purgeAllCache()
    purgeMessage.value = `Purged ${res.purged} tag(s), ${res.already_purged} already absent, freed ${formatBytes(res.freed_bytes)}.`
    await load()
  } catch (e: any) {
    purgeMessage.value = e.response?.data?.message || 'Purge failed'
  }
}

function formatBytes(n: number): string {
  if (n == null || n < 0) return 'unknown'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(1)} ${units[i]}`
}

function formatTime(t: string): string {
  if (!t) return '-'
  const d = new Date(t)
  if (isNaN(d.getTime())) return '-'
  return d.toLocaleString()
}

onMounted(() => {
  void load()
})
</script>
