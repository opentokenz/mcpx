package server

import (
	"context"
	"os"

	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcpx/internal/config"
	"mcpx/internal/mcpresult"
	"mcpx/internal/terminal"
)

func TestCleanCoreExecuteIdempotencyAndUserConfirmation(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	rt.cfg.Security.Commands.Allow = append(rt.cfg.Security.Commands.Allow, `^printf\b`)
	rt.cfg.Security.Commands.Confirm = append(rt.cfg.Security.Commands.Confirm, `^echo\b`)
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	request := map[string]any{
		"action": "run", "remote_session_id": remoteID, "purpose": "run a stable command",
		"command": testPrintCommand("stable"), "scope": "workspace", "idempotency_key": "execute-replay-1",
	}
	first := callEnvelope(t, rt.toolExecute, context.Background(), request)
	if !statusOK(first) {
		t.Fatalf("execute failed: %+v", first)
	}
	firstData := first["data"].(map[string]any)
	if firstData["completed_in_call"] != true || firstData["exit_code"] != float64(0) {
		t.Fatalf("short execute result=%+v", firstData)
	}

	replayRequest := cloneMap(request)
	replayRequest["purpose"] = "same effect with a rephrased purpose"
	replay := callEnvelope(t, rt.toolExecute, context.Background(), replayRequest)
	if !statusOK(replay) || replay["data"].(map[string]any)["idempotent_replay"] != true {
		t.Fatalf("execute retry did not replay=%+v", replay)
	}
	conflictRequest := cloneMap(request)
	conflictRequest["command"] = testPrintCommand("changed")
	conflict := callEnvelope(t, rt.toolExecute, context.Background(), conflictRequest)
	if statusOK(conflict) || errorCode(conflict) != "idempotency_conflict" {
		t.Fatalf("execute conflict=%+v", conflict)
	}

	confirmationRequest := map[string]any{
		"action": "run", "remote_session_id": remoteID, "purpose": "run a confirmed command",
		"command": "echo confirmed", "scope": "workspace", "idempotency_key": "execute-confirm-1",
	}
	waiting := callEnvelope(t, rt.toolExecute, context.Background(), confirmationRequest)
	if waiting["status"] != "waiting_confirmation" {
		t.Fatalf("confirmation should wait=%+v", waiting)
	}
	waitingData, _ := waiting["data"].(map[string]any)
	if waitingData["user_confirmed_required"] != true || waitingData["confirmation_token"] != nil {
		t.Fatalf("clean confirmation leaked token or missed user flag=%+v", waitingData)
	}
	confirmedRequest := cloneMap(confirmationRequest)
	confirmedRequest["user_confirmed"] = true
	confirmed := callEnvelope(t, rt.toolExecute, context.Background(), confirmedRequest)
	if !statusOK(confirmed) {
		t.Fatalf("confirmed execute failed=%+v", confirmed)
	}
	confirmedReplay := callEnvelope(t, rt.toolExecute, context.Background(), confirmedRequest)
	if !statusOK(confirmedReplay) || confirmedReplay["data"].(map[string]any)["idempotent_replay"] != true {
		t.Fatalf("confirmed retry did not replay=%+v", confirmedReplay)
	}
}

func TestCleanCoreCommandConfirmationRefreshesPendingWhenPurposeChanges(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	rt.cfg.Security.Commands.Confirm = append(rt.cfg.Security.Commands.Confirm, `^echo\b`)
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	request := map[string]any{
		"action": "run", "remote_session_id": remoteID,
		"purpose": "push the first release commit", "command": "echo confirmed",
		"scope": "workspace",
	}
	firstWaiting := callEnvelope(t, rt.toolExecute, context.Background(), request)
	if firstWaiting["status"] != "waiting_confirmation" {
		t.Fatalf("first confirmation should wait=%+v", firstWaiting)
	}
	firstDigest := firstWaiting["data"].(map[string]any)["command_digest"].(string)

	changed := cloneMap(request)
	changed["purpose"] = "push two release commits"
	changed["user_confirmed"] = true
	secondWaiting := callEnvelope(t, rt.toolExecute, context.Background(), changed)
	if secondWaiting["status"] != "waiting_confirmation" {
		t.Fatalf("changed business intent must create a fresh pending confirmation=%+v", secondWaiting)
	}
	secondDigest := secondWaiting["data"].(map[string]any)["command_digest"].(string)
	if secondDigest == firstDigest {
		t.Fatalf("changed purpose reused command digest %q", secondDigest)
	}

	confirmed := callEnvelope(t, rt.toolExecute, context.Background(), changed)
	if !statusOK(confirmed) {
		t.Fatalf("confirmation for refreshed pending must execute instead of looping=%+v", confirmed)
	}
}

func TestCleanCoreMissingCommandUsesExecutionTaxonomy(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	rt.cfg.Security.Commands.Allow = append(rt.cfg.Security.Commands.Allow, `^mcpx-command-that-does-not-exist$`)
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	response := callEnvelope(t, rt.toolExecute, context.Background(), map[string]any{
		"action": "run", "remote_session_id": remoteID, "purpose": "classify a missing executable",
		"command": "mcpx-command-that-does-not-exist", "scope": "workspace",
	})
	wantMissingCode, wantMissingExit := testShellMissingCommandOutcome()
	if statusOK(response) || errorCode(response) != wantMissingCode {
		t.Fatalf("missing command response=%+v", response)
	}
	errorBody, _ := response["error"].(map[string]any)
	if errorBody["category"] != "execution" || errorBody["retryable"] != false {
		t.Fatalf("missing command taxonomy=%+v", errorBody)
	}
	details, _ := errorBody["details"].(map[string]any)
	if details["exit_code"] != wantMissingExit {
		t.Fatalf("missing command exit code=%+v", details)
	}

	accepted := callEnvelope(t, rt.toolHandlers["execute"], context.Background(), map[string]any{
		"action": "run", "remote_session_id": remoteID, "purpose": "classify async command failure",
		"command": "mcpx-command-that-does-not-exist", "scope": "workspace", "execution_mode": "async",
	})
	if accepted["status"] != "accepted" {
		t.Fatalf("async missing command was not accepted as an operation: %+v", accepted)
	}
	acceptedData, _ := accepted["data"].(map[string]any)
	operationID, _ := acceptedData["operation_id"].(string)
	if operationID == "" {
		t.Fatalf("async operation id missing: %+v", accepted)
	}
	completed := callEnvelope(t, rt.toolHandlers["operation_manage"], context.Background(), map[string]any{
		"remote_session_id": remoteID, "action": "wait", "operation_id": operationID, "timeout_ms": 5000,
	})
	if statusOK(completed) || errorCode(completed) != "operation_failed" {
		t.Fatalf("async operation failure=%+v", completed)
	}
	operationError, _ := completed["error"].(map[string]any)
	if operationError["category"] != "execution" || operationError["retryable"] != false {
		t.Fatalf("async operation taxonomy=%+v", operationError)
	}
	operationDetails, _ := operationError["details"].(map[string]any)
	if operationDetails["exit_code"] != wantMissingExit {
		t.Fatalf("async operation exit code=%+v", operationDetails)
	}
}

func TestCleanCorePlanEvidenceAndArtifactWorkflow(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	rt.cfg.Security.Commands.Allow = append(rt.cfg.Security.Commands.Allow, `^sleep\b`)
	workspace, _ := rt.reg.Get("demo")
	path := filepath.Join(workspace.Path, "plan.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	created := callEnvelope(t, rt.toolPlanClean, context.Background(), map[string]any{
		"action": "create", "remote_session_id": remoteID, "purpose": "track the workflow",
		"summary": "complete the clean core workflow", "tasks": []any{map[string]any{"local_id": "main", "title": "apply and verify"}},
		"idempotency_key": "plan-create-1",
	})
	if !statusOK(created) {
		t.Fatalf("plan create=%+v", created)
	}
	createdData := created["data"].(map[string]any)
	if _, exists := createdData["goal"]; exists {
		t.Fatalf("clean-core plan output must not expose internal goal: %+v", createdData)
	}
	planID := createdData["plan_id"].(string)
	replay := callEnvelope(t, rt.toolPlanClean, context.Background(), map[string]any{
		"action": "create", "remote_session_id": remoteID, "purpose": "same plan, rephrased",
		"summary": "complete the clean core workflow", "tasks": []any{map[string]any{"local_id": "main", "title": "apply and verify"}},
		"idempotency_key": "plan-create-1",
	})
	if replay["data"].(map[string]any)["idempotent_replay"] != true {
		t.Fatalf("plan create replay=%+v", replay)
	}
	tasks := asMapSlice(createdData["tasks"])
	taskID := tasks[0]["plan_task_id"].(string)
	advanced := callEnvelope(t, rt.toolPlanClean, context.Background(), map[string]any{
		"action": "advance", "remote_session_id": remoteID, "purpose": "start the tracked task", "plan_id": planID, "plan_task_id": taskID,
	})
	if !statusOK(advanced) {
		t.Fatalf("plan advance=%+v", advanced)
	}

	base := compactFileRevision(digestForTest([]byte("before\n")))
	edited := callEnvelope(t, rt.toolEdit, context.Background(), map[string]any{
		"remote_session_id": remoteID, "purpose": "apply the tracked edit", "idempotency_key": "plan-edit-1",
		"edits": []any{map[string]any{"path": "plan.txt", "operation": "update", "rev": base,
			"replacements": []any{map[string]any{"match": "before", "replacement": "after"}}}},
	})
	if !statusOK(edited) {
		t.Fatalf("edit=%+v", edited)
	}
	editID := edited["data"].(map[string]any)["edit_id"].(string)

	executed := callEnvelope(t, rt.toolExecute, context.Background(), map[string]any{
		"action": "run", "remote_session_id": remoteID, "purpose": "run the tracked verification", "command": testSleepCommand(50 * time.Millisecond),
		"scope": "workspace", "yield_time_ms": 1,
	})
	if executed["status"] != "accepted" && !statusOK(executed) {
		t.Fatalf("execute=%+v", executed)
	}
	executeData := executed["data"].(map[string]any)
	taskIDForEvidence, _ := executeData["execution_task_id"].(string)
	if taskIDForEvidence == "" {
		t.Fatalf("expected a task for execution evidence=%+v", executeData)
	}
	attached := callEnvelope(t, rt.toolExecute, context.Background(), map[string]any{
		"action": "attach", "remote_session_id": remoteID, "purpose": "collect verification output", "execution_task_id": taskIDForEvidence, "yield_time_ms": 3000,
	})
	if !statusOK(attached) || attached["data"].(map[string]any)["status"] != "exited" {
		t.Fatalf("attach=%+v", attached)
	}

	artifact := callEnvelope(t, rt.toolArtifactClean, context.Background(), map[string]any{
		"action": "register", "remote_session_id": remoteID, "purpose": "record the edited artifact", "path": "plan.txt", "kind": "other", "idempotency_key": "artifact-register-1",
	})
	if !statusOK(artifact) {
		t.Fatalf("artifact register=%+v", artifact)
	}
	artifactID := artifact["data"].(map[string]any)["artifact_id"].(string)

	completed := callEnvelope(t, rt.toolPlanClean, context.Background(), map[string]any{
		"action": "complete", "remote_session_id": remoteID, "purpose": "record verifiable completion", "plan_id": planID, "plan_task_id": taskID,
		"evidence": []any{
			map[string]any{"kind": "edit", "reference_id": editID},
			map[string]any{"kind": "execute", "reference_id": taskIDForEvidence},
			map[string]any{"kind": "artifact", "reference_id": artifactID},
		},
	})
	if !statusOK(completed) || completed["data"].(map[string]any)["status"] != "completed" {
		t.Fatalf("plan complete=%+v", completed)
	}
	delivered := callEnvelope(t, rt.toolPlanClean, context.Background(), map[string]any{
		"action": "deliver", "remote_session_id": remoteID, "purpose": "deliver the completed workflow", "plan_id": planID,
	})
	deliveryData := delivered["data"].(map[string]any)
	if deliveredPlan, ok := deliveryData["plan"].(map[string]any); !ok || deliveredPlan["goal"] != nil {
		t.Fatalf("delivery plan must not expose internal goal: %+v", deliveryData["plan"])
	}
	if !statusOK(delivered) || deliveryData["ready"] != true {
		t.Fatalf("plan deliver=%+v", delivered)
	}
}

func TestCleanCoreSkillToolDoesNotRequirePublicRevisionTokens(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	rt.cfg.Discovery.Skills.Enabled = true
	rt.cfg.Discovery.Skills.Dirs = []string{".skills"}
	skillDir := filepath.Join(workspace.Path, ".skills", "docs")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: docs\ndescription: return documentation\nruntime: markdown\n---\n\n# Docs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	called := callEnvelope(t, rt.toolSkillTool, context.Background(), map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "call the documentation skill", "name": "docs", "arguments": map[string]any{},
	})
	calledData := called["data"].(map[string]any)
	if !statusOK(called) || !strings.Contains(calledData["content"].(string), "# Docs") {
		t.Fatalf("direct skill_tool call=%+v", called)
	}

	described := callEnvelope(t, rt.toolSkillTool, context.Background(), map[string]any{
		"action": "describe", "remote_session_id": remoteID, "name": "docs",
	})
	if !statusOK(described) {
		t.Fatalf("skill_tool describe=%+v", described)
	}
	describedData := described["data"].(map[string]any)
	if describedData["discovery_id"] != nil || describedData["discovery_revision"] != nil {
		t.Fatalf("skill_tool describe leaked public revision tokens: %+v", describedData)
	}
	if !strings.Contains(describedData["instructions"].(string), "# Docs") {
		t.Fatalf("skill_tool describe missing instructions: %+v", describedData)
	}

	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: docs\ndescription: changed documentation\nruntime: markdown\n---\n\n# Docs v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := callEnvelope(t, rt.toolSkillTool, context.Background(), map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "call the changed documentation skill", "name": "docs", "arguments": map[string]any{},
	})
	if statusOK(changed) || errorCode(changed) != "skill_revision_changed" {
		t.Fatalf("changed skill revision=%+v", changed)
	}
}

func TestSkillToolListValidationRiskConfirmationAndManifestRevision(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	rt.cfg.Discovery.Skills.Enabled = true
	rt.cfg.Discovery.Skills.Dirs = []string{".skills"}
	skillDir := filepath.Join(workspace.Path, ".skills", "publish")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `name: publish
description: publish a checked value
runtime: python
entry: run.py
permissions:
  - workspace-write
arguments_schema:
  type: object
  additionalProperties: false
  properties:
    value:
      type: string
  required: [value]
`
	if err := os.WriteFile(filepath.Join(skillDir, "skill.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "run.py"), []byte("print('published')\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	listed := callEnvelope(t, rt.toolSkillTool, context.Background(), map[string]any{
		"action": "list", "remote_session_id": remoteID, "query": "checked",
	})
	if !statusOK(listed) {
		t.Fatalf("skill list=%+v", listed)
	}
	skills := asMapSlice(listed["data"].(map[string]any)["skills"])
	if len(skills) != 1 || skills[0]["name"] != "publish" {
		t.Fatalf("skill query inventory=%+v", listed)
	}
	missing := callEnvelope(t, rt.toolSkillTool, context.Background(), map[string]any{
		"action": "describe", "remote_session_id": remoteID, "name": "missing",
	})
	if statusOK(missing) || errorCode(missing) != "skill_not_found" {
		t.Fatalf("missing skill=%+v", missing)
	}
	described := callEnvelope(t, rt.toolSkillTool, context.Background(), map[string]any{
		"action": "describe", "remote_session_id": remoteID, "name": "publish",
	})
	if !statusOK(described) {
		t.Fatalf("skill describe=%+v", described)
	}
	describedData := described["data"].(map[string]any)
	risk, _ := describedData["risk"].(map[string]any)
	if risk["destructive"] != false || risk["open_world"] != true || risk["confirmation_required"] != nil {
		t.Fatalf("executable skill risk must be descriptive only=%+v", risk)
	}
	invalid := callEnvelope(t, rt.toolSkillTool, context.Background(), map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "publish checked value", "name": "publish", "arguments": map[string]any{"extra": "x"},
	})
	if statusOK(invalid) || errorCode(invalid) != "skill_argument_invalid" {
		t.Fatalf("skill invalid arguments=%+v", invalid)
	}
	request := map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "publish checked value", "name": "publish", "arguments": map[string]any{"value": "secret-not-in-recovery"},
	}
	completed := callEnvelope(t, rt.toolSkillTool, context.Background(), request)
	if !statusOK(completed) || !strings.Contains(completed["data"].(map[string]any)["stdout"].(string), "published") {
		t.Fatalf("executable skill must run without server confirmation=%+v", completed)
	}

	changedManifest := strings.Replace(manifest, "required: [value]", "required: [replacement]", 1)
	if err := os.WriteFile(filepath.Join(skillDir, "skill.yaml"), []byte(changedManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := callEnvelope(t, rt.toolSkillTool, context.Background(), map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "publish checked value", "name": "publish", "arguments": map[string]any{"replacement": "v2"},
	})
	if statusOK(changed) || errorCode(changed) != "skill_revision_changed" {
		t.Fatalf("manifest-only skill revision change=%+v", changed)
	}
}

func TestCleanCoreMCPToolDoesNotRequirePublicRevisionTokens(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	rt.cfg.Discovery.MCP.Enabled = true
	workspace, _ := rt.reg.Get("demo")
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
	if err := config.WriteMCPFile(config.ProjectMCPPath(workspace.Path), config.MCPFile{MCPServers: map[string]config.MCPServer{
		"fake": testMCPServer(t, "Echo values for contract tests", script, map[string]string{"MCPX_TEST_START_LOG": startLog}),
	}}); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	described := callEnvelope(t, rt.toolMCPTool, context.Background(), map[string]any{
		"action": "describe", "remote_session_id": remoteID, "server": "fake", "tool": "echo",
	})
	if !statusOK(described) {
		t.Fatalf("mcp_tool describe=%+v", described)
	}
	describedData := described["data"].(map[string]any)
	if describedData["discovery_id"] != nil || describedData["discovery_revision"] != nil || describedData["input_schema"] == nil {
		t.Fatalf("mcp_tool describe contract=%+v", describedData)
	}
	startsBeforeCall := fakeMCPStartCount(t, startLog)
	callRequest := map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "call the fake MCP", "server": "fake", "tool": "echo", "arguments": map[string]any{"value": "one"},
	}
	called, err := rt.toolMCPTool(context.Background(), mcpresult.Request(callRequest))
	if err != nil {
		t.Fatal(err)
	}
	if called == nil || called.IsError || mcpresult.FirstText(called) != "echo:one" {
		t.Fatalf("unannotated mcp_tool must execute without server confirmation=%+v", called)
	}
	if startsAfterCall := fakeMCPStartCount(t, startLog); startsAfterCall != startsBeforeCall+1 {
		t.Fatalf("schema check and call must share one upstream instance: before=%d after=%d", startsBeforeCall, startsAfterCall)
	}
}

func TestMCPToolContractCoversInventoryErrorsSchemaAndUpstreamFailure(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	rt.cfg.Discovery.MCP.Enabled = true
	workspace, _ := rt.reg.Get("demo")
	schemaPath := filepath.Join(t.TempDir(), "echo-schema.json")
	if err := os.WriteFile(schemaPath, []byte(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "contract_mcp.py")
	serverCode := `#!/usr/bin/env python3
import json
import os
import sys

def send(message):
    sys.stdout.write(json.dumps(message, separators=(',', ':')) + "\n")
    sys.stdout.flush()

def current_tools():
    with open(os.environ["MCPX_TEST_SCHEMA"], "r", encoding="utf-8") as f:
        schema = json.load(f)
    closed_read = {"readOnlyHint": True, "destructiveHint": False, "openWorldHint": False}
    return [
        {"name": "echo", "description": "echo schema-bound value", "inputSchema": schema, "annotations": closed_read},
        {"name": "fail", "description": "return a structured upstream failure", "inputSchema": {"type":"object"}, "annotations": closed_read},
    ]

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
        send({"jsonrpc":"2.0", "id":request_id, "result":{"protocolVersion":"2025-11-25", "capabilities":{"tools":{}}, "serverInfo":{"name":"contract", "version":"1"}}})
    elif method == "tools/list":
        send({"jsonrpc":"2.0", "id":request_id, "result":{"tools":current_tools()}})
    elif method == "tools/call":
        if request.get("params", {}).get("name") == "fail":
            send({"jsonrpc":"2.0", "id":request_id, "result":{"content":[{"type":"text", "text":"upstream rejected request"}], "isError":True}})
        else:
            value = request.get("params", {}).get("arguments", {}).get("value", "")
            send({"jsonrpc":"2.0", "id":request_id, "result":{"content":[{"type":"text", "text":"echo:" + value}], "isError":False}})
    else:
        send({"jsonrpc":"2.0", "id":request_id, "error":{"code":-32601, "message":"method not found"}})
`
	if err := os.WriteFile(script, []byte(serverCode), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteMCPFile(config.ProjectMCPPath(workspace.Path), config.MCPFile{MCPServers: map[string]config.MCPServer{
		"contract": testMCPServer(t, "Contract-test MCP server", script, map[string]string{"MCPX_TEST_SCHEMA": schemaPath}),
		"offline":  {Description: "Unavailable server", Command: "mcpx-command-that-does-not-exist"},
	}}); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	servers := callEnvelope(t, rt.toolMCPTool, context.Background(), map[string]any{
		"action": "list", "remote_session_id": remoteID, "query": "contract",
	})
	if !statusOK(servers) {
		t.Fatalf("mcp server list=%+v", servers)
	}
	serverItems := asMapSlice(servers["data"].(map[string]any)["servers"])
	if len(serverItems) != 1 || serverItems[0]["name"] != "contract" || serverItems[0]["description"] != "Contract-test MCP server" {
		t.Fatalf("compact MCP inventory must support relevance routing: %+v", servers)
	}
	tools := callEnvelope(t, rt.toolMCPTool, context.Background(), map[string]any{
		"action": "list", "remote_session_id": remoteID, "server": "contract",
	})
	if !statusOK(tools) || len(asMapSlice(tools["data"].(map[string]any)["tools"])) != 2 {
		t.Fatalf("mcp tool list=%+v", tools)
	}
	unknownServer := callEnvelope(t, rt.toolMCPTool, context.Background(), map[string]any{
		"action": "list", "remote_session_id": remoteID, "server": "missing",
	})
	if statusOK(unknownServer) || errorCode(unknownServer) != "mcp_server_not_found" {
		t.Fatalf("unknown MCP server=%+v", unknownServer)
	}
	unknownTool := callEnvelope(t, rt.toolMCPTool, context.Background(), map[string]any{
		"action": "describe", "remote_session_id": remoteID, "server": "contract", "tool": "missing",
	})
	if statusOK(unknownTool) || errorCode(unknownTool) != "mcp_tool_not_found" {
		t.Fatalf("unknown MCP tool=%+v", unknownTool)
	}
	unavailable := callEnvelope(t, rt.toolMCPTool, context.Background(), map[string]any{
		"action": "list", "remote_session_id": remoteID, "server": "offline",
	})
	if statusOK(unavailable) || errorCode(unavailable) != "mcp_server_unavailable" {
		t.Fatalf("unavailable MCP server=%+v", unavailable)
	}

	described := callEnvelope(t, rt.toolMCPTool, context.Background(), map[string]any{
		"action": "describe", "remote_session_id": remoteID, "server": "contract", "tool": "echo",
	})
	if !statusOK(described) {
		t.Fatalf("echo describe=%+v", described)
	}
	describedRisk, _ := described["data"].(map[string]any)["risk"].(map[string]any)
	if describedRisk["read_only"] != true || describedRisk["open_world"] != false || describedRisk["confirmation_required"] != nil {
		t.Fatalf("upstream annotations must remain descriptive without server confirmation policy: %+v", describedRisk)
	}
	if err := os.WriteFile(schemaPath, []byte(`{"type":"object","properties":{"replacement":{"type":"string"}},"required":["replacement"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	schemaChanged := callEnvelope(t, rt.toolMCPTool, context.Background(), map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "call schema-changed echo", "server": "contract", "tool": "echo", "arguments": map[string]any{"replacement": "v2"},
	})
	if statusOK(schemaChanged) || errorCode(schemaChanged) != "mcp_tool_schema_changed" {
		t.Fatalf("MCP schema change=%+v", schemaChanged)
	}

	failDescription := callEnvelope(t, rt.toolMCPTool, context.Background(), map[string]any{
		"action": "describe", "remote_session_id": remoteID, "server": "contract", "tool": "fail",
	})
	if !statusOK(failDescription) {
		t.Fatalf("fail describe=%+v", failDescription)
	}
	failed, err := rt.toolMCPTool(context.Background(), mcpresult.Request(map[string]any{
		"action": "call", "remote_session_id": remoteID, "purpose": "observe an upstream failure", "intent": "test operation", "server": "contract", "tool": "fail", "arguments": map[string]any{},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if failed == nil || !failed.IsError || mcpresult.FirstText(failed) != "upstream rejected request" {
		t.Fatalf("upstream MCP business failure must pass through=%+v", failed)
	}
}

func fakeMCPStartCount(t *testing.T, path string) int {
	t.Helper()
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	normalized := strings.ReplaceAll(string(body), "\r\n", "\n")
	return strings.Count(normalized, "started\n")
}

func TestEphemeralRuntimeValidatesModeAndRedactsObservation(t *testing.T) {
	spec, err := ephemeralRuntimeSpecFromPayload(map[string]any{"runtime": "node", "script": "console.log('ok')\n"})
	if err != nil || spec == nil || spec.Command != "node -" || spec.ScriptSHA256 == "" {
		t.Fatalf("node runtime spec=%+v err=%v", spec, err)
	}
	if _, err := ephemeralRuntimeSpecFromPayload(map[string]any{"runtime": "python", "script": "print(1)", "command": "echo nope"}); err == nil {
		t.Fatal("runtime+script must reject command")
	}
	if _, err := ephemeralRuntimeSpecFromPayload(map[string]any{"runtime": "python", "script": strings.Repeat("x", ephemeralScriptMaxBytes+1)}); err == nil {
		t.Fatal("oversized runtime script was accepted")
	}
	args := map[string]any{"action": "run", "runtime": "python", "script": "print('secret-ish')\n", "purpose": "probe"}
	observed := observationArguments("execute", args)
	if observed["script"] != "[redacted ephemeral script]" || observed["script_sha256"] == "" || observed["script_bytes"] == nil {
		t.Fatalf("observed runtime args=%+v", observed)
	}
	if args["script"] != "print('secret-ish')\n" {
		t.Fatal("observation redaction mutated the live request")
	}
}

func TestDetachedExecutionOutcomeIsConsistentAcrossObserveAndAttach(t *testing.T) {
	python := testPythonInvocation(t)
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	start := func(id, script string, wallLimit time.Duration) *terminal.Task {
		t.Helper()
		task, err := rt.tasks.StartRemoteProcessWithObservationContext(
			id, "", "execute", remoteID, "demo", workspace.Path, python.Command("-"),
			terminal.ProcessSpec{Executable: python.Executable, Args: python.Args("-"), Stdin: script, WallLimit: wallLimit},
		)
		if err != nil {
			t.Fatal(err)
		}
		waitCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if !task.Wait(waitCtx) {
			t.Fatalf("task %s did not finish", task.ID)
		}
		return task
	}

	limitTask := start("req-detached-limit", "import time\ntime.sleep(1)\n", 50*time.Millisecond)
	observedLimit := callEnvelope(t, rt.toolObserve, context.Background(), map[string]any{
		"remote_session_id": remoteID, "view": "task", "execution_task_id": limitTask.ID,
	})
	limitData := observedLimit["data"].(map[string]any)
	if !statusOK(observedLimit) || limitData["outcome"] != "error" || limitData["error_code"] != "RUNTIME_LIMIT_EXCEEDED" || limitData["limit_reason"] != "wall_time_limit" {
		t.Fatalf("observe detached limit=%+v", observedLimit)
	}
	observedLimitLogs := callEnvelope(t, rt.toolObserve, context.Background(), map[string]any{
		"remote_session_id": remoteID, "view": "logs", "execution_task_id": limitTask.ID,
	})
	limitLogsData := observedLimitLogs["data"].(map[string]any)
	if !statusOK(observedLimitLogs) || limitLogsData["outcome"] != "error" || limitLogsData["error_code"] != "RUNTIME_LIMIT_EXCEEDED" {
		t.Fatalf("observe detached limit logs=%+v", observedLimitLogs)
	}
	attachedLimit := callEnvelope(t, rt.toolExecute, context.Background(), map[string]any{
		"action": "attach", "remote_session_id": remoteID, "purpose": "collect detached runtime limit", "execution_task_id": limitTask.ID,
	})
	if statusOK(attachedLimit) || errorCode(attachedLimit) != "runtime_limit_exceeded" {
		t.Fatalf("attach detached limit=%+v", attachedLimit)
	}
	attachedLimitData := attachedLimit["data"].(map[string]any)
	if attachedLimitData["outcome"] != "error" || attachedLimitData["limit_reason"] != "wall_time_limit" {
		t.Fatalf("attach detached limit data=%+v", attachedLimitData)
	}

	exitTask := start("req-detached-exit", "import sys\nsys.exit(7)\n", 0)
	observedExit := callEnvelope(t, rt.toolObserve, context.Background(), map[string]any{
		"remote_session_id": remoteID, "view": "task", "execution_task_id": exitTask.ID,
	})
	exitData := observedExit["data"].(map[string]any)
	if !statusOK(observedExit) || exitData["outcome"] != "error" || exitData["error_code"] != "PROCESS_EXIT" || exitData["exit_code"] != float64(7) {
		t.Fatalf("observe detached exit=%+v", observedExit)
	}
	attachedExit := callEnvelope(t, rt.toolExecute, context.Background(), map[string]any{
		"action": "attach", "remote_session_id": remoteID, "purpose": "collect detached process exit", "execution_task_id": exitTask.ID,
	})
	if statusOK(attachedExit) || errorCode(attachedExit) != "process_exit" {
		t.Fatalf("attach detached exit=%+v", attachedExit)
	}
}

func TestExecutionOutcomeClassification(t *testing.T) {
	for _, test := range []struct {
		name        string
		data        map[string]any
		wantCode    string
		wantOutcome string
	}{
		{name: "running", data: map[string]any{"status": terminal.TaskRunning}, wantOutcome: "running"},
		{name: "success", data: map[string]any{"status": terminal.TaskExited, "exit_code": 0}, wantOutcome: "succeeded"},
		{name: "process exit", data: map[string]any{"status": terminal.TaskExited, "exit_code": 7}, wantCode: "PROCESS_EXIT", wantOutcome: "error"},
		{name: "wall limit", data: map[string]any{"status": terminal.TaskKilled, "exit_code": -1, "limit_reason": "wall_time_limit"}, wantCode: "RUNTIME_LIMIT_EXCEEDED", wantOutcome: "error"},
		{name: "cpu limit", data: map[string]any{"status": terminal.TaskKilled, "exit_code": -1, "limit_reason": "cpu_time_limit"}, wantCode: "RUNTIME_LIMIT_EXCEEDED", wantOutcome: "error"},
		{name: "manual stop", data: map[string]any{"status": terminal.TaskKilled, "exit_code": -1}, wantOutcome: "stopped"},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, _ := annotateExecutionOutcome(test.data)
			if code != test.wantCode || test.data["outcome"] != test.wantOutcome {
				t.Fatalf("outcome code=%q data=%+v", code, test.data)
			}
		})
	}
}

func TestEphemeralPythonRuntimeExecutesAndKeepsReadableTaskID(t *testing.T) {
	_ = testPythonInvocation(t)
	rt := newWorkspaceRuntime(t, "demo")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	result := callEnvelope(t, rt.toolExecute, context.Background(), map[string]any{
		"action": "run", "remote_session_id": remoteID, "purpose": "run a short Python probe",
		"runtime": "python", "script": "print('ephemeral-ok')\n", "idempotency_key": "runtime-python-1",
	})
	if !statusOK(result) {
		t.Fatalf("runtime result=%+v", result)
	}
	data := result["data"].(map[string]any)
	taskID, _ := data["execution_task_id"].(string)
	if taskID == "" || data["runtime"] != "python" || data["completed_in_call"] != true {
		t.Fatalf("runtime metadata=%+v", data)
	}
	stdout, _ := data["stdout"].(string)
	if !strings.Contains(stdout, "ephemeral-ok") || strings.Contains(strings.Join([]string{data["command"].(string), stdout}, "\n"), "print('ephemeral-ok')") {
		t.Fatalf("runtime output/command unexpected: command=%q stdout=%q", data["command"], stdout)
	}
	logs := callEnvelope(t, rt.toolObserve, context.Background(), map[string]any{
		"remote_session_id": remoteID, "view": "logs", "execution_task_id": taskID,
	})
	if !statusOK(logs) || !strings.Contains(logs["data"].(map[string]any)["stdout"].(string), "ephemeral-ok") {
		t.Fatalf("re-read runtime logs=%+v", logs)
	}

	replay := callEnvelope(t, rt.toolExecute, context.Background(), map[string]any{
		"action": "run", "remote_session_id": remoteID, "purpose": "run a short Python probe",
		"runtime": "python", "script": "print('ephemeral-ok')\n", "idempotency_key": "runtime-python-1",
	})
	if !statusOK(replay) || replay["data"].(map[string]any)["idempotent_replay"] != true {
		t.Fatalf("same runtime script did not replay=%+v", replay)
	}
	conflict := callEnvelope(t, rt.toolExecute, context.Background(), map[string]any{
		"action": "run", "remote_session_id": remoteID, "purpose": "run a short Python probe",
		"runtime": "python", "script": "print('different-script')\n", "idempotency_key": "runtime-python-1",
	})
	if statusOK(conflict) || errorCode(conflict) != "idempotency_conflict" {
		t.Fatalf("changed runtime script reused idempotency result=%+v", conflict)
	}
}

func mcpConfirmationKey(t *testing.T, response map[string]any) string {
	t.Helper()
	data, ok := response["data"].(map[string]any)
	if !ok {
		t.Fatalf("confirmation response missing data: %+v", response)
	}
	key, _ := data["confirmation_key"].(string)
	if strings.TrimSpace(key) == "" {
		t.Fatalf("confirmation response missing confirmation_key: %+v", response)
	}
	return key
}

func cloneMap(input map[string]any) map[string]any {
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
