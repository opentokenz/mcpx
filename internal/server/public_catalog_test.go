package server

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"mcpx/internal/mcpresult"

	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestPublicCatalogSchemaTotalBytes(t *testing.T) {
	runtime := &Runtime{}
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	runtime.registerTools(protocol)

	var nestedDescriptionBytes func(any) int
	nestedDescriptionBytes = func(value any) int {
		total := 0
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				if key == "description" {
					if text, ok := child.(string); ok {
						total += len([]byte(text))
					}
					continue
				}
				total += nestedDescriptionBytes(child)
			}
		case []any:
			for _, child := range typed {
				total += nestedDescriptionBytes(child)
			}
		}
		return total
	}
	type schemaSize struct {
		name                  string
		inputBytes            int
		inputDescriptionBytes int
		outputBytes           int
		descBytes             int
	}
	sizes := make([]schemaSize, 0, len(runtime.listedToolMap()))
	inputTotal, outputTotal, descTotal := 0, 0, 0
	for name, tool := range runtime.listedToolMap() {
		inputSchemaJSON := mcpresult.ToolSchemaJSON(tool)
		inputBytes := len(inputSchemaJSON)
		var inputSchema any
		_ = json.Unmarshal(inputSchemaJSON, &inputSchema)
		inputDescriptionBytes := nestedDescriptionBytes(inputSchema)
		outputEncoded, _ := json.Marshal(tool.OutputSchema)
		outputBytes := len(outputEncoded)
		descBytes := len([]byte(tool.Description))
		inputTotal += inputBytes
		outputTotal += outputBytes
		descTotal += descBytes
		sizes = append(sizes, schemaSize{name: name, inputBytes: inputBytes, inputDescriptionBytes: inputDescriptionBytes, outputBytes: outputBytes, descBytes: descBytes})
	}
	sort.Slice(sizes, func(i, j int) bool {
		return sizes[i].inputBytes+sizes[i].outputBytes+sizes[i].descBytes > sizes[j].inputBytes+sizes[j].outputBytes+sizes[j].descBytes
	})
	for _, item := range sizes {
		t.Logf("public tool schema name=%s input=%d input_descriptions=%d output=%d description=%d total=%d", item.name, item.inputBytes, item.inputDescriptionBytes, item.outputBytes, item.descBytes, item.inputBytes+item.outputBytes+item.descBytes)
	}
	total := inputTotal + outputTotal + descTotal
	t.Logf("public tool schema input=%d output=%d description=%d total=%d", inputTotal, outputTotal, descTotal, total)
	if inputTotal > 70_000 {
		t.Fatalf("public input schemas exceeded context budget: %d bytes", inputTotal)
	}
	if outputTotal > 18_000 {
		t.Fatalf("public output schemas exceeded context budget: %d bytes", outputTotal)
	}
	if total > 90_000 {
		t.Fatalf("public tool catalog exceeded context budget: %d bytes", total)
	}
}

func TestPublicExecuteOmitsSingleValueScopeAndActionRootDescription(t *testing.T) {
	runtime := &Runtime{}
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	runtime.registerTools(protocol)

	execute := runtime.listedToolMap()["execute"]
	var executeSchema map[string]any
	if err := json.Unmarshal(mcpresult.ToolSchemaJSON(execute), &executeSchema); err != nil {
		t.Fatal(err)
	}
	properties, _ := executeSchema["properties"].(map[string]any)
	if properties["scope"] != nil {
		t.Fatalf("public execute schema must not expose single-value scope: %s", mcpresult.ToolSchemaJSON(execute))
	}

	for name, tool := range runtime.listedToolMap() {
		var schema map[string]any
		if err := json.Unmarshal(mcpresult.ToolSchemaJSON(tool), &schema); err != nil {
			t.Fatalf("%s schema: %v", name, err)
		}
		rootDescription, _ := schema["description"].(string)
		if rootDescription != "" && rootDescription == tool.Description {
			t.Fatalf("%s repeats Tool.Description in InputSchema root", name)
		}
	}
}

func TestPublicCatalogIsExactlyTheCleanCoreContract(t *testing.T) {
	runtime := &Runtime{}
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	runtime.registerTools(protocol)

	want := []string{
		"workspace", "session", "read", "edit", "move_out", "observe", "progress",
		"operation_batch", "operation_manage",
		"execute", "plan", "artifact", "skill_tool", "mcp_tool", "browser",
		"runtime_read", "environment_read", "environment", "screenshot_capture", "secret_provide",
	}
	got := make([]string, 0, len(runtime.listedToolMap()))
	for name := range runtime.listedToolMap() {
		got = append(got, name)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("public tool catalog = %v, want %v", got, want)
	}
	for _, legacy := range []string{"change", "change_read", "change_prepare", "change_execute", "change_manage", "move_out_prepare", "submit_move_out", "command_run", "command_execute", "task_read", "task", "plan_read", "extension_discover", "artifact_read", "discover", "skill_call", "mcp_call"} {
		if runtime.toolHandlers[legacy] != nil {
			t.Fatalf("legacy handler %q must not be dispatchable", legacy)
		}
	}

	for name, registered := range runtime.listedToolMap() {
		if name == "mcp_tool" {
			if registered.OutputSchema != nil {
				t.Fatalf("mcp_tool must omit OutputSchema: %+v", registered.OutputSchema)
			}
		} else if registered.OutputSchema == nil {
			t.Fatalf("%s must expose the ARC structuredContent output schema", name)
		}
		if _, limited := publishedLimits()[name]; limited {
			var output map[string]any
			encoded, _ := json.Marshal(registered.OutputSchema)
			_ = json.Unmarshal(encoded, &output)
			if output["x-mcpx-limits"] == nil {
				t.Fatalf("%s outputSchema must publish hard limits", name)
			}
		}
		var schema map[string]any
		if err := json.Unmarshal(mcpresult.ToolSchemaJSON(registered), &schema); err != nil {
			t.Fatalf("%s schema: %v", name, err)
		}
		assertRequiredKeywordsAreArrays(t, name, "$", schema)
		if _, union := schema["oneOf"]; union {
			t.Fatalf("%s must not rely on top-level oneOf: %s", name, mcpresult.ToolSchemaJSON(registered))
		}
		if _, anyOf := schema["anyOf"]; anyOf {
			t.Fatalf("%s must not rely on top-level anyOf: %s", name, mcpresult.ToolSchemaJSON(registered))
		}
		if _, allOf := schema["allOf"]; allOf {
			t.Fatalf("%s must not rely on top-level allOf: %s", name, mcpresult.ToolSchemaJSON(registered))
		}
		if schema["additionalProperties"] != false {
			t.Fatalf("%s must reject unknown arguments: %s", name, mcpresult.ToolSchemaJSON(registered))
		}
		properties, _ := schema["properties"].(map[string]any)
		if properties["task_id"] != nil {
			t.Fatalf("%s must not expose ambiguous task_id: %s", name, mcpresult.ToolSchemaJSON(registered))
		}
		if properties["session_id"] != nil {
			t.Fatalf("%s must use remote_session_id: %s", name, mcpresult.ToolSchemaJSON(registered))
		}
		if _, clean := map[string]bool{"session": true, "read": true, "edit": true, "observe": true}[name]; clean && properties["remote_session_id"] == nil {
			t.Fatalf("%s must expose remote_session_id", name)
		}
		required, _ := schema["required"].([]any)
		for _, raw := range required {
			field, _ := raw.(string)
			if field == "" {
				continue
			}
			if properties[field] == nil {
				t.Fatalf("%s required field %q is missing from properties: %s", name, field, mcpresult.ToolSchemaJSON(registered))
			}
		}
	}
	for name, registered := range runtime.listedToolMap() {
		var schema map[string]any
		if err := json.Unmarshal(mcpresult.ToolSchemaJSON(registered), &schema); err != nil {
			t.Fatalf("decode %s activity schema: %v", name, err)
		}
		properties, _ := schema["properties"].(map[string]any)
		activity, ok := properties["activity"].(map[string]any)
		if !toolSupportsEmbeddedActivity(name) {
			if ok {
				t.Fatalf("%s must not advertise activity without a Remote Session contract: %s", name, mcpresult.ToolSchemaJSON(registered))
			}
			continue
		}
		if !ok {
			t.Fatalf("%s must expose optional activity object: %s", name, mcpresult.ToolSchemaJSON(registered))
		}
		if activity["type"] != "object" || activity["additionalProperties"] != false {
			t.Fatalf("%s activity must be a strict object: %+v", name, activity)
		}
		activityProperties, _ := activity["properties"].(map[string]any)
		if len(activityProperties) != 6 {
			t.Fatalf("%s activity fields=%v, want six semantic fields", name, activityProperties)
		}
		for _, field := range []string{"intent", "hypothesis", "evidence", "conclusion", "next", "status"} {
			entry, _ := activityProperties[field].(map[string]any)
			if entry["type"] != "string" {
				t.Fatalf("%s activity.%s must be string: %+v", name, field, entry)
			}
		}
		for _, forbidden := range []string{"kind", "turn_id", "sequence", "state", "summary", "related_call_id"} {
			if activityProperties[forbidden] != nil {
				t.Fatalf("%s activity must not expose runtime field %q: %+v", name, forbidden, activityProperties)
			}
		}
	}
	workspaceTool := runtime.listedToolMap()["workspace"]
	var workspaceSchema map[string]any
	if err := json.Unmarshal(mcpresult.ToolSchemaJSON(workspaceTool), &workspaceSchema); err != nil {
		t.Fatal(err)
	}
	workspaceProperties, _ := workspaceSchema["properties"].(map[string]any)
	if workspaceProperties["remote_session_id"] != nil || workspaceProperties["action"] != nil || len(workspaceProperties) != 0 {
		t.Fatalf("workspace must be a zero-argument catalog query: %s", mcpresult.ToolSchemaJSON(workspaceTool))
	}

	readTool := runtime.listedToolMap()["read"]
	var readSchema map[string]any
	if err := json.Unmarshal(mcpresult.ToolSchemaJSON(readTool), &readSchema); err != nil {
		t.Fatal(err)
	}
	readProperties, _ := readSchema["properties"].(map[string]any)
	if readProperties["line_byte_offset"] == nil {
		t.Fatalf("read schema missing line_byte_offset: %s", mcpresult.ToolSchemaJSON(readTool))
	}
	readItemsSchema, _ := readProperties["items"].(map[string]any)
	readItemSchema, _ := readItemsSchema["items"].(map[string]any)
	readItemProperties, _ := readItemSchema["properties"].(map[string]any)
	if readItemProperties["line_byte_offset"] == nil {
		t.Fatalf("read items schema missing line_byte_offset: %s", mcpresult.ToolSchemaJSON(readTool))
	}

	editTool := runtime.listedToolMap()["edit"]
	if editTool.Annotations == nil || editTool.Annotations.ReadOnlyHint || editTool.Annotations.DestructiveHint == nil || *editTool.Annotations.DestructiveHint || !editTool.Annotations.IdempotentHint || editTool.Annotations.OpenWorldHint == nil || *editTool.Annotations.OpenWorldHint {
		t.Fatalf("edit must expose constrained non-destructive workspace mutation: %+v", editTool.Annotations)
	}
	if editTool.Title != "Workspace 文件变更（不提供删除）" || editTool.Annotations.Title != editTool.Title {
		t.Fatalf("edit title=%q annotations=%+v", editTool.Title, editTool.Annotations)
	}
	safety := toolSafetyMetadata(map[string]any{"_meta": editTool.Meta})
	if safety == nil {
		t.Fatalf("edit must expose mcpx/safety metadata: %+v", editTool.Meta)
	}
	if safety["scope"] != "registered_workspace_root" || safety["approval"] == "host_user_approval_required" {
		t.Fatalf("edit safety metadata is incomplete: %+v", safety)
	}
	if !strings.Contains(editTool.Description, "不提供删除") || !strings.Contains(editTool.Description, "move_out") {
		t.Fatalf("edit description must explain the removal boundary semantically: %s", editTool.Description)
	}
	var editSchema map[string]any
	if err := json.Unmarshal(mcpresult.ToolSchemaJSON(editTool), &editSchema); err != nil {
		t.Fatal(err)
	}
	editProperties, _ := editSchema["properties"].(map[string]any)
	if editProperties["remote_session_id"] == nil || editProperties["purpose"] == nil || editProperties["edits"] == nil {
		t.Fatalf("edit schema missing clean-core fields: %s", mcpresult.ToolSchemaJSON(editTool))
	}
	editItems, _ := editProperties["edits"].(map[string]any)
	itemSchema, _ := editItems["items"].(map[string]any)
	itemProperties, _ := itemSchema["properties"].(map[string]any)
	for _, field := range []string{"operation", "path", "rev", "content", "new_path", "replacements", "range"} {
		if itemProperties[field] == nil {
			t.Fatalf("edit item missing %q: %s", field, mcpresult.ToolSchemaJSON(editTool))
		}
	}
	if itemProperties["base_sha256"] != nil {
		t.Fatalf("edit item must not expose legacy base_sha256: %s", mcpresult.ToolSchemaJSON(editTool))
	}
	moveOutTool := runtime.listedToolMap()["move_out"]
	if moveOutTool.Annotations == nil || moveOutTool.Annotations.ReadOnlyHint || moveOutTool.Annotations.DestructiveHint == nil || !*moveOutTool.Annotations.DestructiveHint || !moveOutTool.Annotations.IdempotentHint || moveOutTool.Annotations.OpenWorldHint == nil || *moveOutTool.Annotations.OpenWorldHint {
		t.Fatalf("move_out annotations=%+v", moveOutTool.Annotations)
	}
	if safety := toolSafetyMetadata(map[string]any{"_meta": moveOutTool.Meta}); safety["approval"] != "web_model_user_confirmation_required" || safety["filesystem_only"] != true || safety["registered_workspace"] != true || safety["confirmation_credential"] != "server_generated_confirmation_uuid" || safety["no_symlink_following"] != true || safety["symlink_entry_move"] != true || safety["reversible"] != true {
		t.Fatalf("move_out safety metadata=%+v", safety)
	}
	moveOutRisk, _ := moveOutTool.Meta["mcpx/action_risk"].(map[string]any)
	prepareRisk, _ := moveOutRisk["prepare"].(map[string]any)
	submitRisk, _ := moveOutRisk["submit"].(map[string]any)
	if prepareRisk["read_only"] != true || prepareRisk["destructive"] != false || prepareRisk["open_world"] != false {
		t.Fatalf("move_out prepare risk=%+v", prepareRisk)
	}
	if submitRisk["read_only"] != false || submitRisk["destructive"] != true || submitRisk["open_world"] != false {
		t.Fatalf("move_out submit risk=%+v", submitRisk)
	}
	for toolName, actions := range map[string][]string{
		"plan":     {"create", "read", "advance", "complete", "block", "replan", "deliver"},
		"artifact": {"register", "list", "read"},
	} {
		tool := runtime.listedToolMap()[toolName]
		if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Fatalf("%s top-level annotations must be non-destructive and closed-world: %+v", toolName, tool.Annotations)
		}
		risk, _ := tool.Meta["mcpx/action_risk"].(map[string]any)
		for _, action := range actions {
			entry, ok := risk[action].(map[string]any)
			if !ok || entry["destructive"] != false || entry["open_world"] != false {
				t.Fatalf("%s action risk %s=%+v", toolName, action, entry)
			}
			if (toolName == "artifact" && (action == "list" || action == "read")) || (toolName == "plan" && action == "read") {
				if entry["read_only"] != true {
					t.Fatalf("%s %s must be read-only: %+v", toolName, action, entry)
				}
			}
		}
	}
	for _, toolName := range []string{"skill_tool", "mcp_tool"} {
		tool := runtime.listedToolMap()[toolName]
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint || tool.Annotations.OpenWorldHint == nil || !*tool.Annotations.OpenWorldHint {
			t.Fatalf("%s top-level annotations must conservatively represent call risk: %+v", toolName, tool.Annotations)
		}
		risk, _ := tool.Meta["mcpx/action_risk"].(map[string]any)
		for _, action := range []string{"list", "describe"} {
			entry, ok := risk[action].(map[string]any)
			if !ok || entry["read_only"] != true || entry["destructive"] != false || entry["idempotent"] != true {
				t.Fatalf("%s %s must be read-only: %+v", toolName, action, entry)
			}
		}
		callRisk, ok := risk["call"].(map[string]any)
		if !ok || callRisk["read_only"] != false || callRisk["destructive"] != true || callRisk["idempotent"] != false || callRisk["open_world"] != true {
			t.Fatalf("%s call risk=%+v", toolName, callRisk)
		}
	}
	progressTool := runtime.listedToolMap()["progress"]
	var progressSchema map[string]any
	if err := json.Unmarshal(mcpresult.ToolSchemaJSON(progressTool), &progressSchema); err != nil {
		t.Fatal(err)
	}
	progressProperties, _ := progressSchema["properties"].(map[string]any)
	for _, field := range []string{"remote_session_id", "status", "current", "result", "next", "phase", "related_tool"} {
		if progressProperties[field] == nil {
			t.Fatalf("progress schema missing %q: %s", field, mcpresult.ToolSchemaJSON(progressTool))
		}
	}
	resultSchema, _ := progressProperties["result"].(map[string]any)
	resultItems, _ := resultSchema["items"].(map[string]any)
	if resultSchema["type"] != "array" || resultSchema["maxItems"] != float64(maxProgressResultItems) || resultItems["type"] != "string" {
		t.Fatalf("progress result must be a bounded string list: %+v", resultSchema)
	}
	statusSchema, _ := progressProperties["status"].(map[string]any)
	statusValues, _ := statusSchema["enum"].([]any)
	if !containsSchemaRequired(statusValues, "failed") || !containsSchemaRequired(statusValues, "completed") {
		t.Fatalf("progress status enum=%v", statusValues)
	}

	observeTool := runtime.listedToolMap()["observe"]
	var observeSchema map[string]any
	if err := json.Unmarshal(mcpresult.ToolSchemaJSON(observeTool), &observeSchema); err != nil {
		t.Fatal(err)
	}
	observeProperties, _ := observeSchema["properties"].(map[string]any)
	for _, field := range []string{"workspace", "view", "event_ids", "request_ids", "operation_ids", "plan_task_ids", "execution_task_ids", "plan_task_id", "execution_task_id", "edit_id", "offset", "keyword", "kinds", "statuses", "created_after", "created_before"} {
		if observeProperties[field] == nil {
			t.Fatalf("observe history schema missing %q: %s", field, mcpresult.ToolSchemaJSON(observeTool))
		}
	}
	for _, removed := range []string{"room_id", "task_id", "task_ids", "changeset_ids", "include_diff", "path"} {
		if observeProperties[removed] != nil {
			t.Fatalf("observe schema exposes removed field %q: %s", removed, mcpresult.ToolSchemaJSON(observeTool))
		}
	}
	viewSchema, _ := observeProperties["view"].(map[string]any)
	viewValues, _ := viewSchema["enum"].([]any)
	for _, wantView := range []string{"session", "task", "plan", "history", "logs", "diff"} {
		if !containsSchemaRequired(viewValues, wantView) {
			t.Fatalf("observe view enum missing %q: %v", wantView, viewValues)
		}
	}
	for _, removedView := range []string{"changes"} {
		if containsSchemaRequired(viewValues, removedView) {
			t.Fatalf("observe view enum exposes removed view %q: %v", removedView, viewValues)
		}
	}
	for _, toolName := range []string{"plan", "execute"} {
		var schema map[string]any
		if err := json.Unmarshal(mcpresult.ToolSchemaJSON(runtime.listedToolMap()[toolName]), &schema); err != nil {
			t.Fatal(err)
		}
		properties, _ := schema["properties"].(map[string]any)
		if toolName == "plan" && properties["plan_task_id"] == nil {
			t.Fatalf("plan must expose plan_task_id: %s", mcpresult.ToolSchemaJSON(runtime.listedToolMap()[toolName]))
		}
		if toolName == "execute" && properties["execution_task_id"] == nil {
			t.Fatalf("execute must expose execution_task_id: %s", mcpresult.ToolSchemaJSON(runtime.listedToolMap()[toolName]))
		}
	}
	operationManage := runtime.listedToolMap()["operation_manage"]
	var operationSchema map[string]any
	if err := json.Unmarshal(mcpresult.ToolSchemaJSON(operationManage), &operationSchema); err != nil {
		t.Fatal(err)
	}
	operationProperties, _ := operationSchema["properties"].(map[string]any)
	operationIDs, _ := operationProperties["operation_ids"].(map[string]any)
	if operationIDs["type"] != "array" {
		t.Fatalf("operation_manage operation_ids schema=%+v", operationIDs)
	}
	for _, raw := range operationSchema["required"].([]any) {
		if raw == "operation_id" {
			t.Fatalf("operation_manage must make operation_id conditional: %s", mcpresult.ToolSchemaJSON(operationManage))
		}
	}
	if _, hasOneOf := operationSchema["oneOf"]; hasOneOf {
		t.Fatalf("operation_manage must not rely on top-level oneOf: %s", mcpresult.ToolSchemaJSON(operationManage))
	}
	for _, key := range []string{"action", "operation_id", "operation_ids", "step_id", "timeout_ms", "confirmation_token", "cursor", "limit"} {
		if operationProperties[key] == nil {
			t.Fatalf("operation_manage missing property %q", key)
		}
	}
	actionProp, _ := operationProperties["action"].(map[string]any)
	actionDesc, _ := actionProp["description"].(string)
	for _, needle := range []string{"operation_id", "operation_ids", "status", "result"} {
		if !strings.Contains(actionDesc, needle) {
			t.Fatalf("operation_manage action.description missing %q: %s", needle, actionDesc)
		}
	}
	opRequired, _ := operationSchema["required"].([]any)
	if !reflect.DeepEqual(opRequired, []any{"action"}) {
		t.Fatalf("operation_manage required must be exactly [action], got %v", opRequired)
	}
}

func TestActionSchemasAreFlatAndCarryActionDescription(t *testing.T) {
	runtime := &Runtime{}
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	runtime.registerTools(protocol)

	for name, registered := range runtime.listedToolMap() {
		var schema map[string]any
		if err := json.Unmarshal(mcpresult.ToolSchemaJSON(registered), &schema); err != nil {
			t.Fatalf("%s schema: %v", name, err)
		}
		if _, hasOneOf := schema["oneOf"]; hasOneOf {
			t.Fatalf("%s must not rely on top-level oneOf", name)
		}
		if schema["additionalProperties"] != false {
			t.Fatalf("%s must have additionalProperties: false", name)
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s missing properties", name)
		}
		actionSchema, hasAction := properties["action"].(map[string]any)
		if !hasAction {
			continue
		}
		enums, _ := actionSchema["enum"].([]any)
		desc, _ := actionSchema["description"].(string)
		if strings.TrimSpace(desc) == "" {
			t.Fatalf("%s action has enum %v but missing action.description", name, enums)
		}
		for _, enumVal := range enums {
			actionName := fmt.Sprint(enumVal)
			if !strings.Contains(desc, actionName) {
				t.Fatalf("%s action.description missing action %q: %s", name, actionName, desc)
			}
		}
	}

	artifact := runtime.listedToolMap()["artifact"]
	var artifactSchema map[string]any
	_ = json.Unmarshal(mcpresult.ToolSchemaJSON(artifact), &artifactSchema)
	artifactProps, _ := artifactSchema["properties"].(map[string]any)
	kindProp, _ := artifactProps["kind"].(map[string]any)
	if kindProp == nil {
		t.Fatal("artifact missing kind property")
	}
	kindEnums, _ := kindProp["enum"].([]any)
	wantEnums := []any{"test_report", "coverage", "build", "screenshot", "log", "other"}
	if !reflect.DeepEqual(kindEnums, wantEnums) {
		t.Fatalf("artifact kind enum = %v, want %v", kindEnums, wantEnums)
	}
	kindDesc, _ := kindProp["description"].(string)
	if !strings.Contains(kindDesc, "register") || !strings.Contains(kindDesc, "list") {
		t.Fatalf("artifact kind description should cover register and list: %s", kindDesc)
	}
	limitProp, _ := artifactProps["limit"].(map[string]any)
	if limitProp == nil {
		t.Fatal("artifact missing limit property")
	}
	limitDesc, _ := limitProp["description"].(string)
	if !strings.Contains(limitDesc, "list") || !strings.Contains(limitDesc, "read") {
		t.Fatalf("artifact limit description should cover list and read: %s", limitDesc)
	}
}

func containsSchemaRequired(required []any, want string) bool {
	for _, item := range required {
		if item == want {
			return true
		}
	}
	return false
}

func assertRequiredKeywordsAreArrays(t *testing.T, toolName, path string, value any) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			childPath := path + "." + key
			if key == "required" {
				if _, ok := child.([]any); !ok {
					t.Fatalf("%s has invalid JSON Schema %s: required must be an array, got %T (%v)", toolName, childPath, child, child)
				}
			}
			assertRequiredKeywordsAreArrays(t, toolName, childPath, child)
		}
	case []any:
		for index, child := range typed {
			assertRequiredKeywordsAreArrays(t, toolName, fmt.Sprintf("%s[%d]", path, index), child)
		}
	}
}
