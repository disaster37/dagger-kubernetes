import { authLoginTarget } from '~/utils/auth-navigation'

export default defineNuxtRouteMiddleware(async (to) => {
  const auth = useAuthStore()
  if (to.meta.public) return
  if (!auth.user) await auth.loadUser()
  if (!auth.isAuthenticated) {
    // Preserve the intended target and show a stable session error (mirrors
    // the backend login-error contract).
    return navigateTo(authLoginTarget('session', to.fullPath))
  }
  if (to.meta.admin && !auth.isAdmin) return navigateTo('/pipelines')
})
