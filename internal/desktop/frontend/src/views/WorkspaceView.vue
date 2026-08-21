<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api, type WorkspaceItem } from '../api'

const items = ref<WorkspaceItem[]>([])
const error = ref('')
const busy = ref(false)
const loaded = ref(false)

// 正在编辑描述的行；null 表示没有行处于编辑态。
const editingName = ref<string | null>(null)
const editingText = ref('')

async function load() {
  try {
    items.value = await api.listWorkspaces()
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    loaded.value = true
  }
}

async function add() {
  error.value = ''
  busy.value = true
  try {
    const { path } = await api.pickDirectory()
    // 用户取消选择时返回空路径，什么都不做。
    if (path) {
      items.value = await api.addWorkspace(path, '')
    }
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

function startEdit(item: WorkspaceItem) {
  editingName.value = item.name
  editingText.value = item.description
}

async function commitEdit() {
  if (!editingName.value) return
  const name = editingName.value
  editingName.value = null
  busy.value = true
  try {
    items.value = await api.patchWorkspace(name, editingText.value)
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

async function remove(item: WorkspaceItem) {
  const ok = window.confirm(
    `确定要移除 Workspace「${item.name}」吗？\n\n` +
      `仅从 config.yaml 移除注册记录，不会删除磁盘上的任何文件。\n\n${item.path}`,
  )
  if (!ok) return
  busy.value = true
  error.value = ''
  try {
    items.value = await api.deleteWorkspace(item.name)
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    busy.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="page">
    <div v-if="error" class="banner">{{ error }}</div>

    <div class="row" style="margin-bottom: 14px">
      <button class="btn primary" :disabled="busy" @click="add">添加 Workspace…</button>
      <span class="spacer"></span>
      <button class="btn" :disabled="busy" @click="load">刷新</button>
    </div>

    <div class="card" style="padding: 0; overflow: hidden">
      <table v-if="items.length">
        <thead>
          <tr>
            <th style="width: 22%">名称</th>
            <th style="width: 40%">路径</th>
            <th>描述</th>
            <th style="width: 72px"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="item in items" :key="item.name">
            <td>
              {{ item.name }}
              <span v-if="item.missing" class="tag-missing">路径失效</span>
            </td>
            <td class="path">{{ item.path }}</td>
            <td>
              <input
                v-if="editingName === item.name"
                v-model="editingText"
                type="text"
                style="width: 100%"
                autofocus
                @blur="commitEdit"
                @keyup.enter="commitEdit"
              />
              <span v-else style="cursor: text" @click="startEdit(item)">
                {{ item.description || '—' }}
              </span>
            </td>
            <td>
              <button class="btn danger" :disabled="busy" @click="remove(item)">移除</button>
            </td>
          </tr>
        </tbody>
      </table>

      <div v-else-if="loaded" class="empty">还没有注册任何 Workspace</div>
      <div v-else class="empty">读取中…</div>
    </div>

    <p class="hint">
      点击描述单元格可直接编辑。「移除」只删除 config.yaml 里的注册记录，磁盘上的项目目录不受影响。<br />
      写入 config.yaml 前会自动备份为 config.yaml.bak；注意整体重写会丢失文件中的注释。
    </p>
  </div>
</template>
