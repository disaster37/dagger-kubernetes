import { vFollowLogs } from '~/utils/followLogs'

export default defineNuxtPlugin((nuxtApp) => {
  nuxtApp.vueApp.directive('follow-logs', vFollowLogs)
})
