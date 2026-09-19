package server

import (
	"fmt"
	"math"
	"runtime"
	"testing"
	"time"

	"mcpx/internal/config"
	"mcpx/internal/pythonruntime"
)

func testPrintCommand(text string) string {
	if runtime.GOOS == "windows" {
		// Keep echo-based output separate from tests that intentionally place
		// bare `echo` behind confirmation rules.
		return "cmd /D /C echo " + text
	}
	return "printf " + text
}

func testSleepCommand(duration time.Duration) string {
	if runtime.GOOS == "windows" {
		// ping is available on supported Windows versions and gives us a shell-
		// independent delay without assuming PowerShell or a Unix sleep binary.
		count := int(math.Ceil(duration.Seconds())) + 1
		if count < 2 {
			count = 2
		}
		return fmt.Sprintf("ping -n %d 127.0.0.1", count)
	}
	return fmt.Sprintf("sleep %.3f", duration.Seconds())
}

func testShellMissingCommandOutcome() (string, float64) {
	if runtime.GOOS == "windows" {
		// cmd.exe reports an unresolved command as exit 1. That value is also a
		// normal program exit code, so production must not relabel every exit 1
		// as COMMAND_NOT_FOUND based on localized stderr text.
		return "process_exit", 1
	}
	return "command_not_found", 127
}

func testStdoutStderrCommand(stdout, stderr string) string {
	// echo and stderr redirection are supported by both cmd.exe and POSIX
	// shells. Tests consuming this helper trim the shell's trailing newline.
	return fmt.Sprintf("echo %s && echo %s 1>&2", stdout, stderr)
}

func testPythonInvocation(t *testing.T) pythonruntime.Invocation {
	t.Helper()
	python, err := pythonruntime.Resolve()
	if err != nil {
		t.Skipf("Python 3 runtime unavailable: %v", err)
	}
	return python
}

func testMCPServer(t *testing.T, description, script string, env map[string]string) config.MCPServer {
	t.Helper()
	python := testPythonInvocation(t)
	return config.MCPServer{
		Description: description,
		Command:     python.Executable,
		Args:        python.Args(script),
		Env:         env,
	}
}
