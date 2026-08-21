//go:build windows

package desktop

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"

	"mcpx/internal/config"
)

// maxLogChunk 限制单次日志读取的字节数。前端是 1s 轮询增量拉取，一次给太多
// 反而会让首屏卡住。
const maxLogChunk = 256 << 10

// api 是挂在 Wails AssetServer `/api` 路由上的处理器。前端全部通过普通 fetch
// 调用它，因此不需要 Wails 的绑定代码生成。
//
// 注意：AssetServer 在转发前会把 `/api` 前缀 TrimPrefix 掉，所以下面注册的
// 路由都不带 `/api`。
type api struct {
	mux *http.ServeMux

	// app 在 application.New 返回后回填。目录选择器等能力必须通过它调用。
	mu  sync.Mutex
	app *application.App
}

func newAPI() *api {
	a := &api{mux: http.NewServeMux()}

	a.mux.HandleFunc("GET /status", a.handleStatus)
	a.mux.HandleFunc("POST /service/{action}", a.handleServiceAction)

	a.mux.HandleFunc("GET /workspaces", a.handleListWorkspaces)
	a.mux.HandleFunc("POST /workspaces", a.handleAddWorkspace)
	a.mux.HandleFunc("PATCH /workspaces/{name}", a.handlePatchWorkspace)
	a.mux.HandleFunc("DELETE /workspaces/{name}", a.handleDeleteWorkspace)
	a.mux.HandleFunc("POST /pick-directory", a.handlePickDirectory)

	a.mux.HandleFunc("GET /logs", a.handleReadLogs)
	a.mux.HandleFunc("POST /logs/clear", a.handleClearLogs)

	a.mux.HandleFunc("GET /config", a.handleGetConfig)
	a.mux.HandleFunc("PUT /config", a.handlePutConfig)
	a.mux.HandleFunc("POST /config/token", a.handleGenerateToken)

	a.mux.HandleFunc("POST /open", a.handleOpenPath)
	return a
}

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mux.ServeHTTP(w, r)
}

func (a *api) setApp(app *application.App) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.app = app
}

func (a *api) application() *application.App {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.app
}

// --- 服务状态与控制 ---

func (a *api) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, currentState())
}

func (a *api) handleServiceAction(w http.ResponseWriter, r *http.Request) {
	var err error
	switch r.PathValue("action") {
	case "start":
		err = startService()
	case "stop":
		err = stopService()
	case "restart":
		err = restartService()
	default:
		writeError(w, http.StatusNotFound, "未知操作")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, currentState())
}

// --- Workspace ---

type workspaceItem struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Description string `json:"description"`
	// Missing 标记路径已不存在。注册记录本身仍然保留，由用户决定要不要删。
	Missing bool `json:"missing"`
}

func (a *api) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.LoadGlobal("")
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("读取配置失败：%v", err))
		return
	}
	items := make([]workspaceItem, 0, len(cfg.Workspaces))
	for _, entry := range cfg.Workspaces {
		path := config.ExpandHome(entry.Path)
		info, statErr := os.Stat(path)
		items = append(items, workspaceItem{
			Name:        entry.Name,
			Path:        entry.Path,
			Description: entry.Description,
			Missing:     statErr != nil || !info.IsDir(),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	writeJSON(w, http.StatusOK, items)
}

func (a *api) handleAddWorkspace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path        string `json:"path"`
		Description string `json:"description"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	path := strings.TrimSpace(body.Path)
	if path == "" {
		writeError(w, http.StatusBadRequest, "路径不能为空")
		return
	}
	absPath, err := filepath.Abs(config.ExpandHome(path))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("解析路径失败：%v", err))
		return
	}
	info, err := os.Stat(absPath)
	if err != nil || !info.IsDir() {
		writeError(w, http.StatusBadRequest, "目录不存在")
		return
	}

	if err := backupGlobalConfig(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 复用 CLI 的注册逻辑：它已经处理了同名/同路径去重。
	if err := config.RegisterWorkspace("", absPath); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("注册失败：%v", err))
		return
	}
	if description := strings.TrimSpace(body.Description); description != "" {
		if err := updateWorkspace(filepath.Base(absPath), func(entry *config.WorkspaceEntry) {
			entry.Description = description
		}); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	a.handleListWorkspaces(w, r)
}

func (a *api) handlePatchWorkspace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Description string `json:"description"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := backupGlobalConfig(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	err := updateWorkspace(r.PathValue("name"), func(entry *config.WorkspaceEntry) {
		entry.Description = strings.TrimSpace(body.Description)
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.handleListWorkspaces(w, r)
}

// handleDeleteWorkspace 只从 config.yaml 移除注册记录，绝不触碰磁盘上的项目目录。
func (a *api) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	configPath, cfg, err := loadGlobalForWrite()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	kept := make([]config.WorkspaceEntry, 0, len(cfg.Workspaces))
	found := false
	for _, entry := range cfg.Workspaces {
		if entry.Name == name {
			found = true
			continue
		}
		kept = append(kept, entry)
	}
	if !found {
		writeError(w, http.StatusNotFound, "Workspace 不存在")
		return
	}
	cfg.Workspaces = kept
	if err := backupGlobalConfig(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := config.WriteGlobal(configPath, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("写入配置失败：%v", err))
		return
	}
	a.handleListWorkspaces(w, r)
}

// handlePickDirectory 弹出系统目录选择器。用户取消时返回空路径而不是错误。
func (a *api) handlePickDirectory(w http.ResponseWriter, r *http.Request) {
	app := a.application()
	if app == nil {
		writeError(w, http.StatusServiceUnavailable, "窗口尚未就绪")
		return
	}
	path, err := app.Dialog.OpenFile().
		SetTitle("选择要注册的项目目录").
		CanChooseDirectories(true).
		CanChooseFiles(false).
		PromptForSingleSelection()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": path})
}

// --- 日志 ---

func (a *api) handleReadLogs(w http.ResponseWriter, r *http.Request) {
	state := currentState()
	if state.LogPath == "" {
		writeError(w, http.StatusInternalServerError, "无法定位日志路径")
		return
	}
	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)

	file, err := os.Open(state.LogPath)
	if err != nil {
		if os.IsNotExist(err) {
			// 服务还没启动过，日志文件尚不存在，这不是错误。
			writeJSON(w, http.StatusOK, map[string]any{
				"content": "", "offset": 0, "next_offset": 0, "size": 0,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	size := info.Size()
	// 日志被清空或轮转后，旧 offset 会越界，从头开始读。
	if offset > size || offset < 0 {
		offset = 0
	}
	if size-offset > maxLogChunk {
		offset = size - maxLogChunk
	}
	buf := make([]byte, size-offset)
	read, err := file.ReadAt(buf, offset)
	if err != nil && read == 0 {
		buf = buf[:0]
		read = 0
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"content":     string(buf[:read]),
		"offset":      offset,
		"next_offset": offset + int64(read),
		"size":        size,
	})
}

func (a *api) handleClearLogs(w http.ResponseWriter, r *http.Request) {
	state := currentState()
	if state.LogPath == "" {
		writeError(w, http.StatusInternalServerError, "无法定位日志路径")
		return
	}
	if err := os.Truncate(state.LogPath, 0); err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- 基础连接配置 ---

type connectionConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	AuthMode string `json:"auth_mode"`
	Token    string `json:"token"`
	// EffectiveMode 是 auth.mode 留空时服务端实际采用的模式，只读展示用。
	EffectiveMode string `json:"effective_mode"`
}

func (a *api) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.LoadGlobal("")
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("读取配置失败：%v", err))
		return
	}
	writeJSON(w, http.StatusOK, connectionConfig{
		Host:          cfg.Server.Host,
		Port:          cfg.Server.Port,
		AuthMode:      cfg.Auth.Mode,
		Token:         cfg.Auth.Token,
		EffectiveMode: config.EffectiveAuthMode(cfg.Auth),
	})
}

// handlePutConfig 只改监听地址与鉴权三项，其余配置（安全策略、保留策略等）
// 一律不碰，仍然由用户手改 config.yaml。
func (a *api) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var body connectionConfig
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Port <= 0 || body.Port > 65535 {
		writeError(w, http.StatusBadRequest, "端口必须在 1-65535 之间")
		return
	}
	switch body.AuthMode {
	case "", "open", "bearer", "oauth", "dual":
	default:
		writeError(w, http.StatusBadRequest, "鉴权模式必须是 open / bearer / oauth / dual")
		return
	}

	configPath, cfg, err := loadGlobalForWrite()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cfg.Server.Host = strings.TrimSpace(body.Host)
	cfg.Server.Port = body.Port
	cfg.Auth.Mode = body.AuthMode
	cfg.Auth.Token = strings.TrimSpace(body.Token)

	if err := config.ValidateAuthMode(cfg.Auth); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := backupGlobalConfig(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := config.WriteGlobal(configPath, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("写入配置失败：%v", err))
		return
	}
	a.handleGetConfig(w, r)
}

// handleGenerateToken 只生成候选 Token 返回给前端，不直接落盘。
// 用户在界面上确认后再走 PUT /config 写入。
func (a *api) handleGenerateToken(w http.ResponseWriter, r *http.Request) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": hex.EncodeToString(raw)})
}

// --- 杂项 ---

// handleOpenPath 在资源管理器/默认程序中打开运行时目录或日志文件。
func (a *api) handleOpenPath(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Target string `json:"target"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	app := a.application()
	if app == nil {
		writeError(w, http.StatusServiceUnavailable, "窗口尚未就绪")
		return
	}
	state := currentState()
	var path string
	switch body.Target {
	case "home":
		path = state.HomeDir
	case "log":
		path = state.LogPath
	case "config":
		path = state.ConfigPath
	default:
		writeError(w, http.StatusBadRequest, "未知目标")
		return
	}
	if path == "" {
		writeError(w, http.StatusInternalServerError, "路径不可用")
		return
	}
	if err := app.Browser.OpenFile(path); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- 共用辅助 ---

func loadGlobalForWrite() (string, config.Config, error) {
	configPath, err := config.GlobalConfigPath()
	if err != nil {
		return "", config.Config{}, err
	}
	cfg, err := config.LoadGlobal(configPath)
	if err != nil {
		return "", config.Config{}, fmt.Errorf("读取配置失败：%w", err)
	}
	return configPath, cfg, nil
}

// updateWorkspace 按名称就地修改一条 Workspace 记录后整体写回。
func updateWorkspace(name string, mutate func(*config.WorkspaceEntry)) error {
	configPath, cfg, err := loadGlobalForWrite()
	if err != nil {
		return err
	}
	for i := range cfg.Workspaces {
		if cfg.Workspaces[i].Name == name {
			mutate(&cfg.Workspaces[i])
			if err := config.WriteGlobal(configPath, cfg); err != nil {
				return fmt.Errorf("写入配置失败：%w", err)
			}
			return nil
		}
	}
	return fmt.Errorf("Workspace %q 不存在", name)
}

// backupGlobalConfig 在每次写入前备份一份 config.yaml.bak。
//
// config.WriteGlobal 是整份 yaml.Marshal，会抹掉用户在 config.yaml 里写的注释。
// 这是 `mcpx workspace register` 的既有行为，但 GUI 的写入频率高得多，所以留一份
// 备份让用户能把注释找回来。
func backupGlobalConfig() error {
	configPath, err := config.GlobalConfigPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("备份配置失败：%w", err)
	}
	if err := os.WriteFile(configPath+".bak", data, 0o600); err != nil {
		return fmt.Errorf("备份配置失败：%w", err)
	}
	return nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("请求格式错误：%v", err))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
