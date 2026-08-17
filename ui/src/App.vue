<template>
  <div id="app">
    <nav class="navbar">
      <router-link to="/" class="logo">Dagger Cache</router-link>
      <div class="nav-links">
        <router-link to="/pipelines">Pipelines</router-link>
        <router-link to="/cache">MagicCache</router-link>
        <router-link to="/fleet">Runners</router-link>
        <router-link to="/services">Services</router-link>
        <router-link to="/settings">Settings</router-link>
        <template v-if="auth.isAdmin">
          <router-link to="/admin/users">Users</router-link>
          <router-link to="/admin/groups">Groups</router-link>
          <router-link to="/admin/projects">Projects</router-link>
        </template>
      </div>
      <StatusIndicator v-if="auth.isAuthenticated" />
      <div class="nav-user">
        <span v-if="auth.isAuthenticated && auth.user">
          {{ auth.user.username }}
          <span class="role-badge" :class="`role-${auth.user.role}`">{{ auth.user.role }}</span>
          <button @click="handleLogout">Logout</button>
        </span>
        <router-link v-else to="/auth/login">Login</router-link>
      </div>
    </nav>
    <main class="content">
      <router-view />
    </main>
  </div>
</template>

<script setup lang="ts">
import { onUnmounted, watch } from 'vue'
import { useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { useStatusStore } from '@/stores/status'
import StatusIndicator from '@/components/StatusIndicator.vue'

const auth = useAuthStore()
const status = useStatusStore()
const router = useRouter()

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

<style scoped>
.role-badge {
  display: inline-block;
  margin: 0 6px;
  padding: 1px 6px;
  border-radius: 4px;
  font-size: 11px;
  font-weight: 600;
  text-transform: uppercase;
  background: #30363d;
  color: #c9d1d9;
}
.role-badge.role-admin {
  background: #1f6feb;
  color: #fff;
}
</style>
