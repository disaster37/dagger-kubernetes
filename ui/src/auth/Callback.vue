<template>
  <div style="max-width: 400px; margin: 80px auto; text-align: center;">
    <p>Authenticating...</p>
  </div>
</template>

<script setup lang="ts">
import { onMounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

onMounted(() => {
  const code = route.query.code as string
  if (code) {
    auth.login(code, '', 'user', 'viewer')
  }
  router.push('/pipelines')
})
</script>
