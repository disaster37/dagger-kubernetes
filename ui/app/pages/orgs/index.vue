<script setup lang="ts">
const org = useOrgStore()
const auth = useAuthStore()
const router = useRouter()

onMounted(() => {
  if (org.activeSlug) router.replace(`/orgs/${org.activeSlug}`)
  else if (auth.isAdmin) router.replace('/admin/orgs')
  else router.replace('/pipelines')
})
</script>

<template>
  <div class="text-sm text-muted">Redirecting…</div>
</template>
