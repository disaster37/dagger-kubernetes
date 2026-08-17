<template>
  <div>
    <h1 class="page-title">Connect your environment</h1>

    <div v-if="error" class="error-banner">
      <p>{{ error }}</p>
      <button class="btn" @click="load">Retry</button>
    </div>

    <div v-if="!snap && !error" class="loading-state">
      <div class="spinner"></div>
      <p>Loading connection environment…</p>
    </div>

    <template v-if="snap">
      <div class="card">
        <h3>Options</h3>
        <div style="display: flex; gap: 24px; flex-wrap: wrap; align-items: flex-end; margin-top: 12px;">
          <div v-if="snap.allowed_versions.length > 0">
            <label style="display: block; margin-bottom: 4px; font-size: 13px; color: #8b949e;">Engine version</label>
            <select v-model="version" @change="load" style="padding: 8px 12px; background: #0d1117; border: 1px solid #30363d; border-radius: 6px; color: #c9d1d9;">
              <option value="">No pin (use CLI default)</option>
              <option v-for="v in snap.allowed_versions" :key="v" :value="v">{{ v }}</option>
            </select>
          </div>
          <div>
            <label style="display: flex; align-items: center; gap: 8px; font-size: 13px; cursor: pointer;">
              <input type="checkbox" v-model="reveal" @change="onRevealChange" />
              Show token plaintext
            </label>
          </div>
        </div>
        <p style="font-size: 12px; color: #8b949e; margin-top: 12px;">
          Server <code>{{ snap.server_url }}</code> · data host <code>{{ snap.data_hostname }}</code> ·
          cache backend <code>{{ snap.cache_backend }}</code> · version floor <code>{{ snap.version_floor }}</code>
        </p>
      </div>

      <div class="card">
        <h3>Environment variables</h3>
        <table style="margin-top: 12px;">
          <thead>
            <tr>
              <th>Variable</th>
              <th>Value</th>
              <th>Notes</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="ev in snap.env_vars" :key="ev.name">
              <td>
                <code>{{ ev.name }}</code>
                <span v-if="ev.required" style="color: #f85149;" title="required">*</span>
              </td>
              <td v-if="ev.name === TOKEN_ENV">
                <template v-if="authDisabled">
                  <span style="color: #8b949e;">Auth disabled — tokens unavailable</span>
                </template>
                <template v-else-if="!snap.token.exists">
                  <span style="color: #f85149;">No token. <router-link to="/settings">Generate one on the Settings page.</router-link></span>
                </template>
                <template v-else-if="!snap.token.recoverable">
                  <span style="color: #f85149;">Token not recoverable (created before this feature). <router-link to="/settings">Regenerate your token</router-link> to enable full-snippet copy.</span>
                </template>
                <template v-else-if="reveal">
                  <code class="secret-value">{{ ev.value }}</code>
                </template>
                <template v-else>
                  <code>{{ snap.token.prefix }}…</code>
                  <span style="color: #8b949e; margin-left: 8px; font-size: 12px;">Check "Show token plaintext" to reveal.</span>
                </template>
              </td>
              <td v-else>
                <code class="env-value">{{ ev.value }}</code>
              </td>
              <td style="color: #8b949e;">{{ ev.description }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <div class="card">
        <h3>Copy ready-to-use snippets</h3>

        <div style="display: flex; gap: 8px; flex-wrap: wrap; margin-top: 12px;">
          <button class="btn" @click="copyText(bashExports, 'Bash exports')">
            Bash/zsh exports
          </button>
          <button class="btn" @click="copyText(bashrcSnippet, '.bashrc snippet')">
            .bashrc snippet
          </button>
          <button class="btn" @click="copyText(genericExports, 'Generic exports')">
            Generic exports
          </button>
          <button class="btn" @click="copyText(ghSnippet, 'GitHub Actions')">
            GitHub Actions env
          </button>
          <button class="btn" @click="copyText(gitlabSnippet, 'GitLab CI')">
            GitLab CI variables
          </button>
          <button
            class="btn"
            :disabled="!canReveal"
            :title="tokenCopyTitle"
            @click="copyText(tokenValue, 'Token value')"
          >
            Copy token value
          </button>
          <span v-if="copied" class="badge badge-success">{{ copied }} copied!</span>
        </div>

        <div style="margin-top: 16px;">
          <label style="display: flex; align-items: center; gap: 8px; font-size: 13px; cursor: pointer;">
            <input type="checkbox" v-model="includePlaintextCI" :disabled="!canReveal" />
            Include plaintext token in CI snippets
          </label>
          <p v-if="includePlaintextCI" style="color: #f85149; font-size: 12px; margin-top: 8px;">
            Warning: this embeds the token plaintext into your CI config. Committed CI files are
            version-controlled — prefer the secret reference unless you accept the risk.
          </p>
        </div>

        <h4 style="margin-top: 20px;">Bash/zsh exports</h4>
        <pre class="snippet">{{ bashExports || '—' }}</pre>

        <h4>.bashrc snippet</h4>
        <pre class="snippet">{{ bashrcSnippet }}</pre>

        <h4>GitHub Actions <code>env:</code></h4>
        <pre class="snippet">{{ ghSnippet }}</pre>

        <h4>GitLab CI <code>variables:</code></h4>
        <pre class="snippet">{{ gitlabSnippet }}</pre>
      </div>

      <div class="card">
        <details>
          <summary style="cursor: pointer;">How to use these</summary>
          <ol style="margin: 12px 0 0 24px; line-height: 1.8;">
            <li>Check "Show token plaintext" to include your token in the snippets.</li>
            <li>Click "Copy .bashrc snippet" and paste it into a shell to persist the env for interactive use.</li>
            <li>Reload your shell (or run <code>source ~/.dagger-cache.env</code>).</li>
            <li>Run <code>dagger call github.com/your-org/ci@v1.0.0 build</code>.</li>
            <li>For CI, paste the GitHub Actions / GitLab CI block into your workflow and store the token in your CI secret store once.</li>
          </ol>
        </details>
      </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { fetchConnectEnv, fetchProviders } from '@/api/client'
import type { ConnectEnvSnapshot, ConnectEnvVar } from '@/api/types'

const snap = ref<ConnectEnvSnapshot | null>(null)
const version = ref('')
const reveal = ref(false)
const includePlaintextCI = ref(false)
const error = ref('')
const copied = ref('')
const authDisabled = ref(false)

const TOKEN_ENV = 'DAGGER_CLOUD_TOKEN'

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
  return `cat >> ~/.dagger-cache.env <<'EOF'\n${body}\nEOF\necho 'source ~/.dagger-cache.env' >> ~/.bashrc`
})

function ciTokenLine(secretRef: string): string {
  if (includePlaintextCI.value && tokenValue.value) {
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
    snap.value = await fetchConnectEnv(version.value || undefined, reveal.value)
  } catch (e: any) {
    error.value = e.response?.data?.message || 'Failed to load connection environment'
  }
}

function onRevealChange() {
  includePlaintextCI.value = false
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

<style scoped>
.secret-value {
  display: inline-block;
  padding: 4px 8px;
  background: #3a1b1b;
  border: 1px solid #f85149;
  border-radius: 6px;
  word-break: break-all;
}
.env-value {
  display: inline-block;
  padding: 4px 8px;
  background: #161b22;
  border-radius: 6px;
  word-break: break-all;
}
.snippet {
  background: #0d1117;
  border: 1px solid #30363d;
  border-radius: 6px;
  padding: 12px;
  margin-top: 8px;
  overflow-x: auto;
  font-size: 13px;
  white-space: pre-wrap;
  word-break: break-all;
}
</style>
