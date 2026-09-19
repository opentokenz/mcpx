package arc

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"
)

func TestWrapToolResultProducesARCSearchEnvelope(t *testing.T) {
	raw := mcpresult.NewStructured(map[string]any{
		"files":     []map[string]any{{"path": "internal/server/runtime.go"}},
		"truncated": true,
		"next_action": map[string]any{
			"tool":      "context_query",
			"arguments": map[string]any{"action": "query", "cursor": "next"},
		},
	}, "Context query returned 1 file.")
	wrapped := WrapToolResult("context_query", ResultContext{
		RequestID: "req_test", TraceID: "tr_test", SpanID: "sp_test",
		Context: Context{Activity: &Activity{TurnID: "turn_test", Sequence: 2, State: "preparing_action", Kind: "next", Summary: "继续读取", RelatedCallID: "req_test"}},
		Timing: Timing{StartedAtMs: 100, ReceivedAtMs: 110, CompletedAtMs: 125,
			NetworkLatencyMs: 10, ProcessingMs: 15, ServerElapsedMs: 25},
	}, raw)

	payload := decodeEnvelope(t, wrapped)
	mcpx := payload["mcpx"].(map[string]any)
	if mcpx["version"] != Version {
		t.Fatalf("version = %v", mcpx["version"])
	}
	trace := mcpx["trace"].(map[string]any)
	if trace["request_id"] != "req_test" || trace["trace_id"] != "tr_test" || trace["span_id"] != "sp_test" {
		t.Fatalf("trace = %+v", trace)
	}
	if trace["started_at_ms"] != float64(100) || trace["received_at_ms"] != float64(110) || trace["completed_at_ms"] != float64(125) {
		t.Fatalf("trace timing = %+v", trace)
	}
	structured := wrapped.StructuredContent.(map[string]any)
	if structured["timing"] != nil {
		t.Fatalf("model structured content must not repeat timing: %+v", structured["timing"])
	}
	structuredContext, _ := structured["context"].(map[string]any)
	if structuredContext["activity"] != nil {
		t.Fatalf("model structured content must not repeat activity snapshot: %+v", structuredContext["activity"])
	}
	if structured["hints"] != nil {
		t.Fatalf("normal success must not expose presentation-only hints: %+v", structured["hints"])
	}
	result := mcpx["result"].(map[string]any)
	if result["type"] != "search_result" || result["schema"] != SchemaSearchResult {
		t.Fatalf("result identity = %+v", result)
	}
	structuredData, _ := structured["data"].(map[string]any)
	if structuredData["truncated"] != true {
		t.Fatalf("structured data = %+v", structuredData)
	}
	if result["data"] != nil {
		t.Fatalf("metadata must not copy business data: %+v", result["data"])
	}
	if result["hints"].(map[string]any)["preferred_behavior"] != "show_directly" {
		t.Fatalf("hints = %+v", result["hints"])
	}
	actions := result["actions"].([]any)
	if len(actions) != 1 || actions[0].(map[string]any)["type"] != "continue" {
		t.Fatalf("actions = %+v", actions)
	}

	text := wrapped.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{"Context query returned 1 file.", "`internal/server/runtime.go`"} {
		if !strings.Contains(text, want) {
			t.Fatalf("context query text missing %q: %q", want, text)
		}
	}
	if strings.Contains(text, "\"files\"") || strings.Contains(text, "structured_content") {
		t.Fatalf("context query text leaked machine JSON: %q", text)
	}
}

func TestWrapToolResultRendersWorkspaceListInsteadOfOK(t *testing.T) {
	raw := mcpresult.NewText(`{"request_id":"req_workspace","status":"ok","data":{"workspaces":[{"name":"fyy","path":"/workspaces/fyy","description":"ERP frontend"}]}}`)
	wrapped := WrapToolResult("workspace_list", ResultContext{}, raw)
	text := wrapped.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{"Available workspaces:", "`fyy`", "`/workspaces/fyy`", "ERP frontend"} {
		if !strings.Contains(text, want) {
			t.Fatalf("workspace list text missing %q: %q", want, text)
		}
	}
	if text == "ok" || strings.Contains(text, "\"workspaces\"") {
		t.Fatalf("workspace list must not degrade to JSON/ok: %q", text)
	}
	if wrapped.Meta == nil || wrapped.Meta[ResultMetadataKey] == nil {
		t.Fatal("workspace list machine result must remain in metadata")
	}
}

func TestWrapToolResultRendersSessionBootstrapAsMarkdown(t *testing.T) {
	raw := mcpresult.NewStructured(map[string]any{
		"remote_session": map[string]any{"id": "rs_demo", "role": "owner", "status": "active"},
		"workspace":      map[string]any{"name": "fyy", "path": "/workspaces/fyy", "git_head": "abc123"},
		"tools":          []map[string]any{{"name": "read"}, {"name": "edit"}},
		"agent_guidance": map[string]any{
			"summary": "Use dedicated tools.",
			"rules":   []string{"Read before editing."},
		},
	}, "Session rs_demo opened for workspace fyy.")
	written := WrapToolResult("session_open", ResultContext{}, raw)
	text := written.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{"- Remote session: `rs_demo`", "`/workspaces/fyy`", "`read`", "`edit`", "Agent guidance", "Read before editing."} {
		if !strings.Contains(text, want) {
			t.Fatalf("session bootstrap text missing %q: %q", want, text)
		}
	}
	if strings.Contains(text, "\"remote_session\"") || strings.Contains(text, "structured_content") {
		t.Fatalf("session bootstrap leaked machine JSON: %q", text)
	}
}

func TestWrapToolResultRendersPlanIdentityAndProgress(t *testing.T) {
	raw := mcpresult.NewText(`{"status":"ok","data":{"plan_id":"pl_demo","goal":"legacy internal value","summary":"修复工具可见性","status":"ready","tasks":[{"plan_task_id":"pt_1"},{"plan_task_id":"pt_2"}],"progress":{"completed":1,"total":2}}}`)
	written := WrapToolResult("plan_manage", ResultContext{}, raw)
	text := written.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{"Plan ID: `pl_demo`", "Status: `ready`", "Summary: 修复工具可见性", "Progress: 1/2 completed", "`pt_1`", "`pt_2`"} {
		if !strings.Contains(text, want) {
			t.Fatalf("plan text missing %q: %q", want, text)
		}
	}
	if strings.Contains(text, "Goal:") || strings.Contains(text, `"plan_id"`) || text == "ok" {
		t.Fatalf("plan must not expose goal or degrade to JSON/ok: %q", text)
	}
}

func TestWrapToolResultRendersExtensionInventory(t *testing.T) {
	raw := mcpresult.NewStructured(map[string]any{
		"skills": []map[string]any{{"name": "ui-ux-pro-max", "description": "UI design"}},
	}, "Extension inventory returned.")
	written := WrapToolResult("extension_manage", ResultContext{}, raw)
	text := written.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{"Extension inventory returned.", "Skills:", "`ui-ux-pro-max`"} {
		if !strings.Contains(text, want) {
			t.Fatalf("extension inventory missing %q: %q", want, text)
		}
	}
}

func TestWrapToolResultRendersSearchMatchesAndSourceSnippet(t *testing.T) {
	raw := mcpresult.NewStructured(map[string]any{
		"matches": []map[string]any{{"path": "src/pages/Sale.vue", "line": 42, "text": "const customer = await findCustomer()"}},
		"files":   []map[string]any{{"path": "src/pages/Sale.vue", "content": "<template>\n  <CustomerSearch />\n</template>\n"}},
	}, "Source search returned 1 match(es).")
	written := WrapToolResult("context_query", ResultContext{}, raw)
	text := written.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{"`src/pages/Sale.vue:42`", "const customer = await findCustomer()", "```vue", "<CustomerSearch />"} {
		if !strings.Contains(text, want) {
			t.Fatalf("search text missing %q: %q", want, text)
		}
	}
	if strings.Contains(text, "\"matches\"") || strings.Contains(text, "\"content\"") {
		t.Fatalf("search text leaked machine JSON: %q", text)
	}
}

func TestWrapToolResultRendersOverriddenStructuredContentWithoutJSON(t *testing.T) {
	raw := mcpresult.NewText(`{"request_id":"req_project","status":"ok","data":{"stacks":["go"],"manifests":["go.mod"],"git_status":"## dev\n M internal/arc/arc.go"}}`)
	raw.StructuredContent = map[string]any{
		"stacks":     []string{"go"},
		"manifests":  []string{"go.mod"},
		"git_status": "## dev\n M internal/arc/arc.go",
	}
	written := WrapToolResult("runtime_inspect", ResultContext{}, raw)
	text := written.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{"Project summary:", "`go`", "`go.mod`", "internal/arc/arc.go"} {
		if !strings.Contains(text, want) {
			t.Fatalf("project summary missing %q: %q", want, text)
		}
	}
	if strings.Contains(text, "\"git_status\"") || strings.Contains(text, "request_id") {
		t.Fatalf("overridden structured content leaked JSON: %q", text)
	}
}

func TestWrapToolResultKeepsHumanTextAndModelStructuredContent(t *testing.T) {
	raw := mcpresult.NewStructured(map[string]any{
		"status": "ok",
		"value":  "ready",
	}, "已完成检查：环境正常。")
	written := WrapToolResult("runtime_inspect", ResultContext{}, raw)

	text, ok := written.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "已完成检查：环境正常。" {
		t.Fatalf("human-visible content = %#v", written.Content[0])
	}
	sc, ok := written.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("models need structuredContent, got %#v", written.StructuredContent)
	}
	if sc["status"] == nil || sc["type"] == nil || sc["context"] == nil || sc["data"] == nil {
		t.Fatalf("structuredContent must expose status/type/context/data: %#v", sc)
	}
	if sc["timing"] != nil {
		t.Fatalf("structuredContent must not repeat timing kept in ARC metadata: %#v", sc["timing"])
	}
	data, _ := sc["data"].(map[string]any)
	if data["value"] != "ready" {
		t.Fatalf("structured data.value = %#v", data)
	}
	if written.Meta == nil || written.Meta[ResultMetadataKey] == nil {
		t.Fatalf("ARC metadata missing: %#v", written.Meta)
	}
	// Human text must stay prose, not dump the machine envelope.
	if strings.Contains(text.Text, `"mcpx"`) || strings.Contains(text.Text, `"structuredContent"`) {
		t.Fatalf("human text must not dump machine JSON: %s", text.Text)
	}
}

func TestWrapToolResultExposesActivityV2ContextToARCAndHumanText(t *testing.T) {
	raw := mcpresult.NewStructured(map[string]any{
		"status": "succeeded",
		"data": map[string]any{
			"reasoning_summary": "legacy reasoning must stay out of ARC V2",
			"progress_summary":  "legacy progress must stay out of ARC V2",
			"next_step":         "legacy next must stay out of ARC V2",
			"activity":          map[string]any{"kind": "fake", "summary": "handler must not inject activity"},
		},
	}, "已完成文件读取")
	written := WrapToolResult("file_read", ResultContext{
		RequestID: "req_context",
		Context: Context{
			Purpose: "读取目标文件",
			Activity: &Activity{
				TurnID: "turn-42", Sequence: 7, State: "reviewing_result", Kind: "evidence",
				Summary: "已确认 ARC 仍使用旧 narration 字段", RelatedCallID: "call-read-1",
			},
			PlanID: "pl_context", PlanTaskID: "pt_context", ExecutionTaskID: "task_context", OperationID: "op_context",
		},
	}, raw)

	structured, ok := written.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent=%T", written.StructuredContent)
	}
	context, ok := structured["context"].(map[string]any)
	if !ok {
		t.Fatalf("context=%#v", structured["context"])
	}
	for key, want := range map[string]string{
		"purpose": "读取目标文件", "plan_id": "pl_context", "plan_task_id": "pt_context",
		"execution_task_id": "task_context", "operation_id": "op_context",
	} {
		if context[key] != want {
			t.Fatalf("context[%q]=%v, want %q", key, context[key], want)
		}
	}
	if context["activity"] != nil {
		t.Fatalf("model context must not repeat activity snapshot: %#v", context["activity"])
	}
	envelope := decodeEnvelope(t, written)
	mcpx, _ := envelope["mcpx"].(map[string]any)
	result, _ := mcpx["result"].(map[string]any)
	metadataContext, _ := result["context"].(map[string]any)
	activity, ok := metadataContext["activity"].(map[string]any)
	if !ok {
		t.Fatalf("ARC metadata activity=%#v", metadataContext["activity"])
	}
	for key, want := range map[string]any{
		"turn_id": "turn-42", "sequence": float64(7), "state": "reviewing_result", "kind": "evidence",
		"summary": "已确认 ARC 仍使用旧 narration 字段", "related_call_id": "call-read-1",
	} {
		if activity[key] != want {
			t.Fatalf("ARC metadata activity[%q]=%v, want %v", key, activity[key], want)
		}
	}
	for _, forbidden := range []string{"goal", "task_id", "reasoning_summary", "progress_summary", "next_step"} {
		if _, exists := context[forbidden]; exists {
			t.Fatalf("ARC V2 context must not expose %q: %+v", forbidden, context)
		}
	}
	text := written.Content[0].(*mcp.TextContent).Text
	for _, forbidden := range []string{"legacy reasoning", "legacy progress", "legacy next", "handler must not inject activity"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("human text leaked legacy/fake context %q: %s", forbidden, text)
		}
	}
	for _, want := range []string{"Context:", "- purpose: 读取目标文件", "activity: Evidence 已确认 ARC 仍使用旧 narration 字段", "- plan: pl_context · plan task: pt_context · execution task: task_context · operation: op_context"} {
		if !strings.Contains(text, want) {
			t.Fatalf("human text missing %q: %s", want, text)
		}
	}
}

func TestWrapToolResultMapsSemanticConfirmationWithoutApprovalAction(t *testing.T) {
	raw := mcpresult.NewText(`{"ok":false,"status":"need_confirmation","data":{"confirmation_required":true,"confirmation_message":"请确认后重试","command":"go test ./...","purpose":"运行测试","confirmation_token":"ct_full_token_1234567890abcdef"},"error":{"code":"USER_CONFIRMATION_REQUIRED","message":"命令执行等待用户语义确认"}}`)
	wrapped := WrapToolResult("command_execute", ResultContext{
		RequestID: "req_confirmation", TraceID: "tr_confirmation", SpanID: "sp_confirmation",
		Timing: Timing{ServerElapsedMs: 4},
	}, raw)
	text := wrapped.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "confirmation_token: `ct_full_token_1234567890abcdef`") {
		t.Fatalf("confirmation display must carry the token in text: %s", text)
	}
	if !strings.Contains(text, "```sh\ngo test ./...\n```") {
		t.Fatalf("confirmation command must be shell-highlightable: %s", text)
	}
	result := decodeEnvelope(t, wrapped)["mcpx"].(map[string]any)["result"].(map[string]any)
	if result["type"] != "error" || result["schema"] != SchemaError {
		t.Fatalf("result = %+v", result)
	}
	if result["hints"].(map[string]any)["preferred_behavior"] != "ask_confirm" {
		t.Fatalf("hints = %+v", result["hints"])
	}
	structuredConfirmation, _ := wrapped.StructuredContent.(map[string]any)
	structuredHints, _ := structuredConfirmation["hints"].(Hints)
	if structuredHints.PreferredBehavior != "ask_confirm" {
		t.Fatalf("model confirmation hint missing: %+v", structuredConfirmation)
	}
	if _, exists := result["actions"]; exists {
		t.Fatalf("semantic confirmation must not create an approval action: %+v", result["actions"])
	}
	structuredData, _ := structuredConfirmation["data"].(map[string]any)
	if structuredData["confirmation_token"] != "ct_full_token_1234567890abcdef" || structuredData["command"] != "go test ./..." {
		t.Fatalf("semantic confirmation credentials must remain in model-facing data: %+v", structuredData)
	}
	if _, exists := structuredConfirmation["actions"]; exists {
		t.Fatalf("semantic confirmation must not expose an automatic model action: %+v", structuredConfirmation)
	}
}

func TestWrapToolResultCanonicalizesAllRecoveryActions(t *testing.T) {
	raw := mcpresult.NewText(`{"status":"failed","data":{"path":"demo.go","next_action":{"tool":"read","arguments":{"path":"demo.go","view":"file"}}},"error":{"code":"STALE_REVISION","message":"stale","category":"conflict","retryable":true,"recovery":{"action":"read","tool":"read","arguments":{"path":"demo.go","view":"file"}},"details":{"current_sha256":"sha256:new","recovery":"refresh and retry","suggested_next":{"tool":"read","arguments":{"path":"demo.go","view":"file"}},"next_actions":[{"tool":"read","arguments":{"path":"demo.go","view":"file"}},{"tool":"edit","arguments":{"path":"demo.go","apply":false}}]}}}`)
	wrapped := WrapToolResult("edit", ResultContext{}, raw)
	structured := wrapped.StructuredContent.(map[string]any)
	data := structured["data"].(map[string]any)
	if data["path"] != "demo.go" || data["next_action"] != nil {
		t.Fatalf("model data must keep business facts without recovery duplication: %+v", data)
	}
	errBody := structured["error"].(map[string]any)
	if errBody["recovery"] != nil {
		t.Fatalf("model error must not duplicate recovery: %+v", errBody)
	}
	details := errBody["details"].(map[string]any)
	for _, key := range []string{"recovery", "suggested_next", "next_action", "next_actions"} {
		if details[key] != nil {
			t.Fatalf("model error details still contain recovery field %q: %+v", key, details)
		}
	}
	if details["current_sha256"] != "sha256:new" {
		t.Fatalf("error fact was lost: %+v", details)
	}
	actions, _ := structured["actions"].([]Action)
	if len(actions) != 2 {
		t.Fatalf("all distinct recovery actions must survive canonicalization: %+v", structured["actions"])
	}
	if actions[0].ID != "read" || actions[1].ID != "edit_2" {
		t.Fatalf("canonical recovery actions=%+v", actions)
	}
}

func TestWrapToolResultRendersCommandAsShellBlock(t *testing.T) {
	raw := mcpresult.NewText(`{"status":"succeeded","data":{"command":"grep -n foo README.md","completed_in_call":true,"exit_code":0,"stdout":"12:foo\n","stderr":""}}`)
	wrapped := WrapToolResult("command_execute", ResultContext{}, raw)
	text := wrapped.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{"Command:", "```sh\ngrep -n foo README.md\n```", "stdout:", "```text\n12:foo\n```"} {
		if !strings.Contains(text, want) {
			t.Fatalf("command ARC display missing %q: %s", want, text)
		}
	}
	result := decodeEnvelope(t, wrapped)["mcpx"].(map[string]any)["result"].(map[string]any)
	if result["type"] != "log" || result["schema"] != SchemaLog {
		t.Fatalf("command result contract changed: %+v", result)
	}
}

func TestWrapToolResultShowsOperationConfirmationToken(t *testing.T) {
	raw := mcpresult.NewText(`{"status":"waiting_confirmation","data":{"operation_id":"op_wait","steps":[{"id":"main","tool":"command_run","state":"waiting_confirmation","confirmation_token":"ct_op_token"}],"confirmation_required":true},"error":{"code":"USER_CONFIRMATION_REQUIRED","message":"操作等待语义确认"}}`)
	wrapped := WrapToolResult("operation_manage", ResultContext{
		RequestID: "req_op_wait", TraceID: "tr_op_wait", SpanID: "sp_op_wait",
		Timing: Timing{ServerElapsedMs: 4},
	}, raw)
	text := wrapped.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{"op_wait", "confirmation_token: `ct_op_token`", "action=resume"} {
		if !strings.Contains(text, want) {
			t.Fatalf("operation confirmation display missing %q: %s", want, text)
		}
	}
}

func TestWrapToolResultPreservesNonTextContent(t *testing.T) {
	resource := mcpresult.NewResourceLink("mcpx://test/resource", "resource", "test resource", "text/plain")
	raw := mcpresult.NewText("plain")
	raw.Content = append(raw.Content, resource)
	wrapped := WrapToolResult("artifact_manage", ResultContext{
		RequestID: "req_resource", TraceID: "tr_resource", SpanID: "sp_resource",
	}, raw)
	if len(wrapped.Content) != 2 {
		t.Fatalf("content length = %d", len(wrapped.Content))
	}
	if _, ok := wrapped.Content[0].(*mcp.TextContent); !ok {
		t.Fatalf("first content = %T", wrapped.Content[0])
	}
	if _, ok := wrapped.Content[1].(*mcp.ResourceLink); !ok {
		t.Fatalf("second content = %T", wrapped.Content[1])
	}
}

func TestOutputSchemaAndRegistry(t *testing.T) {
	rawOutputSchema := OutputSchema()
	if len(rawOutputSchema) > 1024 {
		t.Fatalf("output schema is too large for repeated tools/list exposure: %d bytes", len(rawOutputSchema))
	}
	t.Logf("structured output schema bytes=%d", len(rawOutputSchema))
	var schema map[string]any
	if err := json.Unmarshal(rawOutputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["$id"] != "mcpx.structured_content.v2.0" {
		t.Fatalf("schema id = %v", schema["$id"])
	}
	required, _ := schema["required"].([]any)
	for _, field := range []any{"status", "type", "context", "data"} {
		if !containsAny(required, field) {
			t.Fatalf("output schema missing required %v: %v", field, required)
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	if properties["context"] == nil || properties["data"] == nil || properties["timing"] != nil {
		t.Fatalf("structured output schema must expose context/data without timing: %+v", properties)
	}
	contextSchema, _ := properties["context"].(map[string]any)
	contextProperties, _ := contextSchema["properties"].(map[string]any)
	if contextProperties["activity"] != nil {
		t.Fatalf("model output schema must not expose activity snapshot: %+v", contextSchema)
	}
	for _, forbidden := range []string{"reasoning_summary", "progress_summary", "next_step"} {
		if contextProperties[forbidden] != nil {
			t.Fatalf("ARC V2 context schema must not expose %q: %+v", forbidden, contextProperties)
		}
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("structured output schema must reject unknown envelope fields: %+v", schema)
	}
	outputDataSchema, _ := properties["data"].(map[string]any)
	if outputDataSchema["type"] != "object" || outputDataSchema["additionalProperties"] != true {
		t.Fatalf("structured output schema must keep business data open: %+v", outputDataSchema)
	}
	registry := SchemaRegistry()
	if len(registry) != 13 {
		t.Fatalf("registry size = %d", len(registry))
	}
	for name, raw := range registry {
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatalf("schema %s: %v", name, err)
		}
		if item["$id"] != name {
			t.Fatalf("schema %s id = %v", name, item["$id"])
		}
	}
	var codeChangeSchema map[string]any
	if err := json.Unmarshal(registry[SchemaCodeChange], &codeChangeSchema); err != nil {
		t.Fatal(err)
	}
	codeChangeProperties, _ := codeChangeSchema["properties"].(map[string]any)
	dataSchema, _ := codeChangeProperties["data"].(map[string]any)
	codeChangeProperties, _ = dataSchema["properties"].(map[string]any)
	for _, field := range []string{"edit_id", "status", "results", "total_changed_lines"} {
		if codeChangeProperties[field] == nil {
			t.Fatalf("code change schema missing %s: %+v", field, codeChangeSchema)
		}
	}
	if codeChangeProperties["diff_summary"] != nil {
		t.Fatalf("code change schema must not expose duplicate diff_summary: %+v", codeChangeSchema)
	}
	for _, legacy := range []string{"changeset_id", "digest", "expected_digest", "files"} {
		if codeChangeProperties[legacy] != nil {
			t.Fatalf("code change schema still exposes legacy field %s: %+v", legacy, codeChangeSchema)
		}
	}
	var planSchema map[string]any
	if err := json.Unmarshal(registry[SchemaPlan], &planSchema); err != nil {
		t.Fatal(err)
	}
	planEnvelopeProperties, _ := planSchema["properties"].(map[string]any)
	planDataSchema, _ := planEnvelopeProperties["data"].(map[string]any)
	planProperties, _ := planDataSchema["properties"].(map[string]any)
	if planProperties["goal"] != nil || planProperties["summary"] == nil {
		t.Fatalf("plan schema must expose summary without legacy goal: %+v", planSchema)
	}
	registry[SchemaText][0] = 'x'
	if string(SchemaRegistry()[SchemaText]) == string(registry[SchemaText]) {
		t.Fatal("schema registry must return independent copies")
	}
	output := OutputSchema()
	output[0] = 'x'
	if OutputSchema()[0] == 'x' {
		t.Fatal("output schema must return an independent copy")
	}
}

func containsAny(values []any, want any) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestWrapToolResultAddsPresentationAndDiagramResult(t *testing.T) {
	raw := mcpresult.NewStructured(map[string]any{
		"text": "```mermaid\nflowchart TD\n  A --> B\n```",
	}, "diagram")
	wrapped := WrapToolResult("context_query", ResultContext{}, raw)
	result := decodeEnvelope(t, wrapped)["mcpx"].(map[string]any)["result"].(map[string]any)
	if result["type"] != "diagram" || result["schema"] != SchemaDiagram {
		t.Fatalf("result identity = %+v", result)
	}
	presentation := result["presentation"].(map[string]any)
	if presentation["default"] != "diagram" {
		t.Fatalf("presentation = %+v", presentation)
	}
}

func TestWrapToolResultKeepsTruncatedMermaidAsSearchResult(t *testing.T) {
	raw := mcpresult.NewStructured(map[string]any{
		"text":      "```mermaid\nflowchart TD\n  A --> B\n```",
		"truncated": true,
	}, "truncated diagram")
	wrapped := WrapToolResult("context_query", ResultContext{}, raw)
	result := decodeEnvelope(t, wrapped)["mcpx"].(map[string]any)["result"].(map[string]any)
	if result["type"] != "search_result" || result["schema"] != SchemaSearchResult {
		t.Fatalf("truncated result identity = %+v", result)
	}
}

func TestWrapToolResultUsesDiagramCollectionForMultipleCompleteBlocks(t *testing.T) {
	raw := mcpresult.NewStructured(map[string]any{
		"markdown": "```mermaid\nflowchart TD\n  A --> B\n```\n\n```mermaid\ngraph LR\n  C --> D\n```",
	}, "diagrams")
	wrapped := WrapToolResult("context_query", ResultContext{}, raw)
	result := decodeEnvelope(t, wrapped)["mcpx"].(map[string]any)["result"].(map[string]any)
	if result["type"] != "diagram_collection" || result["schema"] != SchemaDiagramCollection {
		t.Fatalf("collection identity = %+v", result)
	}
	structured := wrapped.StructuredContent.(map[string]any)
	data, _ := structured["data"].(map[string]any)
	encodedDiagrams, err := json.Marshal(data["diagrams"])
	if err != nil {
		t.Fatal(err)
	}
	var diagrams []any
	if err := json.Unmarshal(encodedDiagrams, &diagrams); err != nil || len(diagrams) != 2 {
		t.Fatalf("collection data = %+v", data)
	}
	if result["data"] != nil {
		t.Fatalf("metadata must not copy business data: %+v", result["data"])
	}
}

func TestWrapToolResultUsesStableToolSemantics(t *testing.T) {
	tests := []struct {
		name     string
		tool     string
		data     map[string]any
		wantType string
		wantView string
	}{
		{name: "context query", tool: "context_query", data: map[string]any{"content": "plain source"}, wantType: "search_result", wantView: "table"},
		{name: "clean edit", tool: "edit", data: map[string]any{"edit_id": "edit_1", "results": []any{}}, wantType: "code_change", wantView: "diff"},
		{name: "task manage", tool: "task_manage", data: map[string]any{"status": "running"}, wantType: "log", wantView: "log"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wrapped := WrapToolResult(tt.tool, ResultContext{}, mcpresult.NewStructured(tt.data, "result"))
			result := decodeEnvelope(t, wrapped)["mcpx"].(map[string]any)["result"].(map[string]any)
			if result["type"] != tt.wantType || result["schema"] != schemaForType(tt.wantType) {
				t.Fatalf("result = %+v", result)
			}
			if result["summary"] != "result" {
				t.Fatalf("summary = %v", result["summary"])
			}
			presentation := result["presentation"].(map[string]any)
			if presentation["default"] != tt.wantView {
				t.Fatalf("presentation = %+v", presentation)
			}
		})
	}
}

func TestWrapToolResultDoesNotDuplicateExecuteOrReadPayload(t *testing.T) {
	stdout := strings.Repeat("x", 8000)
	content := strings.Repeat("line\n", 400)
	execute := WrapToolResult("execute", ResultContext{}, mcpresult.NewStructured(map[string]any{
		"command": "python -", "stdout": stdout, "exit_code": 0,
	}, "succeeded"))
	executeText := execute.Content[0].(*mcp.TextContent).Text
	if strings.Contains(executeText, stdout) {
		t.Fatalf("execute text copied full stdout (%d runes)", len([]rune(executeText)))
	}
	if !strings.Contains(executeText, "python -") {
		t.Fatalf("execute text missing command: %q", executeText)
	}
	executeEnvelope := decodeEnvelope(t, execute)["mcpx"].(map[string]any)["result"].(map[string]any)
	if executeEnvelope["data"] != nil {
		t.Fatalf("execute metadata copied data: %+v", executeEnvelope["data"])
	}
	encodedMeta, _ := json.Marshal(execute.Meta[ResultMetadataKey])
	if len(encodedMeta) > len(stdout) {
		t.Fatalf("execute metadata larger than payload: meta=%d stdout=%d", len(encodedMeta), len(stdout))
	}

	read := WrapToolResult("read", ResultContext{}, mcpresult.NewStructured(map[string]any{
		"path": "big.go", "content": content, "sha256": "abc", "truncated": true,
	}, "succeeded"))
	readText := read.Content[0].(*mcp.TextContent).Text
	if strings.Contains(readText, content) {
		t.Fatalf("read text copied file content: %q", readText)
	}
	if !strings.Contains(readText, "`big.go`") {
		t.Fatalf("read text missing path: %q", readText)
	}
	readEnvelope := decodeEnvelope(t, read)["mcpx"].(map[string]any)["result"].(map[string]any)
	if readEnvelope["data"] != nil {
		t.Fatalf("read metadata copied data: %+v", readEnvelope["data"])
	}
}

func decodeEnvelope(t *testing.T, result *mcp.CallToolResult) map[string]any {
	t.Helper()
	// ARC envelope lives in _meta; structuredContent is the model field payload.
	var value any
	if result.Meta != nil {
		value = result.Meta[ResultMetadataKey]
	}
	if value == nil {
		t.Fatal("missing ARC envelope in _meta")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}
