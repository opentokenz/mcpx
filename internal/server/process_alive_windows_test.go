//go:build windows

package server

import (
	"os"

	"mcpx/internal/winproc"
)

func fixtureProcessAlive(pid int) (bool, error) {
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	alive, matches, err := winproc.State(pid, executable)
	return alive && matches, err
}
