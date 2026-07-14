<template>
  <div style="max-width: 400px; margin: 80px auto;">
    <div class="card">
      <h2 style="margin-bottom: 16px;">Login</h2>
      <form @submit.prevent="handleLogin">
        <div style="margin-bottom: 12px;">
          <label style="display: block; margin-bottom: 4px; font-size: 14px;">Token</label>
          <input
            v-model="token"
            type="password"
            placeholder="Enter your DAGGER_CLOUD_TOKEN"
            style="width: 100%; padding: 8px 12px; background: #0d1117; border: 1px solid #30363d; border-radius: 6px; color: #c9d1d9; font-size: 14px;"
          />
        </div>
        <button type="submit" class="btn btn-primary" style="width: 100%;">Login</button>
      </form>
      <p v-if="error" style="color: #f85149; font-size: 13px; margin-top: 12px;">{{ error }}</p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'

const router = useRouter()
const auth = useAuthStore()
const token = ref('')
const error = ref('')

function handleLogin() {
  if (!token.value) {
    error.value = 'Token is required'
    return
  }
  error.value = ''
  auth.login(token.value, '', 'user', 'viewer')
  router.push('/pipelines')
}
</script>
