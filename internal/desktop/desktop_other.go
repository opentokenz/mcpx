//go:build !windows

// Package desktop 提供 `mcpx desktop` 托盘与 GUI。
// 非 Windows 平台只保留占位实现：桌面端依赖 Wails v3，而 Wails 在
// Linux/macOS 需要 CGO 与 GTK/WebKit 头文件，链接进跨平台 CLI 会破坏
// 现有的 CGO_ENABLED=0 发布流程与 ubuntu CI。
package desktop

import "errors"

// Run 在非 Windows 平台直接返回错误，调用方负责打印。
func Run([]string) error {
	return errors.New("mcpx desktop 仅支持 Windows")
}
