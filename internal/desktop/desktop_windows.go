//go:build windows

// Package desktop 提供 `mcpx desktop` 的系统托盘与图形界面。
//
// 设计要点：托盘进程与后台服务进程是两个完全独立的进程。托盘通过调用自身的
// `-d` 和 `stop` 子命令来控制服务，因此退出托盘不会连带停掉服务——这是本功能
// 的核心承诺。
package desktop

import (
	"embed"
	"fmt"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed all:frontend/dist
var frontendAssets embed.FS

//go:embed assets/tray-running.png
var iconRunning []byte

//go:embed assets/tray-starting.png
var iconStarting []byte

//go:embed assets/tray-stopped.png
var iconStopped []byte

// statusPollInterval 是托盘刷新服务状态的间隔。2s 足够跟手，又不至于让
// tasklist 调用变成持续的 CPU 负担。
const statusPollInterval = 2 * time.Second

// x/sys/windows 没有导出 FreeConsole，这里直接取 kernel32 的入口。
var procFreeConsole = windows.NewLazySystemDLL("kernel32.dll").NewProc("FreeConsole")

type desktopApp struct {
	app    *application.App
	tray   *application.SystemTray
	window *application.WebviewWindow
	api    *api

	mu   sync.Mutex
	last serviceState
}

// trayOnlyFlag 让托盘静默驻留、不弹主窗口。开机自启注册的就是带这个参数的
// 命令行：手动敲 `mcpx desktop` 时你是想看界面的，而每次登录都弹窗则很烦。
const trayOnlyFlag = "tray"

// Run 启动托盘与 GUI，直到用户从托盘退出为止。
func Run(args []string) error {
	// mcpx 是 CLI，二进制不能用 -H windowsgui 链接（否则所有命令都没有输出）。
	// 所以这里主动脱离控制台，避免托盘模式后面一直挂着一个黑框。
	// 从双击启动时本来就没有控制台，调用失败可以直接忽略。
	_, _, _ = procFreeConsole.Call()

	trayOnly := hasFlag(args, trayOnlyFlag)
	desk := &desktopApp{api: newAPI()}

	desk.app = application.New(application.Options{
		Name:        "MCPX",
		Description: "MCPX Runtime 托盘与控制台",
		Icon:        iconRunning,
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "io.mcpx.desktop",
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				desk.showWindow()
			},
		},
		Assets: application.AssetOptions{
			Handler: application.BundledAssetFileServer(frontendAssets),
		},
		Services: []application.Service{
			application.NewServiceWithOptions(desk.api, application.ServiceOptions{Route: "/api"}),
		},
	})
	desk.api.setApp(desk.app)

	desk.setupWindow(trayOnly)
	desk.setupTray()
	go desk.pollStatus()

	return desk.app.Run()
}

// hasFlag 接受 -name 和 --name 两种写法，和标准库 flag 的习惯一致。
func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == "-"+name || arg == "--"+name {
			return true
		}
	}
	return false
}

func (d *desktopApp) setupWindow(trayOnly bool) {
	d.window = d.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             "main",
		Title:            "MCPX",
		Width:            940,
		Height:           660,
		MinWidth:         720,
		MinHeight:        480,
		Hidden:           trayOnly,
		BackgroundColour: initialBackground(),
	})

	// 关闭按钮只隐藏窗口。托盘还在，服务也还在——直接销毁窗口会让用户以为
	// 整个程序退出了。真正的退出走托盘菜单。
	d.window.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
		event.Cancel()
		d.window.Hide()
	})
}

func (d *desktopApp) setupTray() {
	d.tray = d.app.SystemTray.New()
	d.tray.SetIcon(iconStopped)
	d.tray.SetTooltip("MCPX")
	d.tray.OnClick(func() { d.showWindow() })
	d.refresh(currentState(), true)
}

func (d *desktopApp) showWindow() {
	d.window.Show()
	d.window.Focus()
}

// initialBackground 返回窗口底色。WebView 加载完成前这层底色是可见的，
// 跟系统主题对齐才不会在浅色环境下闪一下深色。
//
// 这里只看系统偏好：界面里手动切换的主题存在 WebView 的 localStorage 里，
// Go 侧读不到。手动覆盖与系统相反时，首帧仍可能有一次极短的色差。
func initialBackground() application.RGBA {
	if systemPrefersDark() {
		return application.NewRGBA(16, 18, 22, 255) // 对应 CSS 的 --bg 深色值
	}
	return application.NewRGBA(244, 246, 248, 255) // 对应 --bg 浅色值
}

// systemPrefersDark 读取 Windows 的"应用模式"偏好。读不到时按深色处理，
// 与前端 CSS 的默认值保持一致。
func systemPrefersDark() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return true
	}
	defer key.Close()
	// AppsUseLightTheme: 0=深色应用, 1=浅色应用
	value, _, err := key.GetIntegerValue("AppsUseLightTheme")
	if err != nil {
		return true
	}
	return value == 0
}

// pollStatus 周期性刷新服务状态，仅在状态真正变化时才重建托盘菜单。
func (d *desktopApp) pollStatus() {
	ticker := time.NewTicker(statusPollInterval)
	defer ticker.Stop()
	for range ticker.C {
		d.refresh(currentState(), false)
	}
}

// refresh 把最新状态同步到托盘图标、提示文字和菜单。
// force 用于首次装配；其余时候状态没变就不重建菜单，避免菜单在用户眼皮底下闪。
func (d *desktopApp) refresh(state serviceState, force bool) {
	d.mu.Lock()
	changed := force || d.last != state
	d.last = state
	d.mu.Unlock()
	if !changed {
		return
	}

	switch state.Status {
	case statusRunning:
		d.tray.SetIcon(iconRunning)
		d.tray.SetTooltip(fmt.Sprintf("MCPX 运行中 · %s", state.Addr))
	case statusStarting:
		d.tray.SetIcon(iconStarting)
		d.tray.SetTooltip("MCPX 启动中")
	case statusConflict:
		d.tray.SetIcon(iconStarting)
		d.tray.SetTooltip(fmt.Sprintf("MCPX 端口 %s 被其他进程占用", state.Addr))
	default:
		d.tray.SetIcon(iconStopped)
		d.tray.SetTooltip("MCPX 已停止")
	}
	d.tray.SetMenu(d.buildMenu(state))
}

func (d *desktopApp) buildMenu(state serviceState) *application.Menu {
	menu := d.app.NewMenu()

	status := menu.Add(statusLabel(state))
	status.SetEnabled(false)
	menu.AddSeparator()

	start := menu.Add("启动服务")
	start.OnClick(func(*application.Context) { d.runServiceAction(startService) })
	// 端口冲突时启动必然 bind 失败，直接禁用而不是让用户点了没反应。
	start.SetEnabled(state.Status == statusStopped)

	stop := menu.Add("停止服务")
	stop.OnClick(func(*application.Context) { d.runServiceAction(stopService) })
	stop.SetEnabled(managed(state))

	restart := menu.Add("重启服务")
	restart.OnClick(func(*application.Context) { d.runServiceAction(restartService) })
	restart.SetEnabled(managed(state))

	menu.AddSeparator()
	menu.Add("打开主窗口").OnClick(func(*application.Context) { d.showWindow() })
	menu.Add("复制端点地址").OnClick(func(*application.Context) {
		d.app.Clipboard.SetText(state.Endpoint)
	})
	menu.Add("打开运行时目录").OnClick(func(*application.Context) {
		if state.HomeDir != "" {
			_ = d.app.Browser.OpenFile(state.HomeDir)
		}
	})
	menu.Add("打开日志").OnClick(func(*application.Context) {
		if state.LogPath != "" {
			_ = d.app.Browser.OpenFile(state.LogPath)
		}
	})

	menu.AddSeparator()
	autostartOn, err := d.app.Autostart.IsEnabled()
	if err == nil {
		item := menu.AddCheckbox("开机自启", autostartOn)
		item.OnClick(func(ctx *application.Context) {
			d.toggleAutostart(ctx.ClickedMenuItem().Checked())
		})
	}

	menu.AddSeparator()
	// 只退出托盘，后台服务不受影响。
	menu.Add("退出托盘").OnClick(func(*application.Context) { d.app.Quit() })
	return menu
}

// managed 表示当前进程是由托盘/CLI 的 daemon 状态文件跟踪的，可以被停止。
// 端口冲突场景下的占用者不属于此列，我们不去动别人的进程。
func managed(state serviceState) bool {
	return state.Status == statusRunning || state.Status == statusStarting
}

func statusLabel(state serviceState) string {
	switch state.Status {
	case statusRunning:
		return fmt.Sprintf("运行中 · pid=%d · %s", state.PID, state.Addr)
	case statusStarting:
		return fmt.Sprintf("启动中 · pid=%d · 端口未就绪", state.PID)
	case statusConflict:
		return fmt.Sprintf("端口 %s 被其他进程占用", state.Addr)
	default:
		return "已停止"
	}
}

// runServiceAction 在后台执行启停操作。启停会等待状态稳定（最长 10s），
// 直接在菜单回调里同步执行会卡住整个 UI。
func (d *desktopApp) runServiceAction(action func() error) {
	go func() {
		if err := action(); err != nil {
			d.app.Dialog.Error().SetMessage(err.Error()).Show()
		}
		d.refresh(currentState(), true)
	}()
}

func (d *desktopApp) toggleAutostart(enable bool) {
	var err error
	if enable {
		// 必须显式带上子命令：注册项指向的是 mcpx.exe 本身，不带参数的话
		// 登录时会当成 `mcpx` 启动前台服务，而不是托盘。
		// -tray 让它静默驻留，不在每次登录时弹窗。
		err = d.app.Autostart.EnableWithOptions(application.AutostartOptions{
			Arguments: []string{"desktop", "-" + trayOnlyFlag},
		})
	} else {
		err = d.app.Autostart.Disable()
	}
	if err != nil {
		d.app.Dialog.Error().SetMessage(fmt.Sprintf("设置开机自启失败：%v", err)).Show()
	}
	d.refresh(currentState(), true)
}
