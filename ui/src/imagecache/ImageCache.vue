<template>
  <div>
    <h1 class="page-title">Image cache</h1>
    <p style="color: #8b949e; font-size: 13px; max-width: 760px;">
      The local Zot mirrors cache upstream images on demand. Pruning unlinks a manifest
      immediately; unreferenced blob bytes are reclaimed automatically by Zot's online GC
      after <code>gcDelay</code> on the next <code>gcInterval</code> cycle.
    </p>

    <div v-if="loading" class="loading-state">
      <div class="spinner"></div>
      <p>Loading mirrors...</p>
    </div>

    <div v-else-if="error" class="error-banner">
      <p>{{ error }}</p>
      <button class="btn" @click="load">Retry</button>
    </div>

    <div v-else-if="!info || info.mirrors.length === 0" class="empty-state">
      <p>No image-cache mirrors configured. Enable <code>imageCache.enabled</code> in the Helm chart.</p>
    </div>

    <div v-else>
      <div v-for="mirror in info.mirrors" :key="mirror.id" class="card" style="margin-bottom: 16px;">
        <div style="display: flex; align-items: flex-start; justify-content: space-between; gap: 12px; flex-wrap: wrap;">
          <div>
            <h3 style="margin: 0;">{{ mirror.id }}</h3>
            <p style="color: #8b949e; font-size: 13px; margin: 4px 0 0;">
              <code>{{ mirror.host }}</code> &rarr; <code>{{ mirror.upstream }}</code>
              <span class="badge" :class="mirror.reachable ? 'badge-success' : 'badge-failed'">
                {{ mirror.reachable ? 'reachable' : 'unreachable' }}
              </span>
              <span class="badge badge-neutral">backend: {{ mirror.backend }}</span>
            </p>
          </div>
          <div style="display: flex; gap: 8px;">
            <button class="btn" :disabled="busy[mirror.id]" @click="pruneSelected(mirror)">
              {{ busy[mirror.id] ? 'Working…' : 'Prune selected' }}
            </button>
            <button class="btn btn-danger" :disabled="busy[mirror.id]" @click="pruneAll(mirror)">
              Prune all
            </button>
          </div>
        </div>

        <p v-if="mirror.error" style="color: #d29922; font-size: 13px; margin-top: 10px;">
          {{ mirror.error }}
        </p>

        <table v-if="mirror.repositories.length" style="margin-top: 12px;">
          <thead>
            <tr>
              <th style="width: 32px;"></th>
              <th>Repository</th>
              <th>Tag</th>
              <th>Digest</th>
              <th title="Multi-arch tags report a representative platform (linux/amd64 when present)">Size</th>
              <th title="Multi-arch tags report a representative platform (linux/amd64 when present)">Layers</th>
            </tr>
          </thead>
          <tbody>
            <template v-for="repo in mirror.repositories" :key="repo.repository">
              <tr v-for="tag in repo.tags" :key="`${repo.repository}:${tag.tag}`">
                <td>
                  <input
                    type="checkbox"
                    :checked="isSelected(mirror.id, repo.repository, tag.tag)"
                    @change="toggle(mirror.id, repo.repository, tag.tag)"
                  />
                </td>
                <td><code>{{ repo.repository }}</code></td>
                <td>{{ tag.tag }}</td>
                <td><code>{{ tag.digest }}</code></td>
                <td>{{ formatBytes(tag.size_bytes) }}</td>
                <td>{{ formatLayers(tag.layer_count) }}</td>
              </tr>
            </template>
          </tbody>
        </table>
        <p v-else-if="mirror.reachable" style="color: #8b949e; font-size: 13px; margin-top: 10px;">
          No cached images.
        </p>

        <div
          v-if="pruneResults[mirror.id]"
          style="margin-top: 10px; padding: 8px 12px; border-radius: 6px; background: #161b22; font-size: 13px;"
        >
          <p>{{ pruneSummary(pruneResults[mirror.id]) }}</p>
          <ul v-if="failedPruneItems(pruneResults[mirror.id]).length" style="margin: 6px 0 0; padding-left: 18px;">
            <li v-for="(item, i) in failedPruneItems(pruneResults[mirror.id])" :key="i">
              <code>{{ item.repository }}{{ item.tag ? ':' + item.tag : '' }}</code>: {{ item.error }}
            </li>
          </ul>
        </div>

        <div
          v-if="pruneAllResults[mirror.id]"
          style="margin-top: 10px; padding: 8px 12px; border-radius: 6px; background: #161b22; font-size: 13px;"
        >
          <p>{{ pruneAllSummary(pruneAllResults[mirror.id]) }}</p>
          <p v-if="pruneAllResults[mirror.id].error" style="color: #f85149;">{{ pruneAllResults[mirror.id].error }}</p>
          <ul v-if="(pruneAllResults[mirror.id].failed || []).length" style="margin: 6px 0 0; padding-left: 18px;">
            <li v-for="(item, i) in pruneAllResults[mirror.id].failed" :key="i">
              <code>{{ item.repository }}{{ item.tag ? ':' + item.tag : '' }}</code>: {{ item.error }}
            </li>
          </ul>
        </div>

        <div v-if="pruneErrors[mirror.id]" class="error-banner" style="margin-top: 10px;">
          <p>{{ pruneErrors[mirror.id] }}</p>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { fetchImageCacheInfo, pruneAllImageCache, pruneImageCache } from '@/api/client'
import type {
  ImageCacheInfo,
  ImageCacheMirrorInfo,
  ImageCachePruneAllResult,
  ImageCachePruneRef,
  ImageCachePruneResult,
} from '@/api/types'

const info = ref<ImageCacheInfo | null>(null)
const loading = ref(true)
const error = ref<string | null>(null)
const selected = ref<Record<string, boolean>>({})
const busy = ref<Record<string, boolean>>({})
const pruneResults = ref<Record<string, ImageCachePruneResult>>({})
const pruneAllResults = ref<Record<string, ImageCachePruneAllResult['mirrors'][number]>>({})
const pruneErrors = ref<Record<string, string>>({})

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

async function pruneSelected(mirror: ImageCacheMirrorInfo): Promise<void> {
  const refs = selectedRefs(mirror)
  if (refs.length === 0) {
    window.alert('Select at least one image to prune.')
    return
  }
  if (!window.confirm(`Prune ${refs.length} image(s) from ${mirror.id}? The manifest is unlinked now; blob bytes are reclaimed by Zot GC after gcDelay.`)) {
    return
  }
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
  if (!window.confirm(`Prune every cached image from ${mirror.id}? The manifests are unlinked now; blob bytes are reclaimed by Zot GC after gcDelay.`)) {
    return
  }
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

<style scoped>
.badge-neutral {
  background: #30363d;
  color: #c9d1d9;
}
</style>
