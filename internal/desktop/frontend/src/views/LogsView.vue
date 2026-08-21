<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref } from 'vue'
import { api } from '../api'

const raw = ref('')
const filter = ref('')
const follow = ref(true)
const error = ref('')
const viewport = ref<HTMLElement | null>(null)

// offset 是下一次增量读取的起点（日志文件的字节偏移）。
let offset = 0
let timer: number | undefined

const lines = computed(() => {
  if (!filter.value) return raw.value
  const keyword = filter.value.toLowerCase()
  return raw.value
    .split('\n')
    .filter((line) => line.toLowerCase().includes(keyword))
    .join('\n')
})

async function poll() {
  try {
    const chunk = await api.readLogs(offset)
    // 后端在文件被清空或轮转时会把 offset 归零，这里同步丢掉旧内容，
    // 否则界面上会残留已经不存在的日志。
    if (chunk.offset === 0 && offset !== 0) {
      raw.value = ''
    }
    if (chunk.content) {
      raw.value += chunk.content
      if (follow.value) {
        await nextTick()
        scrollToEnd()
      }
    }
    offset = chunk.next_offset
    error.value = ''
  } catch (err) {
    error.value = (err as Error).message
  }
}

function scrollToEnd() {
  const element = viewport.value
  if (element) element.scrollTop = element.scrollHeight
}

async function clear() {
  try {
    await api.clearLogs()
    raw.value = ''
    offset = 0
  } catch (err) {
    error.value = (err as Error).message
  }
}

onMounted(() => {
  poll()
  timer = window.setInterval(poll, 1000)
})

onUnmounted(() => window.clearInterval(timer))
</script>

<template>
  <div class="page no-scroll">
    <div v-if="error" class="banner">{{ error }}</div>

    <div class="row">
      <input v-model="filter" type="text" placeholder="过滤关键字" style="width: 240px" />
      <label class="checkbox">
        <input v-model="follow" type="checkbox" />
        自动滚动到底部
      </label>
      <span class="spacer"></span>
      <button class="btn" @click="api.open('log')">在资源管理器中打开</button>
      <button class="btn danger" @click="clear">清空</button>
    </div>

    <pre ref="viewport" class="log-view">{{ lines || '暂无日志输出。服务以 -d 后台运行后，日志会写入 ~/.mcpx/logs/mcpx-daemon.log。' }}</pre>
  </div>
</template>
