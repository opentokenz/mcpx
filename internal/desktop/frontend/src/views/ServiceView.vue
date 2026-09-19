<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import {
  api,
  type BrowserIntegrationState,
  type CloudflareConfig,
  type CloudflareState,
  type ConnectionConfig,
  type ServiceState,
} from '../api'

const state = ref<ServiceState | null>(null)
const cloudflare = ref<CloudflareState | null>(null)
const browser = ref<BrowserIntegrationState | null>(null)
const browserError = ref('')
const cloudflareConfig = ref<CloudflareConfig | null>(null)
const config = ref<ConnectionConfig | null>(null)
const error = ref('')
const busy = ref(false)
const copied = ref(false)
const showConnection = ref(false)
const savedHint = ref('')

let timer: number | undefined

const statusText: Record<string, string> = {
  running: '运行中',
  starting: '启动中（端口尚未就绪）',
  conflict: '端口被其他进程占用',
  stopped: '已停止',
}

const browserStatusText: Record<BrowserIntegrationState['state'], string> = {
  connected: '浏览器扩展已连接',
  installed_disconnected: '扩展已安装，但未连接',
  not_installed: '未检测到浏览器扩展',
}

const browserStatusClass = computed(() => {
  if (browserError.value) return 'conflict'
  if (browser.value?.state === 'connected') return 'running'
  if (browser.value?.state === 'installed_disconnected') return 'starting'
  return 'stopped'
})

// 受托盘/CLI 管理的进程才可以停止或重启。端口冲突时的占用者不归我们管，
// 也不该被我们停掉。
const managed = computed(() => {
  const localManaged = state.value?.status === 'running' || state.value?.status === 'starting'
  if (cloudflareConfig.value?.manage_with_mcpx === false) return localManaged
  return localManaged || cloudflare.value?.status === 'running' || cloudflare.value?.status === 'starting'
})

const startSatisfied = computed(() => {
  if (state.value?.status !== 'running') return false
  if (cloudflareConfig.value?.manage_with_mcpx === false) return true
  return cloudflare.value?.status === 'running'
})

async function refresh() {
  const browserRequest = api.browserStatus().then(
    (value) => ({ value, error: '' }),
    (err: Error) => ({ value: null, error: err.message }),
  )
  try {
    const [serviceState, cloudflareState, tunnelConfig] = await Promise.all([
      api.status(),
      api.cloudflareStatus(),
      api.getCloudflareConfig(),
    ])
    state.value = serviceState
    cloudflare.value = cloudflareState
    cloudflareConfig.value = tunnelConfig
  } catch (err) {
    error.value = (err as Error).message
  }
  const browserResult = await browserRequest
  browser.value = browserResult.value
  browserError.value = browserResult.error
}

async function act(action: 'start' | 'stop' | 'restart') {
  busy.value = true
  error.value = ''
  try {
    state.value = await api.serviceAction(action)
    cloudflare.value = await api.cloudflareStatus()
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

async function openBrowserHelp() {
  try {
    await api.openBrowserHelp()
  } catch (err) {
    error.value = (err as Error).message
  }
}

async function copyEndpoint() {
  if (!state.value) return
  await navigator.clipboard.writeText(state.value.endpoint)
  copied.value = true
  window.setTimeout(() => (copied.value = false), 1500)
}

async function loadConfig() {
  try {
    config.value = await api.getConfig()
  } catch (err) {
    error.value = (err as Error).message
  }
}

async function toggleConnection() {
  showConnection.value = !showConnection.value
  if (showConnection.value && !config.value) {
    await loadConfig()
  }
}

async function generateToken() {
  if (!config.value) return
  try {
    config.value.token = (await api.generateToken()).token
  } catch (err) {
    error.value = (err as Error).message
  }
}

// 保存后不自动重启：正在跑的 MCPX 使用的还是旧配置，由用户决定何时重启整套服务。
async function saveConfig() {
  if (!config.value) return
  busy.value = true
  error.value = ''
  try {
    config.value = await api.putConfig(config.value)
    savedHint.value = '已保存。改动在下次重启 MCPX 后生效。'
    window.setTimeout(() => (savedHint.value = ''), 4000)
    await refresh()
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

onMounted(() => {
  refresh()
  timer = window.setInterval(refresh, 2000)
})

onUnmounted(() => window.clearInterval(timer))
</script>

<template>
  <div class="page">
    <div v-if="error" class="banner">{{ error }}</div>
    <div v-if="state?.error" class="banner">{{ state.error }}</div>

    <div v-if="state" class="card">
      <div class="row">
        <span class="status-dot" :class="state.status"></span>
        <span class="status-headline">{{ statusText[state.status] }}</span>
        <span class="spacer"></span>
        <button
          class="btn primary"
          :disabled="busy || startSatisfied || state.status === 'conflict' || state.status === 'starting'"
          @click="act('start')"
        >
          启动 MCPX
        </button>
        <button class="btn" :disabled="busy || !managed" @click="act('stop')">
          停止 MCPX
        </button>
        <button class="btn" :disabled="busy || !managed" @click="act('restart')">
          重启 MCPX
        </button>
      </div>

      <p v-if="cloudflareConfig?.manage_with_mcpx !== false" class="hint">
        “启动 MCPX”会一次启动完整服务：本地 MCP Runtime + 已配置的 Cloudflare Tunnel。
        “停止 / 重启”同样作用于整套服务。
      </p>
      <p v-else class="hint">
        Cloudflare 页已关闭 Tunnel 联动。“启动 / 停止 / 重启 MCPX”现在只管理本地 Runtime；
        Cloudflare Tunnel 需要在 Cloudflare 页手动启动或停止。
      </p>

      <p v-if="state.status === 'conflict'" class="hint" style="color: var(--warn)">
        {{ state.addr }} 已被另一个进程占用，但它不受托盘管理（daemon 状态文件里没有记录）。<br />
        可能是旧版本 mcpx、手工前台启动的实例，或别的程序。请先手动结束它，或在下方连接配置里换一个端口。
      </p>

      <dl class="facts">
        <dt>MCP 端点</dt>
        <dd>{{ state.endpoint }}</dd>
        <dt>鉴权模式</dt>
        <dd>{{ state.auth_mode }}</dd>
        <dt>进程</dt>
        <dd>{{ state.pid > 0 ? `pid=${state.pid}` : '—' }}</dd>
        <dt>Cloudflare Tunnel</dt>
        <dd>{{ cloudflare?.status === 'running' ? `运行中 · pid=${cloudflare.pid}` : cloudflare?.status || '—' }}</dd>
        <dt>公网 MCP</dt>
        <dd>{{ cloudflare?.public_mcp_url || '—' }}</dd>
        <dt>运行时目录</dt>
        <dd>{{ state.home_dir }}</dd>
      </dl>

      <div class="row" style="margin-top: 14px">
        <button class="btn" @click="copyEndpoint">
          {{ copied ? '已复制' : '复制端点地址' }}
        </button>
        <button class="btn" @click="api.open('home')">打开运行时目录</button>
        <button class="btn" @click="api.open('config')">打开 config.yaml</button>
      </div>
    </div>

    <div v-else class="empty">读取状态中…</div>

    <div class="card">
      <div class="row">
        <h2 class="card-title" style="margin: 0">浏览器扩展</h2>
        <span class="status-dot" :class="browserStatusClass"></span>
        <span v-if="browserError" class="status-headline" style="font-size: 14px">状态读取失败</span>
        <span v-else-if="browser" class="status-headline" style="font-size: 14px">
          {{ browserStatusText[browser.state] }}
        </span>
        <span class="spacer"></span>
        <button v-if="browser?.state !== 'connected'" class="btn" @click="refresh">重新检测</button>
        <button v-if="browser?.state !== 'connected'" class="btn primary" @click="openBrowserHelp">
          {{ browser?.state === 'not_installed' ? '打开官方安装说明' : '打开官方帮助' }}
        </button>
      </div>
      <p v-if="browserError" class="hint" style="color: var(--warn)">{{ browserError }}</p>
      <p v-else-if="browser" class="hint">{{ browser.message }}</p>
      <dl v-if="browser?.installations.length" class="facts">
        <template v-for="item in browser.installations" :key="`${item.family}:${item.profile}:${item.extension_id}`">
          <dt>{{ item.family }} / {{ item.profile }}</dt>
          <dd>ChatGPT {{ item.version }} · {{ item.extension_id }}</dd>
        </template>
      </dl>
      <p v-if="browser?.state === 'installed_disconnected'" class="hint" style="color: var(--warn)">
        MCPX 已识别本机安装，无需手工配置扩展 ID。若长时间未连接，请确认浏览器和扩展均已启动。
      </p>
    </div>

    <div class="card">
      <div class="row">
        <h2 class="card-title" style="margin: 0">连接配置</h2>
        <span class="spacer"></span>
        <button class="btn" @click="toggleConnection">
          {{ showConnection ? '收起' : '展开' }}
        </button>
      </div>

      <template v-if="showConnection && config">
        <div class="row" style="margin-top: 14px; align-items: flex-end">
          <label class="field">
            监听地址
            <input v-model="config.host" type="text" style="width: 140px" />
          </label>
          <label class="field">
            端口
            <input v-model.number="config.port" type="number" style="width: 100px" />
          </label>
          <label class="field">
            鉴权模式
            <select v-model="config.auth_mode">
              <option value="">默认（{{ config.effective_mode }}）</option>
              <option value="open">open</option>
              <option value="bearer">bearer</option>
              <option value="oauth">oauth</option>
              <option value="dual">dual</option>
            </select>
          </label>
        </div>

        <div class="row" style="margin-top: 12px; align-items: flex-end">
          <label class="field" style="flex: 1">
            Bearer Token
            <input v-model="config.token" type="text" placeholder="未设置" />
          </label>
          <button class="btn" @click="generateToken">生成</button>
          <button class="btn primary" :disabled="busy" @click="saveConfig">保存</button>
        </div>

        <div v-if="config.auth_mode === 'oauth' || config.auth_mode === 'dual'" class="row" style="margin-top: 12px">
          <label class="field" style="flex: 1">
            OAuth server_url（公网 Origin）
            <input v-model="config.oauth_server_url" type="text" placeholder="https://mcp.example.com" />
          </label>
        </div>

        <p class="hint">
          只涉及监听地址、鉴权与 OAuth 公网 Origin；安全策略、保留策略等仍需手改 config.yaml。<br />
          Cloudflare 页启用 OAuth 联动后，会自动维护这里的 server_url。<br />
          保存会整体重写 config.yaml 并<strong>丢失其中的注释</strong>，写入前会自动备份为
          config.yaml.bak。
        </p>
        <p v-if="savedHint" class="hint" style="color: var(--ok)">{{ savedHint }}</p>
      </template>
    </div>
  </div>
</template>
