import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import type { OrgSummary } from '~/api/types'

const STORAGE_KEY = 'dagger_kubernetes_active_org'

export const useOrgStore = defineStore('org', () => {
  const orgs = ref<OrgSummary[]>([])
  const activeSlug = ref('')
  const routeForbidden = ref(false)
  const hasNoOrgs = computed(() => orgs.value.length === 0)

  const activeOrg = computed(() => orgs.value.find(o => o.name === activeSlug.value) ?? null)

  function persist(slug: string): void {
    if (typeof window !== 'undefined') {
      window.localStorage.setItem(STORAGE_KEY, slug)
    }
  }

  function storedSlug(): string {
    if (typeof window === 'undefined') return ''
    return window.localStorage.getItem(STORAGE_KEY) ?? ''
  }

  function setActive(slug: string): void {
    if (!orgs.value.some(o => o.name === slug)) return
    activeSlug.value = slug
    persist(slug)
  }

  // hydrate replaces the accessible org list and applies deterministic
  // selection precedence. It never substitutes an unauthorized organization.
  function hydrate(next: OrgSummary[]): void {
    orgs.value = [...next].sort((a, b) => a.name.localeCompare(b.name))
    if (activeSlug.value && !orgs.value.some(o => o.name === activeSlug.value)) {
      activeSlug.value = ''
    }
    if (!activeSlug.value) {
      const saved = storedSlug()
      if (saved && orgs.value.some(o => o.name === saved)) {
        activeSlug.value = saved
      } else if (orgs.value.length > 0) {
        const first = orgs.value[0]
        if (first) {
          activeSlug.value = first.name
          persist(first.name)
        }
      }
    }
  }

  // syncFromRoute applies the deep-link org when accessible; otherwise it
  // marks the route forbidden without overwriting the stored selection.
  function syncFromRoute(route: { params?: Record<string, unknown> }): void {
    routeForbidden.value = false
    const raw = route.params?.org
    const param = Array.isArray(raw) ? raw[0] : raw
    if (!param) return
    const match = orgs.value.find(o => o.name === param || o.id === param)
    if (!match) {
      routeForbidden.value = true
      return
    }
    activeSlug.value = match.name
    persist(match.name)
  }

  function clear(): void {
    orgs.value = []
    activeSlug.value = ''
    routeForbidden.value = false
  }

  return { orgs, activeSlug, activeOrg, routeForbidden, hasNoOrgs, hydrate, setActive, syncFromRoute, clear }
})
