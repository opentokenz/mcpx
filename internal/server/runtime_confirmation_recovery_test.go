package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 经真实 SDK Schema 和工具入口检查确认响应，不能用直接 handler 调用代替。
func TestRuntimeConfirmationRecoveryThroughPublicSchema(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable")
	}
	rt := newWorkspaceRuntime(t, "恢复测试")
	s := operationTestSession(t, rt, "恢复测试")
	rt.cfg.Security.Commands.Default = "confirm"
	rt.cfg.Security.Commands.Allow = nil
	protocol := mcp.NewServer(&mcp.Implementation{Name: "confirmation-recovery", Version: "test"}, nil)
	rt.registerTools(protocol)
	server := httptest.NewServer(NewGateway(rt.cfg, nil, mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return protocol }, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true, Stateless: true})).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "recovery-client", Version: "test"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return decodeToolResult(t, result)
	}
	script := "require('fs').appendFileSync('效果.txt', '一次\\n', 'utf8');\n"
	request := map[string]any{"action": "run", "remote_session_id": s.ID, "purpose": "验证跨轮确认只执行一次", "scope": "workspace", "runtime": "node", "idempotency_key": "r12-recovery", "script": script}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	// 调用方复用 edit/read 保存并独立回读；不增加 Runtime 正文存储。
	for path, content := range map[string]string{"execution/r12-recovery/script.js": script, "execution/r12-recovery/request.json": string(body)} {
		saved := call("edit", map[string]any{"remote_session_id": s.ID, "purpose": "保存获准的非敏感原始请求", "edits": []any{map[string]any{"path": path, "operation": "create", "content": content}}})
		if !statusOK(saved) {
			t.Fatalf("保存失败: %+v", saved)
		}
	}
	readOriginal := func(path, expected string) ([]byte, bool) {
		t.Helper()
		read := call("read", map[string]any{"remote_session_id": s.ID, "view": "file", "path": path})
		if !statusOK(read) {
			return nil, false
		}
		data, _ := read["data"].(map[string]any)
		content, _ := data["content"].(string)
		return []byte(content), content == expected && data["truncated"] != true
	}
	if _, ok := readOriginal("execution/r12-recovery/script.js", script); !ok {
		t.Fatal("发送前脚本回读失败")
	}
	if _, ok := readOriginal("execution/r12-recovery/request.json", string(body)); !ok {
		t.Fatal("发送前请求回读失败")
	}
	waiting := call("execute", request)
	if waiting["status"] != "waiting_confirmation" {
		t.Fatalf("未等待确认: %+v", waiting)
	}
	data := waiting["data"].(map[string]any)
	if data["script_sha256"] != fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(script))) || data["script_bytes"] != float64(len(script)) || data["pending_digest"] == "" {
		t.Fatalf("原始脚本摘要缺失: %+v", data)
	}
	encoded, _ := json.Marshal(waiting)
	for _, forbidden := range []string{"next_action", `"recovery"`, `"note"`, "appendFileSync"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("确认响应含无效重试模板或脚本正文: %s", forbidden)
		}
	}
	output := filepath.Join(s.WorkspacePath, "效果.txt")
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("确认前产生执行效果")
	}
	// 仅调用方故障夹具：缺失、正文被改和 read 拒绝都须在 execute 之前停止。
	for _, fault := range []string{"missing", "changed", "denied"} {
		t.Run(fault, func(t *testing.T) {
			path := "execution/r12-recovery/script.js"
			switch fault {
			case "missing":
				path = "execution/r12-recovery/missing.js"
			case "changed":
				if err := os.WriteFile(filepath.Join(s.WorkspacePath, path), []byte(script+"// changed\n"), 0600); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := os.WriteFile(filepath.Join(s.WorkspacePath, path), []byte(script), 0600); err != nil {
						t.Fatal(err)
					}
				}()
			case "denied":
				original := rt.cfg.Security.Files.Deny
				rt.cfg.Security.Files.Deny = append(append([]string(nil), original...), `^execution/`)
				defer func() { rt.cfg.Security.Files.Deny = original }()
			}
			if _, ok := readOriginal(path, script); ok {
				t.Fatal("故障回读错误地允许继续执行")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("原请求无法恢复时产生执行效果")
			}
		})
	}
	request = nil
	restoredBody, ok := readOriginal("execution/r12-recovery/request.json", string(body))
	if !ok || sha256.Sum256(restoredBody) != sha256.Sum256(body) {
		t.Fatal("原始请求回读校验失败")
	}
	if _, ok := readOriginal("execution/r12-recovery/script.js", script); !ok {
		t.Fatal("跨轮脚本回读失败")
	}
	if err := json.Unmarshal(restoredBody, &request); err != nil {
		t.Fatal(err)
	}
	request["user_confirmed"] = true
	first := call("execute", request)
	if !statusOK(first) {
		t.Fatalf("原始请求无法通过公共 Schema 并执行: %+v", first)
	}
	taskID := first["data"].(map[string]any)["execution_task_id"]
	if taskID == nil || taskID == "" {
		t.Fatal("缺少 Task 身份")
	}
	replay := call("execute", request)
	if !statusOK(replay) || replay["data"].(map[string]any)["execution_task_id"] != taskID || replay["data"].(map[string]any)["idempotent_replay"] != true {
		t.Fatalf("重试未回读同一 Task: %+v", replay)
	}
	request["script"] = script + "require('fs').appendFileSync('效果.txt', '错误');\n"
	changed := call("execute", request)
	if errorCode(changed) != "idempotency_conflict" {
		t.Fatalf("同 key 改写脚本未拒绝: %+v", changed)
	}
	observed := call("observe", map[string]any{"remote_session_id": s.ID, "view": "task", "execution_task_id": taskID})
	if !statusOK(observed) {
		t.Fatalf("原 Task 不可回读: %+v", observed)
	}
	effect, err := os.ReadFile(output)
	if err != nil || string(effect) != "一次\n" {
		t.Fatalf("效果并非恰好一次: %q, %v", effect, err)
	}
}
