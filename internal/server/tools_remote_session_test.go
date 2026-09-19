package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"
	"mcpx/internal/remotesession"
)

func callEnvelope(t *testing.T, handler mcp.ToolHandler, ctx context.Context, arguments map[string]any) map[string]any {
	t.Helper()
	if _, exists := arguments["intent"]; !exists {
		withIntent := make(map[string]any, len(arguments)+1)
		for key, value := range arguments {
			withIntent[key] = value
		}
		withIntent["intent"] = "test operation"
		arguments = withIntent
	}
	result, err := handler(ctx, mcpresult.Request(arguments))
	if err != nil {
		t.Fatal(err)
	}
	return decodeToolResult(t, result)
}

func errorCode(response map[string]any) string {
	body, _ := response["error"].(map[string]any)
	code, _ := body["code"].(string)
	return strings.ToLower(code)
}

func TestWorkspaceRevisionAggregatesNestedGitRoots(t *testing.T) {
	root := t.TempDir()
	for _, sub := range []string{"a", "b"} {
		subRoot := filepath.Join(root, sub)
		if err := os.MkdirAll(subRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		gitIn := func(args ...string) {
			t.Helper()
			command := exec.Command("git", append([]string{"-C", subRoot}, args...)...)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", args, err, output)
			}
		}
		gitIn("init")
		gitIn("config", "user.email", "test@example.invalid")
		gitIn("config", "user.name", "MCPX Test")
		if err := os.WriteFile(filepath.Join(subRoot, "f.txt"), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitIn("add", ".")
		gitIn("commit", "-m", "base")
	}
	head, digest := workspaceRevision(context.Background(), root)
	if head == "" || digest == "" {
		t.Fatalf("aggregate revision must be non-empty: head=%q digest=%q", head, digest)
	}
	if !strings.Contains(head, "a:") || !strings.Contains(head, "b:") {
		t.Fatalf("head must name both roots: %q", head)
	}
}

func TestWorkspaceRevisionSingleRootUnchanged(t *testing.T) {
	root := t.TempDir()
	gitIn := func(args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", root}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	gitIn("init")
	gitIn("config", "user.email", "test@example.invalid")
	gitIn("config", "user.name", "MCPX Test")
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn("add", ".")
	gitIn("commit", "-m", "base")
	head, digest := workspaceRevision(context.Background(), root)
	if head == "" || digest == "" {
		t.Fatalf("single-root revision must be non-empty: head=%q digest=%q", head, digest)
	}
	if strings.Contains(head, ":") {
		t.Fatalf("single root head must stay bare: %q", head)
	}
}

func TestSessionResumeIncludesPendingConfirmations(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	rt.cfg.Security.Commands.Confirm = append(rt.cfg.Security.Commands.Confirm, `^echo\b`)
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registered, ok := rt.reg.Get("demo")
	if !ok {
		t.Fatal("demo workspace was not registered")
	}
	created, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{
		WorkspaceName: "demo", WorkspacePath: registered.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	command := mcpresult.Request(map[string]any{
		"intent":            "request pending command confirmation",
		"remote_session_id": created.Session.ID,
		"command":           "echo pending", "purpose": "inspect pending", "scope": "workspace",
	})
	commandResult, err := rt.toolCommandExecute(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	commandResponse := decodeToolResult(t, commandResult)
	if commandResponse["status"] != "waiting_confirmation" {
		t.Fatalf("command confirmation = %+v", commandResponse)
	}

	resume := mcpresult.Request(map[string]any{
		"intent":            "resume the existing session",
		"remote_session_id": created.Session.ID,
	})
	attachResult, err := rt.toolSession(context.Background(), resume)
	if err != nil {
		t.Fatal(err)
	}
	attachResponse := decodeToolResult(t, attachResult)
	attachData, _ := attachResponse["data"].(map[string]any)
	items, ok := attachData["pending_confirmations"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("session resume must expose pending confirmations: %+v", attachData)
	}
	item := items[0].(map[string]any)
	if item["command"] != "echo pending" || item["purpose"] != "inspect pending" || item["user_confirmed_required"] != true {
		t.Fatalf("pending confirmation item=%+v", item)
	}
}

func TestRemoteSessionResumeUsesCompactPayload(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{
		"action": "open", "workspace": "demo", "label": "compact-resume",
	})
	openData, _ := opened["data"].(map[string]any)
	remoteID, _ := openData["remote_session_id"].(string)
	if remoteID == "" {
		t.Fatalf("session open missing remote_session_id: %+v", opened)
	}
	for _, field := range []string{"agent_guidance", "client_protocol", "tools", "instructions", "schema_source", "capability_version", "capability_groups", "recommended_workflows"} {
		if openData[field] == nil {
			t.Fatalf("new session bootstrap missing %s: %+v", field, openData)
		}
	}

	resumed := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{
		"action": "open", "remote_session_id": remoteID,
	})
	resumeData, _ := resumed["data"].(map[string]any)
	if resumeData == nil {
		t.Fatalf("session resume missing data: %+v", resumed)
	}
	if resumeData["revisions"] == nil || resumeData["remote_session"] == nil || resumeData["workspace"] == nil {
		t.Fatalf("session resume missing dynamic identity/revision state: %+v", resumeData)
	}
	if resumeData["tasks_scope"] != "running" {
		t.Fatalf("session resume must declare running-only task scope: %+v", resumeData)
	}
	for _, field := range []string{"agent_guidance", "client_protocol", "tools", "instructions", "schema_source", "capability_version", "capability_groups", "recommended_workflows", "mcpx", "extension_inventory", "project", "project_tasks", "git", "opened_at", "artifacts"} {
		if resumeData[field] != nil {
			t.Fatalf("session resume must not repeat nonessential bootstrap field %s: %+v", field, resumeData[field])
		}
	}
	openJSON, err := json.Marshal(openData)
	if err != nil {
		t.Fatal(err)
	}
	resumeJSON, err := json.Marshal(resumeData)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumeJSON) >= len(openJSON) {
		t.Fatalf("compact resume did not reduce serialized bytes: open=%d resume=%d", len(openJSON), len(resumeJSON))
	}
	t.Logf("session_resume_serialized_bytes open=%d resume=%d delta=%d", len(openJSON), len(resumeJSON), len(openJSON)-len(resumeJSON))

	resumedWithInstructions := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{
		"action": "open", "remote_session_id": remoteID, "include_instructions_content": true,
	})
	withInstructionsData, _ := resumedWithInstructions["data"].(map[string]any)
	instructions, _ := withInstructionsData["instructions"].(map[string]any)
	if instructions == nil || instructions["inline"] != true {
		t.Fatalf("explicit instruction content request must be honored on resume: %+v", withInstructionsData)
	}

	resumedWithProjectTasks := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{
		"action": "open", "remote_session_id": remoteID, "include_project_tasks": true,
	})
	withProjectData, _ := resumedWithProjectTasks["data"].(map[string]any)
	if withProjectData["project"] == nil {
		t.Fatalf("explicit project task request must include project summary: %+v", withProjectData)
	}
	if _, ok := withProjectData["project_tasks"]; !ok {
		t.Fatalf("explicit project task request must include project_tasks field: %+v", withProjectData)
	}
}

func TestRemoteSessionResumeIncludesOnlyRunningTasks(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID, _ := opened["remote_session_id"].(string)
	if remoteID == "" {
		t.Fatalf("session open missing remote_session_id: %+v", opened)
	}

	completed := callEnvelope(t, rt.toolExecute, context.Background(), map[string]any{
		"action": "run", "remote_session_id": remoteID, "purpose": "create completed task fixture",
		"command": testPrintCommand("completed-resume-fixture"), "yield_time_ms": 5000,
	})
	completedData, _ := completed["data"].(map[string]any)
	if completedData["completed_in_call"] != true {
		t.Fatalf("completed fixture did not finish inline: %+v", completed)
	}

	running := callEnvelope(t, rt.toolExecute, context.Background(), map[string]any{
		"action": "run", "remote_session_id": remoteID, "purpose": "create running task fixture",
		"command": testSleepCommand(2 * time.Second), "yield_time_ms": 1,
	})
	runningData, _ := running["data"].(map[string]any)
	runningID, _ := runningData["execution_task_id"].(string)
	if runningID == "" {
		t.Fatalf("running fixture missing execution_task_id: %+v", running)
	}

	resumed := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "remote_session_id": remoteID})
	resumeData, _ := resumed["data"].(map[string]any)
	tasks, _ := resumeData["tasks"].([]any)
	if resumeData["tasks_scope"] != "running" || len(tasks) != 1 {
		t.Fatalf("resume must return only running tasks: %+v", resumeData)
	}
	task, _ := tasks[0].(map[string]any)
	if task["execution_task_id"] != runningID || task["status"] != "running" {
		t.Fatalf("resume running task=%+v, want %s", task, runningID)
	}
	if resumeData["artifacts"] != nil {
		t.Fatalf("resume must not inject artifact history: %+v", resumeData["artifacts"])
	}

	_ = callEnvelope(t, rt.toolExecute, context.Background(), map[string]any{
		"action": "stop", "remote_session_id": remoteID, "purpose": "stop running task fixture", "execution_task_id": runningID,
	})
}

func TestRemoteSessionNotFoundExplainsExactCopy(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	request := mcpresult.Request(map[string]any{
		"intent":            "resume a missing remote session",
		"remote_session_id": "rs-does-not-exist",
	})

	result, err := rt.toolSession(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	response := decodeToolResult(t, result)
	errorBody, _ := response["error"].(map[string]any)
	if errorBody["code"] != "NOT_FOUND" {
		t.Fatalf("missing session error = %+v", response)
	}
	message, _ := errorBody["message"].(string)
	for _, phrase := range []string{"原样复制", "session(action=list)", "不要直接创建新 Session"} {
		if !strings.Contains(message, phrase) {
			t.Fatalf("missing session error must explain %q: %s", phrase, message)
		}
	}
	recovery, _ := errorBody["recovery"].(map[string]any)
	arguments, _ := recovery["arguments"].(map[string]any)
	if recovery["tool"] != "session" || arguments["action"] != "list" {
		t.Fatalf("missing session recovery must point to session list: %+v", recovery)
	}
}

func TestCleanCoreSessionListDiscoversExistingSession(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{
		"action": "open", "workspace": "demo", "label": "recover-me",
	})
	remoteID, _ := opened["remote_session_id"].(string)
	if remoteID == "" {
		t.Fatalf("session open did not return remote_session_id: %+v", opened)
	}

	listed := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{
		"action": "list", "workspace": "demo", "query": "recover-me",
	})
	if !statusOK(listed) {
		t.Fatalf("session list failed: %+v", listed)
	}
	data, _ := listed["data"].(map[string]any)
	sessions, _ := data["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("session list returned %d sessions: %+v", len(sessions), data)
	}
	item, _ := sessions[0].(map[string]any)
	if item["remote_session_id"] != remoteID || item["workspace"] != "demo" || item["label"] != "recover-me" {
		t.Fatalf("session list did not return the recoverable session: %+v", item)
	}
	lastActive, _ := item["last_active_at"].(string)
	if strings.TrimSpace(lastActive) == "" {
		t.Fatalf("session list must expose last_active_at for recovery selection: %+v", item)
	}

	byID := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{
		"action": "list", "workspace": "demo", "query": remoteID,
	})
	idData, _ := byID["data"].(map[string]any)
	idSessions, _ := idData["sessions"].([]any)
	if len(idSessions) != 1 {
		t.Fatalf("session list must support ID lookup: %+v", idData)
	}
}

func TestToolCallRefreshesSessionLastActiveAt(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	handler := rt.toolHandlers["session"]
	if handler == nil {
		t.Fatal("session handler is not registered")
	}
	opened := callEnvelope(t, handler, context.Background(), map[string]any{
		"action": "open", "workspace": "demo", "label": "touch-me",
	})
	remoteID, _ := opened["remote_session_id"].(string)
	if remoteID == "" {
		t.Fatalf("session open did not return remote_session_id: %+v", opened)
	}
	listed := callEnvelope(t, handler, context.Background(), map[string]any{
		"action": "list", "workspace": "demo", "query": remoteID,
	})
	first := sessionListLastActiveAt(t, listed)
	time.Sleep(1100 * time.Millisecond)
	resumed := callEnvelope(t, handler, context.Background(), map[string]any{
		"action": "open", "remote_session_id": remoteID,
	})
	if !statusOK(resumed) {
		t.Fatalf("session resume failed: %+v", resumed)
	}
	listed = callEnvelope(t, handler, context.Background(), map[string]any{
		"action": "list", "workspace": "demo", "query": remoteID,
	})
	second := sessionListLastActiveAt(t, listed)
	if !second.After(first) {
		t.Fatalf("last_active_at did not advance after resume: first=%s second=%s", first, second)
	}
}

func sessionListLastActiveAt(t *testing.T, listed map[string]any) time.Time {
	t.Helper()
	data, _ := listed["data"].(map[string]any)
	sessions, _ := data["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("session list=%+v", listed)
	}
	item, _ := sessions[0].(map[string]any)
	raw, _ := item["last_active_at"].(string)
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339, raw)
	}
	if err != nil {
		t.Fatalf("last_active_at=%q: %v", raw, err)
	}
	return parsed
}
