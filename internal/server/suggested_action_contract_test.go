package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCommonSuggestedActionsFitPublicSchemas(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	cases := []struct {
		name   string
		action map[string]any
	}{
		{name: "workspace recovery", action: nextAction("workspace", map[string]any{})},
		{name: "session recovery", action: nextAction("session", map[string]any{"workspace": "demo"})},
		{name: "file continuation", action: nextAction("read", map[string]any{"remote_session_id": "rs_1", "path": "a.go", "offset": 100, "limit": 100})},
		{name: "context continuation", action: nextAction("context_query", map[string]any{"remote_session_id": "rs_1", "action": "query", "query": "needle", "mode": "smart", "cursor": "next"})},
		{name: "task continuation", action: nextAction("execute", map[string]any{"remote_session_id": "rs_1", "action": "attach", "execution_task_id": "task_1", "stdout_offset": 10, "stderr_offset": 20})},
		{name: "task logs", action: nextAction("observe", map[string]any{"remote_session_id": "rs_1", "view": "logs", "execution_task_id": "task_1", "stdout_offset": 10, "stderr_offset": 20})},
		{name: "environment snapshot", action: nextAction("environment_inspect", map[string]any{"remote_session_id": "rs_1", "save_snapshot": true})},
		{name: "environment compare", action: nextAction("environment_inspect", map[string]any{"workspace": "demo", "action": "compare", "compare_to": "env_1"})},
		{name: "legacy task status", action: nextAction("task_manage", map[string]any{"remote_session_id": "rs_1", "action": "status", "execution_task_id": "task_1"})},
		{name: "artifact continuation", action: nextAction("artifact", map[string]any{"remote_session_id": "rs_1", "action": "read", "artifact_id": "art_1", "offset": 1024, "limit": 1024})},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			assertSuggestedActionFitsPublicSchema(t, rt, tt.action)
		})
	}

	contextAction := cases[3].action
	contextArgs := contextAction["arguments"].(map[string]any)
	if contextAction["tool"] != "read" || contextArgs["view"] != "context" || contextArgs["search_mode"] != "smart" || contextArgs["mode"] != nil {
		t.Fatalf("context continuation was not normalized to read semantics: %+v", contextAction)
	}
	snapshotArgs := cases[6].action["arguments"].(map[string]any)
	if cases[6].action["tool"] != "environment" || snapshotArgs["action"] != nil || snapshotArgs["save_snapshot"] != nil {
		t.Fatalf("environment snapshot continuation retains deleted protocol fields: %+v", cases[6].action)
	}
}

func assertSuggestedActionFitsPublicSchema(t *testing.T, rt *Runtime, action map[string]any) {
	t.Helper()
	toolName, _ := action["tool"].(string)
	arguments, _ := action["arguments"].(map[string]any)
	tool, ok := rt.toolIndex[toolName]
	if !ok {
		t.Fatalf("suggested unknown tool %q: %+v", toolName, action)
	}
	var schema map[string]any
	encoded, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &schema); err != nil {
		t.Fatal(err)
	}
	properties, _ := schema["properties"].(map[string]any)
	for key := range arguments {
		if properties[key] == nil {
			t.Fatalf("suggested %s argument %q is not in public schema: args=%+v schema=%s", toolName, key, arguments, encoded)
		}
	}
	required := requiredFields(schema)
	for _, key := range required {
		if value, exists := arguments[key]; !exists || value == nil || fmt.Sprint(value) == "" {
			t.Fatalf("suggested %s missing required %q: args=%+v", toolName, key, arguments)
		}
	}
	if actionName, _ := arguments["action"].(string); actionName != "" {
		if actionProp, ok := properties["action"].(map[string]any); ok {
			actionDesc, _ := actionProp["description"].(string)
			if !strings.Contains(actionDesc, actionName) {
				t.Fatalf("suggested %s action %q is not described in action.description: %s", toolName, actionName, actionDesc)
			}
		}
	}
}

func requiredFields(schema map[string]any) []string {
	raw, _ := schema["required"].([]any)
	result := make([]string, 0, len(raw))
	for _, value := range raw {
		if key, ok := value.(string); ok {
			result = append(result, key)
		}
	}
	return result
}

var _ mcp.Tool
