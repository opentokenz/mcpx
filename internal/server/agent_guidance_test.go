package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/envelope"
	"mcpx/internal/mcpresult"
)

func TestAgentGuidanceIsCompactPrincipledContract(t *testing.T) {
	guidance := agentGuidance()
	if guidance["version"] != agentGuidanceVersion || guidance["priority"] != "high" {
		t.Fatalf("guidance metadata=%+v", guidance)
	}
	if guidance["response_contract"] != nil || guidance["edit_payload"] != nil {
		t.Fatalf("protocol cheat-sheets must live in schemas/runtime, not guidance: %+v", guidance)
	}
	routingIface := guidance["tool_routing"]
	var routing map[string]any
	switch typed := routingIface.(type) {
	case map[string]any:
		routing = typed
	case map[string][]string:
		routing = make(map[string]any, len(typed))
		for key, value := range typed {
			routing[key] = value
		}
	default:
		t.Fatalf("tool routing type=%T", routingIface)
	}
	if !containsAnyString(routing["inspect_source"], "read") || !containsAnyString(routing["inspect_environment"], "environment_read") || !containsAnyString(routing["modify_files"], "edit") {
		t.Fatalf("canonical routing=%+v", routing)
	}
	rules, ok := guidance["rules"].([]string)
	if !ok || len(rules) < 8 || len(rules) > 16 {
		t.Fatalf("guidance should stay compact: %T %+v", guidance["rules"], guidance["rules"])
	}
	joined := strings.Join(rules, "\n")
	for _, required := range []string{"structuredContent", "不要猜测", "canonical tool", "STALE_REVISION", "recovery", "move_out", "purpose", "activity", "必须携带 activity.intent", "activity.evidence", "activity.next", "progress", "最终回复前", "最小充分证据"} {
		if !strings.Contains(joined, required) {
			t.Errorf("compact guidance missing principle %q: %s", required, joined)
		}
	}
	for _, forbidden := range []string{"progress_summary", "reasoning_summary", "准备停止 MCPX 工具调用", "用户可见响应契约", "edit 参数速查"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("guidance still contains protocol bookkeeping %q: %s", forbidden, joined)
		}
	}
	instructions := agentGuidanceInstructions()
	if strings.Contains(instructions, "用户可见响应契约") || strings.Contains(instructions, "edit 参数速查") {
		t.Fatalf("rendered instructions still duplicate tool protocol: %s", instructions)
	}
	if len(instructions) > 6000 {
		t.Fatalf("rendered guidance unexpectedly large: %d bytes", len(instructions))
	}
	for _, forbidden := range []string{"presentation", "renderer", "show_source", "density"} {
		if strings.Contains(strings.ToLower(mustJSON(t, guidance)), `"`+forbidden+`"`) {
			t.Fatalf("guidance contains host presentation argument %q", forbidden)
		}
	}
	revision := agentGuidanceRevision()
	if revision == "" || revision != agentGuidanceRevision() {
		t.Fatalf("guidance revision is not stable: %q", revision)
	}
}

func TestEveryPublicToolHasModelFacingDescriptionAndActionBranches(t *testing.T) {
	runtime := &Runtime{}
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	runtime.registerTools(protocol)
	for name, registered := range runtime.listedToolMap() {
		if strings.TrimSpace(registered.Description) == "" {
			t.Fatalf("tool %s has no description", name)
		}
		if len(mcpresult.ToolSchemaJSON(registered)) == 0 {
			continue
		}
		var schema map[string]any
		if err := json.Unmarshal(mcpresult.ToolSchemaJSON(registered), &schema); err != nil {
			t.Fatalf("tool %s schema: %v", name, err)
		}
		if _, hasOneOf := schema["oneOf"]; hasOneOf {
			t.Fatalf("tool %s must not rely on top-level oneOf: %s", name, mcpresult.ToolSchemaJSON(registered))
		}
		if _, hasAnyOf := schema["anyOf"]; hasAnyOf {
			t.Fatalf("tool %s must not rely on top-level anyOf: %s", name, mcpresult.ToolSchemaJSON(registered))
		}
		if _, hasAllOf := schema["allOf"]; hasAllOf {
			t.Fatalf("tool %s must not rely on top-level allOf: %s", name, mcpresult.ToolSchemaJSON(registered))
		}
		properties, _ := schema["properties"].(map[string]any)
		if actionProp, ok := properties["action"].(map[string]any); ok {
			if _, hasEnum := actionProp["enum"].([]any); hasEnum {
				desc, _ := actionProp["description"].(string)
				if strings.TrimSpace(desc) == "" {
					t.Fatalf("tool %s action has enum but missing action.description", name)
				}
			}
		}
	}
}

func TestRecoveryActionIsStructuredInErrorDetails(t *testing.T) {
	response := envelope.Fail(envelope.StatusError, "req_test", "demo", nil, "NOT_FOUND", "missing")
	addRecoveryAction(&response, "context_query", "locate the missing file", map[string]any{"action": "list"})
	if response.Error == nil {
		t.Fatal("missing error body")
	}
	next, ok := response.Error.Details["next_action"].(map[string]any)
	if !ok || next["tool"] != "read" || next["reason"] != "locate the missing file" {
		t.Fatalf("next action=%+v", response.Error.Details["next_action"])
	}
	if response.Error.Recovery == nil || response.Error.Recovery.Tool != "read" || response.Error.Recovery.Arguments["view"] != "list" {
		t.Fatalf("structured recovery=%+v", response.Error.Recovery)
	}
}

func TestLegacyTaskStatusRecoveryNormalizesToObserveTask(t *testing.T) {
	tool, args := normalizePublicAction("task_manage", map[string]any{
		"action": "status", "remote_session_id": "rs_1", "execution_task_id": "task_1",
	})
	if tool != "observe" || args["view"] != "task" || args["execution_task_id"] != "task_1" {
		t.Fatalf("normalized recovery=%s %+v", tool, args)
	}
}

func TestCleanProjectTaskNotFoundRecoveryUsesRuntimeProject(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	result, err := rt.terminalErrorWithCleanMode(envelope.Request{RequestID: "req_task_missing"}, "rs_1", "demo", "task_not_found", "project task missing not found", true)
	if err != nil {
		t.Fatal(err)
	}
	response := decodeToolResult(t, result)
	errorBody, _ := response["error"].(map[string]any)
	details, _ := errorBody["details"].(map[string]any)
	next, _ := details["next_action"].(map[string]any)
	assertSuggestedActionFitsPublicSchema(t, rt, next)
	args, _ := next["arguments"].(map[string]any)
	if next["tool"] != "runtime_read" || args["view"] != "project" || args["remote_session_id"] != "rs_1" {
		t.Fatalf("project task recovery=%+v", next)
	}
}

func TestEditSchemaIsSelfDescribingAndFlat(t *testing.T) {
	runtime := &Runtime{}
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	runtime.registerTools(protocol)
	registered := runtime.listedToolMap()["edit"]
	if len(mcpresult.ToolSchemaJSON(registered)) == 0 {
		t.Fatal("edit must expose a raw schema")
	}
	var schema map[string]any
	if err := json.Unmarshal(mcpresult.ToolSchemaJSON(registered), &schema); err != nil {
		t.Fatal(err)
	}
	if _, hasOneOf := schema["oneOf"]; hasOneOf {
		t.Fatalf("edit schema must not rely on top-level oneOf: %s", mcpresult.ToolSchemaJSON(registered))
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("edit must reject unknown fields: %s", mcpresult.ToolSchemaJSON(registered))
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok || properties["remote_session_id"] == nil || properties["purpose"] == nil || properties["idempotency_key"] == nil {
		t.Fatalf("edit semantic fields are invalid: %+v", properties)
	}
	edits := properties["edits"].(map[string]any)
	items := edits["items"].(map[string]any)
	itemProperties := items["properties"].(map[string]any)
	for _, field := range []string{"operation", "path", "rev", "content", "new_path", "replacements"} {
		fieldSchema, ok := itemProperties[field].(map[string]any)
		if !ok || strings.TrimSpace(fieldSchema["description"].(string)) == "" {
			t.Fatalf("edit item field %q is not self describing", field)
		}
	}
	replacementItems := itemProperties["replacements"].(map[string]any)["items"].(map[string]any)
	for _, field := range []string{"match", "replacement"} {
		fieldSchema := replacementItems["properties"].(map[string]any)[field].(map[string]any)
		if strings.TrimSpace(fieldSchema["description"].(string)) == "" {
			t.Fatalf("replacement field %q has no description", field)
		}
	}
}

func containsAnyString(value any, wanted string) bool {
	switch items := value.(type) {
	case []string:
		for _, item := range items {
			if item == wanted {
				return true
			}
		}
	case []any:
		for _, item := range items {
			if item, ok := item.(string); ok && item == wanted {
				return true
			}
		}
	}
	return false
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
