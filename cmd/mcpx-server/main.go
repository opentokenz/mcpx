package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"mcpx/internal/desktop"
	"mcpx/internal/logging"
	"mcpx/internal/server"
	buildversion "mcpx/internal/version"
)

// Set by GoReleaser / -ldflags at release build time.
var (
	version = buildversion.Current
	commit  = "none"
	date    = "unknown"
)

type buildProvenance struct {
	Version string
	Commit  string
	Date    string
}

func resolveBuildProvenance(versionValue, commitValue, dateValue string, settings []debug.BuildSetting) buildProvenance {
	resolved := buildProvenance{Version: versionValue, Commit: commitValue, Date: dateValue}
	if strings.TrimSpace(resolved.Version) == "" {
		resolved.Version = buildversion.Current
	}
	if strings.TrimSpace(resolved.Commit) != "" && resolved.Commit != "none" {
		return resolved
	}
	var revision string
	modified := false
	for _, setting := range settings {
		switch setting.Key {
		case "vcs.revision":
			revision = strings.TrimSpace(setting.Value)
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision != "" {
		resolved.Commit = revision
		if modified {
			resolved.Commit += "-dirty"
		}
	} else if strings.TrimSpace(resolved.Commit) == "" {
		resolved.Commit = "none"
	}
	if strings.TrimSpace(resolved.Date) == "" {
		resolved.Date = "unknown"
	}
	return resolved
}

func currentBuildProvenance() buildProvenance {
	var settings []debug.BuildSetting
	if info, ok := debug.ReadBuildInfo(); ok {
		settings = info.Settings
	}
	return resolveBuildProvenance(version, commit, date, settings)
}

func main() {
	build := currentBuildProvenance()
	backgroundChild := false
	if len(os.Args) >= 2 && os.Args[1] == backgroundChildSubcommand {
		backgroundChild = true
		os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
	}
	// Subcommands (before flag.Parse so they own their flags).
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "observe":
			os.Exit(runObserve(os.Args[2:]))
		case "workspace":
			os.Exit(runWorkspaceCommand(os.Args[2:]))
		case "oauth-register":
			os.Exit(runOAuthRegister(os.Args[2:]))
		case "update":
			os.Exit(runUpdate(os.Args[2:], build))
		case "stop":
			os.Exit(runStop())
		case "desktop":
			os.Exit(runDesktop(os.Args[2:]))
		case "help", "-h", "--help":
			printUsage()
			os.Exit(0)
		}
	}

	addr := flag.String("addr", "", "override listen addr host:port")
	logLevel := flag.String("log-level", "", "debug|info|warn|error (or MCPX_LOG_LEVEL)")
	logFormat := flag.String("log-format", "", "text|json (or MCPX_LOG_FORMAT)")
	background := flag.Bool("d", false, "run server in background")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = printUsage
	flag.Parse()

	if *showVersion {
		fmt.Printf("mcpx %s (commit=%s date=%s)\n", build.Version, build.Commit, build.Date)
		os.Exit(0)
	}
	if *background {
		pid, logPath, stoppedPIDs, err := startBackground(os.Args[1:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "start background server: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(backgroundStartMessage(pid, logPath, stoppedPIDs))
		return
	}

	if !backgroundChild {
		stoppedPIDs, err := stopExistingBackground()
		if err != nil {
			fmt.Fprintf(os.Stderr, "stop previous background daemon: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(backgroundStopMessage(stoppedPIDs))
	}

	logging.Init(logging.Options{Level: *logLevel, Format: *logFormat})
	logging.Info("mcpx", "version", build.Version, "commit", build.Commit)

	rt, err := server.New(server.Options{
		AddrOverride: *addr,
		Version:      build.Version,
		Commit:       build.Commit,
		Date:         build.Date,
	})
	if err != nil {
		logging.Error("startup failed", "err", err)
		os.Exit(1)
	}
	if err := rt.Start(); err != nil {
		logging.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func backgroundStopMessage(stoppedPIDs []int) string {
	var output strings.Builder
	for _, stoppedPID := range stoppedPIDs {
		fmt.Fprintf(&output, "mcpx stopped previous background daemon (pid=%d)\n", stoppedPID)
	}
	return output.String()
}

func backgroundStartMessage(pid int, logPath string, stoppedPIDs []int) string {
	var output strings.Builder
	output.WriteString(backgroundStopMessage(stoppedPIDs))
	fmt.Fprintf(&output, "mcpx started in background (pid=%d, log=%s)\n", pid, logPath)
	return output.String()
}

// runStop 停止后台守护进程，是 `mcpx stop` 的入口。
// 托盘的"停止"按钮和 CLI 共用它。没有存活实例时目标已经达成，返回 0 而不是
// 报错——否则托盘重复点"停止"会莫名其妙地变红。
func runStop() int {
	stoppedPIDs, err := stopExistingBackground()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcpx stop: %v\n", err)
		return 1
	}
	if len(stoppedPIDs) == 0 {
		fmt.Println("mcpx 没有正在运行的后台服务")
		return 0
	}
	fmt.Print(backgroundStopMessage(stoppedPIDs))
	return 0
}

// runDesktop 启动托盘与 GUI，是 `mcpx desktop` 的入口。
// 桌面端只在 Windows 上实现；其他平台由 internal/desktop 的占位实现返回错误，
// 这样跨平台 CLI 不必链接 Wails。
func runDesktop(args []string) int {
	if err := desktop.Run(args); err != nil {
		fmt.Fprintf(os.Stderr, "mcpx desktop: %v\n", err)
		return 1
	}
	return 0
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `mcpx — MCPX Runtime

Usage:
  mcpx [flags]                     启动 Streamable HTTP 服务
  mcpx stop                        停止后台服务
  mcpx desktop                     启动托盘与图形界面（仅 Windows；-tray 只驻留托盘不开窗口）
  mcpx observe [flags] <name>      终端只读观测 Workspace 事件
  mcpx workspace register <path>  注册或更新 Workspace（不启动服务）
  mcpx oauth-register [url]        动态注册 OAuth 客户端（粘贴 ChatGPT 回调 URL）
  mcpx update [flags]              从 GitHub Release 检查并安装新版本
  mcpx -version

oauth-register:
  mcpx oauth-register 'https://chatgpt.com/connector/oauth/…'
  mcpx oauth-register          # 交互粘贴回调
  mcpx oauth-register -base https://mcp.example.com 'https://…'

Flags (server):
`)
	flag.PrintDefaults()
}
