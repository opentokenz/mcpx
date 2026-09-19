//go:build !windows

package browseruse

import (
	"context"
	"io"
)

func browserPipeCandidates() ([]string, error) {
	return nil, ErrUnsupported
}

func openBrowserPipe(context.Context, string) (io.ReadWriteCloser, error) {
	return nil, ErrUnsupported
}
