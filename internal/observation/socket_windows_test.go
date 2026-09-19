//go:build windows

package observation

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestWindowsNamedPipeListenerCloseUnblocksAccept(t *testing.T) {
	listenerValue, err := listenObserverSocket(testObserverPath(t))
	if err != nil {
		t.Fatal(err)
	}
	listener := listenerValue.(*namedPipeListener)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if conn != nil {
			_ = conn.Close()
		}
		acceptErr <- err
	}()

	deadline := time.Now().Add(time.Second)
	for {
		listener.mu.Lock()
		pending := listener.handle != 0
		listener.mu.Unlock()
		if pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("named pipe accept did not create a pending handle")
		}
		time.Sleep(time.Millisecond)
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- listener.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("named pipe listener close blocked")
	}
	select {
	case err := <-acceptErr:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("accept error = %v, want net.ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("named pipe accept did not unblock after close")
	}
}

func TestWindowsDialRetriesUntilPipeExists(t *testing.T) {
	path := testObserverPath(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	clientResult := make(chan struct {
		conn net.Conn
		err  error
	}, 1)
	go func() {
		conn, err := dialObserverSocket(ctx, path)
		clientResult <- struct {
			conn net.Conn
			err  error
		}{conn: conn, err: err}
	}()

	time.Sleep(50 * time.Millisecond)
	listener, err := listenObserverSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverResult := make(chan struct {
		conn net.Conn
		err  error
	}, 1)
	go func() {
		conn, err := listener.Accept()
		serverResult <- struct {
			conn net.Conn
			err  error
		}{conn: conn, err: err}
	}()

	select {
	case client := <-clientResult:
		if client.err != nil {
			t.Fatalf("dial before pipe creation failed instead of retrying: %v", client.err)
		}
		defer client.conn.Close()
	case <-ctx.Done():
		t.Fatal("dial did not connect after pipe creation")
	}
	select {
	case server := <-serverResult:
		if server.err != nil {
			t.Fatal(server.err)
		}
		defer server.conn.Close()
	case <-ctx.Done():
		t.Fatal("listener did not accept retried dial")
	}
}
