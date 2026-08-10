<template>
  <div id="app">
    <nav class="navbar">
      <router-link to="/" class="logo">Dagger Cache</router-link>
      <div class="nav-links">
        <router-link to="/pipelines">Pipelines</router-link>
        <router-link to="/cache">MagicCache</router-link>
        <router-link to="/fleet">Runners</router-link>
        <router-link to="/settings">Settings</router-link>
        <template v-if="auth.isAdmin">
          <router-link to="/admin/users">Users</router-link>
          <router-link to="/admin/groups">Groups</router-link>
          <router-link to="/admin/projects">Projects</router-link>
        </template>
      </div>
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
import { useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const router = useRouter()

function handleLogout() {
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
