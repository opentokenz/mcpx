package mcpproxy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
	"mcpx/internal/logging"
	buildversion "mcpx/internal/version"
	"mcpx/internal/winproc"
)

const (
	mcpConnectTimeout      = 30 * time.Second
	mcpListTimeout         = 30 * time.Second
	mcpCallTimeout         = 2 * time.Minute
	clientStartedAtMetaKey = "mcpx/started_at_ms"
)

var (
	clientProgressHeartbeatInterval = 20 * time.Second
	nextProgressToken               atomic.Uint64
)

// ToolProgress is a client-side progress update for one upstream tools/call.
// Synthetic updates are emitted by the MCPX client watchdog when the upstream
// server has not sent a progress notification within the heartbeat interval.
type ToolProgress struct {
	Message   string
	Progress  float64
	Total     float64
	Elapsed   time.Duration
	Synthetic bool
}

// ProgressHandler receives progress for an upstream tools/call.
type ProgressHandler func(ToolProgress)

// ClientSession owns one upstream stdio MCP process. ListTools and CallTool on
// the same value observe and execute against the same upstream instance.
type ClientSession struct {
	srv           config.MCPServer
	session       *mcp.ClientSession
	cancel        context.CancelFunc
	onProgress    ProgressHandler
	callbackMu    sync.Mutex
	progressMu    sync.Mutex
	progressToken string
	progressReset chan struct{}
	callStarted   time.Time
}

// OpenClientSession connects one upstream MCP process for a bounded operation.
func OpenClientSession(ctx context.Context, srv config.MCPServer, onProgress ProgressHandler) (*ClientSession, error) {
	client := &ClientSession{srv: srv, onProgress: onProgress}
	var options *mcp.ClientOptions
	if onProgress != nil {
		options = &mcp.ClientOptions{ProgressNotificationHandler: client.handleProgress}
	}
	session, cancel, err := connect(ctx, srv, mcpConnectTimeout, options)
	if err != nil {
		return nil, err
	}
	client.session = session
	client.cancel = cancel
	return client, nil
}

func (c *ClientSession) handleProgress(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
	if req == nil || req.Params == nil {
		return
	}
	c.progressMu.Lock()
	token := c.progressToken
	reset := c.progressReset
	started := c.callStarted
	c.progressMu.Unlock()
	if token == "" || fmt.Sprint(req.Params.ProgressToken) != token {
		return
	}
	c.deliver(ToolProgress{
		Message: req.Params.Message, Progress: req.Params.Progress, Total: req.Params.Total,
		Elapsed: time.Since(started),
	})
	if reset != nil {
		select {
		case reset <- struct{}{}:
		default:
		}
	}
}

func (c *ClientSession) deliver(update ToolProgress) {
	if c == nil || c.onProgress == nil {
		return
	}
	c.callbackMu.Lock()
	defer c.callbackMu.Unlock()
	c.onProgress(update)
}

// Close terminates the owned upstream MCP process.
func (c *ClientSession) Close() {
	if c == nil {
		return
	}
	if c.session != nil {
		_ = c.session.Close()
	}
	if c.cancel != nil {
		c.cancel()
	}
}

// ListTools reads tools/list on this exact upstream instance.
func (c *ClientSession) ListTools(ctx context.Context) ([]*mcp.Tool, error) {
	if c == nil || c.session == nil {
		return nil, fmt.Errorf("upstream mcp session is not connected")
	}
	listCtx, cancel := context.WithTimeout(ctx, mcpListTimeout)
	defer cancel()
	listed, err := c.session.ListTools(listCtx, nil)
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	if listed == nil {
		return nil, nil
	}
	return listed.Tools, nil
}

// CallTool calls a tool on this exact upstream instance.
func (c *ClientSession) CallTool(ctx context.Context, toolName string, arguments map[string]any, meta mcp.Meta) (*mcp.CallToolResult, error) {
	if c == nil || c.session == nil {
		return nil, fmt.Errorf("upstream mcp session is not connected")
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	callCtx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
	defer cancel()
	started := time.Now()
	progressToken := ""
	progressReset := make(chan struct{}, 1)
	if c.onProgress != nil {
		progressToken = fmt.Sprintf("mcpx-progress-%d", nextProgressToken.Add(1))
	}
	c.progressMu.Lock()
	c.progressToken = progressToken
	c.progressReset = progressReset
	c.callStarted = started
	c.progressMu.Unlock()
	defer func() {
		c.progressMu.Lock()
		c.progressToken = ""
		c.progressReset = nil
		c.progressMu.Unlock()
	}()
	params := newCallToolParams(toolName, arguments, meta, progressToken, started)
	stopHeartbeat := startClientProgressHeartbeat(callCtx, toolName, started, progressReset, c.deliver)
	defer stopHeartbeat()
	res, err := c.session.CallTool(callCtx, params)
	if err != nil {
		return nil, err
	}
	logging.Debug("mcp call ok", "tool", toolName, "cmd", DescribeCommand(c.srv))
	return res, nil
}

// CallTool starts one stdio MCP client, calls tool, and closes the session.
func CallTool(ctx context.Context, srv config.MCPServer, toolName string, arguments map[string]any, meta mcp.Meta) (*mcp.CallToolResult, error) {
	return CallToolWithProgress(ctx, srv, toolName, arguments, meta, nil)
}

// CallToolWithProgress is the one-shot convenience wrapper around ClientSession.
func CallToolWithProgress(ctx context.Context, srv config.MCPServer, toolName string, arguments map[string]any, meta mcp.Meta, onProgress ProgressHandler) (*mcp.CallToolResult, error) {
	client, err := OpenClientSession(ctx, srv, onProgress)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.CallTool(ctx, toolName, arguments, meta)
}

func newCallToolParams(toolName string, arguments map[string]any, meta mcp.Meta, progressToken string, started time.Time) *mcp.CallToolParams {
	requestMeta := make(mcp.Meta, len(meta)+1)
	for key, value := range meta {
		requestMeta[key] = value
	}
	// The client owns the actual call start time. A caller-provided value must
	// never override it, even when the rest of the metadata is forwarded.
	requestMeta[clientStartedAtMetaKey] = started.UnixMilli()
	params := &mcp.CallToolParams{
		Meta:      requestMeta,
		Name:      toolName,
		Arguments: arguments,
	}
	if progressToken != "" {
		params.SetProgressToken(progressToken)
	}
	return params
}

func startClientProgressHeartbeat(ctx context.Context, toolName string, started time.Time, reset <-chan struct{}, deliver ProgressHandler) func() {
	if deliver == nil || clientProgressHeartbeatInterval <= 0 {
		return func() {}
	}
	heartbeatCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(clientProgressHeartbeatInterval)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				deliver(ToolProgress{
					Message:   fmt.Sprintf("%s is still running", toolName),
					Progress:  time.Since(started).Seconds(),
					Elapsed:   time.Since(started),
					Synthetic: true,
				})
				timer.Reset(clientProgressHeartbeatInterval)
			case <-reset:
				resetProgressTimer(timer, clientProgressHeartbeatInterval)
			case <-heartbeatCtx.Done():
				return
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func resetProgressTimer(timer *time.Timer, interval time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(interval)
}

// ListTools starts one upstream stdio server and returns its tools/list items.
func ListTools(ctx context.Context, srv config.MCPServer) ([]*mcp.Tool, error) {
	client, err := OpenClientSession(ctx, srv, nil)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.ListTools(ctx)
}

func connect(ctx context.Context, srv config.MCPServer, timeout time.Duration, options *mcp.ClientOptions) (*mcp.ClientSession, context.CancelFunc, error) {
	if srv.Command == "" {
		return nil, func() {}, fmt.Errorf("empty command")
	}
	connectCtx, cancelConnect := context.WithTimeout(ctx, timeout)
	// The connect timeout bounds the handshake only. The process must outlive it:
	// a tool call may still be running long after the server answered initialize.
	// Binding exec.CommandContext to connectCtx would kill the child once the
	// timeout elapses, which is what breaks long-running tools.
	procCtx, cancelProc := context.WithCancel(ctx)
	cancelAll := func() {
		cancelProc()
		cancelConnect()
	}

	cmd := exec.CommandContext(procCtx, srv.Command, srv.Args...)
	// Windows 下隐藏上游 MCP 命令创建的控制台窗口；其他系统为空操作。
	winproc.ConfigureNoWindow(cmd)
	cmd.Env = append(os.Environ(), ExpandEnv(srv.Env)...)
	client := mcp.NewClient(&mcp.Implementation{Name: "mcpx", Version: buildversion.Current}, options)
	session, err := client.Connect(connectCtx, &mcp.CommandTransport{Command: cmd}, nil)
	// The handshake is over, so the connect timeout must not fire any more.
	cancelConnect()
	if err != nil {
		cancelAll()
		return nil, func() {}, fmt.Errorf("connect upstream mcp: %w", err)
	}
	return session, cancelAll, nil
}
