<script setup lang="ts">
import { ref } from 'vue'
import ServiceView from './views/ServiceView.vue'
import WorkspaceView from './views/WorkspaceView.vue'
import LogsView from './views/LogsView.vue'
import { cycleTheme, THEME_LABEL, themeMode } from './theme'

type TabKey = 'service' | 'workspace' | 'logs'

const tabs: { key: TabKey; label: string }[] = [
  { key: 'service', label: '服务' },
  { key: 'workspace', label: 'Workspace' },
  { key: 'logs', label: '日志' },
]

const active = ref<TabKey>('service')
</script>

<template>
  <nav class="tabs">
    <button
      v-for="tab in tabs"
      :key="tab.key"
      :class="{ active: active === tab.key }"
      @click="active = tab.key"
    >
      {{ tab.label }}
    </button>

    <button
      class="theme-toggle"
      :title="`外观：${THEME_LABEL[themeMode]}（点击切换）`"
      @click="cycleTheme"
    >
      {{ THEME_LABEL[themeMode] }}
    </button>
  </nav>

  <ServiceView v-if="active === 'service'" />
  <WorkspaceView v-else-if="active === 'workspace'" />
  <LogsView v-else />
</template>
