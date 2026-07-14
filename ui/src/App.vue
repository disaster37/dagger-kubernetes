<template>
  <div id="app">
    <nav class="navbar">
      <router-link to="/" class="logo">Dagger Cache</router-link>
      <div class="nav-links">
        <router-link to="/pipelines">Pipelines</router-link>
        <router-link to="/cache">MagicCache</router-link>
        <router-link to="/fleet">Runners</router-link>
        <router-link to="/settings">Settings</router-link>
      </div>
      <div class="nav-user">
        <span v-if="auth.isAuthenticated">
          {{ auth.username }}
          <button @click="auth.logout">Logout</button>
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
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
</script>
