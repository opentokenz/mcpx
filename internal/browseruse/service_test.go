package browseruse

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestServiceBridgesElicitationWithoutAutoApproval(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not available")
	}
	dir := t.TempDir()
	fakeService := filepath.Join(dir, "browser-service.mjs")
	if err := os.WriteFile(fakeService, []byte("export async function handleRpc() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeCodex := filepath.Join(dir, "codex.exe")
	if err := os.WriteFile(fakeCodex, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeNodeRepl := filepath.Join(dir, "fake-node-repl.mjs")
	const fakeNodeReplSource = `
import readline from "node:readline";

const RESPONSE_PREFIX = "__MCPX_BROWSER_RESPONSE__";
let counter = 0;
let nextRequestId = 1000;
const pendingElicitations = new Map();

function send(message) {
  process.stdout.write(JSON.stringify(message) + "\n");
}

function decodeSidecarRequest(code) {
  const match = String(code).match(/const encodedRequest = "([A-Za-z0-9+/=]+)";/);
  if (!match) throw new Error("encoded sidecar request not found");
  return JSON.parse(Buffer.from(match[1], "base64").toString("utf8"));
}

function finishToolCall(id, envelope, meta = {}, extraContent = []) {
  send({
    jsonrpc: "2.0",
    id,
    result: {
      content: [...extraContent, { type: "text", text: RESPONSE_PREFIX + JSON.stringify(envelope) }],
      isError: false,
      _meta: meta,
    },
  });
}

function runCommand(message) {
  const request = decodeSidecarRequest(message.params?.arguments?.code);
  const command = request.command ?? {};
  if (command.type === "permission_probe") {
    const requestId = nextRequestId++;
    pendingElicitations.set(requestId, { callId: message.id, request });
    send({
      jsonrpc: "2.0",
      id: requestId,
      method: "elicitation/create",
      params: {
        _meta: {
          codex_approval_kind: "mcp_tool_call",
          codex_sensitive_action: true,
          origin: "https://example.com",
          progressToken: 0,
          "x-codex-turn-metadata": message.params?._meta?.["x-codex-turn-metadata"],
        },
        message: "Allow Browser use to access https://example.com?",
        mode: "form",
        requestedSchema: { type: "object", properties: {} },
      },
    });
    return;
  }
  if (command.type === "counter") {
    counter++;
    finishToolCall(
      message.id,
      { ok: true, result: { value: counter, browser_id: "browser-1" } },
      {
        "codex/browserUse": true,
        "codex/toolSurface": {
          backend: "chrome",
          browserId: "browser-1",
          screenshot: { pageUrl: "https://example.com", tabId: "7", url: "data:image/jpeg;base64,AAAA" },
        },
      },
      [{ type: "text", text: "auxiliary content" }],
    );
    return;
  }
  if (command.type === "tab_screenshot") {
    finishToolCall(
      message.id,
      { ok: true, result: { browser_id: "browser-1" } },
      {
        "codex/browserUse": true,
        "codex/toolSurface": {
          backend: "chrome",
          browserId: "browser-1",
          screenshot: { pageUrl: "https://example.com", tabId: "7", url: "data:image/jpeg;base64,BBBB" },
        },
      },
    );
    return;
  }
  finishToolCall(message.id, {
    ok: false,
    result: null,
    error: { name: "Error", message: "unexpected command " + String(command.type) },
  });
}

const lines = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
for await (const line of lines) {
  if (!line.trim()) continue;
  const message = JSON.parse(line);
  if (message.method === "initialize" && message.id != null) {
    send({
      jsonrpc: "2.0",
      id: message.id,
      result: { protocolVersion: "2025-06-18", capabilities: { tools: {} }, serverInfo: { name: "fake-node-repl", version: "1" } },
    });
    continue;
  }
  if (message.method === "notifications/initialized") continue;
  if (message.method === "tools/call" && message.id != null) {
    runCommand(message);
    continue;
  }
  if (message.id != null && pendingElicitations.has(message.id)) {
    const pending = pendingElicitations.get(message.id);
    pendingElicitations.delete(message.id);
    if (message.result?.action === "accept") {
      finishToolCall(pending.callId, { ok: true, result: { allowed: true, browser_id: "browser-1" } });
    } else {
      finishToolCall(pending.callId, {
        ok: false,
        result: null,
        error: { name: "Error", message: "permission declined" },
      });
    }
  }
}
`
	if err := os.WriteFile(fakeNodeRepl, []byte(fakeNodeReplSource), 0o600); err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal([]string{fakeNodeRepl})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCPX_BROWSER_SERVICE_PATH", fakeService)
	t.Setenv("MCPX_NODE_REPL_PATH", node)
	t.Setenv("MCPX_NODE_PATH", node)
	t.Setenv("MCPX_CODEX_CLI_PATH", fakeCodex)
	t.Setenv("MCPX_NODE_REPL_ARGS", string(args))

	service := NewService()
	t.Cleanup(func() { _ = service.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	first, err := service.Execute(ctx, ServiceRequest{
		SessionID: "session", TurnID: "turn", BrowserInstanceID: "instance-1",
		Command: map[string]any{"type": "permission_probe"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Error == nil || len(first.Elicitations) != 1 {
		t.Fatalf("first response must fail closed with one elicitation: %+v", first)
	}
	prompt := first.Elicitations[0]
	if prompt.Fingerprint == "" || prompt.Message == "" {
		t.Fatalf("elicitation missing fingerprint/message: %+v", prompt)
	}
	if prompt.Meta["origin"] != "https://example.com" {
		t.Fatalf("elicitation meta = %+v", prompt.Meta)
	}
	if _, leaked := prompt.Meta["x-codex-turn-metadata"]; leaked {
		t.Fatalf("internal turn metadata must not affect approval fingerprint/meta: %+v", prompt.Meta)
	}

	second, err := service.Execute(ctx, ServiceRequest{
		SessionID: "session", TurnID: "turn", BrowserInstanceID: "instance-1",
		Command:              map[string]any{"type": "permission_probe"},
		ApprovedFingerprints: []string{prompt.Fingerprint},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Error != nil || len(second.Elicitations) != 0 {
		t.Fatalf("approved response = %+v", second)
	}
	result, _ := second.Result.(map[string]any)
	if result["allowed"] != true || result["browser_id"] != "browser-1" {
		t.Fatalf("approved result = %+v", second.Result)
	}

	one, err := service.Execute(ctx, ServiceRequest{SessionID: "session", TurnID: "turn", BrowserInstanceID: "instance-1", Command: map[string]any{"type": "counter"}})
	if err != nil || one.Error != nil {
		t.Fatalf("counter one = %+v err=%v", one, err)
	}
	two, err := service.Execute(ctx, ServiceRequest{SessionID: "session", TurnID: "turn", BrowserInstanceID: "instance-1", Command: map[string]any{"type": "counter"}})
	if err != nil || two.Error != nil {
		t.Fatalf("counter two = %+v err=%v", two, err)
	}
	oneResult, _ := one.Result.(map[string]any)
	twoResult, _ := two.Result.(map[string]any)
	if oneResult["value"] != float64(1) || twoResult["value"] != float64(2) {
		t.Fatalf("sidecar process must preserve node_repl state: one=%+v two=%+v", oneResult, twoResult)
	}
	if one.ResponseMeta["codex/browserUse"] != true || len(one.ContentItems) != 1 {
		t.Fatalf("Browser Service metadata/content items were not preserved: %+v", one)
	}

	if err := service.Close(); err != nil {
		t.Fatalf("reset service: %v", err)
	}
	restarted, err := service.Execute(ctx, ServiceRequest{SessionID: "session", TurnID: "turn", BrowserInstanceID: "instance-1", Command: map[string]any{"type": "counter"}})
	if err != nil || restarted.Error != nil {
		t.Fatalf("counter after reset = %+v err=%v", restarted, err)
	}
	restartedResult, _ := restarted.Result.(map[string]any)
	if restartedResult["value"] != float64(1) {
		t.Fatalf("service reset must start a fresh sidecar: %+v", restartedResult)
	}
	toolSurface, _ := one.ResponseMeta["codex/toolSurface"].(map[string]any)
	screenshotMeta, _ := toolSurface["screenshot"].(map[string]any)
	if screenshotMeta["url"] != nil || screenshotMeta["pageUrl"] != "https://example.com" || screenshotMeta["tabId"] != "7" {
		t.Fatalf("non-screenshot action leaked screenshot data URI or lost compact metadata: %+v", one.ResponseMeta)
	}

	screenshot, err := service.Execute(ctx, ServiceRequest{SessionID: "session", TurnID: "turn", BrowserInstanceID: "instance-1", Command: map[string]any{"type": "tab_screenshot", "tab_id": "7"}})
	if err != nil || screenshot.Error != nil {
		t.Fatalf("screenshot = %+v err=%v", screenshot, err)
	}
	screenshotSurface, _ := screenshot.ResponseMeta["codex/toolSurface"].(map[string]any)
	screenshotData, _ := screenshotSurface["screenshot"].(map[string]any)
	if screenshotData["url"] != "data:image/jpeg;base64,BBBB" {
		t.Fatalf("screenshot action must preserve screenshot data URI: %+v", screenshot.ResponseMeta)
	}
}
