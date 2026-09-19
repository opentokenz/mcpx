package pythonruntime

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"mcpx/internal/winproc"
)

const probeTimeout = 3 * time.Second

// Invocation describes a verified Python 3 launcher. PrefixArgs is used for
// launchers such as Windows py.exe, where selecting Python 3 requires -3.
type Invocation struct {
	Executable string
	PrefixArgs []string
	Name       string
}

// Args returns launcher prefix arguments followed by the requested arguments.
func (i Invocation) Args(args ...string) []string {
	result := make([]string, 0, len(i.PrefixArgs)+len(args))
	result = append(result, i.PrefixArgs...)
	result = append(result, args...)
	return result
}

// Command returns a human-readable command matching this invocation. It is
// for policy/audit/display only; execution must use Executable and Args.
func (i Invocation) Command(args ...string) string {
	parts := make([]string, 0, 1+len(i.PrefixArgs)+len(args))
	parts = append(parts, i.Name)
	parts = append(parts, i.PrefixArgs...)
	parts = append(parts, args...)
	for index := range parts {
		if strings.ContainsAny(parts[index], " \t\r\n\"") {
			parts[index] = strconv.Quote(parts[index])
		}
	}
	return strings.Join(parts, " ")
}

type candidate struct {
	name       string
	prefixArgs []string
}

// Resolve finds a working Python 3 interpreter instead of assuming python3 is
// executable. In particular, Windows may expose a broken python3 app alias
// while a real python.exe or py.exe installation is available.
func Resolve() (Invocation, error) {
	candidates := []candidate{{name: "python3"}, {name: "python"}}
	if runtime.GOOS == "windows" {
		candidates = []candidate{{name: "python"}, {name: "py", prefixArgs: []string{"-3"}}, {name: "python3"}}
	}

	var attempted []string
	for _, item := range candidates {
		path, err := exec.LookPath(item.name)
		if err != nil {
			continue
		}
		attempted = append(attempted, item.name)
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		probeArgs := append(append([]string(nil), item.prefixArgs...), "-c", "import sys; raise SystemExit(0 if sys.version_info[0] == 3 else 1)")
		cmd := exec.CommandContext(ctx, path, probeArgs...)
		winproc.ConfigureNoWindow(cmd)
		runErr := cmd.Run()
		cancel()
		if runErr == nil {
			return Invocation{Executable: path, PrefixArgs: append([]string(nil), item.prefixArgs...), Name: item.name}, nil
		}
	}
	if len(attempted) == 0 {
		return Invocation{}, errors.New("Python 3 runtime was not found (tried python3/python and Windows py -3)")
	}
	return Invocation{}, errors.New("no working Python 3 runtime found; discovered launchers failed validation: " + strings.Join(attempted, ", "))
}
