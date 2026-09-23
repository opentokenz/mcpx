package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"
)

func TestRemoteRequestAllowsReadWithoutPurpose(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "demo")
	request := mcpresult.Request(map[string]any{"workspace": "demo"})

	_, _, failure := runtime.remoteRequest(context.Background(), request)
	if failure != nil {
		t.Fatalf("read request should not require purpose: %+v", failure)
	}
}

func TestMutatingRequestRejectsOversizedPurpose(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "demo")
	request := mcpresult.Request(map[string]any{"purpose": strings.Repeat("x", 513), "workspace": "demo"})

	_, _, _, failure := runtime.changeRequest(context.Background(), request, true)
	if failure == nil {
		t.Fatal("oversized purpose was accepted")
	}
	response := decodeToolResult(t, failure)
	if errorCode(response) != "purpose_required" {
		t.Fatalf("response=%+v", response)
	}
}

func TestPublicToolSchemasExcludeObservationBookkeeping(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "demo")
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	runtime.registerTools(protocol)

	forbidden := []string{"reasoning_summary", "progress_summary", "next_step", "goal", "task_id"}
	for name, registered := range runtime.listedToolMap() {
		schema := decodedToolSchema(t, registered)
		assertNoSchemaFields(t, name, schema, forbidden)
		if name != "observe" {
			assertNoSchemaFields(t, name, schema, []string{"call_id"})
		}
	}

	for _, name := range []string{"workspace", "read", "observe", "runtime_read", "environment_read"} {
		schema := decodedToolSchema(t, runtime.listedToolMap()[name])
		properties, _ := schema["properties"].(map[string]any)
		if properties["purpose"] != nil {
			t.Errorf("read-only tool %q must not expose purpose: %+v", name, properties)
		}
	}

	read := decodedToolSchema(t, runtime.listedToolMap()["read"])
	readProperties, _ := read["properties"].(map[string]any)
	if readProperties["execution_mode"] != nil {
		t.Fatalf("read must not expose generic execution_mode: %+v", readProperties)
	}
}

func TestEffectfulToolsOwnPurposeContract(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "demo")
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	runtime.registerTools(protocol)
	registered := runtime.listedToolMap()

	for _, name := range []string{"edit", "operation_batch", "screenshot_capture", "secret_provide"} {
		schema := decodedToolSchema(t, registered[name])
		properties, _ := schema["properties"].(map[string]any)
		if properties["purpose"] == nil || !schemaRequires(schema, "purpose") {
			t.Errorf("effectful tool %q must explicitly require purpose: %+v", name, schema)
		}
	}
	for _, name := range []string{"skill_tool", "mcp_tool"} {
		schema := decodedToolSchema(t, registered[name])
		properties, _ := schema["properties"].(map[string]any)
		actionProp, _ := properties["action"].(map[string]any)
		actionDesc, _ := actionProp["description"].(string)
		if !strings.Contains(actionDesc, "call") || !strings.Contains(actionDesc, "purpose") {
			t.Fatalf("%s action.description must document purpose for call: %s", name, actionDesc)
		}
	}

	execute := decodedToolSchema(t, registered["execute"])
	execProps, _ := execute["properties"].(map[string]any)
	execAction, _ := execProps["action"].(map[string]any)
	execDesc, _ := execAction["description"].(string)
	if !strings.Contains(execDesc, "run") || !strings.Contains(execDesc, "purpose") {
		t.Fatalf("execute action.description must document purpose for run: %s", execDesc)
	}
	moveOut := decodedToolSchema(t, registered["move_out"])
	moveProps, _ := moveOut["properties"].(map[string]any)
	moveAction, _ := moveProps["action"].(map[string]any)
	moveDesc, _ := moveAction["description"].(string)
	if !strings.Contains(moveDesc, "prepare") || !strings.Contains(moveDesc, "purpose") {
		t.Fatalf("move_out action.description must document purpose for prepare: %s", moveDesc)
	}
}

func TestPurposeDescriptionsAreLocalToEffect(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "demo")
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	runtime.registerTools(protocol)
	registered := runtime.listedToolMap()

	checks := map[string]string{
		"execute":            "用户目标",
		"edit":               "文件变更",
		"screenshot_capture": "屏幕",
		"secret_provide":     "Secret",
	}
	for name, fragment := range checks {
		schema := decodedToolSchema(t, registered[name])
		properties, _ := schema["properties"].(map[string]any)
		purpose, _ := properties["purpose"].(map[string]any)
		description, _ := purpose["description"].(string)
		if !strings.Contains(description, fragment) {
			t.Errorf("tool %q purpose description missing %q: %q", name, fragment, description)
		}
	}
}

func decodedToolSchema(t *testing.T, tool mcp.Tool) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(mcpresult.ToolSchemaJSON(tool), &schema); err != nil {
		t.Fatalf("tool %q schema: %v", tool.Name, err)
	}
	return schema
}

func assertNoSchemaFields(t *testing.T, toolName string, schema map[string]any, forbidden []string) {
	t.Helper()
	if properties, _ := schema["properties"].(map[string]any); properties != nil {
		for _, field := range forbidden {
			if properties[field] != nil {
				t.Errorf("tool %q must not expose protocol bookkeeping %q: %+v", toolName, field, properties)
			}
		}
	}
}

func schemaRequires(schema map[string]any, field string) bool {
	required, _ := schema["required"].([]any)
	for _, raw := range required {
		if raw == field {
			return true
		}
	}
	return false
}
