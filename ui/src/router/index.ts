import { createRouter, createWebHistory } from 'vue-router'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    {
      path: '/',
      redirect: '/pipelines',
    },
    {
      path: '/pipelines',
      name: 'pipelines',
      component: () => import('@/views/Pipelines.vue'),
    },
    {
      path: '/pipelines/:id',
      name: 'pipeline-detail',
      component: () => import('@/pipeline/PipelineView.vue'),
    },
    {
      path: '/cache',
      name: 'cache',
      component: () => import('@/magiccache/MagicCache.vue'),
    },
    {
      path: '/fleet',
      name: 'fleet',
      component: () => import('@/fleet/Runners.vue'),
    },
    {
      path: '/settings',
      name: 'settings',
      component: () => import('@/views/Settings.vue'),
    },
    {
      path: '/auth/login',
      name: 'login',
      component: () => import('@/auth/Login.vue'),
    },
    {
      path: '/auth/callback',
      name: 'auth-callback',
      component: () => import('@/auth/Callback.vue'),
    },
  ],
})

export default router
