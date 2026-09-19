//go:build windows

package observation

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

const (
	namedPipeBufferSize = 64 * 1024
	namedPipeRetryDelay = 25 * time.Millisecond
)

// namedPipeListener adapts a Windows Named Pipe to net.Listener so the
// observer protocol remains identical on Unix and Windows.
type namedPipeListener struct {
	path string

	mu     sync.Mutex
	handle windows.Handle
	closed bool
}

func listenObserverSocket(path string) (net.Listener, error) {
	if path == "" {
		return nil, fmt.Errorf("observer named pipe path is required")
	}
	return &namedPipeListener{path: path}, nil
}

func (l *namedPipeListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, net.ErrClosed
	}
	handle, err := createObserverPipe(l.path)
	if err != nil {
		l.mu.Unlock()
		return nil, fmt.Errorf("create observer named pipe: %w", err)
	}
	l.handle = handle
	l.mu.Unlock()

	err = windows.ConnectNamedPipe(handle, nil)
	if err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
		l.clearPendingHandle(handle)
		_ = windows.CloseHandle(handle)
		if l.isClosed() {
			return nil, net.ErrClosed
		}
		return nil, fmt.Errorf("accept observer named pipe: %w", err)
	}

	l.mu.Lock()
	closed := l.closed
	if l.handle == handle {
		l.handle = 0
	}
	l.mu.Unlock()
	if closed {
		_ = windows.CloseHandle(handle)
		return nil, net.ErrClosed
	}

	file := os.NewFile(uintptr(handle), l.path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("wrap observer named pipe handle")
	}
	return &namedPipeConn{file: file, addr: namedPipeAddr(l.path)}, nil
}

func (l *namedPipeListener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	handle := l.handle
	l.mu.Unlock()
	if handle != 0 {
		wakeObserverPipe(l.path)
	}
	return nil
}

func (l *namedPipeListener) Addr() net.Addr {
	return namedPipeAddr(l.path)
}

func (l *namedPipeListener) clearPendingHandle(handle windows.Handle) {
	l.mu.Lock()
	if l.handle == handle {
		l.handle = 0
	}
	l.mu.Unlock()
}

func (l *namedPipeListener) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

func createObserverPipe(path string) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateNamedPipe(
		name,
		windows.PIPE_ACCESS_DUPLEX,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		windows.PIPE_UNLIMITED_INSTANCES,
		namedPipeBufferSize,
		namedPipeBufferSize,
		0,
		nil,
	)
}

func dialObserverSocket(ctx context.Context, path string) (net.Conn, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	for {
		handle, err := windows.CreateFile(
			name,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0,
			nil,
			windows.OPEN_EXISTING,
			0,
			0,
		)
		if err == nil {
			file := os.NewFile(uintptr(handle), path)
			if file == nil {
				_ = windows.CloseHandle(handle)
				return nil, fmt.Errorf("wrap observer named pipe handle")
			}
			return &namedPipeConn{file: file, addr: namedPipeAddr(path)}, nil
		}
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil, err
		}
		timer := time.NewTimer(namedPipeRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func removeObserverSocket(string) error { return nil }

func wakeObserverPipe(path string) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
	if err == nil {
		_ = windows.CloseHandle(handle)
	}
}

type namedPipeConn struct {
	file *os.File
	addr namedPipeAddr
}

func (c *namedPipeConn) Read(p []byte) (int, error)  { return c.file.Read(p) }
func (c *namedPipeConn) Write(p []byte) (int, error) { return c.file.Write(p) }
func (c *namedPipeConn) Close() error                { return c.file.Close() }
func (c *namedPipeConn) LocalAddr() net.Addr         { return c.addr }
func (c *namedPipeConn) RemoteAddr() net.Addr        { return c.addr }

func (c *namedPipeConn) SetDeadline(deadline time.Time) error {
	return c.file.SetDeadline(deadline)
}

func (c *namedPipeConn) SetReadDeadline(deadline time.Time) error {
	return c.file.SetReadDeadline(deadline)
}

func (c *namedPipeConn) SetWriteDeadline(deadline time.Time) error {
	return c.file.SetWriteDeadline(deadline)
}

type namedPipeAddr string

func (a namedPipeAddr) Network() string { return "windows-named-pipe" }
func (a namedPipeAddr) String() string  { return string(a) }
