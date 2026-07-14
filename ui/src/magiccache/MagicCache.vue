<template>
  <div>
    <h1 class="page-title">MagicCache</h1>
    <div class="card">
      <h3>Cache Configuration</h3>
      <table>
        <tbody>
          <tr><td>Backend</td><td>{{ cacheInfo.backend }}</td></tr>
          <tr><td>Registry</td><td><code>{{ cacheInfo.registry }}</code></td></tr>
        </tbody>
      </table>
    </div>
    <p style="color: #8b949e; font-size: 13px; margin-top: 16px;">
      Cache data is sourced from the remote OCI registry and Prometheus metrics.
    </p>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { fetchCacheInfo } from '@/api/client'

const cacheInfo = ref({ backend: '-', registry: '-' })

onMounted(async () => {
  try {
    cacheInfo.value = await fetchCacheInfo()
  } catch (e) {
    console.error('Failed to fetch cache info', e)
  }
})
</script>
