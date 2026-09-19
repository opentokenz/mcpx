package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
	"mcpx/internal/mcpresult"
)

func TestMCPToolCallPassesContextAndReplaysFullUpstreamResult(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	rt.cfg.Discovery.MCP.Enabled = true
	workspace, _ := rt.reg.Get("demo")
	tmp := t.TempDir()
	script := filepath.Join(tmp, "passthrough_mcp.py")
	callLog := filepath.Join(tmp, "calls.log")
	startLog := filepath.Join(tmp, "starts.log")
	serverCode := `#!/usr/bin/env python3
import json
import os
import sys

with open(os.environ["MCPX_TEST_START_LOG"], "a", encoding="utf-8") as f:
    f.write("started\n")

def send(message):
    sys.stdout.write(json.dumps(message, separators=(',', ':')) + "\n")
    sys.stdout.flush()

tools = [{
    "name": "passthrough",
    "description": "return a mixed MCP result",
    "inputSchema": {"type": "object", "properties": {"value": {"type": "string"}}, "required": ["value"]},
    "outputSchema": {"type": "array"},
    "annotations": {"readOnlyHint": True, "destructiveHint": False, "openWorldHint": False}
}]
for line in sys.stdin:
    try:
        request = json.loads(line)
    except Exception:
        continue
    request_id = request.get("id")
    if request_id is None:
        continue
    method = request.get("method")
    if method == "initialize":
        send({"jsonrpc":"2.0", "id":request_id, "result":{"protocolVersion":"2025-11-25", "capabilities":{"tools":{}}, "serverInfo":{"name":"passthrough", "version":"1"}}})
    elif method == "tools/list":
        send({"jsonrpc":"2.0", "id":request_id, "result":{"tools":tools}})
    elif method == "tools/call":
        params = request.get("params", {})
        with open(os.environ["MCPX_TEST_CALL_LOG"], "a", encoding="utf-8") as f:
            f.write(json.dumps(params, separators=(',', ':')) + "\n")
        value = params.get("arguments", {}).get("value", "")
        send({"jsonrpc":"2.0", "id":request_id, "result":{
            "content":[
                {"type":"text", "text":"upstream:" + value},
                {"type":"image", "data":"aGVsbG8=", "mimeType":"image/png"}
            ],
            "structuredContent":["upstream", {"value":value}],
            "_meta":{
                "provider":"fake",
                "mcpx/remote_session_id":"spoofed-session",
                "mcpx/workspace":"spoofed-workspace",
                "mcpx/request_id":"spoofed-request",
                "mcpx/call_id":"spoofed-call",
                "mcpx/server":"spoofed-server",
                "mcpx/tool":"spoofed-tool",
                "mcpx/idempotent_replay":True
            },
            "isError":False
        }})
    else:
        send({"jsonrpc":"2.0", "id":request_id, "error":{"code":-32601, "message":"method not found"}})
`
	if err := os.WriteFile(script, []byte(serverCode), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteMCPFile(config.ProjectMCPPath(workspace.Path), config.MCPFile{MCPServers: map[string]config.MCPServer{
		"fake": testMCPServer(t, "Passthrough contract server", script, map[string]string{
			"MCPX_TEST_CALL_LOG": callLog, "MCPX_TEST_START_LOG": startLog,
		}),
	}}); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	request := map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "exercise transparent MCP result", "intent": "test operation",
		"server": "fake", "tool": "passthrough", "arguments": map[string]any{"value": "one"}, "idempotency_key": "mcp-passthrough-1",
	}

	result := callRawToolResult(t, rt.toolHandlers["mcp_tool"], request)
	assertPassthroughResult(t, result, remoteID, "upstream:one", false, false)
	assertMixedUpstreamResult(t, result, "one")
	if fakeMCPStartCount(t, startLog) != 1 {
		t.Fatalf("preflight and call must share one upstream instance; starts=%d", fakeMCPStartCount(t, startLog))
	}

	body, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 1 {
		t.Fatalf("upstream tools/call count=%d log=%s", len(lines), body)
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &params); err != nil {
		t.Fatal(err)
	}
	arguments, _ := params["arguments"].(map[string]any)
	if len(arguments) != 1 || arguments["value"] != "one" || arguments["remote_session_id"] != nil || arguments["workspace"] != nil {
		t.Fatalf("MCPX context leaked into upstream arguments: %+v", arguments)
	}
	meta, _ := params["_meta"].(map[string]any)
	if meta[mcpMetaRemoteSessionID] != remoteID || meta[mcpMetaWorkspace] != "demo" || meta[mcpMetaRequestID] != result.Meta[mcpMetaRequestID] {
		t.Fatalf("upstream request metadata=%+v result=%+v", meta, result.Meta)
	}
	if meta[mcpMetaCallID] != nil {
		t.Fatalf("call id must be omitted when no trusted call id exists: %+v", meta)
	}
	if started, ok := meta[clientStartedAtMetaKey].(float64); !ok || started <= 0 {
		t.Fatalf("missing client start timestamp in upstream request meta: %+v", meta)
	}

	firstRequestID := result.Meta[mcpMetaRequestID]
	rt.cfg.Discovery.MCP.Enabled = false
	replay := callRawToolResult(t, rt.toolHandlers["mcp_tool"], request)
	assertPassthroughResult(t, replay, remoteID, "upstream:one", false, true)
	assertMixedUpstreamResult(t, replay, "one")
	if replay.Meta[mcpMetaRequestID] != firstRequestID {
		t.Fatalf("replay must preserve the original executed result metadata: first=%v replay=%v", firstRequestID, replay.Meta[mcpMetaRequestID])
	}
	if fakeMCPStartCount(t, startLog) != 1 {
		t.Fatalf("idempotency replay restarted upstream MCP: starts=%d", fakeMCPStartCount(t, startLog))
	}
}

func TestMCPToolUpstreamIsErrorPassesThroughAndReplays(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	rt.cfg.Discovery.MCP.Enabled = true
	workspace, _ := rt.reg.Get("demo")
	tmp := t.TempDir()
	script := filepath.Join(tmp, "failing_mcp.py")
	startLog := filepath.Join(tmp, "starts.log")
	serverCode := `#!/usr/bin/env python3
import json
import os
import sys

with open(os.environ["MCPX_TEST_START_LOG"], "a", encoding="utf-8") as f:
    f.write("started\n")

def send(message):
    sys.stdout.write(json.dumps(message, separators=(',', ':')) + "\n")
    sys.stdout.flush()

tools = [{"name":"fail", "description":"business failure", "inputSchema":{"type":"object"}}]
for line in sys.stdin:
    try:
        request = json.loads(line)
    except Exception:
        continue
    request_id = request.get("id")
    if request_id is None:
        continue
    method = request.get("method")
    if method == "initialize":
        send({"jsonrpc":"2.0", "id":request_id, "result":{"protocolVersion":"2025-11-25", "capabilities":{"tools":{}}, "serverInfo":{"name":"failing", "version":"1"}}})
    elif method == "tools/list":
        send({"jsonrpc":"2.0", "id":request_id, "result":{"tools":tools}})
    elif method == "tools/call":
        send({"jsonrpc":"2.0", "id":request_id, "result":{
            "content":[{"type":"text", "text":"upstream rejected after partial effect"}],
            "structuredContent":{"code":"UPSTREAM_REJECTED", "partial":True},
            "_meta":{"provider":"fake-failure"},
            "isError":True
        }})
    else:
        send({"jsonrpc":"2.0", "id":request_id, "error":{"code":-32601, "message":"method not found"}})
`
	if err := os.WriteFile(script, []byte(serverCode), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteMCPFile(config.ProjectMCPPath(workspace.Path), config.MCPFile{MCPServers: map[string]config.MCPServer{
		"failing": testMCPServer(t, "Failure contract server", script, map[string]string{"MCPX_TEST_START_LOG": startLog}),
	}}); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	request := map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "exercise upstream business failure", "intent": "test operation",
		"server": "failing", "tool": "fail", "arguments": map[string]any{}, "idempotency_key": "mcp-failure-1",
	}
	failed := callRawToolResult(t, rt.toolHandlers["mcp_tool"], request)
	assertPassthroughResult(t, failed, remoteID, "upstream rejected after partial effect", true, false)
	if failed.Meta["provider"] != "fake-failure" {
		t.Fatalf("ordinary upstream metadata was lost: %+v", failed.Meta)
	}
	structured, _ := failed.StructuredContent.(map[string]any)
	if structured["code"] != "UPSTREAM_REJECTED" || structured["partial"] != true {
		t.Fatalf("upstream structured failure was rewritten: %+v", failed.StructuredContent)
	}
	startsAfterFailure := fakeMCPStartCount(t, startLog)
	rt.cfg.Discovery.MCP.Enabled = false
	replay := callRawToolResult(t, rt.toolHandlers["mcp_tool"], request)
	assertPassthroughResult(t, replay, remoteID, "upstream rejected after partial effect", true, true)
	if fakeMCPStartCount(t, startLog) != startsAfterFailure {
		t.Fatalf("failed idempotency replay restarted upstream MCP: before=%d after=%d", startsAfterFailure, fakeMCPStartCount(t, startLog))
	}
}

func callRawToolResult(t *testing.T, handler mcp.ToolHandler, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	if handler == nil {
		t.Fatal("tool handler is nil")
	}
	result, err := handler(context.Background(), mcpresult.Request(arguments))
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("tool returned nil result")
	}
	return result
}

func assertMixedUpstreamResult(t *testing.T, result *mcp.CallToolResult, value string) {
	t.Helper()
	if len(result.Content) != 2 {
		t.Fatalf("upstream content items=%d want=2: %+v", len(result.Content), result.Content)
	}
	if _, ok := result.Content[1].(*mcp.ImageContent); !ok {
		t.Fatalf("second upstream content item was not preserved as image: %T %+v", result.Content[1], result.Content[1])
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	want := `["upstream",{"value":"` + value + `"}]`
	if string(encoded) != want {
		t.Fatalf("upstream structuredContent was rewritten: got=%s want=%s", encoded, want)
	}
	if result.Meta["provider"] != "fake" {
		t.Fatalf("ordinary upstream metadata was lost: %+v", result.Meta)
	}
}

func assertPassthroughResult(t *testing.T, result *mcp.CallToolResult, remoteID, text string, wantError, replay bool) {
	t.Helper()
	if result.IsError != wantError {
		t.Fatalf("IsError=%v want=%v result=%+v", result.IsError, wantError, result)
	}
	if mcpresult.FirstText(result) != text {
		t.Fatalf("upstream text was rewritten: got=%q want=%q", mcpresult.FirstText(result), text)
	}
	if result.Meta[mcpMetaRemoteSessionID] != remoteID || result.Meta[mcpMetaWorkspace] != "demo" || result.Meta[mcpMetaServer] == nil || result.Meta[mcpMetaTool] == nil {
		t.Fatalf("missing trusted MCPX result metadata: %+v", result.Meta)
	}
	if result.Meta[mcpMetaCallID] == "spoofed-call" || result.Meta[mcpMetaRemoteSessionID] == "spoofed-session" {
		t.Fatalf("upstream spoofed reserved MCPX metadata: %+v", result.Meta)
	}
	if result.Meta[mcpMetaReplay] != replay {
		t.Fatalf("idempotent replay meta=%v want=%v meta=%+v", result.Meta[mcpMetaReplay], replay, result.Meta)
	}
	if result.Meta["mcpx.result"] != nil {
		t.Fatalf("ARC wrapper rewrote transparent mcp_tool result: %+v", result.Meta)
	}
}
