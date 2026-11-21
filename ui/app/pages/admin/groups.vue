<script setup lang="ts">
const org = useOrgStore()
const router = useRouter()
onMounted(() => router.replace(org.activeSlug ? `/orgs/${org.activeSlug}/groups` : '/admin/orgs'))
</script>

<template>
  <div class="text-sm text-muted">Redirecting…</div>
</template>
