package server

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStreamableHTTPReconnectRestoresRemoteSession(t *testing.T) {
	rt := newWorkspaceRuntime(t, "恢复工作区")
	registered, _ := rt.reg.Get("恢复工作区")
	protocol := mcp.NewServer(&mcp.Implementation{Name: "issue823-reconnect", Version: "test"}, nil)
	rt.registerTools(protocol)
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return protocol }, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true, Stateless: true})
	gateway := NewGateway(rt.cfg, nil, streamable).Handler()
	var offline atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			http.Error(w, "injected transport unavailable", http.StatusServiceUnavailable)
			return
		}
		gateway.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connect := func(endpoint string) *mcp.ClientSession {
		t.Helper()
		client := mcp.NewClient(&mcp.Implementation{Name: "issue823-client", Version: "test"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint + "/mcp"}, nil)
		if err != nil {
			t.Fatalf("连接失败: %v", err)
		}
		return session
	}
	call := func(session *mcp.ClientSession, name string, args map[string]any) map[string]any {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s 传输失败: %v", name, err)
		}
		if result.IsError {
			t.Fatalf("%s 返回业务错误", name)
		}
		return decodeToolResult(t, result)
	}
	first := connect(server.URL)
	opened := call(first, "session", map[string]any{"action": "open", "workspace": "恢复工作区"})
	remoteID, _ := opened["remote_session_id"].(string)
	if remoteID == "" {
		t.Fatal("未返回持久 Remote Session ID")
	}
	editArgs := map[string]any{
		"remote_session_id": remoteID, "purpose": "准备精确字节恢复 fixture", "idempotency_key": "issue823-persisted-edit",
		"edits": []any{map[string]any{
			"path": "proof.txt", "operation": "create", "newline_policy": "exact",
			"content_base64":  base64.StdEncoding.EncodeToString([]byte("恢复前后的同一文件\r\n")),
			"expected_format": map[string]any{"charset": "utf-8", "bom": "none", "line_ending": "CRLF"},
		}},
	}
	created := call(first, "edit", editArgs)
	if !statusOK(created) {
		t.Fatal("公共精确字节编辑失败")
	}
	readArgs := map[string]any{"view": "file", "remote_session_id": remoteID, "path": "proof.txt"}
	before := call(first, "read", readArgs)
	beforeData, _ := before["data"].(map[string]any)
	if beforeData["rev"] == nil || beforeData["sha256"] != nil {
		t.Fatal("缺少 compact rev 或泄漏完整 SHA")
	}
	batchArgs := map[string]any{
		"remote_session_id": remoteID, "purpose": "核对恢复前后同一读取操作",
		"run_id": "reconnect-run", "operation_id": "reconnect-operation",
		"operations": []any{map[string]any{"id": "read-proof", "tool": "read", "arguments": readArgs}},
	}
	call(first, "operation_batch", batchArgs)
	waitArgs := map[string]any{"remote_session_id": remoteID, "operation_id": "reconnect-operation", "action": "wait", "timeout_ms": 5000}
	waited := call(first, "operation_manage", waitArgs)
	waitData, _ := waited["data"].(map[string]any)
	terminalID, _ := waitData["terminal_event_id"].(string)
	if waitData["state"] != "succeeded" || waitData["run_id"] != "reconnect-run" || terminalID == "" {
		t.Fatalf("实际 HTTP 等待没有返回稳定终态: %+v", waitData)
	}
	offline.Store(true)
	failedCtx, failedCancel := context.WithTimeout(ctx, 2*time.Second)
	_, err := first.CallTool(failedCtx, &mcp.CallToolParams{Name: "read", Arguments: readArgs})
	failedCancel()
	if err == nil {
		t.Fatal("故障窗口的请求意外成功")
	}
	_ = first.Close()
	// 关闭 HTTP 和 Runtime，再从同一独立测试数据库恢复；不复用内存 Session。
	server.Close()
	if err := rt.Close(); err != nil {
		t.Fatalf("关闭测试 Runtime: %v", err)
	}
	restored, err := New(Options{})
	if err != nil {
		t.Fatalf("重启测试 Runtime: %v", err)
	}
	defer restored.Close()
	protocol = mcp.NewServer(&mcp.Implementation{Name: "issue823-reconnect", Version: "test"}, nil)
	restored.registerTools(protocol)
	streamable = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return protocol }, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true, Stateless: true})
	gateway = NewGateway(restored.cfg, nil, streamable).Handler()
	server = httptest.NewServer(gateway)
	defer server.Close()
	second := connect(server.URL)
	defer second.Close()
	resumed := call(second, "session", map[string]any{"action": "open", "remote_session_id": remoteID})
	if resumed["remote_session_id"] != remoteID {
		t.Fatal("恢复创建了不同的 Remote Session")
	}
	replayed := call(second, "edit", editArgs)
	replayedData, _ := replayed["data"].(map[string]any)
	if !statusOK(replayed) || replayedData["idempotent_replay"] != true {
		t.Fatal("重启后没有复用精确编辑回执")
	}
	after := call(second, "read", readArgs)
	afterData, _ := after["data"].(map[string]any)
	if afterData["rev"] != beforeData["rev"] || afterData["content"] != beforeData["content"] {
		t.Fatal("重连后文件身份或内容改变")
	}
	// 使用同一稳定提交身份恢复，不因 HTTP/Runtime 重启生成新操作。
	batchReplay := call(second, "operation_batch", batchArgs)
	batchData, _ := batchReplay["data"].(map[string]any)
	if batchData["terminal_event_id"] != terminalID || batchData["state_sequence"] != waitData["state_sequence"] {
		t.Fatal("跨重启提交回读改变了终态身份")
	}
	again := call(second, "operation_manage", waitArgs)
	againData, _ := again["data"].(map[string]any)
	if againData["terminal_event_id"] != terminalID || againData["operation_id"] != "reconnect-operation" {
		t.Fatal("跨重启等待没有复用原终态")
	}
	principal, err := restored.principalFromContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := restored.remote.Get(ctx, principal, remoteID)
	if err != nil || persisted.WorkspacePath != registered.Path {
		t.Fatal("持久 Session 与工作区关联未保持")
	}
}
