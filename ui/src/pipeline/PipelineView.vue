<template>
  <div>
    <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 16px;">
      <router-link to="/pipelines" class="btn">&larr; Back</router-link>
      <h1 class="page-title" style="margin: 0;">Pipeline {{ traceId }}</h1>
      <div></div>
    </div>

    <div style="display: grid; grid-template-columns: 1fr 1fr; gap: 16px;">
      <div class="card">
        <h3>Pipeline DAG</h3>
        <div ref="dagContainer" style="height: 500px; position: relative;">
          <div v-if="dagNodes.length === 0" style="padding: 24px; color: #8b949e; text-align: center;">
            Loading span tree...
          </div>
          <div v-for="node in dagNodes" :key="node.span_id" class="dag-node" :style="nodeStyle(node)">
            <span :class="['badge', `badge-${node.status}`]">●</span>
            {{ node.name }}
          </div>
        </div>
      </div>

      <div class="card">
        <h3>Step Logs</h3>
        <div ref="logContainer" style="height: 500px; overflow-y: auto; font-family: monospace; font-size: 12px; background: #0d1117; padding: 12px; border-radius: 4px;">
          <div v-for="(log, i) in logs" :key="i" style="padding: 2px 0;">
            <span style="color: #f0f6fc;">{{ log }}</span>
          </div>
          <p v-if="logs.length === 0" style="color: #8b949e;">Click a node to see its logs</p>
        </div>
      </div>
    </div>

    <div class="card" style="margin-top: 16px;">
      <h3>Details</h3>
      <table>
        <tbody>
          <tr><td>Status</td><td><span :class="['badge', `badge-${trace.status}`]">{{ trace.status }}</span></td></tr>
          <tr><td>Duration</td><td>{{ trace.duration_ms || 0 }}ms</td></tr>
          <tr><td>Version</td><td>{{ trace.version }}</td></tr>
          <tr><td>CI Provider</td><td>{{ trace.ci_provider || '-' }}</td></tr>
        </tbody>
      </table>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted, onUnmounted, watch } from 'vue'
import { useRoute } from 'vue-router'
import { fetchTrace, connectLiveTrace } from '@/api/client'

const route = useRoute()
const traceId = route.params.id as string
const trace = ref<any>({ status: 'running', duration_ms: 0, version: '-', ci_provider: '-' })
const dagNodes = ref<any[]>([])
const logs = ref<string[]>([])
const logContainer = ref<HTMLElement>()

let ws: WebSocket | null = null

onMounted(async () => {
  try {
    trace.value = await fetchTrace(traceId)
    if (trace.value.root_span) {
      dagNodes.value = flattenTree(trace.value.root_span)
    }
  } catch (e) {
    console.error('Failed to fetch trace', e)
  }

  ws = connectLiveTrace(traceId)
  ws.onmessage = (event) => {
    const update = JSON.parse(event.data)
    if (update.type === 'span_update' && update.span) {
      if (trace.value.root_span) {
        updateNode(update.span.parent_span_id, update.span)
        dagNodes.value = flattenTree(trace.value.root_span)
      }
    }
    if (update.type === 'log') {
      logs.value.push(update.message)
    }
  }
})

onUnmounted(() => {
  if (ws) ws.close()
})

function flattenTree(node: any): any[] {
  const children = node.children || []
  return [node, ...children.flatMap(flattenTree)]
}

function updateNode(parentId: string, span: any) {
  const update = (node: any): boolean => {
    if (node.span_id === span.span_id) {
      Object.assign(node, span)
      return true
    }
    return (node.children || []).some(update)
  }
  if (trace.value.root_span) update(trace.value.root_span)
}

function nodeStyle(node: any) {
  const colors: Record<string, string> = {
    success: '#3fb950',
    failed: '#f85149',
    running: '#58a6ff',
    unset: '#8b949e',
  }
  return {
    borderLeft: `3px solid ${colors[node.status] || '#8b949e'}`,
    padding: '8px 12px',
    margin: '4px 0',
    background: '#21262d',
    borderRadius: '0 4px 4px 0',
  }
}

watch(() => trace.value.root_span, (root) => {
  if (root) dagNodes.value = flattenTree(root)
})
</script>
