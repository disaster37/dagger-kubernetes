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

const auth = useAuthStore()
const status = useStatusStore()
const router = useRouter()

const config = useRuntimeConfig()
const appVersion = computed(() => String((config.public as { appVersion?: unknown }).appVersion || 'unknown'))

const navItems = computed(() => {
  const items: { label: string; to: string }[] = [
    { label: 'Pipelines', to: '/pipelines' },
    { label: 'History', to: '/history' },
  ]
  if (auth.isAdmin) items.push({ label: 'Image cache', to: '/image-cache' })
  items.push(
    { label: 'Runners', to: '/fleet' },
    { label: 'Services', to: '/services' },
    { label: 'Settings', to: '/settings' },
    { label: 'Connect', to: '/connect' },
  )
  if (auth.isAdmin) {
    items.push(
      { label: 'Users', to: '/admin/users' },
      { label: 'Groups', to: '/admin/groups' },
      { label: 'Projects', to: '/admin/projects' },
    )
  }
  return items
})

const userMenuItems = computed(() => [
  [
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

onUnmounted(() => {
  status.stop()
})

function handleLogout() {
  status.stop()
  auth.logout()
  router.push('/auth/login')
}
</script>
