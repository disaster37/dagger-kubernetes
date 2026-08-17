import { defineStore } from 'pinia'
import { ref } from 'vue'
import { fetchPlatformStatus } from '@/api/client'
import type { ServiceState, ServiceStatus } from '@/api/types'

const REFRESH_MS = 10_000

export const useStatusStore = defineStore('status', () => {
  const state = ref<ServiceState>('unknown')
  const services = ref<ServiceStatus[]>([])
  const lastError = ref<string | null>(null)
  const loading = ref(true)

  let timer: number | undefined
  let inFlight = false

  async function refresh(): Promise<void> {
    if (inFlight) return
    inFlight = true
    try {
      const status = await fetchPlatformStatus()
      state.value = status.state
      services.value = status.services
      lastError.value = null
    } catch (e) {
      lastError.value = e instanceof Error ? e.message : 'Failed to load status'
    } finally {
      loading.value = false
      inFlight = false
    }
  }

  function start(): void {
    if (timer !== undefined) return
    void refresh()
    timer = window.setInterval(() => void refresh(), REFRESH_MS)
  }

  function stop(): void {
    if (timer !== undefined) {
      window.clearInterval(timer)
      timer = undefined
    }
  }

  return { state, services, lastError, loading, refresh, start, stop }
})
