<template>
  <div>
    <div class="mb-6 flex items-center justify-between gap-3">
      <h1 class="text-2xl font-semibold">Runner Fleet</h1>
      <div v-if="auth.isAdmin" class="flex items-center gap-2">
        <span class="text-sm text-muted">All organizations</span>
        <USwitch v-model="allOrgs" @change="load" />
      </div>
    </div>

    <div v-if="loading" class="py-12 text-center text-muted">
      <UIcon name="i-lucide-loader-circle" class="mx-auto size-7 animate-spin text-primary" />
      <p class="mt-3">Loading fleet...</p>
    </div>

    <UAlert v-else-if="error" color="error" variant="soft" :description="error">
      <template #actions>
        <UButton color="error" variant="soft" @click="load">Retry</UButton>
      </template>
    </UAlert>

    <UEmpty
      v-else-if="fleet.length === 0"
      icon="i-lucide-server-off"
      title="No engine fleets deployed"
      description="Run a Dagger pipeline to auto-provision engines."
    />

    <div v-else>
      <UCard v-for="version in fleet" :key="version.stsName || version.version" class="mb-4">
        <div class="flex items-center justify-between gap-3">
          <h3 class="font-semibold">
            {{ version.version }}
            <UBadge v-if="version.org" color="neutral" variant="soft" size="sm" class="ml-1">{{ version.org_slug || version.org }}</UBadge>
          </h3>
          <UButton
            v-if="auth.isAdmin"
            color="neutral"
            variant="outline"
            :loading="purging[key(version)]"
            @click="askPurge(version)"
          >
            {{ purging[key(version)] ? 'Purging…' : 'Purge local cache' }}
          </UButton>
        </div>
        <p class="text-[13px] text-muted">
          {{ version.readyReplicas }}/{{ version.replicas }} ready
        </p>

        <UAlert
          v-if="purgeResults[key(version)]"
          class="mt-2"
          color="neutral"
          variant="soft"
          :description="purgeSummary(purgeResults[key(version)]!)"
        >
          <template v-if="failedPods(purgeResults[key(version)]!).length" #description>
            <p>{{ purgeSummary(purgeResults[key(version)]!) }}</p>
            <ul class="mt-1 ml-4 list-disc">
              <li v-for="pod in failedPods(purgeResults[key(version)]!)" :key="pod.pod_name">
                <code>{{ pod.pod_name }}</code>: {{ pod.error }}
              </li>
            </ul>
          </template>
        </UAlert>

        <UAlert
          v-if="purgeErrors[key(version)]"
          class="mt-2"
          color="error"
          variant="soft"
          :description="purgeErrors[key(version)]"
        />

        <UTable class="mt-3" :data="version.ordinals" :columns="podColumns">
          <template #name-cell="{ row }">
            <code>{{ row.original.name }}</code>
          </template>
          <template #ready-cell="{ row }">
            <UBadge :color="row.original.ready ? 'success' : 'error'" variant="soft">
              {{ row.original.ready ? 'Ready' : 'Down' }}
            </UBadge>
          </template>
          <template #startedAt-cell="{ row }">
            {{ formatTime(row.original.startedAt) }}
          </template>
        </UTable>

        <div v-if="metricsByVersion[key(version)] === null" class="mt-4 text-[13px] text-muted">
          Metrics unavailable
        </div>
        <div v-else-if="metricsByVersion[key(version)]" class="mt-4">
          <h4 class="mb-2 font-semibold">Runner metrics</h4>
          <div v-if="metricsByVersion[key(version)]!.series.length === 0" class="text-[13px] text-muted">
            No metrics
          </div>
          <div v-else class="grid grid-cols-[repeat(auto-fit,minmax(220px,1fr))] gap-2.5">
            <MetricChart
              v-for="s in metricsByVersion[key(version)]!.series"
              :key="s.name"
              :series="s"
              :unit="s.unit"
            />
          </div>
          <div v-if="storageOf(version)" class="mt-3 rounded border border-default p-3">
            <span class="text-[13px] text-muted">Storage</span>
            <div class="mt-1 flex items-baseline gap-2">
              <span class="text-lg font-semibold">{{ formatBytes(storageOf(version)!.used_bytes) }}</span>
              <span class="text-[13px] text-muted">
                of {{ formatBytes(storageOf(version)!.capacity_bytes) }}
                ({{ storagePercentOf(version) }}%)
              </span>
            </div>
          </div>
        </div>
      </UCard>
    </div>

    <UModal v-model:open="confirmOpen" title="Purge local cache">
      <template #body>
        <p>{{ confirmMessage }}</p>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2">
          <UButton color="neutral" variant="outline" @click="confirmOpen = false">Cancel</UButton>
          <UButton color="error" @click="runConfirmed">Confirm</UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>

<script setup lang="ts">
import { fetchFleetInfo, fetchFleetMetrics, purgeEngineCache } from '~/api/client'
import { useAuthStore } from '~/stores/auth'
import { formatBytes } from '~/utils/format'
import type {
  EngineCachePurgeResult,
  EnginePodPurgeResult,
  FleetInfo,
  FleetMetrics,
  FleetStorage,
} from '~/api/types'

const REFRESH_MS = 10_000

const auth = useAuthStore()
const fleet = ref<FleetInfo[]>([])
const allOrgs = ref(false)
const loading = ref(true)
const error = ref<string | null>(null)
const purging = ref<Record<string, boolean>>({})
const purgeResults = ref<Record<string, EngineCachePurgeResult>>({})
const purgeErrors = ref<Record<string, string>>({})
const metricsByVersion = ref<Record<string, FleetMetrics | null>>({})
let timer: number | undefined
let loadingInFlight = false

const confirmOpen = ref(false)
const confirmMessage = ref('')
let confirmAction: (() => void) | null = null

const podColumns = [
  { id: 'name', accessorKey: 'name', header: 'Pod' },
  { id: 'ordinal', accessorKey: 'ordinal', header: 'Ordinal' },
  { id: 'ready', accessorKey: 'ready', header: 'Status' },
  { id: 'pinnedSessions', accessorKey: 'pinnedSessions', header: 'Sessions' },
  { id: 'startedAt', accessorKey: 'startedAt', header: 'Uptime' },
]

// key uniquely identifies a (org, version) fleet entry: the org slug when
// present (cross-org "all" view), else the version (single-org view).
function key(v: FleetInfo): string {
  return v.org_slug ? `${v.org_slug}:${v.version}` : v.version
}

async function load(): Promise<void> {
  if (loadingInFlight) return
  loadingInFlight = true
  try {
    fleet.value = await fetchFleetInfo(allOrgs.value ? 'all' : undefined)
    error.value = null
    await Promise.all(fleet.value.map((v) => loadVersionMetrics(v)))
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'Failed to load fleet'
  } finally {
    loading.value = false
    loadingInFlight = false
  }
}

// Per-version metrics are best-effort: an error/disabled endpoint records
// null so the card renders "Metrics unavailable" instead of failing the page.
async function loadVersionMetrics(v: FleetInfo): Promise<void> {
  try {
    metricsByVersion.value = { ...metricsByVersion.value, [key(v)]: await fetchFleetMetrics(v.version, v.org_slug) }
  } catch {
    metricsByVersion.value = { ...metricsByVersion.value, [key(v)]: null }
  }
}

function storageOf(v: FleetInfo): FleetStorage | null {
  const s = metricsByVersion.value[key(v)]?.storage
  return s ? s : null
}

function storagePercentOf(v: FleetInfo): string {
  const p = storageOf(v)?.percent
  return p !== undefined && p >= 0 ? p.toFixed(1) : '-'
}

function askPurge(v: FleetInfo): void {
  confirmMessage.value = `Purge the local BuildKit cache on every ${v.version} engine pod and delete the retained PVCs of scaled-down runners? Running pipelines are not interrupted.`
  confirmAction = () => purge(v)
  confirmOpen.value = true
}

function runConfirmed() {
  confirmOpen.value = false
  confirmAction?.()
  confirmAction = null
}

async function purge(v: FleetInfo): Promise<void> {
  const k = key(v)
  purging.value = { ...purging.value, [k]: true }
  purgeErrors.value = { ...purgeErrors.value, [k]: '' }
  try {
    const result = await purgeEngineCache(v.version, v.org_slug)
    purgeResults.value = { ...purgeResults.value, [k]: result }
    void load()
  } catch (e) {
    const err = e as { response?: { data?: { message?: string } } }
    purgeErrors.value = { ...purgeErrors.value, [k]: err.response?.data?.message || 'Purge failed' }
  } finally {
    purging.value = { ...purging.value, [k]: false }
  }
}

function purgeSummary(result: EngineCachePurgeResult): string {
  const pruned = result.pods.filter((p) => p.pruned).length
  const base = `Pruned ${pruned}/${result.pods.length} pods`
  const pvcs = result.pvcs ?? []
  if (!pvcs.length) return base
  const deleted = pvcs.filter((p) => p.deleted).length
  return `${base}; deleted ${deleted}/${pvcs.length} orphaned PVCs`
}

function failedPods(result: EngineCachePurgeResult): EnginePodPurgeResult[] {
  return result.pods.filter((p) => p.error)
}

onMounted(() => {
  void load()
  timer = window.setInterval(() => void load(), REFRESH_MS)
})

onUnmounted(() => {
  if (timer !== undefined) window.clearInterval(timer)
})

function formatTime(t: string): string {
  if (!t) return '-'
  const d = new Date(t)
  if (isNaN(d.getTime())) return '-'
  if (d.getFullYear() < 2000) return '-'
  const diff = (Date.now() - d.getTime()) / 1000
  if (diff < 0) return '-'
  if (diff < 60) return `${Math.floor(diff)}s`
  if (diff < 3600) return `${Math.floor(diff / 60)}m`
  if (diff < 86400) return `${Math.floor(diff / 3600)}h`
  return `${Math.floor(diff / 86400)}d`
}
</script>