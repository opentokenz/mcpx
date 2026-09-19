package server

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
	"mcpx/internal/mcpresult"
)

// TestCleanCoreSkillToolIdempotentReplaySurvivesRevisionChange ensures a
// completed skill_tool call replays its persisted result even when the skill
// definition changed after the original call: preflight must not run for a
// replay, otherwise the stale revision check would reject the retry.
func TestCleanCoreSkillToolIdempotentReplaySurvivesRevisionChange(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	rt.cfg.Discovery.Skills.Enabled = true
	rt.cfg.Discovery.Skills.Dirs = []string{".skills"}
	skillDir := filepath.Join(workspace.Path, ".skills", "docs")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: docs\ndescription: return documentation\nruntime: markdown\n---\n\n# Docs\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	described := callEnvelope(t, rt.toolSkillTool, context.Background(), map[string]any{
		"action": "describe", "remote_session_id": remoteID, "name": "docs",
	})
	if !statusOK(described) {
		t.Fatalf("skill_tool describe=%+v", described)
	}

	request := map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "read the docs",
		"name": "docs", "arguments": map[string]any{}, "idempotency_key": "skill-docs-replay-1",
	}
	first := callEnvelope(t, rt.toolSkillTool, context.Background(), request)
	if !statusOK(first) {
		t.Fatalf("skill_tool call=%+v", first)
	}

	// The skill changes after the original call. The same-key retry must still
	// replay the persisted result instead of failing the revision preflight.
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: docs\ndescription: changed\nruntime: markdown\n---\n\n# Docs v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	replay := callEnvelope(t, rt.toolSkillTool, context.Background(), request)
	if !statusOK(replay) || replay["data"].(map[string]any)["idempotent_replay"] != true {
		t.Fatalf("same-key replay after revision change=%+v", replay)
	}

	// A fresh key must still observe the revision change.
	fresh := cloneMap(request)
	fresh["idempotency_key"] = "skill-docs-fresh-2"
	changed := callEnvelope(t, rt.toolSkillTool, context.Background(), fresh)
	if statusOK(changed) || errorCode(changed) != "skill_revision_changed" {
		t.Fatalf("fresh call after revision change=%+v", changed)
	}
}

// writeFakeMCPTestServer registers a minimal echo MCP server for the workspace
// and returns the start log path used to count upstream process launches.
func writeFakeMCPTestServer(t *testing.T, workspacePath string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "fake_mcp.py")
	serverCode := `#!/usr/bin/env python3
import json
import os
import sys

start_log = os.environ.get("MCPX_TEST_START_LOG")
if start_log:
    with open(start_log, "a", encoding="utf-8") as f:
        f.write("started\n")

def send(message):
    sys.stdout.write(json.dumps(message, separators=(',', ':')) + "\n")
    sys.stdout.flush()

tools = [{"name": "echo", "description": "echo a value", "inputSchema": {"type": "object", "properties": {"value": {"type": "string"}}, "required": ["value"]}}]
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
        send({"jsonrpc": "2.0", "id": request_id, "result": {"protocolVersion": "2025-11-25", "capabilities": {"tools": {}}, "serverInfo": {"name": "fake", "version": "1"}}})
    elif method == "tools/list":
        send({"jsonrpc": "2.0", "id": request_id, "result": {"tools": tools}})
    elif method == "tools/call":
        value = request.get("params", {}).get("arguments", {}).get("value", "")
        send({"jsonrpc": "2.0", "id": request_id, "result": {"content": [{"type": "text", "text": "echo:" + value}], "isError": False}})
    else:
        send({"jsonrpc": "2.0", "id": request_id, "error": {"code": -32601, "message": "method not found"}})
`
	if err := os.WriteFile(script, []byte(serverCode), 0o700); err != nil {
		t.Fatal(err)
	}
	startLog := filepath.Join(t.TempDir(), "fake-mcp-starts.log")
	if err := config.WriteMCPFile(config.ProjectMCPPath(workspacePath), config.MCPFile{MCPServers: map[string]config.MCPServer{
		"fake": testMCPServer(t, "Echo values for contract tests", script, map[string]string{"MCPX_TEST_START_LOG": startLog}),
	}}); err != nil {
		t.Fatal(err)
	}
	return startLog
}

// TestCleanCoreMCPToolIdempotentReplayDoesNotRestartUpstream ensures an
// idempotent replay of a completed mcp_tool call returns the persisted result
// without starting a new upstream MCP process.
func TestCleanCoreMCPToolIdempotentReplayDoesNotRestartUpstream(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	rt.cfg.Discovery.MCP.Enabled = true
	workspace, _ := rt.reg.Get("demo")
	startLog := writeFakeMCPTestServer(t, workspace.Path)
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	described := callEnvelope(t, rt.toolMCPTool, context.Background(), map[string]any{
		"action": "describe", "remote_session_id": remoteID, "server": "fake", "tool": "echo",
	})
	if !statusOK(described) {
		t.Fatalf("mcp_tool describe=%+v", described)
	}

	request := map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "call the fake MCP",
		"server": "fake", "tool": "echo", "arguments": map[string]any{"value": "one"},
		"idempotency_key": "mcp-echo-replay-1",
	}
	completed := callEnvelope(t, rt.toolMCPTool, context.Background(), request)
	if !statusOK(completed) {
		t.Fatalf("mcp_tool call must execute without server confirmation=%+v", completed)
	}
	startsAfterCall := fakeMCPStartCount(t, startLog)

	replay := callEnvelope(t, rt.toolMCPTool, context.Background(), request)
	if !statusOK(replay) || replay["data"].(map[string]any)["idempotent_replay"] != true {
		t.Fatalf("mcp_tool idempotent replay=%+v", replay)
	}
	if startsAfterReplay := fakeMCPStartCount(t, startLog); startsAfterReplay != startsAfterCall {
		t.Fatalf("idempotent replay must not restart upstream: before=%d after=%d", startsAfterCall, startsAfterReplay)
	}
}

// TestCleanCoreMCPToolIdempotentReplaySurvivesConfigRemoval ensures a
// completed mcp_tool call replays its persisted result even when the MCP
// configuration changed after the original call. Replay must not resolve the
// current MCP config (a removed server or disabled discovery) nor start the
// upstream process; only a request that actually executes needs the current
// environment.
func TestCleanCoreMCPToolIdempotentReplaySurvivesConfigRemoval(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, rt *Runtime, workspacePath string)
	}{
		{
			name: "server configuration removed",
			mutate: func(t *testing.T, _ *Runtime, workspacePath string) {
				t.Helper()
				if err := config.WriteMCPFile(config.ProjectMCPPath(workspacePath), config.MCPFile{}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "discovery disabled",
			mutate: func(t *testing.T, rt *Runtime, _ string) {
				t.Helper()
				rt.cfg.Discovery.MCP.Enabled = false
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newWorkspaceRuntime(t, "demo")
			rt.cfg.Discovery.MCP.Enabled = true
			workspace, _ := rt.reg.Get("demo")
			startLog := writeFakeMCPTestServer(t, workspace.Path)
			opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
			remoteID := opened["remote_session_id"].(string)

			described := callEnvelope(t, rt.toolMCPTool, context.Background(), map[string]any{
				"action": "describe", "remote_session_id": remoteID, "server": "fake", "tool": "echo",
			})
			if !statusOK(described) {
				t.Fatalf("mcp_tool describe=%+v", described)
			}

			request := map[string]any{
				"action": "call", "remote_session_id": remoteID, "purpose": "call the fake MCP",
				"server": "fake", "tool": "echo", "arguments": map[string]any{"value": "one"},
				"idempotency_key": "mcp-echo-replay-config-1",
			}
			completed := callEnvelope(t, rt.toolMCPTool, context.Background(), request)
			if !statusOK(completed) {
				t.Fatalf("mcp_tool call must execute without server confirmation=%+v", completed)
			}
			startsAfterCall := fakeMCPStartCount(t, startLog)

			tc.mutate(t, rt, workspace.Path)

			replay := callEnvelope(t, rt.toolMCPTool, context.Background(), request)
			if !statusOK(replay) || replay["data"].(map[string]any)["idempotent_replay"] != true {
				t.Fatalf("mcp_tool idempotent replay after config change=%+v", replay)
			}
			if startsAfterReplay := fakeMCPStartCount(t, startLog); startsAfterReplay != startsAfterCall {
				t.Fatalf("idempotent replay must not restart upstream: before=%d after=%d", startsAfterCall, startsAfterReplay)
			}

			// A fresh key with the changed config must observe the current
			// environment: the removed/disabled server is not reachable.
			fresh := cloneMap(request)
			fresh["idempotency_key"] = "mcp-echo-fresh-config-2"
			changed := callEnvelope(t, rt.toolMCPTool, context.Background(), fresh)
			if statusOK(changed) {
				t.Fatalf("fresh call after config change must fail: %+v", changed)
			}
		})
	}
}

// TestCleanCoreMCPToolConcurrentSameKeyRunsSingleUpstream ensures concurrent
// retries with the same idempotency_key merge before semantic preflight: only
// the durable owner starts the upstream MCP process, and the merged request
// replays the persisted result.
func TestCleanCoreMCPToolConcurrentSameKeyRunsSingleUpstream(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	rt.cfg.Discovery.MCP.Enabled = true
	workspace, _ := rt.reg.Get("demo")
	startLog := writeFakeMCPTestServer(t, workspace.Path)
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	described := callEnvelope(t, rt.toolMCPTool, context.Background(), map[string]any{
		"action": "describe", "remote_session_id": remoteID, "server": "fake", "tool": "echo",
	})
	if !statusOK(described) {
		t.Fatalf("mcp_tool describe=%+v", described)
	}

	request := map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "call the fake MCP",
		"server": "fake", "tool": "echo", "arguments": map[string]any{"value": "one"},
		"idempotency_key": "mcp-echo-concurrent-1",
	}
	before := fakeMCPStartCount(t, startLog)
	raw := make([]*mcp.CallToolResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range raw {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			args := cloneMap(request)
			if _, exists := args["intent"]; !exists {
				args["intent"] = "test operation"
			}
			raw[i], errs[i] = rt.toolMCPTool(context.Background(), mcpresult.Request(args))
		}(i)
	}
	wg.Wait()
	replays := 0
	for i, result := range raw {
		if errs[i] != nil {
			t.Fatalf("concurrent same-key call %d error=%v", i, errs[i])
		}
		res := decodeToolResult(t, result)
		if !statusOK(res) {
			t.Fatalf("concurrent same-key call %d=%+v", i, res)
		}
		if data, ok := res["data"].(map[string]any); ok && data["idempotent_replay"] == true {
			replays++
		}
	}
	if replays != 1 {
		t.Fatalf("exactly one merged call must replay: %d replays", replays)
	}
	if after := fakeMCPStartCount(t, startLog); after != before+1 {
		t.Fatalf("concurrent same-key calls must start upstream once: before=%d after=%d", before, after)
	}
}
