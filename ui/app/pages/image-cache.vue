<template>
  <div>
    <h1 class="mb-6 text-2xl font-semibold">Image cache</h1>
    <p class="mb-4 max-w-[760px] text-[13px] text-muted">
      The local Zot mirrors cache upstream images on demand. Pruning unlinks a manifest
      immediately; unreferenced blob bytes are reclaimed automatically by Zot's online GC
      after <code>gcDelay</code> on the next <code>gcInterval</code> cycle.
    </p>

    <div v-if="loading" class="py-12 text-center text-muted">
      <UIcon name="i-lucide-loader-circle" class="mx-auto size-7 animate-spin text-primary" />
      <p class="mt-3">Loading mirrors...</p>
    </div>

    <UAlert v-else-if="error" color="error" variant="soft" :description="error">
      <template #actions>
        <UButton color="error" variant="soft" @click="load">Retry</UButton>
      </template>
    </UAlert>

    <UEmpty
      v-else-if="!info || info.mirrors.length === 0"
      icon="i-lucide-package-x"
      title="No image-cache mirrors configured"
      description="Enable imageCache.enabled in the Helm chart."
    />

    <div v-else>
      <UCard v-for="mirror in info.mirrors" :key="mirror.id" class="mb-4">
        <div class="flex flex-wrap items-start justify-between gap-3">
          <div>
            <h3 class="font-semibold">{{ mirror.id }}</h3>
            <p class="mt-1 text-[13px] text-muted">
              <code>{{ mirror.host }}</code> &rarr; <code>{{ mirror.upstream }}</code>
              <UBadge :color="mirror.reachable ? 'success' : 'error'" variant="soft">
                {{ mirror.reachable ? 'reachable' : 'unreachable' }}
              </UBadge>
              <UBadge color="neutral" variant="soft">backend: {{ mirror.backend }}</UBadge>
            </p>
          </div>
          <div class="flex gap-2">
            <UButton color="neutral" variant="outline" :loading="busy[mirror.id]" @click="askPruneSelected(mirror)">
              {{ busy[mirror.id] ? 'Working…' : 'Prune selected' }}
            </UButton>
            <UButton color="error" variant="soft" :loading="busy[mirror.id]" @click="askPruneAll(mirror)">
              Prune all
            </UButton>
          </div>
        </div>

        <UAlert
          v-if="mirror.error"
          class="mt-2"
          color="warning"
          variant="soft"
          :description="mirror.error"
        />

        <UTable
          v-if="mirror.repositories.length"
          class="mt-3"
          :data="mirrorRows(mirror)"
          :columns="tagColumns"
        >
          <template #select-cell="{ row }">
            <UCheckbox
              :model-value="isSelected(mirror.id, row.original.repository, row.original.tag)"
              @update:model-value="toggle(mirror.id, row.original.repository, row.original.tag)"
            />
          </template>
          <template #repository-cell="{ row }">
            <code>{{ row.original.repository }}</code>
          </template>
          <template #digest-cell="{ row }">
            <code>{{ row.original.digest }}</code>
          </template>
          <template #size_bytes-cell="{ row }">
            {{ formatBytes(row.original.size_bytes) }}
          </template>
          <template #layer_count-cell="{ row }">
            {{ formatLayers(row.original.layer_count) }}
          </template>
        </UTable>
        <p v-else-if="mirror.reachable" class="mt-2 text-[13px] text-muted">No cached images.</p>

        <UAlert
          v-if="pruneResults[mirror.id]"
          class="mt-2"
          color="neutral"
          variant="soft"
          :description="pruneSummary(pruneResults[mirror.id]!)"
        >
          <template v-if="failedPruneItems(pruneResults[mirror.id]!).length" #description>
            <p>{{ pruneSummary(pruneResults[mirror.id]!) }}</p>
            <ul class="mt-1 ml-4 list-disc">
              <li v-for="(item, i) in failedPruneItems(pruneResults[mirror.id]!)" :key="i">
                <code>{{ item.repository }}{{ item.tag ? ':' + item.tag : '' }}</code>: {{ item.error }}
              </li>
            </ul>
          </template>
        </UAlert>

        <UAlert
          v-if="pruneAllResults[mirror.id]"
          class="mt-2"
          color="neutral"
          variant="soft"
          :description="pruneAllSummary(pruneAllResults[mirror.id]!)"
        >
          <template #description>
            <p>{{ pruneAllSummary(pruneAllResults[mirror.id]!) }}</p>
            <p v-if="pruneAllResults[mirror.id]!.error" class="text-error">{{ pruneAllResults[mirror.id]!.error }}</p>
            <ul v-if="(pruneAllResults[mirror.id]!.failed || []).length" class="mt-1 ml-4 list-disc">
              <li v-for="(item, i) in pruneAllResults[mirror.id]!.failed" :key="i">
                <code>{{ item.repository }}{{ item.tag ? ':' + item.tag : '' }}</code>: {{ item.error }}
              </li>
            </ul>
          </template>
        </UAlert>

        <UAlert
          v-if="pruneErrors[mirror.id]"
          class="mt-2"
          color="error"
          variant="soft"
          :description="pruneErrors[mirror.id]"
        />
      </UCard>
    </div>

    <UModal v-model:open="confirmOpen" :title="confirmTitle">
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
import { fetchImageCacheInfo, pruneAllImageCache, pruneImageCache } from '~/api/client'
import type {
  ImageCacheInfo,
  ImageCacheMirrorInfo,
  ImageCachePruneAllResult,
  ImageCachePruneRef,
  ImageCachePruneResult,
} from '~/api/types'

const info = ref<ImageCacheInfo | null>(null)
const loading = ref(true)
const error = ref<string | null>(null)
const selected = ref<Record<string, boolean>>({})
const busy = ref<Record<string, boolean>>({})
const pruneResults = ref<Record<string, ImageCachePruneResult>>({})
const pruneAllResults = ref<Record<string, ImageCachePruneAllResult['mirrors'][number]>>({})
const pruneErrors = ref<Record<string, string>>({})

const confirmOpen = ref(false)
const confirmTitle = ref('')
const confirmMessage = ref('')
let confirmAction: (() => void) | null = null

const tagColumns = [
  { id: 'select', header: '' },
  { id: 'repository', accessorKey: 'repository', header: 'Repository' },
  { id: 'tag', accessorKey: 'tag', header: 'Tag' },
  { id: 'digest', accessorKey: 'digest', header: 'Digest' },
  { id: 'size_bytes', accessorKey: 'size_bytes', header: 'Size' },
  { id: 'layer_count', accessorKey: 'layer_count', header: 'Layers' },
]

interface TagRow {
  repository: string
  tag: string
  digest: string
  size_bytes: number
  layer_count: number
}

function mirrorRows(mirror: ImageCacheMirrorInfo): TagRow[] {
  const rows: TagRow[] = []
  for (const repo of mirror.repositories) {
    for (const tag of repo.tags) {
      rows.push({
        repository: repo.repository,
        tag: tag.tag,
        digest: tag.digest,
        size_bytes: tag.size_bytes,
        layer_count: tag.layer_count,
      })
    }
  }
  return rows
}

onMounted(load)

async function load(): Promise<void> {
  try {
    info.value = await fetchImageCacheInfo()
    error.value = null
  } catch (e) {
    error.value = errorMessage(e, 'Failed to load image cache')
  } finally {
    loading.value = false
  }
}

function selKey(mirrorId: string, repo: string, tag: string): string {
  return `${mirrorId}\u0000${repo}\u0000${tag}`
}

function isSelected(mirrorId: string, repo: string, tag: string): boolean {
  return selected.value[selKey(mirrorId, repo, tag)] === true
}

function toggle(mirrorId: string, repo: string, tag: string): void {
  selected.value = { ...selected.value, [selKey(mirrorId, repo, tag)]: !isSelected(mirrorId, repo, tag) }
}

function selectedRefs(mirror: ImageCacheMirrorInfo): ImageCachePruneRef[] {
  const refs: ImageCachePruneRef[] = []
  for (const repo of mirror.repositories) {
    for (const tag of repo.tags) {
      if (isSelected(mirror.id, repo.repository, tag.tag)) {
        refs.push({ repository: repo.repository, tag: tag.tag })
      }
    }
  }
  return refs
}

function askPruneSelected(mirror: ImageCacheMirrorInfo): void {
  const refs = selectedRefs(mirror)
  if (refs.length === 0) {
    confirmTitle.value = 'Prune selected'
    confirmMessage.value = 'Select at least one image to prune.'
    confirmAction = null
    confirmOpen.value = true
    return
  }
  confirmTitle.value = 'Prune selected'
  confirmMessage.value = `Prune ${refs.length} image(s) from ${mirror.id}? The manifest is unlinked now; blob bytes are reclaimed by Zot GC after gcDelay.`
  confirmAction = () => pruneSelected(mirror, refs)
  confirmOpen.value = true
}

function askPruneAll(mirror: ImageCacheMirrorInfo): void {
  confirmTitle.value = 'Prune all'
  confirmMessage.value = `Prune every cached image from ${mirror.id}? The manifests are unlinked now; blob bytes are reclaimed by Zot GC after gcDelay.`
  confirmAction = () => pruneAll(mirror)
  confirmOpen.value = true
}

function runConfirmed() {
  confirmOpen.value = false
  confirmAction?.()
  confirmAction = null
}

async function pruneSelected(mirror: ImageCacheMirrorInfo, refs: ImageCachePruneRef[]): Promise<void> {
  await run(mirror.id, async () => {
    const result = await pruneImageCache(mirror.id, refs)
    pruneResults.value = { ...pruneResults.value, [mirror.id]: result }
    if (result.message) pruneErrors.value = { ...pruneErrors.value, [mirror.id]: result.message }
    // Drop the pruned refs from the selection.
    const next = { ...selected.value }
    for (const ref of refs) delete next[selKey(mirror.id, ref.repository, ref.tag ?? '')]
    selected.value = next
  })
}

async function pruneAll(mirror: ImageCacheMirrorInfo): Promise<void> {
  await run(mirror.id, async () => {
    const result = await pruneAllImageCache(mirror.id)
    const out = result.mirrors.find((m) => m.mirror_id === mirror.id) ?? result.mirrors[0]
    if (out) pruneAllResults.value = { ...pruneAllResults.value, [mirror.id]: out }
    if (result.message) pruneErrors.value = { ...pruneErrors.value, [mirror.id]: result.message }
  })
}

async function run(mirrorId: string, action: () => Promise<unknown>): Promise<void> {
  busy.value = { ...busy.value, [mirrorId]: true }
  pruneErrors.value = { ...pruneErrors.value, [mirrorId]: '' }
  try {
    await action()
    await load()
  } catch (e) {
    pruneErrors.value = { ...pruneErrors.value, [mirrorId]: errorMessage(e, 'Prune failed') }
  } finally {
    busy.value = { ...busy.value, [mirrorId]: false }
  }
}

function pruneSummary(result: ImageCachePruneResult): string {
  if (result.errors > 0) return `Pruned ${result.pruned}/${result.items.length} refs; ${result.errors} failed`
  return `Pruned ${result.pruned} ref(s).`
}

function failedPruneItems(result: ImageCachePruneResult) {
  return result.items.filter((i) => i.error)
}

function pruneAllSummary(result: ImageCachePruneAllResult['mirrors'][number]): string {
  return `Pruned ${result.manifests_pruned} manifest(s) across ${result.repositories_processed} repo(s), ${result.errors} error(s).`
}

function formatLayers(n: number): string {
  return n < 0 ? 'unknown' : String(n)
}

function formatBytes(n: number): string {
  if (n < 0) return 'unknown'
  if (n === 0) return '0 B'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let value = n
  let i = 0
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024
    i++
  }
  return `${value.toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}

function errorMessage(e: unknown, fallback: string): string {
  const err = e as { response?: { data?: { message?: string } }; message?: string }
  return err.response?.data?.message || err.message || fallback
}
</script>
