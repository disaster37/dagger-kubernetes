import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '@/stores/auth'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', redirect: '/pipelines' },
    { path: '/pipelines', name: 'pipelines', component: () => import('@/views/Pipelines.vue') },
    { path: '/pipelines/:id', name: 'pipeline-detail', component: () => import('@/pipeline/PipelineView.vue') },
    { path: '/cache', name: 'cache', component: () => import('@/magiccache/MagicCache.vue') },
    { path: '/history', name: 'history', component: () => import('@/history/History.vue') },
    { path: '/fleet', name: 'fleet', component: () => import('@/fleet/Runners.vue') },
    { path: '/services', name: 'services', component: () => import('@/views/Services.vue') },
    { path: '/settings', name: 'settings', component: () => import('@/views/Settings.vue') },
    { path: '/connect', name: 'connect', component: () => import('@/views/Connect.vue') },
    { path: '/auth/login', name: 'login', component: () => import('@/auth/Login.vue'), meta: { public: true } },
    { path: '/auth/callback', name: 'auth-callback', component: () => import('@/auth/Callback.vue'), meta: { public: true } },
    { path: '/admin/users', name: 'admin-users', component: () => import('@/views/admin/Users.vue'), meta: { admin: true } },
    { path: '/admin/groups', name: 'admin-groups', component: () => import('@/views/admin/Groups.vue'), meta: { admin: true } },
    { path: '/admin/projects', name: 'admin-projects', component: () => import('@/views/admin/Projects.vue'), meta: { admin: true } },
  ],
})

router.beforeEach(async (to) => {
  const auth = useAuthStore()
  if (to.meta.public) {
    return true
  }
  // Bootstrap the session from the httpOnly cookie on first navigation (/me).
  if (!auth.user) {
    await auth.loadUser()
  }
  if (!auth.isAuthenticated) {
    return { path: '/auth/login', query: { redirect: to.fullPath } }
  }
  if (to.meta.admin && !auth.isAdmin) {
    return { path: '/pipelines' }
  }
  return true
})

export default router
