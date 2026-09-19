//go:build !windows

package processutil

import "os/exec"

// ConfigureBackground is a no-op on non-Windows platforms.
func ConfigureBackground(_ *exec.Cmd) {}
