package server

import (
	"mcpx/internal/mcpresult"

	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestReadOnlyToolAnnotationsAndSessionOpenDefaults(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	rt.registerTools(protocol)
	tools := rt.listedToolMap()
	if _, exists := tools["approval_manage"]; exists {
		t.Fatal("approval_manage must not be exposed; semantic confirmation uses the original tool")
	}
	for _, name := range []string{"observe", "read", "runtime_read", "environment_read"} {
		annotation := tools[name].Annotations
		if !annotation.ReadOnlyHint || annotation.DestructiveHint != nil && *annotation.DestructiveHint || !annotation.IdempotentHint {
			t.Fatalf("%s annotation is unsafe or incomplete: %+v", name, annotation)
		}
	}
	sessionAnnotation := tools["session"].Annotations
	if sessionAnnotation.DestructiveHint != nil && *sessionAnnotation.DestructiveHint {
		t.Fatalf("session should not be marked destructive: %+v", sessionAnnotation)
	}
	for _, name := range []string{"execute"} {
		annotation := tools[name].Annotations
		if annotation.DestructiveHint != nil && *annotation.DestructiveHint {
			t.Fatalf("%s should not be marked destructive: %+v", name, annotation)
		}
	}
	var commandSchema map[string]any
	if err := json.Unmarshal(mcpresult.ToolSchemaJSON(tools["execute"]), &commandSchema); err != nil {
		t.Fatal(err)
	}
	commandProperties := commandSchema["properties"].(map[string]any)
	if commandProperties["remote_session_id"] == nil || commandProperties["purpose"] == nil || commandProperties["user_confirmed"] == nil || commandProperties["scope"] != nil {
		t.Fatalf("execute schema must expose clean-core semantic fields without single-value scope: %+v", commandProperties)
	}
	for _, name := range []string{"skill_tool", "mcp_tool"} {
		var extensionSchema map[string]any
		if err := json.Unmarshal(mcpresult.ToolSchemaJSON(tools[name]), &extensionSchema); err != nil {
			t.Fatal(err)
		}
		properties := extensionSchema["properties"].(map[string]any)
		if properties["confirmation_key"] != nil || properties["user_confirmed"] != nil {
			t.Fatalf("%s schema must leave confirmation decisions to the client: %+v", name, properties)
		}
	}
	var sessionSchema map[string]any
	if err := json.Unmarshal(mcpresult.ToolSchemaJSON(tools["session"]), &sessionSchema); err != nil {
		t.Fatal(err)
	}
	if _, exists := sessionSchema["properties"].(map[string]any)["include_skills"]; exists {
		t.Fatal("session must not expose request-level include_skills; use server discovery.skills config")
	}
	var readSchema map[string]any
	if err := json.Unmarshal(mcpresult.ToolSchemaJSON(tools["read"]), &readSchema); err != nil {
		t.Fatal(err)
	}
	modeSchema, ok := readSchema["properties"].(map[string]any)["mode"].(map[string]any)
	enumValues, _ := modeSchema["enum"].([]any)
	fullMode := false
	for _, value := range enumValues {
		if value == "full" {
			fullMode = true
			break
		}
	}
	if !ok || !fullMode {
		t.Fatalf("read must expose mode=full for complete client previews: %+v", modeSchema)
	}

	request := mcpresult.Request(map[string]any{"intent": "open the demo workspace", "workspace": "demo"})

	result, err := rt.toolSessionOpen(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	data := structuredBusinessData(result)
	if data == nil {
		t.Fatalf("session structured content type=%T", result.StructuredContent)
	}
	instructions, _ := data["instructions"].(map[string]any)
	if instructions["inline"] != false {
		t.Fatalf("session should default to instruction metadata only: %+v", instructions)
	}
	if data["project_tasks"] != nil {
		t.Fatalf("session should not expand project tasks by default: %+v", data["project_tasks"])
	}
	for _, item := range asMapSlice(data["tools"]) {
		for _, forbidden := range []string{"input_schema", "description", "annotations"} {
			if _, exists := item[forbidden]; exists {
				t.Fatalf("session capability state must not duplicate tools/list field %q: %+v", forbidden, item)
			}
		}
	}

	remote := data["remote_session"].(map[string]any)
	remoteID := remote["id"].(string)
	capabilityRequest := mcpresult.Request(map[string]any{"intent": "inspect capabilities", "action": "capabilities", "remote_session_id": remoteID})

	capabilityResult, err := rt.toolRuntimeInspect(context.Background(), capabilityRequest)
	if err != nil {
		t.Fatal(err)
	}
	capabilityData := structuredBusinessData(capabilityResult)
	editCapabilityFound := false
	moveOutCapabilityFound := false
	for _, item := range asMapSlice(capabilityData["tools"]) {
		if item["name"] == "move_out" {
			moveOutCapabilityFound = true
			safety, _ := item["safety"].(map[string]any)
			if safety["approval"] != "web_model_user_confirmation_required" || safety["registered_workspace"] != true || safety["confirmation_credential"] != "server_generated_confirmation_uuid" || safety["no_symlink_following"] != true || safety["symlink_entry_move"] != true || safety["reversible"] != true {
				t.Fatalf("runtime_read must publish move_out safety metadata: %+v", item)
			}
			continue
		}
		if item["name"] != "edit" {
			continue
		}
		editCapabilityFound = true
		safety, _ := item["safety"].(map[string]any)
		if safety["approval"] == "host_user_approval_required" || safety["scope"] != "registered_workspace_root" {
			t.Fatalf("runtime_read must publish edit safety metadata: %+v", item)
		}
	}
	if !editCapabilityFound {
		t.Fatal("runtime_read did not publish the edit capability")
	}
	if !moveOutCapabilityFound {
		t.Fatal("runtime_read did not publish the move_out capability")
	}
	if capabilityData["tool_manifest"] != nil || capabilityData["client_refresh"] != nil {
		t.Fatalf("runtime capabilities must not duplicate schema or client bookkeeping: %+v", capabilityData)
	}
	for _, item := range asMapSlice(capabilityData["tools"]) {
		for _, forbidden := range []string{"input_schema", "inputSchema", "description", "annotations"} {
			if _, exists := item[forbidden]; exists {
				t.Fatalf("runtime capability state must not duplicate tools/list field %q: %+v", forbidden, item)
			}
		}
	}
	var runtimeReadSchema map[string]any
	if err := json.Unmarshal(mcpresult.ToolSchemaJSON(tools["runtime_read"]), &runtimeReadSchema); err != nil {
		t.Fatal(err)
	}
	runtimeReadProperties := runtimeReadSchema["properties"].(map[string]any)
	for _, removed := range []string{"include_tool_schemas", "known_revisions"} {
		if runtimeReadProperties[removed] != nil {
			t.Fatalf("runtime_read must not expose client schema bookkeeping %q: %+v", removed, runtimeReadProperties)
		}
	}
	if data["client_refresh"] != nil || data["omitted_sections"] != nil {
		t.Fatalf("session bootstrap must not expose client revision bookkeeping: %+v", data)
	}
}
