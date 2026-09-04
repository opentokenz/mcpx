package server

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"
	"mcpx/internal/remotesession"
)

func TestCompactSessionAndTaskObservationStayBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("large stderr fixture uses Unix tools")
	}
	rt := newWorkspaceRuntime(t, "demo")
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	workspace, _ := rt.reg.Get("demo")
	created, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{
		WorkspaceName: "demo", WorkspacePath: workspace.Path, Label: "cross-chat fixture",
	})
	if err != nil {
		t.Fatal(err)
	}

	wrappedSession := rt.instrumentTool("session", rt.toolSession)
	compact, err := wrappedSession(context.Background(), mcpresult.Request(map[string]any{"remote_session_id": created.Session.ID}))
	if err != nil {
		t.Fatal(err)
	}
	compactBytes := serializedResultBytes(t, compact)
	if compactBytes > 32<<10 {
		t.Fatalf("compact session bytes=%d, want <=32768", compactBytes)
	}
	compactData := structuredBusinessData(compact)
	if compactData["tools"] != nil || compactData["extension_inventory"] != nil {
		t.Fatalf("compact session repeated inventories: %+v", compactData)
	}
	guidance, _ := json.Marshal(compactData["agent_guidance"])
	if len(guidance) > 2<<10 {
		t.Fatalf("compact guidance bytes=%d, want <=2048", len(guidance))
	}
	repeat, err := wrappedSession(context.Background(), mcpresult.Request(map[string]any{
		"remote_session_id": created.Session.ID,
		"known_revisions":   compactData["revisions"],
	}))
	if err != nil {
		t.Fatal(err)
	}
	if size := serializedResultBytes(t, repeat); size > 8<<10 {
		t.Fatalf("repeat session bytes=%d, want <=8192", size)
	}
	if changed := structuredBusinessData(repeat)["changed_sections"]; fmt.Sprint(changed) != "[]" {
		t.Fatalf("unchanged session reports changes: %+v", changed)
	}
	for index := 0; index < 100; index++ {
		task, startErr := rt.tasks.StartRemote(context.Background(), created.Session.ID, "demo", workspace.Path, fmt.Sprintf("printf completed-%03d", index))
		if startErr != nil {
			t.Fatal(startErr)
		}
		finished, finishedCancel := context.WithTimeout(context.Background(), time.Second)
		if !task.Wait(finished) {
			finishedCancel()
			t.Fatalf("completed task fixture %d timed out", index)
		}
		finishedCancel()
	}
	afterHistory, err := wrappedSession(context.Background(), mcpresult.Request(map[string]any{
		"remote_session_id": created.Session.ID, "known_revisions": compactData["revisions"],
	}))
	if err != nil {
		t.Fatal(err)
	}
	afterHistoryData := structuredBusinessData(afterHistory)
	if serializedResultBytes(t, afterHistory) > 8<<10 || strings.Contains(fmt.Sprint(afterHistoryData), "completed-099") {
		t.Fatalf("completed task history amplified compact session: %+v", afterHistoryData)
	}
	active, err := rt.tasks.StartRemote(context.Background(), created.Session.ID, "demo", workspace.Path, "sleep 2 # active-command-must-not-repeat")
	if err != nil {
		t.Fatal(err)
	}
	withActive, err := wrappedSession(context.Background(), mcpresult.Request(map[string]any{"remote_session_id": created.Session.ID}))
	if err != nil {
		t.Fatal(err)
	}
	activeData := structuredBusinessData(withActive)["active_tasks"].(map[string]any)
	if fmt.Sprint(activeData["count"]) != "1" || !strings.Contains(fmt.Sprint(activeData["execution_task_ids"]), active.ID) || strings.Contains(fmt.Sprint(withActive), "active-command-must-not-repeat") {
		t.Fatalf("active task summary=%+v", activeData)
	}
	_ = active.Kill()
	full, err := wrappedSession(context.Background(), mcpresult.Request(map[string]any{
		"remote_session_id": created.Session.ID, "response_profile": "full",
	}))
	if err != nil {
		t.Fatal(err)
	}
	fullBytes := serializedResultBytes(t, full)
	if fullBytes <= compactBytes {
		t.Fatal("full session must be larger than compact session")
	}
	t.Logf("session_compact_bytes=%d session_repeat_bytes=%d session_full_bytes=%d", compactBytes, serializedResultBytes(t, repeat), fullBytes)

	command := "head -c 1048576 /dev/zero | tr '\\000' x >&2 # prompt-marker-that-must-not-repeat"
	task, err := rt.tasks.StartRemote(context.Background(), created.Session.ID, "demo", workspace.Path, command)
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !task.Wait(waitCtx) {
		t.Fatal("large stderr fixture did not finish")
	}

	wrappedExecute := rt.instrumentTool("execute", rt.toolExecute)
	offset := 0
	for attempt := 0; attempt < 10; attempt++ {
		result, callErr := wrappedExecute(context.Background(), mcpresult.Request(map[string]any{
			"action": "attach", "remote_session_id": created.Session.ID,
			"execution_task_id": task.ID, "stderr_offset": offset, "purpose": "observe bounded fixture",
		}))
		if callErr != nil {
			t.Fatal(callErr)
		}
		if size := serializedResultBytes(t, result); size > 16<<10 {
			t.Fatalf("attach bytes=%d, want <=16384", size)
		} else if attempt == 0 {
			t.Logf("attach_chunk_bytes=%d", size)
		}
		data := structuredBusinessData(result)
		encoded, _ := json.Marshal(data)
		if data["command"] != nil || strings.Contains(string(encoded), "prompt-marker-that-must-not-repeat") {
			t.Fatalf("attach repeated command or prompt: %s", encoded)
		}
		if data["command_digest"] == nil || data["resource_uri"] == nil || data["truncated"] != true {
			t.Fatalf("attach continuation metadata missing: %+v", data)
		}
		next := int(data["stderr_next_offset"].(float64))
		if next <= offset || next-offset > 2<<10 {
			t.Fatalf("stderr offset %d -> %d is invalid", offset, next)
		}
		offset = next
	}
}

func TestSessionSchemaChangeIsOptionalOnly(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	protocol := newTestMCPServer()
	rt.registerTools(protocol)
	var current map[string]any
	if err := json.Unmarshal(mcpresult.ToolSchemaJSON(rt.listedToolMap()["session"]), &current); err != nil {
		t.Fatal(err)
	}
	properties := current["properties"].(map[string]any)
	for _, added := range []string{"response_profile", "known_revisions"} {
		if properties[added] == nil {
			t.Fatalf("optional session field %s missing", added)
		}
	}
	required, _ := current["required"].([]any)
	for _, item := range required {
		if item == "response_profile" || item == "known_revisions" {
			t.Fatalf("new field became required: %v", required)
		}
	}
}

func serializedResultBytes(t *testing.T, value any) int {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return len(encoded)
}

func newTestMCPServer() *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "test"}, nil)
}
