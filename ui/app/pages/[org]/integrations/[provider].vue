<script setup lang="ts">
const route = useRoute()
const router = useRouter()
onMounted(() => router.replace(`/orgs/${route.params.org}/integrations/${route.params.provider}`))
</script>

<template>
  <div class="text-sm text-muted">Redirecting…</div>
</template>
