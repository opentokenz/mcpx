package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
	"mcpx/internal/mcpresult"
)

type authRoundTripper struct {
	base http.RoundTripper
	auth string
}

func (t authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", t.auth)
	return t.base.RoundTrip(clone)
}

func roundTripperWithAuth(base http.RoundTripper, authHeader string) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return authRoundTripper{base: base, auth: authHeader}
}

// normalizeAcceptanceResult maps machine result shapes onto the acceptance
// harness envelope used by older assertions (ok/status/data).
func normalizeAcceptanceResult(payload map[string]any, res *mcp.CallToolResult, text string) (map[string]any, bool) {
	if payload == nil {
		return nil, false
	}

	// Full ARC envelope in _meta (or accidentally inlined).
	if mcpx, ok := payload["mcpx"].(map[string]any); ok {
		if _, hasLegacyOK := payload["ok"]; hasLegacyOK {
			return nil, false
		}
		result, _ := mcpx["result"].(map[string]any)
		return acceptanceNormalized(result, payload, res, text), true
	}

	// Model structuredContent after WrapToolResult: {status, type, data, error?}
	if payload["type"] != nil && payload["data"] != nil {
		return acceptanceNormalized(payload, nil, res, text), true
	}

	// Handler public wire: {status, data, error?}
	if status, ok := payload["status"].(string); ok && status != "" {
		if _, hasData := payload["data"]; hasData {
			return acceptanceNormalized(payload, nil, res, text), true
		}
		if _, hasError := payload["error"]; hasError {
			return acceptanceNormalized(payload, nil, res, text), true
		}
	}
	return nil, false
}

func acceptanceNormalized(result map[string]any, arcEnvelope map[string]any, res *mcp.CallToolResult, text string) map[string]any {
	publicStatus, _ := result["status"].(string)
	if publicStatus == "" {
		publicStatus = "succeeded"
	}
	status := publicStatus
	if status == "succeeded" {
		status = "ok"
	} else if status == "waiting_confirmation" {
		status = "need_confirmation"
	}
	okValue := publicStatus == "succeeded"
	hints, _ := result["hints"].(map[string]any)
	if result["type"] == "error" {
		okValue = false
	}
	if hints["preferred_behavior"] == "ask_confirm" && status == "succeeded" {
		status, okValue = "waiting_confirmation", false
	}
	data := result["data"]
	// Wire envelope already has data nested; model SC also uses data.
	// When result is the ARC result body, data is business payload.
	normalized := map[string]any{
		"ok":            okValue,
		"status":        status,
		"public_status": publicStatus,
		"data":          data,
		"_result":       res,
		"_text":         text,
	}
	if arcEnvelope == nil {
		if value := resultARCValue(res); value != nil {
			raw, err := json.Marshal(value)
			if err == nil {
				var decoded map[string]any
				if json.Unmarshal(raw, &decoded) == nil {
					arcEnvelope = decoded
				}
			}
		}
	}
	if arcEnvelope != nil {
		normalized["_arc"] = arcEnvelope
	}
	if actions, ok := result["actions"]; ok && actions != nil {
		normalized["actions"] = actions
	}
	if errBody, ok := result["error"]; ok && errBody != nil {
		normalized["error"] = errBody
	} else if resultData, ok := data.(map[string]any); ok {
		if errData, exists := resultData["error"]; exists {
			normalized["error"] = errData
		}
	}
	return normalized
}

// TestA01A02A03A07A10A13ViaMCPProtocol exercises the real Streamable HTTP path:
// client → tools/list / call_tool → MCPX handlers (acceptance A01/A02/A03/A07/A10/A13 core).
func TestA01A02A03A07A10A13ViaMCPProtocol(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	workspace := filepath.Join(home, "project")
	if err := os.MkdirAll(filepath.Join(workspace, "frontend", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"demo.go":                     "package demo\n\nconst Value = 1\n",
		"a.go":                        "package demo\n\nfunc Alpha() int { return 1 }\n",
		"b.go":                        "package demo\n\nfunc Beta() int { return 2 }\n",
		"delete_me.txt":               "remove through confirmation\n",
		"AGENTS.md":                   "# Project: chinese comments\n",
		"frontend/AGENTS.md":          "# frontend: use pnpm\n",
		"frontend/src/AGENTS.md":      "# src: no generated\n",
		"go.mod":                      "module demo\n\ngo 1.22\n",
		"frontend/src/views/Home.vue": "<template><div/></template>\n",
	}
	for rel, content := range files {
		path := filepath.Join(workspace, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	globalAgents := filepath.Join(home, "GLOBAL_AGENTS.md")
	if err := os.WriteFile(globalAgents, []byte("# Global: run all tests\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	token := "acceptance-token"
	cfg := config.DefaultConfig()
	cfg.Auth.Mode = "bearer"
	cfg.Auth.Token = token
	cfg.Logging.Enabled = false
	cfg.Security.Commands.Allow = append(cfg.Security.Commands.Allow, `^printf\b`, `^sleep\b`, `^go test\b`)
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "project", Path: workspace}}
	cfg.Discovery.Instructions.GlobalAgentsPath = globalAgents
	// Keep this protocol fixture independent of the developer machine's global
	// ~/.agents/skills and ~/.codex/skills directories.
	cfg.Discovery.Skills.Dirs = []string{filepath.Join(home, "skills")}
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := New(Options{Version: "0.9.0-test", Commit: "deadbeef", Date: "2026-07-31"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx", Version: "0.9.0-test"}, &mcp.ServerOptions{
		Instructions: agentGuidanceInstructions(),
	})
	runtime.registerTools(protocol)
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return protocol
	}, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true, Stateless: true})
	gw := NewGateway(cfg, nil, streamable)
	ts := httptest.NewServer(gw.Handler())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	httpClient := &http.Client{Transport: roundTripperWithAuth(http.DefaultTransport, "Bearer "+token)}
	client := mcp.NewClient(&mcp.Implementation{Name: "acceptance-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   ts.URL + "/mcp",
		HTTPClient: httpClient,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	// --- A01: tools/list schema ---
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	byName := map[string]mcp.Tool{}
	for _, tool := range listed.Tools {
		if tool != nil {
			byName[tool.Name] = *tool
		}
	}
	expectedTools := []string{
		"workspace", "session", "read", "edit", "move_out", "observe", "progress",
		"operation_batch", "operation_manage",
		"execute", "plan", "artifact", "skill_tool", "mcp_tool", "browser",
		"runtime_read", "environment_read", "environment", "screenshot_capture", "secret_provide",
	}
	if len(byName) != len(expectedTools) {
		t.Fatalf("tools/list count=%d, want %d: %v", len(byName), len(expectedTools), byName)
	}
	for _, required := range expectedTools {
		if _, ok := byName[required]; !ok {
			t.Fatalf("tools/list missing %s", required)
		}
	}
	for name, tool := range byName {
		encoded, err := json.Marshal(tool)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		var listedTool map[string]any
		if err := json.Unmarshal(encoded, &listedTool); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		rawOutputSchema, hasOutputSchema := listedTool["outputSchema"]
		if name == "mcp_tool" {
			if hasOutputSchema && rawOutputSchema != nil {
				t.Fatalf("mcp_tool must omit outputSchema: %+v", rawOutputSchema)
			}
		} else {
			outputSchema, ok := rawOutputSchema.(map[string]any)
			if !ok {
				t.Fatalf("%s must expose an OutputSchema: %+v", name, rawOutputSchema)
			}
			if outputSchema["$id"] != "mcpx.structured_content.v2.0" {
				t.Fatalf("%s must expose the ARC structuredContent OutputSchema: %+v", name, rawOutputSchema)
			}
		}
		inputSchema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal input schema %s: %v", name, err)
		}
		if strings.Contains(string(inputSchema), `"started_at_ms"`) {
			t.Fatalf("%s must not expose started_at_ms: %s", name, inputSchema)
		}
		if strings.Contains(string(inputSchema), `"goal"`) {
			t.Fatalf("%s must not expose deprecated goal: %s", name, inputSchema)
		}
		for _, forbidden := range []string{"presentation", "renderer", "show_source", "density"} {
			if strings.Contains(string(inputSchema), `"`+forbidden+`"`) {
				t.Fatalf("%s exposes host presentation argument %q: %s", name, forbidden, inputSchema)
			}
		}
	}
	editTool := byName["edit"]
	schemaJSON, err := json.Marshal(editTool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	schemaText := string(schemaJSON)
	for _, needle := range []string{"remote_session_id", "rev", "content", "replacements", "edits"} {
		if !strings.Contains(schemaText, needle) {
			t.Fatalf("edit schema missing %q: %s", needle, schemaText)
		}
	}
	if strings.Contains(schemaText, "base_sha256") {
		t.Fatalf("edit schema must not expose legacy base_sha256: %s", schemaText)
	}
	if !strings.Contains(schemaText, "update") || !strings.Contains(schemaText, "create") || !strings.Contains(schemaText, "rename") || strings.Contains(schemaText, "user_confirmed") || strings.Contains(schemaText, "\"delete\"") {
		t.Fatalf("edit operation enum incomplete: %s", schemaText)
	}
	moveSchema, _ := json.Marshal(byName["move_out"].InputSchema)
	var moveSchemaMap map[string]any
	if err := json.Unmarshal(moveSchema, &moveSchemaMap); err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"prepare", "submit", "targets", "rev", "expected_sha256", "symlink", "confirmation_uuid"} {
		if !strings.Contains(string(moveSchema), needle) {
			t.Fatalf("move_out schema missing %q: %s", needle, moveSchema)
		}
	}
	if _, hasOneOf := moveSchemaMap["oneOf"]; hasOneOf {
		t.Fatalf("move_out must not rely on oneOf: %s", moveSchema)
	}
	moveProperties, _ := moveSchemaMap["properties"].(map[string]any)
	for _, field := range []string{"action", "purpose", "targets", "confirmation_uuid", "remote_session_id", "idempotency_key"} {
		if moveProperties[field] == nil {
			t.Fatalf("move_out schema missing property %q: %s", field, moveSchema)
		}
	}
	if moveProperties["workspace"] != nil || strings.Contains(string(moveSchema), `"kind"`) {
		t.Fatalf("move_out must let Runtime infer workspace/kind: %s", moveSchema)
	}
	actionProp, _ := moveProperties["action"].(map[string]any)
	actionDesc, _ := actionProp["description"].(string)
	for _, needle := range []string{"prepare", "submit", "purpose", "targets", "confirmation_uuid"} {
		if !strings.Contains(actionDesc, needle) {
			t.Fatalf("move_out action.description missing %q: %s", needle, actionDesc)
		}
	}
	moveRequired, _ := moveSchemaMap["required"].([]any)
	if !reflect.DeepEqual(moveRequired, []any{"action"}) {
		t.Fatalf("move_out required must be exactly [action], got %v", moveRequired)
	}
	for _, forbidden := range []string{"workspace", "move_request_id", "manifest_sha256"} {
		if moveProperties[forbidden] != nil {
			t.Fatalf("move_out must bind %q server-side: %s", forbidden, moveSchema)
		}
	}
	commandSchema, _ := json.Marshal(byName["execute"].InputSchema)
	for _, field := range []string{"remote_session_id", "purpose"} {
		if !strings.Contains(string(commandSchema), `"`+field+`"`) {
			t.Fatalf("execute schema missing %q: %s", field, commandSchema)
		}
	}
	var commandSchemaMap map[string]any
	if err := json.Unmarshal(commandSchema, &commandSchemaMap); err != nil {
		t.Fatal(err)
	}
	if containsSchemaRequired(commandSchemaMap["required"].([]any), "remote_session_id") {
		t.Fatalf("execute remote_session_id must be optional when transport is bound: %s", commandSchema)
	}
	if strings.Contains(string(commandSchema), `"scope"`) {
		t.Fatalf("execute schema must not expose single-value scope: %s", commandSchema)
	}
	contextSchema, _ := json.Marshal(byName["read"].InputSchema)
	for _, removed := range []string{"pattern", "max_files"} {
		if strings.Contains(string(contextSchema), removed) {
			t.Fatalf("read exposes removed compatibility field %q: %s", removed, contextSchema)
		}
	}
	if !strings.Contains(string(contextSchema), `"mode"`) || !strings.Contains(string(contextSchema), `"items"`) || !strings.Contains(string(contextSchema), `"entries_cursor"`) || !strings.Contains(string(contextSchema), `"entries_limit"`) {
		t.Fatalf("read schema must expose file and direct-entry pagination fields: %s", contextSchema)
	}
	for _, extensionName := range []string{"skill_tool", "mcp_tool"} {
		extensionSchema, _ := json.Marshal(byName[extensionName].InputSchema)
		text := string(extensionSchema)
		for _, action := range []string{"list", "describe", "call"} {
			if !strings.Contains(text, `"`+action+`"`) {
				t.Fatalf("%s schema missing action %q: %s", extensionName, action, extensionSchema)
			}
		}
		for _, legacy := range []string{"discovery_id", "discovery_revision"} {
			if strings.Contains(text, legacy) {
				t.Fatalf("%s schema exposes legacy field %q: %s", extensionName, legacy, extensionSchema)
			}
		}
	}
	planSchema, _ := json.Marshal(byName["plan"].InputSchema)
	for _, forbidden := range []string{"presentation", "renderer", "show_source", "density"} {
		if strings.Contains(string(planSchema), forbidden) {
			t.Fatalf("plan_manage exposes host presentation field %q: %s", forbidden, planSchema)
		}
	}

	// Catalog names must match tools/list (A01).
	declared := capabilityToolNames()
	if len(declared) != len(listed.Tools) {
		t.Fatalf("catalog count %d != tools/list %d", len(declared), len(listed.Tools))
	}

	needsPurpose := func(name string, args map[string]any) bool {
		switch name {
		case "edit", "execute", "plan", "operation_batch", "screenshot_capture", "secret_provide":
			return true
		case "skill_tool", "mcp_tool":
			return fmt.Sprint(args["action"]) == "call"
		case "move_out":
			return fmt.Sprint(args["action"]) == "prepare"
		case "artifact":
			return fmt.Sprint(args["action"]) == "register"
		default:
			return false
		}
	}
	rawCall := func(name string, args map[string]any) map[string]any {
		t.Helper()
		if needsPurpose(name, args) {
			if _, exists := args["purpose"]; !exists {
				withPurpose := make(map[string]any, len(args)+1)
				for key, value := range args {
					withPurpose[key] = value
				}
				withPurpose["purpose"] = "acceptance operation"
				args = withPurpose
			}
		}
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_ = mcpresult.FirstText // keep helper package used if text path needs it
		if len(res.Content) == 0 {
			t.Fatalf("%s empty content", name)
		}
		text, ok := res.Content[0].(*mcp.TextContent)
		if !ok {
			// The public result is human-first. Keep this fallback for attached
			// content and older clients that still return a machine payload.
			if value := resultMachineValue(res); value != nil {
				raw, _ := json.Marshal(value)
				var asMap map[string]any
				if json.Unmarshal(raw, &asMap) == nil {
					// Normalize to envelope-like shape for callers that expect status.
					if _, hasOK := asMap["ok"]; !hasOK {
						return map[string]any{"ok": true, "status": "succeeded", "data": asMap, "_raw_structured": true, "_result": res}
					}
					return asMap
				}
			}
			t.Fatalf("%s content type %T", name, res.Content[0])
		}
		var envelope map[string]any
		if err := json.Unmarshal([]byte(text.Text), &envelope); err != nil {
			// The first text content is the host-visible display; models read
			// structuredContent / _meta instead of parsing prose.
			if value := resultMachineValue(res); value != nil {
				raw, _ := json.Marshal(value)
				_ = json.Unmarshal(raw, &envelope)
			}
		}
		if normalized, ok := normalizeAcceptanceResult(envelope, res, text.Text); ok {
			return normalized
		}
		if envelope == nil {
			t.Fatalf("%s decode: no structuredContent/meta and text is not JSON\n%s", name, text.Text)
		}
		envelope["_result"] = res
		envelope["_text"] = text.Text
		return envelope
	}

	copyArgs := func(input map[string]any) map[string]any {
		output := make(map[string]any, len(input)+3)
		for key, value := range input {
			output[key] = value
		}
		return output
	}
	normalizeCall := func(name string, input map[string]any) (string, map[string]any) {
		args := copyArgs(input)
		if value, exists := args["session_id"]; exists {
			if _, hasRemote := args["remote_session_id"]; !hasRemote {
				args["remote_session_id"] = value
			}
			delete(args, "session_id")
		}
		if _, exists := args["purpose"]; !exists {
			if value, exists := args["intent"]; exists {
				args["purpose"] = value
			}
		}
		delete(args, "intent")
		switch name {
		case "file_read":
			name, args["view"] = "read", "file"
		case "context_query":
			action, _ := args["action"].(string)
			view := map[string]string{"list": "list", "query": "context", "search": "search"}[action]
			if view == "" {
				view = "search"
			}
			name, args["view"] = "read", view
			delete(args, "action")
		case "command_execute":
			name = "execute"
			args["action"] = "run"
		case "task_manage":
			action, _ := args["action"].(string)
			if action == "attach" || action == "stop" || action == "stdin" {
				name = "execute"
				args["action"] = action
			} else {
				name = "observe"
				if action == "status" {
					args["view"] = "task"
				} else {
					args["view"] = action
				}
				delete(args, "action")
			}
		case "plan_manage":
			action, _ := args["action"].(string)
			switch action {
			case "create":
				name = "plan"
				args["action"] = "create"
			case "get":
				name = "plan"
				args["action"] = "read"
			case "start_task":
				name = "plan"
				args["action"] = "advance"
			case "complete_task":
				name = "plan"
				args["action"] = "complete"
			case "block_task":
				name = "plan"
				args["action"] = "block"
			default:
				name = "plan"
				args["action"] = action
			}
		case "runtime_inspect":
			name, args["view"] = "runtime_read", args["action"]
			delete(args, "action")
		}
		return name, args
	}
	call := func(name string, input map[string]any) map[string]any {
		publicName, args := normalizeCall(name, input)
		return rawCall(publicName, args)
	}

	// --- A03: session_open single call ---
	opened := call("session", map[string]any{
		"action":                       "open",
		"workspace":                    "project",
		"label":                        "acceptance",
		"include_instructions_content": true,
		"include_project_tasks":        true,
	})
	if opened["status"] != "ok" && opened["ok"] != true {
		t.Fatalf("session_open: %+v", opened)
	}
	openData, _ := opened["data"].(map[string]any)
	if openData == nil {
		t.Fatalf("session_open data missing: %+v", opened)
	}
	guidance, _ := openData["agent_guidance"].(map[string]any)
	routing, _ := guidance["tool_routing"].(map[string]any)
	if guidance["version"] != agentGuidanceVersion || !containsAnyString(routing["modify_files"], "edit") || guidance["response_contract"] != nil || guidance["edit_payload"] != nil {
		t.Fatalf("session_open compact guidance missing or incomplete: %+v", guidance)
	}
	remoteSession, _ := openData["remote_session"].(map[string]any)
	remoteID, _ := remoteSession["id"].(string)
	if remoteID == "" {
		t.Fatalf("session_open missing remote_session.id: %+v", openData)
	}
	if openData["remote_session_id"] != remoteID {
		t.Fatalf("session_open missing top-level remote_session_id: %+v", openData)
	}
	revs, _ := openData["revisions"].(map[string]any)
	for _, key := range []string{
		"tool_schema_revision", "capability_manifest_revision", "guidance_revision",
		"instruction_revision", "session_capability_revision", "client_protocol_revision",
	} {
		if revs[key] == nil || revs[key] == "" {
			t.Fatalf("missing revision %s: %+v", key, revs)
		}
	}
	for _, removed := range []string{"skill_revision", "mcp_revision"} {
		if revs[removed] != nil {
			t.Fatalf("session_open must not expose extension revision %s: %+v", removed, revs)
		}
	}
	extensions, _ := openData["extension_inventory"].(map[string]any)
	if extensions == nil || extensions["skills"] == nil || extensions["mcp_servers"] == nil || openData["skills"] != nil || openData["upstream_mcp"] != nil {
		t.Fatalf("session_open extension inventory contract: %+v", openData)
	}
	mcpxMeta, _ := openData["mcpx"].(map[string]any)
	if mcpxMeta["version"] != "0.9.0-test" {
		t.Fatalf("mcpx version: %+v", mcpxMeta)
	}
	instr, _ := openData["instructions"].(map[string]any)
	docs, _ := instr["documents"].([]any)
	if len(docs) < 2 {
		t.Fatalf("expected global+project instructions, got %+v", instr)
	}
	// Inline content present for at least one doc.
	hasContent := false
	for _, raw := range docs {
		doc, _ := raw.(map[string]any)
		if c, ok := doc["content"].(string); ok && c != "" {
			hasContent = true
		}
	}
	if !hasContent {
		t.Fatalf("session_open should inline AGENTS content: %+v", instr)
	}
	schemaRev1 := fmt.Sprint(revs["tool_schema_revision"])
	sessionRev1 := fmt.Sprint(revs["session_capability_revision"])
	if openData["client_refresh"] != nil || openData["omitted_sections"] != nil {
		t.Fatalf("session bootstrap must not require client revision bookkeeping: %+v", openData)
	}

	// --- P0 plan: create, advance, and deliver ---
	statusOK := func(resp map[string]any) bool {
		return resp["status"] == "ok" || resp["status"] == "succeeded" || resp["ok"] == true
	}
	executeRun := call("execute", map[string]any{
		"action": "run", "remote_session_id": remoteID, "purpose": "acceptance execute run",
		"command": testPrintCommand("acceptance-execute"),
	})
	if !statusOK(executeRun) {
		t.Fatalf("execute run = %+v", executeRun)
	}
	executedData, _ := executeRun["data"].(map[string]any)
	if executedData["completed_in_call"] != true || executedData["exit_code"] != float64(0) {
		t.Fatalf("execute run did not reach handler: %+v", executedData)
	}
	planCreated := call("plan_manage", map[string]any{
		"action": "create", "remote_session_id": remoteID, "summary": "acceptance plan", "purpose": "acceptance plan create",
		"tasks": []any{map[string]any{"local_id": "verify", "title": "Verify protocol"}},
	})
	planData, _ := planCreated["data"].(map[string]any)
	if !statusOK(planCreated) || planData["plan_id"] == nil {
		t.Fatalf("plan create = %+v", planCreated)
	}
	planID, _ := planData["plan_id"].(string)
	planTasks, _ := planData["tasks"].([]any)
	taskID := ""
	if len(planTasks) > 0 {
		taskID, _ = planTasks[0].(map[string]any)["plan_task_id"].(string)
	}
	if !strings.HasPrefix(taskID, "pt_") {
		t.Fatalf("plan_create must issue server task id: %+v", planData)
	}
	started := call("plan_manage", map[string]any{"action": "start_task", "remote_session_id": remoteID, "plan_id": planID, "plan_task_id": taskID, "purpose": "start task"})
	if !statusOK(started) || started["data"].(map[string]any)["plan_task_id"] != taskID {
		t.Fatalf("plan start = %+v", started)
	}
	completed := call("plan_manage", map[string]any{
		"action": "complete_task", "remote_session_id": remoteID, "plan_id": planID, "plan_task_id": taskID, "purpose": "complete task",
		"evidence": []any{map[string]any{"kind": "source", "reference_id": "demo.go"}},
	})
	if !statusOK(completed) || completed["data"].(map[string]any)["status"] != "completed" {
		t.Fatalf("plan complete = %+v", completed)
	}
	delivered := call("plan_manage", map[string]any{"action": "deliver", "remote_session_id": remoteID, "plan_id": planID, "purpose": "deliver plan"})
	if !statusOK(delivered) {
		t.Fatalf("plan deliver = %+v", delivered)
	}
	if ready, _ := delivered["data"].(map[string]any)["ready"].(bool); !ready {
		// delivery payload may live under nested fields depending on presentation path
		if delivered["data"].(map[string]any)["status"] != "delivered" && delivered["data"].(map[string]any)["status"] != "ready" {
			t.Fatalf("plan deliver data = %+v", delivered["data"])
		}
	}

	// --- A02: runtime_inspect capabilities revisions; role-independent tool_schema_revision ---
	caps := call("runtime_inspect", map[string]any{"action": "capabilities", "remote_session_id": remoteID})
	capData, _ := caps["data"].(map[string]any)
	capRevs, _ := capData["revisions"].(map[string]any)
	if fmt.Sprint(capRevs["tool_schema_revision"]) != schemaRev1 {
		t.Fatalf("tool_schema_revision drifted between session_open and capability_list: %v vs %v", schemaRev1, capRevs["tool_schema_revision"])
	}
	if fmt.Sprint(capRevs["session_capability_revision"]) != sessionRev1 {
		// Same session/role — should match.
		t.Fatalf("session_capability_revision mismatch: %v vs %v", sessionRev1, capRevs["session_capability_revision"])
	}
	resumed := call("session", map[string]any{"action": "open", "remote_session_id": remoteID})
	resumedData, _ := resumed["data"].(map[string]any)
	if resumedData["client_refresh"] != nil || resumedData["omitted_sections"] != nil {
		t.Fatalf("session re-open must not add legacy revision bookkeeping: %+v", resumedData)
	}
	if resumedData["revisions"] == nil || resumedData["remote_session"] == nil || resumedData["workspace"] == nil {
		t.Fatalf("session re-open must preserve dynamic identity and revision facts: %+v", resumedData)
	}
	for _, field := range []string{"agent_guidance", "client_protocol", "tools", "instructions", "schema_source", "capability_version", "capability_groups", "recommended_workflows"} {
		if resumedData[field] != nil {
			t.Fatalf("session re-open must not repeat static bootstrap field %s: %+v", field, resumedData[field])
		}
	}

	// --- A04 nested AGENTS ---
	listedInstr := call("runtime_inspect", map[string]any{
		"action":            "instructions",
		"remote_session_id": remoteID,
		"anchor_path":       "frontend/src/views/Home.vue",
	})
	listedData, _ := listedInstr["data"].(map[string]any)
	chain, _ := listedData["instructions"].([]any)
	if len(chain) < 4 {
		t.Fatalf("nested AGENTS chain too short: %+v", listedData)
	}
	scopes := []string{}
	for _, raw := range chain {
		doc, _ := raw.(map[string]any)
		scopes = append(scopes, fmt.Sprint(doc["scope"]))
	}
	joined := strings.Join(scopes, ",")
	if !strings.Contains(joined, "global") || !strings.Contains(joined, "project") || !strings.Contains(joined, "directory") {
		t.Fatalf("expected global/project/directory scopes, got %v", scopes)
	}

	// --- A07 file_read_batch ---
	batch := call("file_read", map[string]any{
		"remote_session_id": remoteID,
		"items": []any{
			map[string]any{"path": "a.go", "offset": 0, "limit": 20},
			map[string]any{"path": "b.go", "offset": 0, "limit": 20},
			map[string]any{"path": "missing.go", "offset": 0, "limit": 10},
		},
	})
	batchData, _ := batch["data"].(map[string]any)
	results, _ := batchData["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("batch results: %+v", batchData)
	}
	okCount, failCount := 0, 0
	var demoRev string
	for _, raw := range results {
		item, _ := raw.(map[string]any)
		if item["ok"] == true {
			okCount++
			if item["path"] == "a.go" {
				demoRev, _ = item["rev"].(string)
			}
		} else {
			failCount++
		}
	}
	if okCount != 2 || failCount != 1 {
		t.Fatalf("batch ok/fail = %d/%d data=%+v", okCount, failCount, batchData)
	}
	// Consistency with single file_read
	single := call("file_read", map[string]any{"remote_session_id": remoteID, "path": "a.go", "offset": 0, "limit": 20})
	singleData, _ := single["data"].(map[string]any)
	if singleData["rev"] != demoRev || demoRev == "" {
		t.Fatalf("batch rev %q != single %q", demoRev, singleData["rev"])
	}

	// --- A08 code_search context ---
	search := call("context_query", map[string]any{
		"action":            "search",
		"remote_session_id": remoteID,
		"query":             "Alpha",
		"context_before":    1,
		"context_after":     1,
	})
	searchData, _ := search["data"].(map[string]any)
	matches, _ := searchData["matches"].([]any)
	if len(matches) == 0 {
		t.Fatalf("search missed Alpha: %+v", searchData)
	}
	match0, _ := matches[0].(map[string]any)
	if match0["rev"] == nil || match0["rev"] == "" || match0["sha256"] != nil {
		t.Fatalf("search missing compact rev or leaking sha256: %+v", match0)
	}
	scopedQuery := call("context_query", map[string]any{
		"action": "query", "remote_session_id": remoteID,
		"query": "检查 Alpha 实现代码", "mode": "smart", "parallel": true, "max_results": 10,
		"paths": []any{"."}, "include_glob": "**/*.go",
	})
	scopedData, _ := scopedQuery["data"].(map[string]any)
	scopedFiles, _ := scopedData["files"].([]any)
	if len(scopedFiles) == 0 {
		t.Fatalf("recursive directory query returned no source files: %+v", scopedData)
	}
	foundAlpha := false
	for _, raw := range scopedFiles {
		item, _ := raw.(map[string]any)
		if item["path"] == "a.go" {
			foundAlpha = true
		}
	}
	if !foundAlpha {
		t.Fatalf("recursive directory query missed a.go: %+v", scopedData)
	}

	// --- A08 command_execute / task_manage short and long command paths ---
	short := call("command_execute", map[string]any{
		"remote_session_id": remoteID, "command": testPrintCommand("short-command"), "purpose": "run the short command protocol check",
	})
	shortData, _ := short["data"].(map[string]any)
	if shortData["completed_in_call"] != true || shortData["exit_code"] != float64(0) || shortData["execution_task_id"] != nil {
		t.Fatalf("short command should complete in one call: %+v", short)
	}
	long := call("command_execute", map[string]any{
		"remote_session_id": remoteID, "command": testSleepCommand(50 * time.Millisecond), "purpose": "verify short wait task handoff", "yield_time_ms": 1,
	})
	longData, _ := long["data"].(map[string]any)
	longTaskID, _ := longData["execution_task_id"].(string)
	if longData["completed_in_call"] != false || longTaskID == "" {
		t.Fatalf("long command should return a unified Task: %+v", long)
	}
	actions, _ := long["actions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("running Task must expose one canonical continuation action: %+v", long["actions"])
	}
	next, _ := actions[0].(map[string]any)
	nextArgs, _ := next["arguments"].(map[string]any)
	if next["id"] == "" || nextArgs["execution_task_id"] != longTaskID || nextArgs["yield_time_ms"] != nil {
		t.Fatalf("short run yield must not be inherited by continuation: %+v", next)
	}
	if nextArgs["stdout_offset"] != longData["stdout_next_offset"] || nextArgs["stderr_offset"] != longData["stderr_next_offset"] {
		t.Fatalf("continuation offsets must match returned stream offsets: next=%+v data=%+v", nextArgs, longData)
	}
	attached := call(next["id"].(string), nextArgs)
	attachedData, _ := attached["data"].(map[string]any)
	if attachedData["status"] != "exited" || attachedData["exit_code"] != float64(0) || attachedData["stdout_next_offset"] == nil || attachedData["stderr_next_offset"] == nil {
		t.Fatalf("task attach must return stream-specific offsets: %+v", attached)
	}
	overTen := call("command_execute", map[string]any{
		"remote_session_id": remoteID, "command": testSleepCommand(11 * time.Second), "purpose": "verify default long-task handoff",
	})
	overTenData, _ := overTen["data"].(map[string]any)
	overTenTaskID, _ := overTenData["execution_task_id"].(string)
	if overTenData["completed_in_call"] != false || overTenTaskID == "" {
		t.Fatalf("command longer than the default 10s yield should return a Task: %+v", overTen)
	}
	stopped := call("task_manage", map[string]any{
		"action": "stop", "remote_session_id": remoteID, "execution_task_id": overTenTaskID,
	})
	if stopped["status"] != "ok" {
		t.Fatalf("long command Task could not be stopped: %+v", stopped)
	}

	// --- A10/A13 clean edit + inline diff summary ---
	sum := sha256.Sum256([]byte(files["demo.go"]))
	base := compactFileRevision(fmt.Sprintf("sha256:%x", sum[:]))
	executed := call("edit", map[string]any{
		"remote_session_id": remoteID,
		"idempotency_key":   "edit-idempotency-key",
		"purpose":           "bump Value",
		"edits": []any{map[string]any{
			"operation": "update", "path": "demo.go", "rev": base,
			"replacements": []any{map[string]any{"match": "const Value = 1", "replacement": "const Value = 2"}},
		}},
	})
	execData, _ := executed["data"].(map[string]any)
	if execData == nil {
		t.Fatalf("edit: %+v", executed)
	}
	if !statusOK(executed) {
		t.Fatalf("edit was not applied: %+v", executed)
	}
	if execData["total_changed_lines"] != float64(2) && execData["total_changed_lines"] != 2 {
		t.Fatalf("unexpected changed line count: %+v", execData)
	}
	if execData["diff_summary"] != nil {
		t.Fatalf("edit response must not duplicate per-file diff as diff_summary: %+v", execData)
	}
	editResults, _ := execData["results"].([]any)
	if len(editResults) != 1 {
		t.Fatalf("edit results missing: %+v", execData)
	}
	editResult, _ := editResults[0].(map[string]any)
	if diff, _ := editResult["diff"].(string); !strings.Contains(diff, "-const Value = 1") || !strings.Contains(diff, "+const Value = 2") {
		t.Fatalf("inline per-file diff missing concrete change: %+v", execData)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "demo.go"))
	if err != nil || !strings.Contains(string(content), "Value = 2") {
		t.Fatalf("file not updated: %q err=%v", content, err)
	}
	replayed := call("edit", map[string]any{
		"remote_session_id": remoteID, "idempotency_key": "edit-idempotency-key", "purpose": "bump Value",
		"edits": []any{map[string]any{"operation": "update", "path": "demo.go", "rev": base,
			"replacements": []any{map[string]any{"match": "const Value = 1", "replacement": "const Value = 2"}}}},
	})
	replayData, _ := replayed["data"].(map[string]any)
	replayResults, _ := replayData["results"].([]any)
	replayResult, _ := replayResults[0].(map[string]any)
	if replayData["idempotent_replay"] != true || replayResult["diff"] != editResult["diff"] || replayData["diff_summary"] != nil {
		t.Fatalf("edit retry must replay its original per-file diff without a duplicate summary: %+v", replayed)
	}
	// --- A12 stale revision ---
	stale := call("edit", map[string]any{
		"remote_session_id": remoteID, "purpose": "stale",
		"edits": []any{map[string]any{
			"operation": "update", "path": "demo.go", "rev": base, // old revision
			"replacements": []any{map[string]any{"match": "const Value = 2", "replacement": "const Value = 3"}},
		}},
	})
	if stale["status"] == "ok" {
		t.Fatalf("stale revision should fail: %+v", stale)
	}
	errBody, _ := stale["error"].(map[string]any)
	code, _ := errBody["code"].(string)
	if code != "STALE_REVISION" || errBody["category"] != "conflict" || errBody["retryable"] != true {
		t.Fatalf("expected retryable stale-revision contract, got %+v", stale)
	}
	// File must remain at Value = 2
	content, _ = os.ReadFile(filepath.Join(workspace, "demo.go"))
	if strings.Contains(string(content), "Value = 3") {
		t.Fatal("stale write must not apply")
	}

	// --- A11 zero match fails ---
	freshSum := sha256.Sum256(content)
	freshBase := compactFileRevision(fmt.Sprintf("sha256:%x", freshSum[:]))
	nomatch := call("edit", map[string]any{
		"remote_session_id": remoteID, "purpose": "no match", "apply": false,
		"edits": []any{map[string]any{
			"operation": "update", "path": "demo.go", "rev": freshBase,
			"replacements": []any{map[string]any{"match": "DOES_NOT_EXIST_XYZ", "replacement": "x"}},
		}},
	})
	if nomatch["status"] == "ok" {
		t.Fatalf("zero match should fail: %+v", nomatch)
	}

	removed := call("observe", map[string]any{"remote_session_id": remoteID, "view": "changes"})
	if statusOK(removed) {
		t.Fatalf("observe must reject removed view changes: %+v", removed)
	}
	if body, _ := removed["error"].(map[string]any); strings.ToUpper(fmt.Sprint(body["code"])) != "INVALID_ACTION" {
		t.Fatalf("observe removed view changes must return INVALID_ACTION: %+v", removed)
	}
	missingDiffID := call("observe", map[string]any{"remote_session_id": remoteID, "view": "diff"})
	if statusOK(missingDiffID) {
		t.Fatalf("observe diff without edit_id must fail validation: %+v", missingDiffID)
	}
	if body, _ := missingDiffID["error"].(map[string]any); strings.ToUpper(fmt.Sprint(body["code"])) != "EDIT_ID_REQUIRED" {
		t.Fatalf("observe diff without edit_id must return EDIT_ID_REQUIRED: %+v", missingDiffID)
	}
}
