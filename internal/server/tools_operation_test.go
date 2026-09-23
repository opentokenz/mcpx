package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/envelope"
	"mcpx/internal/mcpresult"
	"mcpx/internal/operation"
	"mcpx/internal/remotesession"
)

func operationTestSession(t *testing.T, rt *Runtime, workspace string) remotesession.Session {
	t.Helper()
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registered, ok := rt.reg.Get(workspace)
	if !ok {
		t.Fatalf("workspace %q was not registered", workspace)
	}
	created, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{
		WorkspaceName: workspace, WorkspacePath: registered.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created.Session
}

func callOperationTool(t *testing.T, rt *Runtime, name string, arguments map[string]any) map[string]any {
	t.Helper()
	result, err := rt.toolHandlers[name](context.Background(), mcpresult.Request(arguments))
	if err != nil {
		t.Fatal(err)
	}
	// Assert the same structured contract consumed by clients. Presentation
	// metadata intentionally omits some diagnostics and is not the machine API.
	payload, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatal("operation result has no model-visible structured content")
	}
	return payload
}

func submitOperationForTest(t *testing.T, rt *Runtime, session remotesession.Session, stepID, result string) operation.Record {
	t.Helper()
	record, err := rt.operations.Submit(context.Background(), operation.SubmitSpec{
		RemoteSessionID: session.ID,
		WorkspaceName:   session.WorkspaceName,
		RequestID:       "req_batch_test_" + stepID,
		Purpose:         "批量查询测试",
		Steps:           []operation.StepSpec{{ID: stepID, Tool: "read"}},
	}, func(context.Context, operation.ExecuteInput) operation.ExecuteResult {
		return operation.ExecuteResult{Result: []byte(result)}
	})
	if err != nil {
		t.Fatal(err)
	}
	completed, timedOut, err := rt.operations.Wait(context.Background(), record.ID, 5*time.Second)
	if err != nil || timedOut || completed.State != operation.StateSucceeded {
		t.Fatalf("operation did not complete: record=%+v timedOut=%v err=%v", completed, timedOut, err)
	}
	return completed
}

func operationErrorMessage(response map[string]any) string {
	errorBody, _ := response["error"].(map[string]any)
	if errorBody == nil {
		data, _ := response["data"].(map[string]any)
		errorBody, _ = data["error"].(map[string]any)
	}
	if errorBody != nil {
		if message := fmt.Sprint(errorBody["message"]); message != "<nil>" && message != "" {
			return message
		}
	}
	return fmt.Sprint(response["summary"])
}

func acceptedOperationID(response map[string]any) string {
	data, _ := response["data"].(map[string]any)
	id, _ := data["operation_id"].(string)
	return id
}

func assertOperationFailed(t *testing.T, response map[string]any) {
	t.Helper()
	if response["status"] != "failed" {
		t.Fatalf("expected failed response, got %+v", response)
	}
}

func TestAsyncToolReturnsOperationAndWaitsForResult(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	session := operationTestSession(t, rt, "demo")

	accepted := callOperationTool(t, rt, "read", map[string]any{
		"remote_session_id": session.ID, "purpose": "异步读取工作区", "execution_mode": "async", "view": "list", "limit": 5,
	})
	if accepted["status"] != "accepted" {
		t.Fatalf("accepted response=%+v", accepted)
	}
	operationID := acceptedOperationID(accepted)
	if operationID == "" {
		t.Fatalf("missing operation id: %+v", accepted)
	}

	completed := callOperationTool(t, rt, "operation_manage", map[string]any{
		"remote_session_id": session.ID, "operation_id": operationID, "action": "wait", "timeout_ms": 5000,
	})
	if completed["status"] != "succeeded" {
		t.Fatalf("completed response=%+v", completed)
	}
}

func TestAsyncCommandOperationWaitsForTerminalTask(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	session := operationTestSession(t, rt, "demo")
	accepted := callOperationTool(t, rt, "execute", map[string]any{
		"remote_session_id": session.ID, "purpose": "验证异步命令完成语义", "execution_mode": "async", "action": "run",
		"command": testSleepCommand(200 * time.Millisecond), "yield_time_ms": 1,
	})
	if accepted["status"] != "accepted" {
		t.Fatalf("accepted response=%+v", accepted)
	}
	operationID := acceptedOperationID(accepted)
	if operationID == "" {
		t.Fatalf("missing operation id: %+v", accepted)
	}

	completed := callOperationTool(t, rt, "operation_manage", map[string]any{
		"remote_session_id": session.ID, "operation_id": operationID, "action": "wait", "timeout_ms": 5000,
	})
	if completed["status"] != "succeeded" {
		t.Fatalf("completed response=%+v", completed)
	}
	completedData, _ := completed["data"].(map[string]any)
	if completedData["result"] != nil {
		t.Fatalf("wait must not duplicate step results in a top-level aggregate: %+v", completedData["result"])
	}
	completedSteps, _ := completedData["steps"].([]any)
	if len(completedSteps) != 1 {
		t.Fatalf("wait steps=%+v", completedData["steps"])
	}
	completedStep, _ := completedSteps[0].(map[string]any)
	completedResult, _ := completedStep["result"].(map[string]any)
	if completedResult["status"] != "succeeded" {
		t.Fatalf("wait step must expose the machine-readable operation result: %+v", completed)
	}
	record, err := rt.operations.Get(context.Background(), operationID)
	if err != nil {
		t.Fatal(err)
	}
	var resultValue any
	if err := json.Unmarshal(record.Result, &resultValue); err != nil {
		t.Fatal(err)
	}
	taskID := findStringValue(resultValue, "execution_task_id")
	if taskID == "" {
		if wrapper, ok := resultValue.(map[string]any); ok {
			if content, ok := wrapper["content"].([]any); ok {
				for _, item := range content {
					contentItem, ok := item.(map[string]any)
					if !ok {
						continue
					}
					text, _ := contentItem["text"].(string)
					var nested any
					if json.Unmarshal([]byte(text), &nested) == nil {
						taskID = findStringValue(nested, "execution_task_id")
						if taskID != "" {
							break
						}
					}
				}
			}
		}
	}
	if taskID == "" {
		t.Fatalf("operation result did not retain terminal task id: %s", record.Result)
	}
	task, err := rt.tasks.Get(session.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	taskStatus := task.StatusView()
	if fmt.Sprint(taskStatus["status"]) != "exited" || taskStatus["exit_code"] != 0 {
		t.Fatalf("task was not terminal-success: %+v", taskStatus)
	}
	finishedAt, _ := taskStatus["finished_at"].(time.Time)
	if record.CompletedAt == nil || finishedAt.IsZero() || record.CompletedAt.Before(finishedAt) {
		t.Fatalf("operation completed before task: operation=%v task=%v", record.CompletedAt, finishedAt)
	}

	events, err := rt.observation.store.History(context.Background(), "demo", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.ParentOperationID == operationID && (event.Type == "tool.started" || event.Type == "tool.completed") {
			t.Fatalf("internal operation step leaked duplicate tool event: %+v", event)
		}
	}
}

func TestOperationBatchRunsAndRecordsChildSteps(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	session := operationTestSession(t, rt, "demo")
	accepted := callOperationTool(t, rt, "operation_batch", map[string]any{
		"remote_session_id": session.ID,
		"purpose":           "并行读取工作区目录",
		"operations": []any{
			map[string]any{"id": "list_a", "tool": "read", "arguments": map[string]any{"view": "list", "limit": 5}},
			map[string]any{"id": "list_b", "tool": "read", "arguments": map[string]any{"view": "list", "limit": 5}},
		},
	})
	if accepted["status"] != "accepted" {
		t.Fatalf("batch response=%+v", accepted)
	}
	operationID := acceptedOperationID(accepted)
	if operationID == "" {
		t.Fatalf("missing operation id: %+v", accepted)
	}
	completed := callOperationTool(t, rt, "operation_manage", map[string]any{
		"remote_session_id": session.ID, "operation_id": operationID, "action": "wait", "timeout_ms": 5000,
	})
	if completed["status"] != "succeeded" {
		t.Fatalf("batch completion=%+v", completed)
	}

	events, err := rt.observation.store.History(context.Background(), "demo", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	seenSteps := map[string]bool{}
	for _, event := range events {
		if event.OperationID == operationID && event.StepID != "" {
			seenSteps[event.StepID] = true
		}
	}
	if !seenSteps["list_a"] || !seenSteps["list_b"] {
		t.Fatalf("missing operation step events: %+v", seenSteps)
	}
}

func TestOperationBatchPublishesBoundedStatisticsForMaxSteps(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	session := operationTestSession(t, rt, "demo")
	steps := make([]any, 0, operation.MaxSteps)
	for index := 0; index < operation.MaxSteps; index++ {
		steps = append(steps, map[string]any{
			"id": fmt.Sprintf("read_%02d", index), "tool": "read",
			"arguments": map[string]any{"view": "list", "limit": 1},
		})
	}
	accepted := callOperationTool(t, rt, "operation_batch", map[string]any{
		"remote_session_id": session.ID, "purpose": "验证最大批次统计", "operations": steps,
	})
	if accepted["status"] != "accepted" {
		t.Fatalf("batch accepted=%+v", accepted)
	}
	operationID := acceptedOperationID(accepted)
	if operationID == "" {
		t.Fatalf("missing operation id: %+v", accepted)
	}
	completed := callOperationTool(t, rt, "operation_manage", map[string]any{
		"remote_session_id": session.ID, "operation_id": operationID, "action": "wait", "timeout_ms": 30000,
	})
	if completed["status"] != "succeeded" {
		t.Fatalf("batch completion=%+v", completed)
	}
	data := completed["data"].(map[string]any)
	if data["result"] != nil {
		t.Fatalf("wait must not duplicate the aggregate result alongside steps: %+v", data["result"])
	}
	stats, ok := data["stats"].(map[string]any)
	if !ok {
		t.Fatalf("batch stats missing: %+v", data)
	}
	if stats["step_count"] != float64(operation.MaxSteps) || stats["success_count"] != float64(operation.MaxSteps) || stats["failure_count"] != float64(0) {
		t.Fatalf("batch step counts=%+v", stats)
	}
	maxConcurrency, ok := stats["max_concurrency"].(float64)
	if !ok || maxConcurrency < 1 || maxConcurrency > float64(operation.MaxSteps) {
		t.Fatalf("batch max concurrency=%+v", stats)
	}
	for _, field := range []string{"scheduling_wait_ms", "server_duration_ms"} {
		value, ok := stats[field].(float64)
		if !ok || value < 0 {
			t.Fatalf("batch timing %s=%v stats=%+v", field, stats[field], stats)
		}
	}
	if stats["events"] != nil {
		t.Fatalf("batch stats must not copy event streams: %+v", stats)
	}
	encoded, _ := json.Marshal(completed)
	if len(encoded) > 128<<10 {
		t.Fatalf("batch response is unbounded: %d bytes", len(encoded))
	}
}

func TestOperationManageBatchStatusAndResult(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	session := operationTestSession(t, rt, "demo")
	first := submitOperationForTest(t, rt, session, "first", `{"value":"first"}`)
	second := submitOperationForTest(t, rt, session, "second", `{"value":"second"}`)
	ids := []any{first.ID, second.ID}

	status := callOperationTool(t, rt, "operation_manage", map[string]any{
		"remote_session_id": session.ID, "action": "status", "operation_ids": ids,
	})
	if status["status"] != "succeeded" {
		t.Fatalf("batch status=%+v", status)
	}
	statusData, ok := status["data"].(map[string]any)
	if !ok || statusData["action"] != "status" {
		t.Fatalf("batch status data=%T %+v", status["data"], status)
	}
	statusItems, ok := statusData["items"].([]any)
	if !ok || len(statusItems) != 2 {
		t.Fatalf("batch status items=%T %+v", statusData["items"], statusData["items"])
	}
	for index, item := range statusItems {
		view, ok := item.(map[string]any)
		if !ok || view["operation_id"] != ids[index] || view["state"] != "succeeded" {
			t.Fatalf("batch status item[%d]=%T %+v", index, item, item)
		}
	}

	result := callOperationTool(t, rt, "operation_manage", map[string]any{
		"remote_session_id": session.ID, "action": "result", "operation_ids": ids,
	})
	if result["status"] != "succeeded" {
		t.Fatalf("batch result=%+v", result)
	}
	resultData := result["data"].(map[string]any)
	resultItems := resultData["items"].([]any)
	for index, item := range resultItems {
		view := item.(map[string]any)
		value := view["result"].(map[string]any)["value"]
		want := []string{"first", "second"}[index]
		if view["operation_id"] != ids[index] || value != want || view["next_cursor"] != "" {
			t.Fatalf("batch result item[%d]=%+v", index, view)
		}
	}

	longFirst := submitOperationForTest(t, rt, session, "long-first", `{"value":"012345678901234567890123456789"}`)
	longSecond := submitOperationForTest(t, rt, session, "long-second", `{"value":"abcdefghijabcdefghijabcdefghij"}`)
	paged := callOperationTool(t, rt, "operation_manage", map[string]any{
		"remote_session_id": session.ID, "action": "result", "operation_ids": []any{longFirst.ID, longSecond.ID}, "limit": 10,
	})
	pagedItems := paged["data"].(map[string]any)["items"].([]any)
	if len(pagedItems) != 2 {
		t.Fatalf("paged items=%+v", pagedItems)
	}
	firstItem := pagedItems[0].(map[string]any)
	cursor, _ := firstItem["next_cursor"].(string)
	if cursor == "" {
		t.Fatalf("first batch result must expose next_cursor: %+v", firstItem)
	}
	nextAction, _ := firstItem["next_action"].(map[string]any)
	if nextAction["tool"] != "operation_manage" {
		t.Fatalf("truncated batch result must expose operation_manage next_action: %+v", firstItem)
	}
	nextArgs, _ := nextAction["arguments"].(map[string]any)
	if nextArgs["cursor"] != cursor || nextArgs["operation_id"] != longFirst.ID || nextArgs["action"] != "result" || nextArgs["remote_session_id"] != session.ID {
		t.Fatalf("next_action arguments=%+v", nextArgs)
	}
	if _, exists := nextArgs["session_id"]; exists {
		t.Fatalf("next_action must use remote_session_id: %+v", nextArgs)
	}
	continued := callOperationTool(t, rt, "operation_manage", map[string]any{
		"remote_session_id": session.ID, "operation_id": longFirst.ID, "action": "result", "cursor": cursor, "limit": 10,
	})
	if continued["status"] != "succeeded" {
		t.Fatalf("single result continuation=%+v", continued)
	}
}

func TestOperationBatchResultUnwrapsToolContent(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	session := operationTestSession(t, rt, "demo")
	// Real async steps persist the mcp.CallToolResult envelope; the batch
	// result must hand models the inner tool JSON instead of escaped nesting.
	record := submitOperationForTest(t, rt, session, "unwrapped", `{"result":{"available":true,"content":[{"text":"{\"status\":\"succeeded\",\"data\":{\"hello\":\"world\"}}"}]}}`)
	response := callOperationTool(t, rt, "operation_manage", map[string]any{
		"remote_session_id": session.ID, "action": "result", "operation_ids": []any{record.ID},
	})
	if response["status"] != "succeeded" {
		t.Fatalf("batch result=%+v", response)
	}
	items := response["data"].(map[string]any)["items"].([]any)
	view := items[0].(map[string]any)
	data, _ := view["result"].(map[string]any)["data"].(map[string]any)
	if data["hello"] != "world" {
		t.Fatalf("batch result must unwrap tool content: %+v", view)
	}
	if view["next_cursor"] != "" {
		t.Fatalf("unexpected cursor for short result: %+v", view)
	}
}

func TestOperationResultValuePrefersARCMetaOverHumanText(t *testing.T) {
	// Matches persisted step shape after WrapToolResult: human text + machine ARC in _meta.
	raw := []byte(`{
		"_meta": {
			"mcpx.result": {
				"mcpx": {
					"result": {
						"type": "search_result",
						"status": "succeeded",
						"summary": "Source search returned 1 match(es).",
						"data": {
							"matches": [{"path": "src/A.java", "line": 10, "text": "class A"}]
						}
					}
				}
			}
		},
		"content": [{"type": "text", "text": "Source search returned 1 match(es).\n- ` + "`src/A.java`" + `"}]
	}`)
	value := operationResultValue(raw)
	asMap, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("want map result, got %T %#v", value, value)
	}
	if asMap["type"] != "search_result" {
		t.Fatalf("type=%v", asMap["type"])
	}
	data, _ := asMap["data"].(map[string]any)
	matches, _ := data["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("matches must come from ARC meta, not prose text: %+v", value)
	}
	match, _ := matches[0].(map[string]any)
	if match["path"] != "src/A.java" {
		t.Fatalf("match path=%v", match["path"])
	}
}

func TestOperationManageBatchValidationAndPermissions(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	session := operationTestSession(t, rt, "demo")
	valid := submitOperationForTest(t, rt, session, "valid", `{"ok":true}`)

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"missing targets", map[string]any{"remote_session_id": session.ID, "action": "status"}, "operation_id or operation_ids"},
		{"both target forms", map[string]any{"remote_session_id": session.ID, "action": "status", "operation_id": valid.ID, "operation_ids": []any{valid.ID}}, "mutually exclusive"},
		{"empty operation_ids", map[string]any{"remote_session_id": session.ID, "action": "status", "operation_ids": []any{}}, "operation_ids"},
		{"duplicate operation_ids", map[string]any{"remote_session_id": session.ID, "action": "status", "operation_ids": []any{valid.ID, valid.ID}}, "duplicate"},
		{"batch wait", map[string]any{"remote_session_id": session.ID, "action": "wait", "operation_ids": []any{valid.ID}}, "status or action=result"},
		{"batch step", map[string]any{"remote_session_id": session.ID, "action": "result", "operation_ids": []any{valid.ID}, "step_id": "valid"}, "step_id"},
		{"batch cursor", map[string]any{"remote_session_id": session.ID, "action": "result", "operation_ids": []any{valid.ID}, "cursor": "10"}, "cursor"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response := callOperationTool(t, rt, "operation_manage", test.args)
			assertOperationFailed(t, response)
			if !strings.Contains(operationErrorMessage(response), test.want) {
				t.Fatalf("error=%q, want substring %q", operationErrorMessage(response), test.want)
			}
		})
	}

	tooMany := make([]any, operation.MaxBatchQueries+1)
	for index := range tooMany {
		tooMany[index] = fmt.Sprintf("op-%d", index)
	}
	tooManyResponse := callOperationTool(t, rt, "operation_manage", map[string]any{
		"remote_session_id": session.ID, "action": "status", "operation_ids": tooMany,
	})
	assertOperationFailed(t, tooManyResponse)
	if !strings.Contains(operationErrorMessage(tooManyResponse), "operation_ids") {
		t.Fatalf("too many IDs error=%q", operationErrorMessage(tooManyResponse))
	}

	missing := callOperationTool(t, rt, "operation_manage", map[string]any{
		"remote_session_id": session.ID, "action": "result", "operation_ids": []any{valid.ID, "missing-operation"},
	})
	assertOperationFailed(t, missing)
	if data, _ := missing["data"].(map[string]any); data["items"] != nil {
		t.Fatalf("missing ID must not return partial data: %+v", missing)
	}

	otherSession := operationTestSession(t, rt, "demo")
	foreign := submitOperationForTest(t, rt, otherSession, "foreign", `{"foreign":true}`)
	forbidden := callOperationTool(t, rt, "operation_manage", map[string]any{
		"remote_session_id": session.ID, "action": "status", "operation_ids": []any{valid.ID, foreign.ID},
	})
	assertOperationFailed(t, forbidden)
	if !strings.Contains(strings.ToLower(operationErrorMessage(forbidden)), "another remote session") {
		t.Fatalf("foreign operation error=%q", operationErrorMessage(forbidden))
	}

	nested := callOperationTool(t, rt, "operation_batch", map[string]any{
		"remote_session_id": session.ID, "purpose": "批量读取结果", "operations": []any{
			map[string]any{"id": "read", "tool": "operation_manage", "arguments": map[string]any{}},
		},
	})
	assertOperationFailed(t, nested)
	if !strings.Contains(operationErrorMessage(nested), "operation_ids") || !strings.Contains(operationErrorMessage(nested), "action=status/result") {
		t.Fatalf("nested operation guidance=%q", operationErrorMessage(nested))
	}

	legacy := callOperationTool(t, rt, "operation_batch", map[string]any{
		"remote_session_id": session.ID, "purpose": "确认历史工具不可调度", "operations": []any{
			map[string]any{"id": "legacy", "tool": "command_run", "arguments": map[string]any{"command": "pwd"}},
		},
	})
	assertOperationFailed(t, legacy)
	if !strings.Contains(operationErrorMessage(legacy), `tool "command_run" is not available in the clean-core operation catalog`) {
		t.Fatalf("legacy tool must be rejected: %q", operationErrorMessage(legacy))
	}
}

func TestOperationBatchStatusAggregation(t *testing.T) {
	tests := []struct {
		name   string
		states []operation.State
		want   envelope.Status
	}{
		{"confirmation wins", []operation.State{operation.StateSucceeded, operation.StateWaitingConfirmation}, envelope.StatusNeedConfirmation},
		{"running after confirmation", []operation.State{operation.StateRunning, operation.StateSucceeded}, envelope.StatusAccepted},
		{"failure after terminal", []operation.State{operation.StateFailed, operation.StateCancelled}, envelope.StatusError},
		{"interrupted", []operation.State{operation.StateInterrupted, operation.StateCancelled}, envelope.StatusInterrupted},
		{"all success", []operation.State{operation.StateSucceeded, operation.StateSucceeded}, envelope.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			records := make([]operation.Record, 0, len(test.states))
			for _, state := range test.states {
				records = append(records, operation.Record{State: state})
			}
			if got := operationBatchStatus(records); got != test.want {
				t.Fatalf("status=%q, want %q", got, test.want)
			}
		})
	}
}

func TestValidateOperationSchemaValueHandlesUntypedSchemas(t *testing.T) {
	if err := validateOperationSchemaValue("value", map[string]any{"type": nil}, "arguments.value"); err != nil {
		t.Fatalf("type-less schema should not panic or reject an unconstrained value: %v", err)
	}
	if err := validateOperationSchemaValue(map[string]any{"name": "demo"}, map[string]any{
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
		},
		"required": []any{"name"},
	}, "arguments"); err != nil {
		t.Fatalf("object constraints should be inferred when type is omitted: %v", err)
	}
}

func TestValidateOperationSchemaValueFlatToolPreflight(t *testing.T) {
	runtime := &Runtime{}
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	runtime.registerTools(protocol)

	executeTool := runtime.listedToolMap()["execute"]
	var executeSchema map[string]any
	if err := json.Unmarshal(mcpresult.ToolSchemaJSON(executeTool), &executeSchema); err != nil {
		t.Fatal(err)
	}

	// 1. Missing conditional required field (e.g. command/argv for run) passes schema preflight
	// because schema required is only ["action"]. Handler will enforce at execution.
	if err := validateOperationSchemaValue(map[string]any{"action": "run"}, executeSchema, "arguments"); err != nil {
		t.Fatalf("missing conditional required fields should pass schema preflight: %v", err)
	}

	// 2. Unknown field is rejected due to additionalProperties: false
	if err := validateOperationSchemaValue(map[string]any{"action": "run", "unknown_bogus_field": "val"}, executeSchema, "arguments"); err == nil {
		t.Fatal("unknown field must be rejected by additionalProperties: false")
	}

	// 3. Field from another action in root properties is permitted at schema preflight
	if err := validateOperationSchemaValue(map[string]any{"action": "run", "execution_task_id": "task_1"}, executeSchema, "arguments"); err != nil {
		t.Fatalf("root properties from other actions are accepted at schema preflight: %v", err)
	}

	// 4. Missing root required field "action" is rejected
	if err := validateOperationSchemaValue(map[string]any{"command": "ls"}, executeSchema, "arguments"); err == nil {
		t.Fatal("missing root required 'action' must be rejected")
	}
}
