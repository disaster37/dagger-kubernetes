<template>
  <div>
    <h1 class="mb-6 text-2xl font-semibold">Connect your environment</h1>

    <UAlert v-if="error" color="error" variant="soft" :description="error">
      <template #actions>
        <UButton color="error" variant="soft" @click="load">Retry</UButton>
      </template>
    </UAlert>

    <div v-if="!snap && !error" class="py-12 text-center text-muted">
      <UIcon name="i-lucide-loader-circle" class="mx-auto size-7 animate-spin text-primary" />
      <p class="mt-3">Loading connection environment…</p>
    </div>

    <template v-if="snap">
      <UCard class="mb-4">
        <h3 class="font-semibold">Options</h3>
        <div class="mt-3 flex flex-wrap items-end gap-6">
          <div v-if="snap.allowed_versions.length > 0">
            <label class="mb-1 block text-[13px] text-muted">Engine version</label>
            <USelect v-model="version" :items="versionOptions" @change="load" />
          </div>
          <div>
            <UCheckbox v-model="reveal" label="Show token plaintext" @change="onRevealChange" />
          </div>
        </div>
        <p class="mt-3 text-xs text-muted">
          Server <code>{{ snap.server_url }}</code> · data host <code>{{ snap.data_hostname }}</code> ·
          version floor <code>{{ snap.version_floor }}</code>
        </p>
      </UCard>

      <UCard class="mb-4">
        <h3 class="font-semibold">Environment variables</h3>
        <UTable class="mt-3" :data="snap.env_vars" :columns="envColumns">
          <template #name-cell="{ row }">
            <code>{{ row.original.name }}</code>
            <span v-if="row.original.required" class="text-error" title="required">*</span>
          </template>
          <template #value-cell="{ row }">
            <template v-if="row.original.name === TOKEN_ENV">
              <template v-if="authDisabled">
                <span class="text-muted">Auth disabled — tokens unavailable</span>
              </template>
              <template v-else-if="!snap.token.exists">
                <span class="text-error">No token. <ULink to="/settings">Generate one on the Settings page.</ULink></span>
              </template>
              <template v-else-if="!snap.token.recoverable">
                <span class="text-error">Token not recoverable (created before this feature). <ULink to="/settings">Regenerate your token</ULink> to enable full-snippet copy.</span>
              </template>
              <template v-else-if="reveal">
                <code class="secret-value">{{ row.original.value }}</code>
              </template>
              <template v-else>
                <code>{{ snap.token.prefix }}…</code>
                <span class="ml-2 text-xs text-muted">Check "Show token plaintext" to reveal.</span>
              </template>
            </template>
            <code v-else class="env-value">{{ row.original.value }}</code>
          </template>
          <template #description-cell="{ row }">
            <span class="text-muted">{{ row.original.description }}</span>
          </template>
        </UTable>
      </UCard>

      <UCard class="mb-4">
        <h3 class="font-semibold">Copy ready-to-use snippets</h3>

        <div class="mt-3 flex flex-wrap items-center gap-2">
          <UButton color="neutral" variant="outline" @click="copyText(bashExports, 'Bash exports')">
            Bash/zsh exports
          </UButton>
          <UButton color="neutral" variant="outline" @click="copyText(bashrcSnippet, '.bashrc snippet')">
            .bashrc snippet
          </UButton>
          <UButton color="neutral" variant="outline" @click="copyText(genericExports, 'Generic exports')">
            Generic exports
          </UButton>
          <UButton color="neutral" variant="outline" @click="copyText(ghSnippet, 'GitHub Actions')">
            GitHub Actions env
          </UButton>
          <UButton color="neutral" variant="outline" @click="copyText(gitlabSnippet, 'GitLab CI')">
            GitLab CI variables
          </UButton>
          <UButton
            color="neutral"
            variant="outline"
            :disabled="!canReveal"
            :title="tokenCopyTitle"
            @click="copyText(tokenValue, 'Token value')"
          >
            Copy token value
          </UButton>
          <UBadge v-if="copied" color="success" variant="soft">{{ copied }} copied!</UBadge>
        </div>

        <h4 class="mt-5 font-semibold">Bash/zsh exports</h4>
        <pre class="snippet">{{ bashExports || '—' }}</pre>

        <h4 class="font-semibold">.bashrc snippet</h4>
        <pre class="snippet">{{ bashrcSnippet }}</pre>

        <h4 class="font-semibold">GitHub Actions <code>env:</code></h4>
        <pre class="snippet">{{ ghSnippet }}</pre>

        <h4 class="font-semibold">GitLab CI <code>variables:</code></h4>
        <pre class="snippet">{{ gitlabSnippet }}</pre>
      </UCard>

      <UCard>
        <details>
          <summary class="cursor-pointer">How to use these</summary>
          <ol class="mt-3 ml-6 list-decimal leading-8">
            <li>Check "Show token plaintext" to include your token in the snippets.</li>
            <li>Click "Copy .bashrc snippet" and paste it into a shell to persist the env for interactive use.</li>
            <li>Reload your shell (or run <code>source ~/.dagger-kubernetes.env</code>).</li>
            <li>Run <code>dagger call github.com/your-org/ci@v1.0.0 build</code>.</li>
            <li>For CI, paste the GitHub Actions / GitLab CI block into your workflow and store the token in your CI secret store once.</li>
          </ol>
        </details>
      </UCard>
    </template>
  </div>
</template>

<script setup lang="ts">
import { fetchConnectEnv, fetchProviders } from '~/api/client'
import type { ConnectEnvSnapshot, ConnectEnvVar } from '~/api/types'

const snap = ref<ConnectEnvSnapshot | null>(null)
const version = ref('__none__')
const reveal = ref(false)
const error = ref('')
const copied = ref('')
const authDisabled = ref(false)

const TOKEN_ENV = 'DAGGER_CLOUD_TOKEN'

const envColumns = [
  { id: 'name', accessorKey: 'name', header: 'Variable' },
  { id: 'value', accessorKey: 'value', header: 'Value' },
  { id: 'description', accessorKey: 'description', header: 'Notes' },
]

const versionOptions = computed(() => [
  { label: 'No pin (use CLI default)', value: '__none__' },
  ...(snap.value?.allowed_versions ?? [])
    .filter((v) => v !== '')
    .map((v) => ({ label: v, value: v })),
])

const tokenValue = computed(() => {
  return (snap.value?.env_vars ?? []).find((e) => e.name === TOKEN_ENV)?.value ?? ''
})

const canReveal = computed(() => {
  return reveal.value && (snap.value?.token.recoverable ?? false) && tokenValue.value !== ''
})

const tokenCopyTitle = computed(() =>
  canReveal.value ? '' : 'Check "Show token plaintext" and ensure the token is recoverable'
)

function envVarsWithValue(): ConnectEnvVar[] {
  return (snap.value?.env_vars ?? []).filter((e) => e.value !== '')
}

function exportLines(quoted: boolean): string {
  return envVarsWithValue()
    .map((e) => (quoted ? `export ${e.name}='${e.value}'` : `export ${e.name}=${e.value}`))
    .join('\n')
}

const bashExports = computed(() => exportLines(true))

const genericExports = computed(() => exportLines(false))

const bashrcSnippet = computed(() => {
  const body = bashExports.value
  return `cat >> ~/.dagger-kubernetes.env <<'EOF'\n${body}\nEOF\necho 'source ~/.dagger-kubernetes.env' >> ~/.bashrc`
})

function ciTokenLine(secretRef: string): string {
  if (canReveal.value) {
    return `  ${TOKEN_ENV}: ${JSON.stringify(tokenValue.value)}`
  }
  return `  ${TOKEN_ENV}: ${secretRef}`
}

function ciSnippet(header: string, tokenLine: string): string {
  const lines = [header]
  for (const e of snap.value?.env_vars ?? []) {
    if (e.name === TOKEN_ENV) {
      lines.push(tokenLine)
    } else if (e.value !== '') {
      lines.push(`  ${e.name}: ${JSON.stringify(e.value)}`)
    }
  }
  return lines.join('\n')
}

const ghSnippet = computed(() => ciSnippet('env:', ciTokenLine('${{ secrets.DAGGER_CLOUD_TOKEN }}')))
const gitlabSnippet = computed(() => ciSnippet('variables:', ciTokenLine('$DAGGER_CLOUD_TOKEN')))

async function load() {
  error.value = ''
  try {
    snap.value = await fetchConnectEnv(version.value === '__none__' ? undefined : version.value, reveal.value)
  } catch (e: any) {
    error.value = e.response?.data?.message || 'Failed to load connection environment'
  }
}

function onRevealChange() {
  load()
}

onMounted(async () => {
  try {
    const providers = await fetchProviders()
    authDisabled.value = !providers.internal
  } catch {
    // ignore — leave authDisabled at its default
  }
  await load()
})

async function copyText(text: string, label: string) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text)
    } else {
      fallbackCopy(text)
    }
  } catch {
    fallbackCopy(text)
  }
  copied.value = label
  window.setTimeout(() => {
    if (copied.value === label) copied.value = ''
  }, 1500)
}

function fallbackCopy(text: string) {
  const ta = document.createElement('textarea')
  ta.value = text
  ta.style.position = 'fixed'
  ta.style.left = '-9999px'
  document.body.appendChild(ta)
  ta.select()
  document.execCommand('copy')
  document.body.removeChild(ta)
}
</script>
