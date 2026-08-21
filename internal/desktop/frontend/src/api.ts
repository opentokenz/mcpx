// 与 internal/desktop/api_windows.go 的 JSON 契约一一对应。
// 后端挂在 Wails AssetServer 的 /api 路由上，这里直接用相对路径 fetch。

export interface ServiceState {
  status: 'running' | 'starting' | 'conflict' | 'stopped'
  pid: number
  addr: string
  endpoint: string
  auth_mode: string
  executable: string
  home_dir: string
  log_path: string
  config_path: string
  error?: string
}

export interface WorkspaceItem {
  name: string
  path: string
  description: string
  missing: boolean
}

export interface ConnectionConfig {
  host: string
  port: number
  auth_mode: string
  token: string
  effective_mode: string
}

export interface LogChunk {
  content: string
  offset: number
  next_offset: number
  size: number
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(`/api${path}`, {
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
  const text = await response.text()
  const payload = text ? JSON.parse(text) : null
  if (!response.ok) {
    throw new Error(payload?.error ?? `请求失败（${response.status}）`)
  }
  return payload as T
}

export const api = {
  status: () => request<ServiceState>('/status'),

  serviceAction: (action: 'start' | 'stop' | 'restart') =>
    request<ServiceState>(`/service/${action}`, { method: 'POST' }),

  listWorkspaces: () => request<WorkspaceItem[]>('/workspaces'),

  addWorkspace: (path: string, description: string) =>
    request<WorkspaceItem[]>('/workspaces', {
      method: 'POST',
      body: JSON.stringify({ path, description }),
    }),

  patchWorkspace: (name: string, description: string) =>
    request<WorkspaceItem[]>(`/workspaces/${encodeURIComponent(name)}`, {
      method: 'PATCH',
      body: JSON.stringify({ description }),
    }),

  deleteWorkspace: (name: string) =>
    request<WorkspaceItem[]>(`/workspaces/${encodeURIComponent(name)}`, {
      method: 'DELETE',
    }),

  pickDirectory: () =>
    request<{ path: string }>('/pick-directory', { method: 'POST' }),

  readLogs: (offset: number) => request<LogChunk>(`/logs?offset=${offset}`),

  clearLogs: () => request<{ ok: boolean }>('/logs/clear', { method: 'POST' }),

  getConfig: () => request<ConnectionConfig>('/config'),

  putConfig: (config: ConnectionConfig) =>
    request<ConnectionConfig>('/config', {
      method: 'PUT',
      body: JSON.stringify(config),
    }),

  generateToken: () =>
    request<{ token: string }>('/config/token', { method: 'POST' }),

  open: (target: 'home' | 'log' | 'config') =>
    request<{ ok: boolean }>('/open', {
      method: 'POST',
      body: JSON.stringify({ target }),
    }),
}
