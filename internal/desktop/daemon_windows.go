//go:build windows

package desktop

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mcpx/internal/config"
)

const (
	// 与 cmd/mcpx-server/background.go 保持一致。那里是 package main，无法被
	// 引用，所以这里只能重复常量；改动时两边必须同步。
	daemonStateFilename = "mcpx-daemon.json"
	daemonLogFilename   = "mcpx-daemon.log"

	statusRunning  = "running"
	statusStarting = "starting"
	statusConflict = "conflict"
	statusStopped  = "stopped"
)

// daemonState 是 ~/.mcpx/mcpx-daemon.json 的内容，字段与
// cmd/mcpx-server/background.go 的同名结构一致。
type daemonState struct {
	PID        int    `json:"pid"`
	Executable string `json:"executable"`
}

// serviceState 是托盘菜单和 GUI 共用的一份服务状态快照。
type serviceState struct {
	Status     string `json:"status"`
	PID        int    `json:"pid"`
	Addr       string `json:"addr"`
	Endpoint   string `json:"endpoint"`
	AuthMode   string `json:"auth_mode"`
	Executable string `json:"executable"`
	HomeDir    string `json:"home_dir"`
	LogPath    string `json:"log_path"`
	ConfigPath string `json:"config_path"`
	Error      string `json:"error,omitempty"`
}

// running 用于托盘图标与菜单文案，只关心"是否已经能服务"。
func (s serviceState) running() bool { return s.Status == statusRunning }

// selfExecutable 返回当前 mcpx.exe 自身路径。单二进制方案下托盘不需要去
// 别处查找服务端程序——启动和停止调用的就是自己。
func selfExecutable() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	return executable, nil
}

// currentState 汇总一次完整的服务状态判定。
//
// 判定顺序是有意为之的：先看 daemon 状态文件里的 PID 是否真的活着且镜像名匹配，
// 再拨一次监听端口。四种结果：
//
//   - running  —— 受管进程存活且端口可连
//   - starting —— 受管进程存活但端口还没起来
//   - conflict —— 没有受管进程，但端口已被别的进程占用。此时点"启动"必然
//     因为 bind 失败而静默退出，所以要单独报出来而不是谎称"已停止"
//   - stopped  —— 没有受管进程，端口也没人听
func currentState() serviceState {
	state := serviceState{Status: statusStopped}

	home, err := config.HomeDir()
	if err != nil {
		state.Error = err.Error()
		return state
	}
	state.HomeDir = home
	state.LogPath = filepath.Join(home, "logs", daemonLogFilename)

	configPath, err := config.GlobalConfigPath()
	if err == nil {
		state.ConfigPath = configPath
	}

	cfg, err := config.LoadGlobal("")
	if err != nil {
		// 配置读不出来时仍然回退到默认监听地址，让托盘至少能显示端口。
		cfg = config.DefaultConfig()
		state.Error = fmt.Sprintf("读取配置失败：%v", err)
	}
	host := strings.TrimSpace(cfg.Server.Host)
	if host == "" {
		host = "127.0.0.1"
	}
	port := cfg.Server.Port
	if port == 0 {
		port = 9090
	}
	state.Addr = net.JoinHostPort(host, strconv.Itoa(port))
	state.Endpoint = fmt.Sprintf("http://%s/mcp", state.Addr)
	state.AuthMode = config.EffectiveAuthMode(cfg.Auth)

	persisted, err := readDaemonState(filepath.Join(home, daemonStateFilename))
	if err != nil {
		state.Status = untrackedStatus(state.Addr)
		return state
	}
	state.PID = persisted.PID
	state.Executable = persisted.Executable

	alive, matches := processAlive(persisted.PID, persisted.Executable)
	if !alive || !matches {
		// 状态文件过期（进程已退出或 PID 被复用给了别的程序）。
		state.PID = 0
		state.Status = untrackedStatus(state.Addr)
		return state
	}
	if portListening(state.Addr) {
		state.Status = statusRunning
		return state
	}
	state.Status = statusStarting
	return state
}

// untrackedStatus 判定"没有受管 daemon"时的真实状态。端口仍被占用说明有别的
// 进程（旧版本、手工前台启动、或别的程序）在用这个端口。
func untrackedStatus(addr string) string {
	if portListening(addr) {
		return statusConflict
	}
	return statusStopped
}

func readDaemonState(path string) (daemonState, error) {
	var state daemonState
	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("decode %s: %w", path, err)
	}
	return state, nil
}

// processAlive 用 tasklist 判断 PID 是否存活，并校验镜像名是否就是 mcpx。
// 逻辑与 cmd/mcpx-server/background_windows.go 的 windowsBackgroundProcessState
// 一致：只比对镜像名可以挡住 PID 被系统复用后误杀无关进程的情况。
func processAlive(pid int, executable string) (alive bool, matches bool) {
	if pid <= 0 {
		return false, false
	}
	command := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH")
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	output, err := command.Output()
	if err != nil {
		return false, false
	}
	line := strings.TrimSpace(string(output))
	if line == "" || strings.HasPrefix(line, "INFO:") {
		return false, false
	}
	first := line
	if index := strings.Index(first, ","); index >= 0 {
		first = first[:index]
	}
	image := strings.Trim(first, "\" ")
	if executable == "" {
		return true, true
	}
	return true, strings.EqualFold(image, filepath.Base(executable))
}

// portListening 拨一次监听端口，确认服务真的可以接受连接。
func portListening(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// runSelf 以隐藏窗口的方式调用自己的子命令。HideWindow 是必需的：
// 托盘进程已经脱离控制台，子进程若自带控制台会闪出一个黑框。
func runSelf(args ...string) (string, error) {
	executable, err := selfExecutable()
	if err != nil {
		return "", err
	}
	command := exec.Command(executable, args...)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	output, err := command.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("%s：%s", strings.Join(args, " "), text)
		}
		return text, fmt.Errorf("%s：%w", strings.Join(args, " "), err)
	}
	return text, nil
}

// startService 启动后台服务。`mcpx -d` 自身会先停掉状态文件里仍存活的旧实例，
// 所以这里不需要额外做去重。
func startService() error {
	if _, err := runSelf("-d"); err != nil {
		return err
	}
	waitForStatus(statusRunning, 10*time.Second)
	return nil
}

// stopService 停止后台服务。`mcpx stop` 对"本来就没在跑"是幂等的。
func stopService() error {
	if _, err := runSelf("stop"); err != nil {
		return err
	}
	waitForStatus(statusStopped, 10*time.Second)
	return nil
}

// restartService 先停到进程真正消失再启动。中间不等待的话，新实例会撞上
// 旧实例尚未释放的监听端口。
func restartService() error {
	if err := stopService(); err != nil {
		return err
	}
	return startService()
}

// waitForStatus 轮询到期望状态或超时。超时不算错误——调用方的下一次状态刷新
// 会如实反映真实情况，这里只是让 UI 少闪一下中间态。
func waitForStatus(want string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if currentState().Status == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}
