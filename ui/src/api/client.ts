import axios from 'axios'
import { useAuthStore } from '@/stores/auth'

const api = axios.create({
  baseURL: '/',
  timeout: 30000,
})

api.interceptors.request.use((config) => {
  const auth = useAuthStore()
  if (auth.token) {
    config.headers.Authorization = `Bearer ${auth.token}`
  }
  return config
})

api.interceptors.response.use(
  (response) => response,
  (error) => {
    if (error.response?.status === 401) {
      const auth = useAuthStore()
      auth.logout()
    }
    return Promise.reject(error)
  }
)

export default api

export async function fetchTraces() {
  const { data } = await api.get('/api/v1/traces')
  return data
}

export async function fetchTrace(id: string) {
  const { data } = await api.get(`/api/v1/traces/${id}`)
  return data
}

export async function fetchFleetInfo() {
  const { data } = await api.get('/api/v1/fleet')
  return data
}

export async function fetchCacheInfo() {
  const { data } = await api.get('/api/v1/cache')
  return data
}

export function connectLiveTrace(id: string): WebSocket {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const ws = new WebSocket(`${protocol}//${window.location.host}/api/v1/traces/${id}/live`)
  return ws
}
