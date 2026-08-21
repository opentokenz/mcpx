import { ref, watchEffect } from 'vue'

// system 表示跟随操作系统；light / dark 是用户手动覆盖。
export type ThemeMode = 'system' | 'light' | 'dark'

const STORAGE_KEY = 'mcpx.theme'
const CYCLE: ThemeMode[] = ['system', 'light', 'dark']

export const THEME_LABEL: Record<ThemeMode, string> = {
  system: '跟随系统',
  light: '浅色',
  dark: '深色',
}

function readStoredMode(): ThemeMode {
  const stored = localStorage.getItem(STORAGE_KEY)
  return stored === 'light' || stored === 'dark' || stored === 'system' ? stored : 'system'
}

const darkQuery = window.matchMedia('(prefers-color-scheme: dark)')

export const themeMode = ref<ThemeMode>(readStoredMode())

// 把 mode 解析成实际生效的主题并写到 <html data-theme>。CSS 只认这个属性，
// 不自己判断 prefers-color-scheme，否则手动覆盖会被系统偏好盖掉。
function apply() {
  const resolved =
    themeMode.value === 'system' ? (darkQuery.matches ? 'dark' : 'light') : themeMode.value
  document.documentElement.dataset.theme = resolved
}

// 跟随系统时，系统主题切换要实时反映到界面上。
darkQuery.addEventListener('change', apply)

watchEffect(() => {
  localStorage.setItem(STORAGE_KEY, themeMode.value)
  apply()
})

export function cycleTheme() {
  const next = (CYCLE.indexOf(themeMode.value) + 1) % CYCLE.length
  themeMode.value = CYCLE[next]
}
