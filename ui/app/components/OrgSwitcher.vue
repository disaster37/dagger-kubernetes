<template>
  <div v-if="auth.isAuthenticated && org.activeOrg" class="flex items-center gap-2">
    <UBadge v-if="org.orgs.length === 1 && !auth.isAdmin" variant="subtle" color="neutral">
      {{ org.activeOrg.display_name || org.activeOrg.name }}
    </UBadge>
    <UDropdownMenu v-else :items="items">
      <UButton color="neutral" variant="ghost" size="sm" trailing-icon="i-lucide-chevron-down">
        <span class="max-w-40 truncate">{{ org.activeOrg.display_name || org.activeOrg.name }}</span>
        <UBadge variant="soft" size="sm">{{ org.activeOrg.role }}</UBadge>
      </UButton>
    </UDropdownMenu>
  </div>
</template>

<script setup lang="ts">
import { useAuthStore } from '~/stores/auth'
import { useOrgStore } from '~/stores/org'

const auth = useAuthStore()
const org = useOrgStore()
const router = useRouter()

type SwitcherRow = { label: string; suffix: string; badge?: string; onSelect: () => void }

const items = computed(() => {
  const rows: SwitcherRow[] = org.orgs.map(o => ({
    label: o.display_name || o.name,
    suffix: o.name,
    badge: o.role,
    onSelect: () => { void switchOrg(o.name) },
  }))
  if (auth.isAdmin) {
    rows.push({ label: 'Organizations & Quotas', suffix: '', onSelect: () => { void router.push('/admin/orgs') } })
  }
  return [rows]
})

async function switchOrg(slug: string) {
  if (!slug || slug === org.activeSlug) return
  const current = router.currentRoute.value
  org.setActive(slug)
  const orgMatch = current.path.match(/^\/orgs\/[^/]+(\/.*)?$/)
  if (orgMatch) {
    await router.push(`/orgs/${slug}${orgMatch[1] ?? ''}`)
  } else {
    await router.replace({ path: current.path, query: current.query })
  }
}
</script>
