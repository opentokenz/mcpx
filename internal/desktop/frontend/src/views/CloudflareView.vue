<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref } from 'vue'
import {
  api,
  type CloudflareConfig,
  type CloudflareHealth,
  type CloudflareHealthItem,
  type CloudflareState,
} from '../api'

const state = ref<CloudflareState | null>(null)
const config = ref<CloudflareConfig | null>(null)
const health = ref<CloudflareHealth | null>(null)
const error = ref('')
const hint = ref('')
const busy = ref(false)
const installBusy = ref(false)
const copied = ref(false)

const rawLogs = ref('')
const followLogs = ref(true)
const logViewport = ref<HTMLElement | null>(null)
let logOffset = 0
let statusTimer: number | undefined
let logTimer: number | undefined

const statusText: Record<string, string> = {
  running: 'Tunnel 运行中',
  starting: 'Tunnel 启动中',
  stopped: 'Tunnel 已停止',
  error: 'Tunnel 状态异常',
}

const softwareText = computed(() => {
  if (!state.value?.software.installed) return '未安装'
  const source = state.value.software.managed ? 'MCPX 管理' : '系统安装'
  return `${source}${state.value.software.version ? ` · ${state.value.software.version}` : ''}`
})

const healthItems = computed<{ label: string; value: CloudflareHealthItem }[]>(() => {
  if (!health.value) return []
  return [
    { label: 'cloudflared', value: health.value.software },
    { label: '本地 MCP', value: health.value.local_mcp },
    { label: 'Tunnel 进程', value: health.value.tunnel_process },
    { label: '公网 MCP', value: health.value.public_mcp },
    { label: 'OAuth metadata', value: health.value.oauth_metadata },
  ]
})

async function refreshStatus() {
  try {
    state.value = await api.cloudflareStatus()
    if (!config.value) config.value = await api.getCloudflareConfig()
  } catch (err) {
    error.value = (err as Error).message
  }
}

async function saveConfig(showHint = true) {
  if (!config.value) return
  try {
    config.value = await api.putCloudflareConfig(config.value)
    if (showHint) {
      hint.value = 'Cloudflare 配置已保存。'
      window.setTimeout(() => (hint.value = ''), 3000)
    }
  } catch (err) {
    throw err
  }
}

async function install() {
  installBusy.value = true
  error.value = ''
  hint.value = '正在从 Cloudflare 官方 GitHub Release 安装 cloudflared…'
  try {
    await api.installCloudflared()
    hint.value = 'cloudflared 已安装到 MCPX 运行时目录。'
    await refreshStatus()
  } catch (err) {
    error.value = (err as Error).message
    hint.value = ''
  } finally {
    installBusy.value = false
  }
}

async function uninstall() {
  installBusy.value = true
  error.value = ''
  try {
    await api.uninstallCloudflared()
    hint.value = '已移除 MCPX 管理的 cloudflared。系统安装不会被删除。'
    await refreshStatus()
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    installBusy.value = false
  }
}

async function start() {
  busy.value = true
  error.value = ''
  try {
    await saveConfig(false)
    state.value = await api.startCloudflare()
    hint.value = 'Cloudflare Tunnel 已手动启动。'
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

async function stop() {
  busy.value = true
  error.value = ''
  try {
    state.value = await api.stopCloudflare()
    hint.value = 'Cloudflare Tunnel 已停止。'
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

async function runHealth() {
  error.value = ''
  try {
    health.value = await api.cloudflareHealth()
  } catch (err) {
    error.value = (err as Error).message
  }
}

async function copyPublicURL() {
  if (!state.value?.public_mcp_url) return
  await navigator.clipboard.writeText(state.value.public_mcp_url)
  copied.value = true
  window.setTimeout(() => (copied.value = false), 1500)
}

async function pollLogs() {
  try {
    const chunk = await api.readCloudflareLogs(logOffset)
    if (chunk.offset === 0 && logOffset !== 0) rawLogs.value = ''
    if (chunk.content) {
      rawLogs.value += chunk.content
      if (followLogs.value) {
        await nextTick()
        const element = logViewport.value
        if (element) element.scrollTop = element.scrollHeight
      }
    }
    logOffset = chunk.next_offset
  } catch (err) {
    error.value = (err as Error).message
  }
}

async function clearLogs() {
  try {
    await api.clearCloudflareLogs()
    rawLogs.value = ''
    logOffset = 0
  } catch (err) {
    error.value = (err as Error).message
  }
}

onMounted(async () => {
  await refreshStatus()
  await pollLogs()
  statusTimer = window.setInterval(refreshStatus, 2000)
  logTimer = window.setInterval(pollLogs, 1000)
})

onUnmounted(() => {
  window.clearInterval(statusTimer)
  window.clearInterval(logTimer)
})
</script>

<template>
  <div class="page">
    <div v-if="error" class="banner">{{ error }}</div>
    <div v-if="state?.error" class="banner">{{ state.error }}</div>

    <div class="card">
      <div class="row">
        <h2 class="card-title" style="margin: 0">cloudflared</h2>
        <span class="spacer"></span>
        <strong>{{ softwareText }}</strong>
      </div>
      <p class="hint mono-path">{{ state?.software.path || '尚未检测到 cloudflared' }}</p>
      <div class="row" style="margin-top: 12px">
        <button
          class="btn primary"
          :disabled="installBusy || (state?.status === 'running' && state?.software.managed)"
          @click="install"
        >
          {{ installBusy ? '处理中…' : state?.software.installed ? '重新安装 / 更新' : '安装 cloudflared' }}
        </button>
        <button
          class="btn danger"
          :disabled="installBusy || !state?.software.managed || state?.status === 'running'"
          @click="uninstall"
        >
          卸载受管版本
        </button>
        <button class="btn" @click="refreshStatus">重新检测</button>
      </div>
      <p class="hint">自动安装写入 <code>~/.mcpx/bin/cloudflared.exe</code>；若系统 PATH 已安装，也会直接识别。</p>
    </div>

    <div v-if="config" class="card">
      <div class="row">
        <h2 class="card-title" style="margin: 0">Tunnel 配置</h2>
        <span class="spacer"></span>
        <button class="btn" :disabled="busy" @click="saveConfig()">保存配置</button>
      </div>

      <div class="row" style="margin-top: 14px; align-items: flex-end">
        <label class="field">
          模式
          <select v-model="config.mode">
            <option value="quick">Quick（临时）</option>
            <option value="named">Named（Token）</option>
          </select>
        </label>
        <label class="checkbox" style="margin-bottom: 7px">
          <input v-model="config.manage_with_mcpx" type="checkbox" />
          随 MCPX 启停 Cloudflare Tunnel
        </label>
        <label class="checkbox" style="margin-bottom: 7px">
          <input v-model="config.sync_oauth_server_url" type="checkbox" />
          公网 Origin 自动联动 OAuth server_url
        </label>
        <label class="checkbox" style="margin-bottom: 7px">
          <input v-model="config.auto_recover" type="checkbox" />
          自动健康检查并故障恢复
        </label>
      </div>

      <template v-if="config.mode === 'named'">
        <div class="row" style="margin-top: 12px; align-items: flex-end">
          <label class="field" style="flex: 1">
            Tunnel Token
            <input v-model="config.tunnel_token" type="password" autocomplete="off" placeholder="eyJ…" />
          </label>
          <label class="field" style="width: 280px">
            Tunnel ID（可选，用于识别）
            <input v-model="config.tunnel_id" type="text" placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx" />
          </label>
        </div>
        <label class="field" style="margin-top: 12px">
          公网 Origin
          <input v-model="config.public_url" type="text" placeholder="https://mcp.example.com" />
        </label>
        <p class="hint">
          Named Tunnel 使用 Token 启动；请先在 Cloudflare 的 Published application 中把这个 hostname 路由到本机 MCPX 端口。
          Tunnel ID 不会替代公网 hostname。Token 通过环境变量传给 cloudflared，不出现在进程命令行。
        </p>
      </template>
      <p v-else class="hint">
        Quick 模式自动执行 <code>cloudflared tunnel --url http://127.0.0.1:&lt;port&gt;</code> 并解析
        <code>trycloudflare.com</code> 地址。它只适合开发测试，公网地址每次启动都会变化；Cloudflare 官方还明确说明
        Quick Tunnel 不支持 SSE，因此长期 Remote MCP / OAuth 接入请使用 Named Tunnel。
      </p>
      <p class="hint">
        公网启动前会拒绝 <code>auth.mode=open</code>，并自动启用 MCPX 的反向代理 Host 处理；开启联动时还会同步
        <code>auth.oauth.server_url</code>，必要时自动重启 MCPX 服务使配置立即生效。
      </p>
      <p class="hint">
        开启自动恢复后，Desktop 每 30 秒运行一次健康检查；连续 2 次出现本地 MCP、Tunnel、公网 MCP 或 5xx/网络类 OAuth metadata
        异常时，会自动重启本地 MCPX 与 Cloudflare Tunnel，并设置 2 分钟恢复冷却。手动停止 Tunnel 会解除自动恢复，重新启动后再次生效。
      </p>
    </div>

    <div v-if="state" class="card">
      <div class="row">
        <span class="status-dot" :class="state.status === 'error' ? 'conflict' : state.status"></span>
        <span class="status-headline">{{ statusText[state.status] }}</span>
        <span class="spacer"></span>
        <button
          class="btn primary"
          :disabled="busy || state.status === 'running' || state.status === 'starting'"
          @click="start"
        >
          启动公网 Tunnel
        </button>
        <button class="btn" :disabled="busy || state.status !== 'running'" @click="stop">停止公网 Tunnel</button>
        <button class="btn" :disabled="busy" @click="runHealth">健康检查</button>
      </div>

      <p class="hint">
        开启“随 MCPX 启停 Cloudflare Tunnel”时，服务页会继续把本地 Runtime 与 Tunnel 作为整套服务管理。
        关闭后，服务页的启动 / 停止 / 重启只管理本地 Runtime，Cloudflare Tunnel 请在这里手动启动或停止。
      </p>

      <dl class="facts" style="margin-top: 14px">
        <dt>本地 MCP</dt>
        <dd>{{ state.local_mcp_url || '—' }}</dd>
        <dt>公网 MCP</dt>
        <dd>{{ state.public_mcp_url || '—' }}</dd>
        <dt>OAuth server_url</dt>
        <dd>{{ state.oauth_server_url || '—' }}</dd>
        <dt>OAuth 联动</dt>
        <dd>{{ state.public_url ? (state.oauth_linked ? '一致' : '未同步') : '—' }}</dd>
        <dt>Tunnel</dt>
        <dd>{{ state.pid > 0 ? `pid=${state.pid}` : '—' }}{{ state.tunnel_id ? ` · ${state.tunnel_id}` : '' }}</dd>
      </dl>

      <div class="row" style="margin-top: 12px">
        <button class="btn" :disabled="!state.public_mcp_url" @click="copyPublicURL">
          {{ copied ? '已复制' : '复制公网 MCP URL' }}
        </button>
        <button class="btn" @click="api.open('cloudflare-log')">打开 Cloudflare 日志</button>
        <button class="btn" @click="api.open('cloudflare-config')">打开 Tunnel 配置</button>
      </div>
      <p v-if="hint" class="hint" style="color: var(--ok)">{{ hint }}</p>
    </div>

    <div v-if="health" class="card">
      <div class="row">
        <h2 class="card-title" style="margin: 0">健康检查</h2>
        <span class="spacer"></span>
        <strong :style="{ color: health.ok ? 'var(--ok)' : 'var(--warn)' }">
          {{ health.ok ? '全部通过' : '存在异常' }}
        </strong>
      </div>
      <p v-if="health.diagnosis" class="hint" style="margin-top: 10px">
        诊断：{{ health.diagnosis }}
      </p>
      <div class="health-grid">
        <div v-for="item in healthItems" :key="item.label" class="health-item">
          <div class="row">
            <span class="status-dot" :class="item.value.ok ? 'running' : 'conflict'"></span>
            <strong>{{ item.label }}</strong>
          </div>
          <div class="hint">{{ item.value.detail || '未检查' }}</div>
          <div v-if="item.value.url" class="hint mono-path">{{ item.value.url }}</div>
        </div>
      </div>
    </div>

    <div class="card">
      <div class="row">
        <h2 class="card-title" style="margin: 0">Cloudflare 日志</h2>
        <label class="checkbox">
          <input v-model="followLogs" type="checkbox" />
          自动滚动
        </label>
        <span class="spacer"></span>
        <button class="btn danger" @click="clearLogs">清空</button>
      </div>
      <pre ref="logViewport" class="log-view cloudflare-log">{{ rawLogs || '暂无 cloudflared 日志。' }}</pre>
    </div>
  </div>
</template>

<style scoped>
.mono-path {
  overflow-wrap: anywhere;
  font-family: ui-monospace, SFMono-Regular, Consolas, monospace;
}

.health-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
  gap: 10px;
  margin-top: 14px;
}

.health-item {
  padding: 12px;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--bg-input);
}

.cloudflare-log {
  height: 220px;
  margin-top: 12px;
}
</style>
