<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { api, type ConnectionConfig, type ServiceState } from '../api'

const state = ref<ServiceState | null>(null)
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

// 受托盘/CLI 管理的进程才可以停止或重启。端口冲突时的占用者不归我们管，
// 也不该被我们停掉。
const managed = computed(
  () => state.value?.status === 'running' || state.value?.status === 'starting',
)

async function refresh() {
  try {
    state.value = await api.status()
  } catch (err) {
    error.value = (err as Error).message
  }
}

async function act(action: 'start' | 'stop' | 'restart') {
  busy.value = true
  error.value = ''
  try {
    state.value = await api.serviceAction(action)
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    busy.value = false
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

// 保存后不自动重启：正在跑的服务用的还是旧配置，由用户决定何时重启。
async function saveConfig() {
  if (!config.value) return
  busy.value = true
  error.value = ''
  try {
    config.value = await api.putConfig(config.value)
    savedHint.value = '已保存。改动在下次重启服务后生效。'
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
        <button class="btn primary" :disabled="busy || state.status !== 'stopped'" @click="act('start')">
          启动
        </button>
        <button class="btn" :disabled="busy || !managed" @click="act('stop')">
          停止
        </button>
        <button class="btn" :disabled="busy || !managed" @click="act('restart')">
          重启
        </button>
      </div>

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

        <p class="hint">
          只涉及监听地址与鉴权三项；安全策略、保留策略等仍需手改 config.yaml。<br />
          保存会整体重写 config.yaml 并<strong>丢失其中的注释</strong>，写入前会自动备份为
          config.yaml.bak。
        </p>
        <p v-if="savedHint" class="hint" style="color: var(--ok)">{{ savedHint }}</p>
      </template>
    </div>
  </div>
</template>
