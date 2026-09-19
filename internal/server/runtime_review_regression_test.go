package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"mcpx/internal/edit"
	"mcpx/internal/envelope"
	"mcpx/internal/file"
	"mcpx/internal/idempotency"
	"mcpx/internal/operation"
	"mcpx/internal/winproc"
)

func TestReviewGitOutputRequiresConfirmation(t *testing.T) {
	rt := newWorkspaceRuntime(t, "review")
	rt.cfg.Security.Commands.Default = "confirm"
	rt.cfg.Security.Commands.Allow = nil
	s := operationTestSession(t, rt, "review")
	for name, body := range map[string]string{"left.txt": "a\n", "right.txt": "b\n"} {
		if err := os.WriteFile(filepath.Join(s.WorkspacePath, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	args := map[string]any{"remote_session_id": s.ID, "action": "run", "purpose": "输出选项副作用回归", "argv": []any{"git", "diff", "--no-index", "--output=probe.diff", "left.txt", "right.txt"}, "shell": false, "idempotency_key": "git-output"}
	first := callOperationTool(t, rt, "execute", args)
	if first["status"] != "waiting_confirmation" {
		t.Fatalf("确认被绕过: %+v", first)
	}
	if _, err := os.Stat(filepath.Join(s.WorkspacePath, "probe.diff")); !os.IsNotExist(err) {
		t.Fatal("确认前产生输出文件")
	}
	args["user_confirmed"] = true
	callOperationTool(t, rt, "execute", args)
	content, err := os.ReadFile(filepath.Join(s.WorkspacePath, "probe.diff"))
	if err != nil || len(content) == 0 {
		t.Fatalf("确认后没有 Git 输出: %v", err)
	}
}

func TestReviewEditCanonicalPolicy(t *testing.T) {
	for _, alias := range []string{"dotdot", "symlink", "junction"} {
		t.Run(alias, func(t *testing.T) {
			rt := newWorkspaceRuntime(t, "review")
			s := operationTestSession(t, rt, "review")
			rt.cfg.Security.Files.Allow = []string{`^public/`}
			rt.cfg.Security.Files.Deny = []string{`^private/`}
			if err := os.MkdirAll(filepath.Join(s.WorkspacePath, "private"), 0700); err != nil {
				t.Fatal(err)
			}
			path := "public/../private/new.txt"
			if alias == "symlink" {
				if err := os.Symlink(filepath.Join(s.WorkspacePath, "private"), filepath.Join(s.WorkspacePath, "public")); err != nil {
					t.Skipf("当前平台 symlink 不可用: %v", err)
				}
				path = "public/new.txt"
			}
			if alias == "junction" {
				if runtime.GOOS != "windows" {
					t.Skip("Windows junction fixture")
				}
				command := exec.Command("cmd.exe", "/c", "mklink", "/J", filepath.Join(s.WorkspacePath, "public"), filepath.Join(s.WorkspacePath, "private"))
				winproc.ConfigureNoWindow(command)
				if err := command.Run(); err != nil {
					t.Fatalf("创建隔离 junction: %v", err)
				}
				path = "public/new.txt"
			}
			result := callOperationTool(t, rt, "edit", map[string]any{"remote_session_id": s.ID, "purpose": "物理路径策略回归", "edits": []any{map[string]any{"path": path, "operation": "create", "content": "不得写入"}}})
			if result["status"] != "failed" {
				resolved, _ := file.Resolve(s.WorkspacePath, path)
				t.Fatalf("拒绝区写入未拦截: resolved=%q deny=%v result=%+v", resolved, rt.effectiveConfig(s.WorkspacePath).Security.Files.Deny, result)
			}
			if _, err := os.Stat(filepath.Join(s.WorkspacePath, "private", "new.txt")); !os.IsNotExist(err) {
				t.Fatal("拒绝区发生写入")
			}
		})
	}
}

func TestReviewSDKConfirmationKeepsDAGWaiting(t *testing.T) {
	rt := newWorkspaceRuntime(t, "review")
	s := operationTestSession(t, rt, "review")
	rt.cfg.Security.Commands.Default = "confirm"
	rt.cfg.Security.Commands.Allow = nil
	protocol := mcp.NewServer(&mcp.Implementation{Name: "review-confirm", Version: "test"}, nil)
	rt.registerTools(protocol)
	httpServer := httptest.NewServer(NewGateway(rt.cfg, nil, mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return protocol }, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true, Stateless: true})).Handler())
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "review-client", Version: "test"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpServer.URL + "/mcp"}, nil)
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
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(s.WorkspacePath, "once.json")
	args := map[string]any{"remote_session_id": s.ID, "purpose": "确认 DAG 回归", "operation_id": "confirm-dag", "operations": []any{
		map[string]any{"id": "a", "tool": "execute", "arguments": map[string]any{"action": "run", "argv": []any{executable, "-test.run=^TestIssue823ArgvChild$", "--", "--issue823-argv-child", output, "once"}, "shell": false, "idempotency_key": "confirm-dag-a"}},
		map[string]any{"id": "b", "tool": "read", "depends_on": []any{"a"}, "arguments": map[string]any{"view": "file", "path": "once.json"}},
	}}
	accepted := call("operation_batch", args)
	if accepted["status"] != "accepted" {
		t.Fatalf("submit: %+v", accepted)
	}
	waitArgs := map[string]any{"remote_session_id": s.ID, "operation_id": "confirm-dag", "action": "wait", "timeout_ms": 4000}
	wireWaiting := call("operation_manage", waitArgs)
	wireData, ok := wireWaiting["data"].(map[string]any)
	if !ok {
		t.Fatalf("HTTP wait 无数据: %+v", wireWaiting)
	}
	wireSteps, ok := wireData["steps"].([]any)
	if !ok || len(wireSteps) != 2 {
		t.Fatalf("HTTP wait 无步骤: %+v", wireData)
	}
	wireA := wireSteps[0].(map[string]any)
	wireToken, ok := wireA["confirmation_token"].(string)
	if !ok || wireToken == "" || wireA["state"] != "waiting_confirmation" || wireSteps[1].(map[string]any)["state"] != "queued" {
		t.Fatal("HTTP 等待回执缺恢复字段")
	}
	waiting, err := rt.operations.Get(ctx, "confirm-dag")
	if err != nil || waiting.State != operation.StateWaitingConfirmation {
		t.Fatalf("提前成功: %s %v", waiting.State, err)
	}
	if waiting.Steps[1].State != operation.StateQueued {
		t.Fatal("后继未等待")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("确认前启动子进程")
	}
	resumed := call("operation_manage", map[string]any{"remote_session_id": s.ID, "operation_id": "confirm-dag", "action": "resume", "step_id": "a", "confirmation_token": wireToken})
	if resumed["status"] == "failed" {
		t.Fatalf("resume: %+v", resumed)
	}
	call("operation_manage", waitArgs)
	final, err := rt.operations.Get(ctx, "confirm-dag")
	if err != nil || final.State != operation.StateSucceeded {
		t.Fatalf("未完成: %+v err=%v", final, err)
	}
	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var once []string
	if json.Unmarshal(content, &once) != nil || !reflect.DeepEqual(once, []string{"once"}) {
		t.Fatal("效果非唯一")
	}
}

func TestReviewEditCrashReadbackRecovery(t *testing.T) {
	rt := newWorkspaceRuntime(t, "review")
	s := operationTestSession(t, rt, "review")
	ctx := context.Background()
	p, _ := rt.principalFromContext(ctx)
	edits := []edit.FileEdit{{Path: "crash.txt", Operation: edit.OpCreate, Content: "恢复字节\r\n"}}
	fingerprint := cleanEditFingerprint(envelope.Request{}, edits)
	key := idempotency.Key{RemoteSessionID: s.ID, PrincipalID: p.ID, Operation: cleanEditIdempotencyOperation, Value: "crash-receipt"}
	if _, err := rt.idempotency.Claim(ctx, key, fingerprint, time.Hour); err != nil {
		t.Fatal(err)
	}
	_, err := edit.ApplyBatchWithHook(edit.BatchRequest{WorkspaceRoot: s.WorkspacePath, Edits: edits}, func(prepared edit.BatchResult) error {
		encoded, err := json.Marshal(storedEditResult{EditID: "fixture-crash", Result: prepared})
		if err != nil {
			return err
		}
		return rt.idempotency.UpdatePending(ctx, key, fingerprint, encoded, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	// 注入写后、Complete 前失联：仅更换 fixture 幂等存储的内存实例。
	if err := rt.idempotency.MarkInDoubt(ctx, key, fingerprint, nil); err != nil {
		t.Fatal(err)
	}
	rt.idempotency = idempotency.NewStore(rt.state.DB())
	args := map[string]any{"remote_session_id": s.ID, "purpose": "写后失联恢复", "idempotency_key": key.Value, "edits": []any{map[string]any{"path": "crash.txt", "operation": "create", "content": "恢复字节\r\n"}}}
	if _, err := rt.state.DB().Exec(`CREATE TRIGGER fixture_fail_complete BEFORE UPDATE ON clean_idempotency_records WHEN NEW.state='succeeded' BEGIN SELECT RAISE(FAIL, 'fixture complete failure'); END`); err != nil {
		t.Fatal(err)
	}
	failed := callOperationTool(t, rt, "edit", args)
	if failed["status"] != "failed" {
		t.Fatal("Complete 失败仍返回成功")
	}
	if _, err := rt.state.DB().Exec(`DROP TRIGGER fixture_fail_complete`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result := callOperationTool(t, rt, "edit", args)
		if result["status"] != "succeeded" {
			t.Fatalf("recovery: %+v", result)
		}
	}
	record, exists, err := rt.idempotency.Lookup(ctx, key)
	if err != nil || !exists || record.State != idempotency.StateSucceeded {
		t.Fatal("回执未持久化")
	}
	stored, err := decodeStoredEdit(record.Response)
	if err != nil {
		t.Fatal(err)
	}
	rb := stored.Result.Results[0].Readback
	content, _ := os.ReadFile(filepath.Join(s.WorkspacePath, "crash.txt"))
	if rb == nil || rb.ByteLength != len(content) || rb.SHA256 != stored.Result.Results[0].NewSHA256 || rb.Format.LineEnding != "CRLF" || rb.TailHex == "" {
		t.Fatalf("恢复回执缺完整字节证明: %+v", rb)
	}
}

func TestReviewDelayedChild(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "--review-delayed-child" {
			continue
		}
		if err := os.WriteFile(os.Args[i+1], []byte("started"), 0600); err != nil {
			os.Exit(2)
		}
		time.Sleep(1500 * time.Millisecond)
		if err := os.WriteFile(os.Args[i+2], []byte("late"), 0600); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
}

func TestReviewCancelStopsActualTaskAndFreezesTerminal(t *testing.T) {
	rt := newWorkspaceRuntime(t, "review")
	s := operationTestSession(t, rt, "review")
	rt.cfg.Security.Commands.Default = "allow"
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(s.WorkspacePath, "started")
	late := filepath.Join(s.WorkspacePath, "late")
	accepted := callOperationTool(t, rt, "execute", map[string]any{"remote_session_id": s.ID, "purpose": "取消真实 Task 回归", "action": "run", "execution_mode": "async", "yield_time_ms": 1, "argv": []any{executable, "-test.run=^TestReviewDelayedChild$", "--", "--review-delayed-child", started, late}, "shell": false})
	if accepted["status"] != "accepted" {
		t.Fatalf("未提交: %+v", accepted)
	}
	id := acceptedOperationID(accepted)
	if id == "" {
		t.Fatalf("operation id missing: %+v", accepted)
	}
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("子进程未启动")
		}
		time.Sleep(5 * time.Millisecond)
	}
	callOperationTool(t, rt, "operation_manage", map[string]any{"remote_session_id": s.ID, "operation_id": id, "action": "cancel"})
	final, timeout, err := rt.operations.Wait(context.Background(), id, time.Second)
	if err != nil || timeout || final.State != operation.StateCancelled {
		t.Fatalf("取消未收敛: %s %v", final.State, err)
	}
	time.Sleep(1600 * time.Millisecond)
	if _, err := os.Stat(late); !os.IsNotExist(err) {
		t.Fatal("取消终态之后仍产生写入")
	}
	after, err := rt.operations.Get(context.Background(), id)
	if err != nil || !reflect.DeepEqual(final, after) {
		t.Fatal("终态快照被迟到结果改写")
	}
}
