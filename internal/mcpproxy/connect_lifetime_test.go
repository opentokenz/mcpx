package mcpproxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
)

// pythonSlowUpstream answers tools/list at once and sleeps inside tools/call,
// which is what a long-running upstream tool looks like. The script speaks
// newline-delimited JSON-RPC over stdin/stdout, so it needs no dependencies.
const pythonSlowUpstream = `import json, sys, time

def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    req = json.loads(line)
    method = req.get("method")
    rid = req.get("id")
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": rid, "result": {
            "protocolVersion": req["params"]["protocolVersion"],
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "slow-upstream", "version": "0.0.1"}}})
    elif method == "tools/list":
        send({"jsonrpc": "2.0", "id": rid, "result": {"tools": [{
            "name": "sleep_tool",
            "description": "sleeps before answering",
            "inputSchema": {"type": "object", "properties": {"seconds": {"type": "number"}}}}]}})
    elif method == "tools/call":
        seconds = float(req["params"].get("arguments", {}).get("seconds", 0))
        time.sleep(seconds)
        send({"jsonrpc": "2.0", "id": rid, "result": {"content": [
            {"type": "text", "text": "slept"}]}})
    elif rid is not None:
        send({"jsonrpc": "2.0", "id": rid, "result": {}})
`

// slowUpstreamServer writes the stub upstream and returns its command.
func slowUpstreamServer(t *testing.T) config.MCPServer {
	t.Helper()
	script := filepath.Join(t.TempDir(), "slow_upstream.py")
	if err := os.WriteFile(script, []byte(pythonSlowUpstream), 0o600); err != nil {
		t.Fatalf("write stub upstream: %v", err)
	}
	for _, candidate := range []string{"python3", "python"} {
		if _, err := exec.LookPath(candidate); err == nil {
			return config.MCPServer{Command: candidate, Args: []string{script}}
		}
	}
	t.Skip("no python interpreter to run the stub stdio upstream")
	return config.MCPServer{}
}

// TestStdioToolCallOutlivesConnectTimeout calls a stdio tool that keeps working
// after the connect timeout has elapsed. The call must succeed: the connect
// timeout bounds the handshake, not the lifetime of the process that connect
// started. A timeout bound to exec.CommandContext kills the child while the
// tool is still running, and the call fails early with an EOF instead.
func TestStdioToolCallOutlivesConnectTimeout(t *testing.T) {
	srv := slowUpstreamServer(t)

	// The connect timeout is short so the test stays fast; the call outlives it.
	const connectTimeout = 200 * time.Millisecond
	const callDuration = 4 * connectTimeout

	ctx, cancel := context.WithTimeout(context.Background(), 3*callDuration)
	defer cancel()
	session, release, err := connect(ctx, srv, connectTimeout, nil)
	if err != nil {
		t.Fatalf("connect upstream: %v", err)
	}
	defer func() { _ = session.Close() }()
	defer release()

	callCtx, callCancel := context.WithTimeout(ctx, 3*callDuration)
	defer callCancel()
	started := time.Now()
	result, err := session.CallTool(callCtx, &mcp.CallToolParams{
		Name:      "sleep_tool",
		Arguments: map[string]any{"seconds": callDuration.Seconds()},
	})
	if err != nil {
		t.Fatalf("tools/call failed %v after connecting with a %v connect timeout: %v",
			time.Since(started), connectTimeout, err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("unexpected result content: %+v", result.Content)
	}
}

// TestConnectTimeoutStillBoundsTheHandshake keeps the fix honest: the connect
// timeout must still cancel a handshake that never finishes, so an upstream
// that ignores initialize is not left running forever.
func TestConnectTimeoutStillBoundsTheHandshake(t *testing.T) {
	srv := silentUpstreamServer(t)

	const connectTimeout = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*connectTimeout)
	defer cancel()
	started := time.Now()
	_, release, err := connect(ctx, srv, connectTimeout, nil)
	defer release()
	if err == nil {
		t.Fatal("connect succeeded against an upstream that never answers initialize")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("connect error = %v, want a deadline exceeded error", err)
	}
	if elapsed := time.Since(started); elapsed > 5*connectTimeout {
		t.Fatalf("connect returned after %v, which is not bounded by the %v connect timeout", elapsed, connectTimeout)
	}
}

// silentUpstreamServer is a stdio upstream that reads its input and never
// answers, so the client handshake cannot complete.
func silentUpstreamServer(t *testing.T) config.MCPServer {
	t.Helper()
	script := filepath.Join(t.TempDir(), "silent_upstream.py")
	body := fmt.Sprintf("import sys%[1]cfor _ in sys.stdin: pass%[1]c", '\n')
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatalf("write silent upstream: %v", err)
	}
	for _, candidate := range []string{"python3", "python"} {
		if _, err := exec.LookPath(candidate); err == nil {
			return config.MCPServer{Command: candidate, Args: []string{script}}
		}
	}
	t.Skip("no python interpreter to run the stub stdio upstream")
	return config.MCPServer{}
}
