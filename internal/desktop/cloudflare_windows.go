//go:build windows

package desktop

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"mcpx/internal/config"
)

const (
	cloudflareConfigFilename = "cloudflare-desktop.json"
	cloudflareStateFilename  = "cloudflare-desktop-state.json"
	cloudflareLogFilename    = "cloudflared.log"
	cloudflareBinaryFilename = "cloudflared.exe"

	cloudflareStatusRunning  = "running"
	cloudflareStatusStarting = "starting"
	cloudflareStatusStopped  = "stopped"
	cloudflareStatusError    = "error"

	cloudflareQuickReadyTimeout        = 30 * time.Second
	cloudflareHTTPTimeout              = 8 * time.Second
	cloudflareDownloadTimeout          = 3 * time.Minute
	cloudflareMaxDownloadBytes         = int64(200 << 20)
	cloudflareWatchdogCheckInterval    = 30 * time.Second
	cloudflareWatchdogFailureThreshold = 2
	cloudflareWatchdogRecoveryCooldown = 2 * time.Minute
)

var tryCloudflareURLPattern = regexp.MustCompile(`https://[a-zA-Z0-9-]+\.trycloudflare\.com`)

var cloudflaredVersionMemo struct {
	sync.Mutex
	path    string
	modTime time.Time
	size    int64
	version string
}

// cloudflareDesktopConfig is intentionally stored outside config.yaml. Tunnel
// lifecycle is a Desktop concern; keeping it separate prevents desktop-only
// fields from becoming part of the MCP Runtime configuration contract.
type cloudflareDesktopConfig struct {
	Mode               string `json:"mode"`
	TunnelToken        string `json:"tunnel_token"`
	TunnelID           string `json:"tunnel_id"`
	PublicURL          string `json:"public_url"`
	LastPublicURL      string `json:"last_public_url"`
	ManageWithMCPX     bool   `json:"manage_with_mcpx"`
	SyncOAuthServerURL bool   `json:"sync_oauth_server_url"`
	AutoRecover        bool   `json:"auto_recover"`
	DesiredRunning     bool   `json:"desired_running"`
}

type cloudflareProcessState struct {
	PID        int    `json:"pid"`
	Executable string `json:"executable"`
	Mode       string `json:"mode"`
	TunnelID   string `json:"tunnel_id"`
	PublicURL  string `json:"public_url"`
}

type cloudflareSoftwareStatus struct {
	Installed bool   `json:"installed"`
	Managed   bool   `json:"managed"`
	Path      string `json:"path"`
	Version   string `json:"version"`
}

type cloudflareState struct {
	Status         string                   `json:"status"`
	PID            int                      `json:"pid"`
	Mode           string                   `json:"mode"`
	TunnelID       string                   `json:"tunnel_id"`
	PublicURL      string                   `json:"public_url"`
	PublicMCPURL   string                   `json:"public_mcp_url"`
	LocalMCPURL    string                   `json:"local_mcp_url"`
	OAuthServerURL string                   `json:"oauth_server_url"`
	OAuthLinked    bool                     `json:"oauth_linked"`
	LogPath        string                   `json:"log_path"`
	ConfigPath     string                   `json:"config_path"`
	Executable     string                   `json:"executable"`
	Software       cloudflareSoftwareStatus `json:"software"`
	Error          string                   `json:"error,omitempty"`
}

type cloudflareHealthItem struct {
	OK         bool   `json:"ok"`
	Detail     string `json:"detail"`
	URL        string `json:"url,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
}

type cloudflareHealth struct {
	OK            bool                 `json:"ok"`
	CheckedAt     string               `json:"checked_at"`
	Software      cloudflareHealthItem `json:"software"`
	LocalMCP      cloudflareHealthItem `json:"local_mcp"`
	TunnelProcess cloudflareHealthItem `json:"tunnel_process"`
	PublicMCP     cloudflareHealthItem `json:"public_mcp"`
	OAuthMetadata cloudflareHealthItem `json:"oauth_metadata"`
	OAuthLinked   bool                 `json:"oauth_linked"`
	Diagnosis     string               `json:"diagnosis,omitempty"`
}

func defaultCloudflareDesktopConfig() cloudflareDesktopConfig {
	return cloudflareDesktopConfig{
		Mode:               "quick",
		ManageWithMCPX:     false,
		SyncOAuthServerURL: true,
		AutoRecover:        true,
	}
}

func cloudflarePaths() (home, configPath, statePath, logPath, managedBinary string, err error) {
	home, err = config.HomeDir()
	if err != nil {
		return "", "", "", "", "", err
	}
	configPath = filepath.Join(home, cloudflareConfigFilename)
	statePath = filepath.Join(home, cloudflareStateFilename)
	logPath = filepath.Join(home, "logs", cloudflareLogFilename)
	managedBinary = filepath.Join(home, "bin", cloudflareBinaryFilename)
	return
}

func loadCloudflareDesktopConfig() (cloudflareDesktopConfig, error) {
	cfg := defaultCloudflareDesktopConfig()
	_, path, _, _, _, err := cloudflarePaths()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("读取 Cloudflare Desktop 配置失败：%w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("解析 %s 失败：%w", path, err)
	}
	if cfg.Mode == "" {
		cfg.Mode = "quick"
	}
	return cfg, nil
}

func setCloudflareDesiredRunning(desired bool) error {
	cfg, err := loadCloudflareDesktopConfig()
	if err != nil {
		return err
	}
	if cfg.DesiredRunning == desired {
		return nil
	}
	cfg.DesiredRunning = desired
	_, err = saveCloudflareDesktopConfig(cfg)
	return err
}

func saveCloudflareDesktopConfig(cfg cloudflareDesktopConfig) (cloudflareDesktopConfig, error) {
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	if cfg.Mode == "" {
		cfg.Mode = "quick"
	}
	if cfg.Mode != "quick" && cfg.Mode != "named" {
		return cfg, fmt.Errorf("Cloudflare 模式必须是 quick 或 named")
	}
	cfg.TunnelToken = strings.TrimSpace(cfg.TunnelToken)
	cfg.TunnelID = strings.TrimSpace(cfg.TunnelID)
	cfg.LastPublicURL = strings.TrimRight(strings.TrimSpace(cfg.LastPublicURL), "/")
	if strings.TrimSpace(cfg.PublicURL) != "" {
		origin, err := normalizePublicOrigin(cfg.PublicURL)
		if err != nil {
			return cfg, err
		}
		cfg.PublicURL = origin
	} else {
		cfg.PublicURL = ""
	}

	home, path, _, _, _, err := cloudflarePaths()
	if err != nil {
		return cfg, err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return cfg, err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return cfg, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return cfg, fmt.Errorf("写入 Cloudflare Desktop 配置失败：%w", err)
	}
	return cfg, nil
}

func readCloudflareProcessState() (cloudflareProcessState, error) {
	var state cloudflareProcessState
	_, _, path, _, _, err := cloudflarePaths()
	if err != nil {
		return state, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("解析 Cloudflare 进程状态失败：%w", err)
	}
	return state, nil
}

func writeCloudflareProcessState(state cloudflareProcessState) error {
	home, _, path, _, _, err := cloudflarePaths()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func clearCloudflareProcessStateIfPID(pid int) {
	state, err := readCloudflareProcessState()
	if err != nil || state.PID != pid {
		return
	}
	_, _, path, _, _, err := cloudflarePaths()
	if err == nil {
		_ = os.Remove(path)
	}
}

func resolveCloudflared() (path string, managed bool) {
	_, _, _, _, managedPath, err := cloudflarePaths()
	if err == nil {
		if info, statErr := os.Stat(managedPath); statErr == nil && !info.IsDir() {
			return managedPath, true
		}
	}
	if found, lookErr := exec.LookPath("cloudflared"); lookErr == nil {
		return found, false
	}

	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles"), "cloudflared", cloudflareBinaryFilename),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "cloudflared", cloudflareBinaryFilename),
		filepath.Join(os.Getenv("ProgramFiles"), "Cloudflare", "Cloudflare Tunnel", cloudflareBinaryFilename),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "WinGet", "Links", cloudflareBinaryFilename),
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		candidates = append(candidates, filepath.Join(home, ".cloudflared", cloudflareBinaryFilename))
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, false
		}
	}
	return "", false
}

func cloudflaredVersion(path string) string {
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	cloudflaredVersionMemo.Lock()
	if cloudflaredVersionMemo.path == path &&
		cloudflaredVersionMemo.modTime.Equal(info.ModTime()) &&
		cloudflaredVersionMemo.size == info.Size() {
		version := cloudflaredVersionMemo.version
		cloudflaredVersionMemo.Unlock()
		return version
	}
	cloudflaredVersionMemo.Unlock()

	command := exec.Command(path, "--version")
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	output, err := command.CombinedOutput()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(output))
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	cloudflaredVersionMemo.Lock()
	cloudflaredVersionMemo.path = path
	cloudflaredVersionMemo.modTime = info.ModTime()
	cloudflaredVersionMemo.size = info.Size()
	cloudflaredVersionMemo.version = line
	cloudflaredVersionMemo.Unlock()
	return line
}

func currentCloudflareSoftwareStatus() cloudflareSoftwareStatus {
	path, managed := resolveCloudflared()
	return cloudflareSoftwareStatus{
		Installed: path != "",
		Managed:   managed,
		Path:      path,
		Version:   cloudflaredVersion(path),
	}
}

func cloudflaredDownloadURL() (string, error) {
	var asset string
	switch runtime.GOARCH {
	case "amd64":
		asset = "cloudflared-windows-amd64.exe"
	case "arm64":
		asset = "cloudflared-windows-arm64.exe"
	default:
		return "", fmt.Errorf("当前 Windows 架构 %s 暂不支持 Desktop 自动安装 cloudflared", runtime.GOARCH)
	}
	return "https://github.com/cloudflare/cloudflared/releases/latest/download/" + asset, nil
}

func installCloudflared() (cloudflareSoftwareStatus, error) {
	current := currentCloudflareState()
	if current.Status == cloudflareStatusRunning && current.Software.Managed {
		return current.Software, fmt.Errorf("请先停止使用 MCPX 受管 cloudflared 的 Tunnel，再更新 cloudflared")
	}
	downloadURL, err := cloudflaredDownloadURL()
	if err != nil {
		return cloudflareSoftwareStatus{}, err
	}
	_, _, _, _, destination, err := cloudflarePaths()
	if err != nil {
		return cloudflareSoftwareStatus{}, err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return cloudflareSoftwareStatus{}, err
	}

	client := &http.Client{Timeout: cloudflareDownloadTimeout}
	req, err := http.NewRequest(http.MethodGet, downloadURL, nil)
	if err != nil {
		return cloudflareSoftwareStatus{}, err
	}
	req.Header.Set("User-Agent", "MCPX Desktop cloudflared installer")
	resp, err := client.Do(req)
	if err != nil {
		return cloudflareSoftwareStatus{}, fmt.Errorf("下载 cloudflared 失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return cloudflareSoftwareStatus{}, fmt.Errorf("下载 cloudflared 失败：HTTP %s", resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(destination), "cloudflared-*.exe")
	if err != nil {
		return cloudflareSoftwareStatus{}, err
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	defer cleanup()

	written, err := io.Copy(tmp, io.LimitReader(resp.Body, cloudflareMaxDownloadBytes+1))
	if err != nil {
		return cloudflareSoftwareStatus{}, fmt.Errorf("保存 cloudflared 失败：%w", err)
	}
	if written > cloudflareMaxDownloadBytes {
		return cloudflareSoftwareStatus{}, fmt.Errorf("cloudflared 下载文件异常过大")
	}
	if err := tmp.Close(); err != nil {
		return cloudflareSoftwareStatus{}, err
	}
	_ = os.Remove(destination)
	if err := os.Rename(tmpPath, destination); err != nil {
		return cloudflareSoftwareStatus{}, fmt.Errorf("安装 cloudflared 失败：%w", err)
	}

	status := currentCloudflareSoftwareStatus()
	if !status.Installed || status.Path == "" {
		return status, fmt.Errorf("cloudflared 已下载，但安装验证失败")
	}
	return status, nil
}

func uninstallManagedCloudflared() (cloudflareSoftwareStatus, error) {
	state := currentCloudflareState()
	if state.Status == cloudflareStatusRunning || state.Status == cloudflareStatusStarting {
		return state.Software, fmt.Errorf("请先停止 Cloudflare Tunnel，再卸载 cloudflared")
	}
	_, _, _, _, managedPath, err := cloudflarePaths()
	if err != nil {
		return cloudflareSoftwareStatus{}, err
	}
	if err := os.Remove(managedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return currentCloudflareSoftwareStatus(), fmt.Errorf("删除受管 cloudflared 失败：%w", err)
	}
	return currentCloudflareSoftwareStatus(), nil
}

func normalizePublicOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("公网地址必须是完整 HTTPS Origin，例如 https://mcp.example.com")
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return "", fmt.Errorf("Cloudflare 公网地址必须使用 https://")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("公网地址只填写 Origin，不要追加 /mcp、查询参数或片段")
	}
	return "https://" + parsed.Host, nil
}

func publicMCPURL(origin string) string {
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if origin == "" {
		return ""
	}
	return origin + "/mcp"
}

func currentCloudflareState() cloudflareState {
	result := cloudflareState{
		Status:   cloudflareStatusStopped,
		Software: currentCloudflareSoftwareStatus(),
	}
	home, configPath, _, logPath, _, err := cloudflarePaths()
	if err != nil {
		result.Status = cloudflareStatusError
		result.Error = err.Error()
		return result
	}
	_ = home
	result.ConfigPath = configPath
	result.LogPath = logPath

	desktopCfg, cfgErr := loadCloudflareDesktopConfig()
	if cfgErr != nil {
		result.Error = cfgErr.Error()
	}
	result.Mode = desktopCfg.Mode
	result.TunnelID = desktopCfg.TunnelID
	if desktopCfg.Mode == "named" {
		result.PublicURL = desktopCfg.PublicURL
	}

	runtimeCfg, runtimeErr := config.LoadGlobal("")
	if runtimeErr == nil {
		host := strings.TrimSpace(runtimeCfg.Server.Host)
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		port := runtimeCfg.Server.Port
		if port == 0 {
			port = 9090
		}
		result.LocalMCPURL = fmt.Sprintf("http://%s/mcp", net.JoinHostPort(host, strconv.Itoa(port)))
		result.OAuthServerURL = strings.TrimRight(runtimeCfg.Auth.OAuth.ServerURL, "/")
	}

	processState, processErr := readCloudflareProcessState()
	if processErr == nil && processState.PID > 0 {
		alive, matches := processAlive(processState.PID, processState.Executable)
		if alive && matches {
			result.Status = cloudflareStatusRunning
			result.PID = processState.PID
			result.Executable = processState.Executable
			result.Mode = processState.Mode
			if processState.TunnelID != "" {
				result.TunnelID = processState.TunnelID
			}
			if processState.PublicURL != "" {
				result.PublicURL = processState.PublicURL
			}
		} else {
			clearCloudflareProcessStateIfPID(processState.PID)
		}
	}

	result.PublicMCPURL = publicMCPURL(result.PublicURL)
	result.OAuthLinked = result.PublicURL != "" && strings.EqualFold(result.OAuthServerURL, strings.TrimRight(result.PublicURL, "/"))
	return result
}

func ensureCloudflareRuntimeConfig(publicOrigin string, syncOAuth bool) (bool, error) {
	configPath, cfg, err := loadGlobalForWrite()
	if err != nil {
		return false, err
	}
	changed := false
	if !cfg.Server.DisableLocalhostProtection {
		cfg.Server.DisableLocalhostProtection = true
		changed = true
	}
	if !cfg.Server.TrustProxyHeaders {
		cfg.Server.TrustProxyHeaders = true
		changed = true
	}
	if syncOAuth && publicOrigin != "" {
		origin, err := normalizePublicOrigin(publicOrigin)
		if err != nil {
			return false, err
		}
		if strings.TrimRight(cfg.Auth.OAuth.ServerURL, "/") != origin {
			cfg.Auth.OAuth.ServerURL = origin
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	if err := backupGlobalConfig(); err != nil {
		return false, err
	}
	if err := config.WriteGlobal(configPath, cfg); err != nil {
		return false, fmt.Errorf("写入 Cloudflare 联动配置失败：%w", err)
	}
	return true, nil
}

func ensureMCPServiceForCloudflare() error {
	state := currentState()
	switch state.Status {
	case statusStopped:
		if err := startService(); err != nil {
			return fmt.Errorf("启动 MCPX 服务失败：%w", err)
		}
	case statusConflict:
		return fmt.Errorf("无法启动 Tunnel：MCPX 端口 %s 被未受管进程占用", state.Addr)
	}
	state = currentState()
	if state.Status != statusRunning {
		return fmt.Errorf("MCPX 服务尚未就绪（当前状态 %s）", state.Status)
	}
	return nil
}

// startMCPXStack is the single Desktop lifecycle entrypoint. Cloudflare can
// either follow the MCPX lifecycle or be managed manually from the Cloudflare
// page, depending on the Desktop tunnel configuration.
func startMCPXStack() error {
	desktopCfg, err := loadCloudflareDesktopConfig()
	if err != nil {
		return err
	}
	if !desktopCfg.ManageWithMCPX {
		return startService()
	}
	cloudflare := currentCloudflareState()
	if cloudflare.Status == cloudflareStatusRunning || cloudflare.Status == cloudflareStatusStarting {
		return ensureMCPServiceForCloudflare()
	}
	_, err = startCloudflareTunnel()
	return err
}

// stopMCPXStack follows the configured lifecycle policy. In linked mode it
// removes the public edge first; in manual mode it leaves the Tunnel alone.
func stopMCPXStack() error {
	desktopCfg, err := loadCloudflareDesktopConfig()
	if err != nil {
		return err
	}
	if !desktopCfg.ManageWithMCPX {
		return stopService()
	}

	var errs []error
	if err := stopCloudflareTunnel(); err != nil {
		errs = append(errs, fmt.Errorf("停止 Cloudflare Tunnel 失败：%w", err))
	}
	if err := stopService(); err != nil {
		errs = append(errs, fmt.Errorf("停止 MCPX 服务失败：%w", err))
	}
	return errors.Join(errs...)
}

func restartMCPXStack() error {
	desktopCfg, err := loadCloudflareDesktopConfig()
	if err != nil {
		return err
	}
	if !desktopCfg.ManageWithMCPX {
		return restartService()
	}
	if err := stopMCPXStack(); err != nil {
		return err
	}
	return startMCPXStack()
}

func validateCloudflareStartConfig(desktopCfg cloudflareDesktopConfig, runtimeCfg config.Config) error {
	if config.EffectiveAuthMode(runtimeCfg.Auth) == "open" {
		return fmt.Errorf("拒绝把 auth.mode=open 的 MCPX 暴露到公网；请先在“服务”页配置 bearer、oauth 或 dual")
	}
	if desktopCfg.Mode == "named" {
		if strings.TrimSpace(desktopCfg.TunnelToken) == "" {
			return fmt.Errorf("Named Tunnel 需要填写 Tunnel Token")
		}
		if strings.TrimSpace(desktopCfg.PublicURL) == "" {
			return fmt.Errorf("Named Tunnel 需要填写已经在 Cloudflare 配置好的公网 Origin")
		}
		if _, err := normalizePublicOrigin(desktopCfg.PublicURL); err != nil {
			return err
		}
	}
	return nil
}

func startCloudflareTunnel() (cloudflareState, error) {
	current := currentCloudflareState()
	if current.Status == cloudflareStatusRunning || current.Status == cloudflareStatusStarting {
		if err := setCloudflareDesiredRunning(true); err != nil {
			return current, err
		}
		return current, nil
	}

	desktopCfg, err := loadCloudflareDesktopConfig()
	if err != nil {
		return current, err
	}
	runtimeCfg, err := config.LoadGlobal("")
	if err != nil {
		return current, fmt.Errorf("读取 MCPX 配置失败：%w", err)
	}
	if err := validateCloudflareStartConfig(desktopCfg, runtimeCfg); err != nil {
		return current, err
	}
	if err := ensureMCPServiceForCloudflare(); err != nil {
		return current, err
	}

	// cloudflared brings requests in with the public Host. Enable the existing
	// reverse-proxy settings before exposing the port. For named tunnels the
	// public origin is already known, so OAuth can be pinned before launch.
	preOrigin := ""
	if desktopCfg.Mode == "named" {
		preOrigin = desktopCfg.PublicURL
	}
	changed, err := ensureCloudflareRuntimeConfig(preOrigin, desktopCfg.SyncOAuthServerURL)
	if err != nil {
		return current, err
	}
	if changed {
		if err := restartService(); err != nil {
			return current, fmt.Errorf("Cloudflare 联动配置已写入，但重启 MCPX 服务失败：%w", err)
		}
	}

	software := currentCloudflareSoftwareStatus()
	if !software.Installed {
		// Desktop 的“公网 MCP”入口应当是一键启动。首次使用时没有
		// cloudflared 就自动安装 MCPX 受管版本，避免用户必须先理解安装、
		// 本地服务、Tunnel 三个独立步骤。
		software, err = installCloudflared()
		if err != nil {
			return current, fmt.Errorf("自动安装 cloudflared 失败：%w", err)
		}
	}
	executable := software.Path
	if executable == "" {
		return current, fmt.Errorf("cloudflared 已检测/安装，但无法定位可执行文件")
	}
	localHost := strings.TrimSpace(runtimeCfg.Server.Host)
	if localHost == "" || localHost == "0.0.0.0" || localHost == "::" {
		localHost = "127.0.0.1"
	}
	localPort := runtimeCfg.Server.Port
	if localPort == 0 {
		localPort = 9090
	}
	localOrigin := "http://" + net.JoinHostPort(localHost, strconv.Itoa(localPort))
	var args []string
	token := ""
	if desktopCfg.Mode == "named" {
		args = []string{"tunnel", "--no-autoupdate", "--loglevel", "info", "--protocol", "http2", "run"}
		// Do not put the token in argv. cloudflared supports TUNNEL_TOKEN and this
		// keeps the credential out of process listings and Desktop logs.
		token = desktopCfg.TunnelToken
	} else {
		args = []string{"tunnel", "--protocol", "http2", "--url", localOrigin}
	}

	cmd, lines, err := spawnCloudflared(executable, args, token, cloudflareProcessState{
		Mode:      desktopCfg.Mode,
		TunnelID:  desktopCfg.TunnelID,
		PublicURL: desktopCfg.PublicURL,
	})
	if err != nil {
		return currentCloudflareState(), err
	}

	publicOrigin := desktopCfg.PublicURL
	if desktopCfg.Mode == "quick" {
		publicOrigin, err = waitForTryCloudflareURL(cmd, lines, cloudflareQuickReadyTimeout)
		if err != nil {
			_ = stopCloudflareTunnel()
			return currentCloudflareState(), err
		}
		desktopCfg.LastPublicURL = publicOrigin
		if _, saveErr := saveCloudflareDesktopConfig(desktopCfg); saveErr != nil {
			_ = stopCloudflareTunnel()
			return currentCloudflareState(), saveErr
		}
		processState, readErr := readCloudflareProcessState()
		if readErr == nil && processState.PID == cmd.Process.Pid {
			processState.PublicURL = publicOrigin
			_ = writeCloudflareProcessState(processState)
		}

		changed, err = ensureCloudflareRuntimeConfig(publicOrigin, desktopCfg.SyncOAuthServerURL)
		if err != nil {
			_ = stopCloudflareTunnel()
			return currentCloudflareState(), err
		}
		if changed {
			if err := restartService(); err != nil {
				_ = stopCloudflareTunnel()
				return currentCloudflareState(), fmt.Errorf("已获得 Quick Tunnel 地址，但 OAuth 联动后重启 MCPX 失败：%w", err)
			}
		}
	}

	if err := setCloudflareDesiredRunning(true); err != nil {
		return currentCloudflareState(), err
	}
	return currentCloudflareState(), nil
}

func spawnCloudflared(executable string, args []string, token string, processState cloudflareProcessState) (*exec.Cmd, <-chan string, error) {
	_, _, _, logPath, _, err := cloudflarePaths()
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, err
	}

	command := exec.Command(executable, args...)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if token != "" {
		command.Env = append(os.Environ(), "TUNNEL_TOKEN="+token)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = logFile.Close()
		return nil, nil, err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		_ = logFile.Close()
		return nil, nil, err
	}
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		return nil, nil, fmt.Errorf("启动 cloudflared 失败：%w", err)
	}

	processState.PID = command.Process.Pid
	processState.Executable = executable
	if err := writeCloudflareProcessState(processState); err != nil {
		_ = command.Process.Kill()
		_ = logFile.Close()
		return nil, nil, err
	}

	lines := make(chan string, 256)
	var writeMu sync.Mutex
	var scanWG sync.WaitGroup
	scanWG.Add(2)
	scan := func(source string, reader io.Reader) {
		defer scanWG.Done()
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			writeMu.Lock()
			_, _ = fmt.Fprintf(logFile, "%s [%s] %s\n", time.Now().Format(time.RFC3339), source, line)
			writeMu.Unlock()
			select {
			case lines <- line:
			default:
			}
		}
	}
	go scan("stdout", stdout)
	go scan("stderr", stderr)
	go func(pid int) {
		scanWG.Wait()
		close(lines)
		err := command.Wait()
		writeMu.Lock()
		if err != nil {
			_, _ = fmt.Fprintf(logFile, "%s [desktop] cloudflared exited: %v\n", time.Now().Format(time.RFC3339), err)
		} else {
			_, _ = fmt.Fprintf(logFile, "%s [desktop] cloudflared exited\n", time.Now().Format(time.RFC3339))
		}
		_ = logFile.Close()
		writeMu.Unlock()
		clearCloudflareProcessStateIfPID(pid)
	}(command.Process.Pid)

	return command, lines, nil
}

func waitForTryCloudflareURL(command *exec.Cmd, lines <-chan string, timeout time.Duration) (string, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return "", fmt.Errorf("cloudflared 在返回 Quick Tunnel 公网地址前退出；请查看 Cloudflare 日志")
			}
			if match := tryCloudflareURLPattern.FindString(line); match != "" {
				return strings.TrimRight(match, "/"), nil
			}
		case <-timer.C:
			if command.Process != nil {
				return "", fmt.Errorf("cloudflared 已启动，但 %s 内没有返回 trycloudflare.com 地址；请查看 Cloudflare 日志", timeout)
			}
			return "", fmt.Errorf("cloudflared Quick Tunnel 启动超时")
		}
	}
}

func stopCloudflareTunnel() error {
	return stopCloudflareTunnelWithDesired(false)
}

func stopCloudflareTunnelForRecovery() error {
	return stopCloudflareTunnelWithDesired(true)
}

func stopCloudflareTunnelWithDesired(desired bool) error {
	desiredErr := setCloudflareDesiredRunning(desired)
	processErr := stopCloudflareTunnelProcess()
	// 再写一次最终意图，关闭 watchdog 恰好在进程退出期间观察到旧进程并
	// 重新接管 DesiredRunning 的竞态窗口。
	finalDesiredErr := setCloudflareDesiredRunning(desired)
	return errors.Join(desiredErr, processErr, finalDesiredErr)
}

func stopCloudflareTunnelProcess() error {
	state, err := readCloudflareProcessState()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if state.PID <= 0 {
		clearCloudflareProcessStateIfPID(state.PID)
		return nil
	}
	alive, matches := processAlive(state.PID, state.Executable)
	if !alive {
		clearCloudflareProcessStateIfPID(state.PID)
		return nil
	}
	if !matches {
		// The state file is stale and Windows has reused the PID. Never kill the
		// replacement process; just discard our stale tracking record.
		clearCloudflareProcessStateIfPID(state.PID)
		return nil
	}

	gracefulOutput, gracefulErr := runDesktopTaskkill(state.PID, false)
	if waitDesktopProcessGone(state.PID, state.Executable, 750*time.Millisecond) {
		clearCloudflareProcessStateIfPID(state.PID)
		return nil
	}

	// taskkill /T can report success while a detached cloudflared process is
	// still alive. Escalate based on observed process state, not command exit.
	forceOutput, forceErr := runDesktopTaskkill(state.PID, true)
	if waitDesktopProcessGone(state.PID, state.Executable, 5*time.Second) {
		clearCloudflareProcessStateIfPID(state.PID)
		return nil
	}
	if forceErr != nil {
		return fmt.Errorf("停止 cloudflared PID %d 失败：强制停止 %v（%s）；普通停止 %v（%s）",
			state.PID, forceErr, forceOutput, gracefulErr, gracefulOutput)
	}
	return fmt.Errorf("cloudflared PID %d 在 taskkill /T /F 后仍然存活", state.PID)
}

func runDesktopTaskkill(pid int, force bool) (string, error) {
	args := []string{"/PID", strconv.Itoa(pid), "/T"}
	if force {
		args = append(args, "/F")
	}
	command := exec.Command("taskkill", args...)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func waitDesktopProcessGone(pid int, executable string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		alive, matches := processAlive(pid, executable)
		if !alive || !matches {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func restartCloudflareAndMCP() error {
	if err := stopCloudflareTunnelForRecovery(); err != nil {
		return fmt.Errorf("停止 Cloudflare Tunnel 失败：%w", err)
	}
	if err := restartService(); err != nil {
		return fmt.Errorf("重启 MCPX 服务失败：%w", err)
	}
	if _, err := startCloudflareTunnel(); err != nil {
		return fmt.Errorf("重启 Cloudflare Tunnel 失败：%w", err)
	}
	return nil
}

func cloudflareHealthNeedsAutoRecovery(health cloudflareHealth) bool {
	if health.OK {
		return false
	}
	if !health.LocalMCP.OK || !health.TunnelProcess.OK || !health.PublicMCP.OK {
		return true
	}
	if !health.OAuthMetadata.OK {
		return health.OAuthMetadata.StatusCode == 0 || health.OAuthMetadata.StatusCode >= http.StatusInternalServerError
	}
	return false
}

func appendCloudflareWatchdogLog(format string, args ...any) {
	_, _, _, logPath, _, err := cloudflarePaths()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return
	}
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = fmt.Fprintf(file, "%s [watchdog] %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func runCloudflareHealthCheck() cloudflareHealth {
	state := currentCloudflareState()
	runtimeCfg, _ := config.LoadGlobal("")
	result := cloudflareHealth{CheckedAt: time.Now().Format(time.RFC3339), OAuthLinked: state.OAuthLinked}

	result.Software = cloudflareHealthItem{OK: state.Software.Installed}
	if state.Software.Installed {
		result.Software.Detail = state.Software.Version
		if result.Software.Detail == "" {
			result.Software.Detail = state.Software.Path
		}
	} else {
		result.Software.Detail = "未检测到 cloudflared"
	}

	service := currentState()
	result.LocalMCP = checkLocalMCPHealth(service)
	result.TunnelProcess = cloudflareHealthItem{
		OK:     state.Status == cloudflareStatusRunning,
		Detail: fmt.Sprintf("cloudflared %s", state.Status),
	}
	if state.PID > 0 {
		result.TunnelProcess.Detail += fmt.Sprintf(" · pid=%d", state.PID)
	}

	if state.PublicMCPURL != "" {
		result.PublicMCP = checkCloudflareHTTP(state.PublicMCPURL, false)
	} else {
		result.PublicMCP = cloudflareHealthItem{Detail: "尚无公网 MCP URL"}
	}

	mode := config.EffectiveAuthMode(runtimeCfg.Auth)
	if (mode == "oauth" || mode == "dual") && state.PublicURL != "" {
		metadataURL := strings.TrimRight(state.PublicURL, "/") + "/.well-known/oauth-authorization-server"
		result.OAuthMetadata = checkCloudflareHTTP(metadataURL, true)
		if !state.OAuthLinked {
			result.OAuthMetadata.OK = false
			if result.OAuthMetadata.Detail != "" {
				result.OAuthMetadata.Detail += "；"
			}
			result.OAuthMetadata.Detail += "auth.oauth.server_url 与当前公网 Origin 不一致"
		}
	} else {
		result.OAuthMetadata = cloudflareHealthItem{OK: true, Detail: "当前鉴权模式无需 OAuth metadata 检查"}
	}

	result.OK = result.Software.OK && result.LocalMCP.OK && result.TunnelProcess.OK && result.PublicMCP.OK && result.OAuthMetadata.OK
	result.Diagnosis = diagnoseCloudflareHealth(result)
	return result
}

func checkLocalMCPHealth(service serviceState) cloudflareHealthItem {
	baseDetail := fmt.Sprintf("MCPX %s · %s", service.Status, service.Addr)
	if service.Status != statusRunning || strings.TrimSpace(service.Endpoint) == "" {
		return cloudflareHealthItem{Detail: baseDetail, URL: service.Endpoint}
	}

	item := checkCloudflareHTTP(service.Endpoint, false)
	if item.Detail != "" {
		item.Detail = baseDetail + " · HTTP " + item.Detail
	} else {
		item.Detail = baseDetail
	}
	return item
}

func diagnoseCloudflareHealth(result cloudflareHealth) string {
	switch {
	case !result.LocalMCP.OK:
		return "本地 MCPX HTTP 探测失败；优先检查 MCPX Runtime、监听端口和 mcpx-daemon.log。"
	case !result.Software.OK:
		return "未检测到 cloudflared；本地 MCPX 可达，但无法建立 Cloudflare Tunnel。"
	case !result.TunnelProcess.OK:
		return "本地 MCPX HTTP 正常，但 cloudflared Tunnel 未运行；优先检查或重启 Tunnel。"
	case !result.PublicMCP.OK && result.PublicMCP.StatusCode == http.StatusBadGateway:
		return "本地 MCPX HTTP 正常、Tunnel 进程存在，但公网 MCP 返回 502；优先检查 cloudflared.log 中的 origin 连接、EOF、connection reset 或 timeout。"
	case !result.PublicMCP.OK:
		return fmt.Sprintf("本地 MCPX HTTP 正常，但公网 MCP 异常（%s）；优先检查 Cloudflare Tunnel 与公网路由。", result.PublicMCP.Detail)
	case !result.OAuthMetadata.OK:
		return "MCP 与 Tunnel 可达，但 OAuth metadata 异常；检查 auth.oauth.server_url 与公网 Origin。"
	default:
		return "本地 MCPX、cloudflared Tunnel、公网 MCP 与 OAuth metadata 均正常。"
	}
}

func checkCloudflareHTTP(target string, requireOK bool) cloudflareHealthItem {
	item := cloudflareHealthItem{URL: target}
	client := &http.Client{
		Timeout: cloudflareHTTPTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		item.Detail = err.Error()
		return item
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "MCPX Desktop health-check")
	resp, err := client.Do(req)
	if err != nil {
		item.Detail = err.Error()
		return item
	}
	defer resp.Body.Close()
	item.StatusCode = resp.StatusCode
	item.Detail = resp.Status
	if requireOK {
		item.OK = resp.StatusCode >= 200 && resp.StatusCode < 300
		return item
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted, http.StatusNoContent,
		http.StatusBadRequest, http.StatusUnauthorized, http.StatusMethodNotAllowed, http.StatusNotAcceptable:
		item.OK = true
	}
	return item
}

func extractTryCloudflareURL(text string) string {
	return tryCloudflareURLPattern.FindString(text)
}
