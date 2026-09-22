export default defineNuxtRouteMiddleware(async (to) => {
  const auth = useAuthStore()
  if (to.meta.public) return
  if (!auth.user) await auth.loadUser()
  if (!auth.isAuthenticated) {
    return navigateTo({ path: '/auth/login', query: { redirect: to.fullPath } })
  }
  if (to.meta.admin && !auth.isAdmin) return navigateTo('/pipelines')
})
