//go:build windows

package browseruse

import (
	"context"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

const windowsPipeRoot = `\\.\pipe\`

func browserPipeCandidates() ([]string, error) {
	entries, err := os.ReadDir(windowsPipeRoot)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "codex-browser-use-") && !strings.HasPrefix(name, `codex-browser-use\`) {
			continue
		}
		seen[windowsPipeRoot+name] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for pipe := range seen {
		result = append(result, pipe)
	}
	sort.Strings(result)
	return result, nil
}

func openBrowserPipe(ctx context.Context, path string) (io.ReadWriteCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	for {
		handle, openErr := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
		if openErr == nil {
			file := os.NewFile(uintptr(handle), path)
			if file == nil {
				_ = windows.CloseHandle(handle)
				return nil, os.ErrInvalid
			}
			return file, nil
		}
		if openErr != windows.ERROR_PIPE_BUSY {
			return nil, openErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
