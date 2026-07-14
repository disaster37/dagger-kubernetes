<template>
  <div>
    <h1 class="page-title">Settings</h1>
    <div class="card">
      <h3>Authentication</h3>
      <table style="margin-top: 12px;">
        <tbody>
          <tr><td>Internal auth</td><td>{{ settings.internalAuth }}</td></tr>
          <tr><td>OAuth</td><td>{{ settings.oauth }}</td></tr>
        </tbody>
      </table>
    </div>
    <div class="card">
      <h3>Version Allowlist</h3>
      <ul style="padding-left: 24px; margin-top: 8px;">
        <li v-for="v in settings.versions" :key="v">{{ v }}</li>
      </ul>
    </div>
    <div class="card">
      <h3>Fleet Configuration</h3>
      <table>
        <tbody>
          <tr><td>Max replicas/version</td><td>{{ settings.maxReplicas }}</td></tr>
          <tr><td>Max sessions/replica</td><td>{{ settings.maxSessions }}</td></tr>
          <tr><td>Replica idle TTL</td><td>{{ settings.replicaIdleTTL }}</td></tr>
        </tbody>
      </table>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'

const settings = ref({
  internalAuth: 'Enabled',
  oauth: 'Disabled',
  versions: ['0.19', '0.20', '0.21'],
  maxReplicas: '3',
  maxSessions: '8',
  replicaIdleTTL: '5m',
})
</script>
