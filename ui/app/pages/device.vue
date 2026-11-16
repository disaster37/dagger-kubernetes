<template>
  <div class="mx-auto max-w-lg">
    <h1 class="mb-2 text-2xl font-semibold">Approve CLI login</h1>
    <p class="mb-6 text-[13px] text-muted">
      Enter the code shown by <code>dagger login</code>.
    </p>
    <UCard>
      <form class="space-y-4" @submit.prevent="approve">
        <UFormField label="Device code">
          <UInput v-model="code" placeholder="ABCD-EFGH" class="w-full" />
        </UFormField>
        <UAlert v-if="error" color="error" variant="soft" :description="error" />
        <UAlert v-if="done" color="success" variant="soft" description="Device approved. You can return to the CLI." />
        <UButton type="submit" :loading="loading" block>Approve</UButton>
      </form>
    </UCard>
  </div>
</template>

<script setup lang="ts">
import api from '~/api/client'

const code = ref('')
const loading = ref(false)
const error = ref('')
const done = ref(false)

async function approve() {
  loading.value = true
  error.value = ''
  done.value = false
  try {
    await api.post('/api/v1/auth/device/approve', { user_code: code.value })
    done.value = true
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Approval failed'
  } finally {
    loading.value = false
  }
}
</script>
