//go:build windows

package processutil

import (
	"os/exec"
	"syscall"
)

// ConfigureBackground prevents console-window flashes for child processes
// started by the long-running MCPX process on Windows.
func ConfigureBackground(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}
