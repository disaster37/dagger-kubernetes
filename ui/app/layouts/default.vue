<template>
  <UHeader title="Dagger Kubernetes">
    <template #title>
      <NuxtLink to="/" class="flex items-center gap-2">
        <AppLogo class="size-9" aria-hidden="true" />
        <span class="font-semibold">Dagger Kubernetes</span>
      </NuxtLink>
    </template>
    <UNavigationMenu :items="navItems" orientation="horizontal" />

    <template #right>
      <OrgSwitcher />
      <StatusIndicator v-if="auth.isAuthenticated" />
      <UDropdownMenu v-if="auth.isAuthenticated && auth.user" :items="userMenuItems">
        <UButton color="neutral" variant="ghost" size="sm" trailing-icon="i-lucide-chevron-down">
          {{ auth.user.username }}
          <UBadge :color="auth.user.role === 'admin' ? 'primary' : 'neutral'" variant="soft" size="sm">
            {{ auth.user.role }}
          </UBadge>
        </UButton>
      </UDropdownMenu>
      <UButton v-else to="/auth/login" color="primary" variant="soft" size="sm">Login</UButton>
    </template>
  </UHeader>

  <UMain>
    <div class="p-6">
      <slot />
    </div>
  </UMain>

  <UFooter>
    <!-- default slot = centered -->
    <p class="text-sm text-muted">
      Dagger Kubernetes · {{ appVersion }}
    </p>

    <template #right>
      <UButton
        icon="i-lucide-github"
        color="neutral"
        variant="link"
        size="sm"
        to="https://github.com/disaster37/dagger-kubernetes/issues"
        target="_blank"
        rel="noopener noreferrer"
        aria-label="Report an issue on GitHub"
      >
        Issues
      </UButton>
    </template>
  </UFooter>
</template>

<script setup lang="ts">
import { onUnmounted, watch } from 'vue'
import { useAuthStore } from '~/stores/auth'
import { useStatusStore } from '~/stores/status'
import { useOrgStore } from '~/stores/org'

const auth = useAuthStore()
const status = useStatusStore()
const org = useOrgStore()
const router = useRouter()
const route = useRoute()

const config = useRuntimeConfig()
const appVersion = computed(() => String((config.public as { appVersion?: unknown }).appVersion || 'unknown'))

const navItems = computed(() => {
  const items: { label: string; to?: string; children?: { label: string; to: string }[] }[] = [
    { label: 'Pipelines', to: '/pipelines' },
    { label: 'Checks', to: '/checks' },
    { label: 'Workflows', to: '/workflows' },
    { label: 'Artifacts', to: '/artifacts' },
  ]
  const infra = [
    { label: 'Runners', to: '/fleet' },
    { label: 'Remote Cache', to: '/remote-cache' },
    { label: 'History', to: '/history' },
    { label: 'Services', to: '/services' },
  ]
  if (auth.isAdmin) infra.push({ label: 'Image Cache', to: '/image-cache' })
  items.push({ label: 'Infrastructure', children: infra })
  if (org.activeSlug) {
    const base = `/orgs/${org.activeSlug}`
    items.push({
      label: 'Organization',
      children: [
        { label: 'Overview & Settings', to: base },
        { label: 'Groups', to: `${base}/groups` },
        { label: 'Projects', to: `${base}/projects` },
        { label: 'Members', to: `${base}/members` },
      ],
    })
  }
  if (auth.isAdmin) {
    items.push({
      label: 'Admin',
      children: [
        { label: 'Organizations & Quotas', to: '/admin/orgs' },
        { label: 'Users', to: '/admin/users' },
      ],
    })
  }
  return items
})

const userMenuItems = computed(() => [
  [
    {
      label: 'Connect CLI',
      icon: 'i-lucide-terminal',
      to: '/connect',
    },
    {
      label: 'Account Settings',
      icon: 'i-lucide-settings',
      to: '/settings',
    },
    {
      label: 'Logout',
      icon: 'i-lucide-log-out',
      onSelect: () => handleLogout(),
    },
  ],
])

// Own the status-polling lifecycle here (the header indicator must reflect
// platform health on every authenticated page). A watcher (instead of
// onMounted-only) covers login-after-mount and interceptor-triggered logout.
watch(
  () => auth.isAuthenticated,
  (authed) => {
    if (authed) status.start()
    else status.stop()
  },
  { immediate: true },
)

watch(
  () => route.fullPath,
  () => org.syncFromRoute(route),
  { immediate: true },
)

onUnmounted(() => {
  status.stop()
})

async function handleLogout() {
  status.stop()
  try {
    await auth.logout()
  } catch {
    // The local session is already cleared; server-side revocation may have
    // failed, so the user is signed out locally without claiming revocation.
    useToast().add({ title: 'Signed out locally; the server session may still be active', color: 'warning' })
  }
  router.push('/auth/login')
}
</script>
