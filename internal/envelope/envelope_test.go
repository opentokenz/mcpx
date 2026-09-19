package envelope

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOK(t *testing.T) {
	r := OK("req1", "ws", map[string]any{"x": 1})
	if !r.OK || r.Status != StatusOK || r.RequestID != "req1" || r.Workspace != "ws" {
		t.Fatalf("unexpected OK response: %+v", r)
	}
	if r.Error != nil {
		t.Fatalf("error should be nil")
	}
}

func TestFail(t *testing.T) {
	r := Fail(StatusDenied, "req1", "ws", nil, "denied", "blocked")
	if r.OK || r.Status != StatusDenied {
		t.Fatalf("unexpected Fail: %+v", r)
	}
	if r.Error == nil || r.Error.Code != "DENIED" || r.Error.Category != "permission" || r.Error.Details == nil {
		t.Fatalf("expected full error contract, got %+v", r.Error)
	}
}

func TestTooManyChangesUsesValidationRecoveryContract(t *testing.T) {
	r := Fail(StatusError, "req1", "demo", nil, "TOO_MANY_CHANGES", "too many changed lines")
	if r.Error == nil {
		t.Fatal("missing error")
	}
	if r.Error.Category != "validation" || !r.Error.Retryable {
		t.Fatalf("unexpected TOO_MANY_CHANGES classification: %+v", r.Error)
	}
	hint, _ := r.Error.Details["retry_hint"].(string)
	if hint == "" || strings.Contains(strings.ToLower(hint), "server log") || !strings.Contains(strings.ToLower(hint), "split") {
		t.Fatalf("inconsistent retry hint: %q", hint)
	}
}

func TestBooleanConfirmationRetryHintDoesNotMentionToken(t *testing.T) {
	response := Fail(StatusNeedConfirmation, "req_confirm", "demo", map[string]any{
		"confirmation_required": true, "user_confirmed_required": true,
	}, "USER_CONFIRMATION_REQUIRED", "delete requires confirmation")
	if response.Error == nil {
		t.Fatal("missing confirmation error")
	}
	hint, _ := response.Error.Details["retry_hint"].(string)
	if strings.Contains(hint, "confirmation_token") || !strings.Contains(hint, "user_confirmed=true") {
		t.Fatalf("boolean confirmation hint=%q", hint)
	}
}

func TestConfirmationKeyRetryHintUsesReturnedKey(t *testing.T) {
	response := Fail(StatusNeedConfirmation, "req_confirm", "demo", map[string]any{
		"confirmation_required": true, "confirmation_key": "ct_example",
	}, "USER_CONFIRMATION_REQUIRED", "MCP call requires confirmation")
	if response.Error == nil {
		t.Fatal("missing confirmation error")
	}
	hint, _ := response.Error.Details["retry_hint"].(string)
	if !strings.Contains(hint, "confirmation_key") || strings.Contains(hint, "confirmation_token") || strings.Contains(hint, "user_confirmed=true") {
		t.Fatalf("confirmation key hint=%q", hint)
	}
}

func TestCommandFailuresUseExecutionTaxonomy(t *testing.T) {
	for _, code := range []string{"COMMAND_NOT_FOUND", "PROCESS_EXIT", "COMMAND_FAILED", "OPERATION_FAILED"} {
		t.Run(code, func(t *testing.T) {
			response := Fail(StatusError, "req1", "demo", map[string]any{"exit_code": 127}, code, "command failed")
			if response.Error == nil || response.Error.Category != "execution" || response.Error.Retryable {
				t.Fatalf("unexpected %s classification: %+v", code, response.Error)
			}
			if response.Error.Details["exit_code"] != 127 {
				t.Fatalf("exit code missing from details: %+v", response.Error.Details)
			}
		})
	}
}

func TestBrowserErrorsUseActionableTaxonomy(t *testing.T) {
	for _, test := range []struct {
		code         string
		category     string
		retryable    bool
		hintContains string
	}{
		{code: "BROWSER_NODE_STALE", category: "conflict", retryable: true, hintContains: "browser snapshot"},
		{code: "BROWSER_ATTACHMENT_STALE", category: "conflict", retryable: false, hintContains: "reconnect"},
		{code: "BROWSER_TAB_NOT_FOUND", category: "not_found", retryable: false, hintContains: "browser tab list"},
	} {
		t.Run(test.code, func(t *testing.T) {
			response := Fail(StatusError, "req1", "demo", nil, test.code, "browser action failed")
			if response.Error == nil || response.Error.Category != test.category || response.Error.Retryable != test.retryable {
				t.Fatalf("unexpected %s classification: %+v", test.code, response.Error)
			}
			hint, _ := response.Error.Details["retry_hint"].(string)
			if !strings.Contains(strings.ToLower(hint), test.hintContains) {
				t.Fatalf("unexpected %s retry hint: %q", test.code, hint)
			}
		})
	}
}

func TestOperationFailureExtractsExitCodeFromStepResults(t *testing.T) {
	response := Fail(StatusError, "req1", "demo", map[string]any{
		"steps": []map[string]any{{
			"id": "run", "state": "failed",
			"result": map[string]any{"data": map[string]any{"exit_code": 127}},
		}},
	}, "OPERATION_FAILED", "operation failed")
	if response.Error == nil || response.Error.Category != "execution" || response.Error.Retryable {
		t.Fatalf("unexpected operation classification: %+v", response.Error)
	}
	if response.Error.Details["exit_code"] != 127 {
		t.Fatalf("exit code missing from nested step result: %+v", response.Error.Details)
	}
}

func TestMarshalUsesSingleStatusAndCompactMeta(t *testing.T) {
	response := OK("req_1", "demo", map[string]any{"session_id": "rs_1", "value": "ready"})
	response.RemoteSessionID = "rs_1"
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["status"] != "succeeded" || payload["ok"] != nil || payload["remote_session_id"] != nil {
		t.Fatalf("public response leaked legacy status fields: %s", encoded)
	}
	meta, _ := payload["meta"].(map[string]any)
	if meta["request_id"] != "req_1" || meta["operation_id"] != "op_1" || meta["session_id"] != "rs_1" {
		t.Fatalf("response meta = %+v", meta)
	}
}

func TestEnsureRequestID(t *testing.T) {
	if EnsureRequestID("keep") != "keep" {
		t.Fatal("should keep existing id")
	}
	id := EnsureRequestID("")
	if id == "" || len(id) < 8 {
		t.Fatalf("generated id too short: %q", id)
	}
	id2 := EnsureRequestID("")
	if id == id2 {
		t.Fatal("expected unique ids")
	}
}

func TestParseRequest(t *testing.T) {
	raw := json.RawMessage(`{"request_id":"r1","trace_id":"tr1","started_at_ms":123,"workspace":"p","payload":{"command":"ls","span_id":"sp1"}}`)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if req.RequestID == "r1" || req.RequestID == "" || req.Workspace != "p" {
		t.Fatalf("runtime context fields must not come from arguments: %+v", req)
	}
	if req.Payload["command"] != "ls" || req.Payload["request_id"] != nil || req.Payload["trace_id"] != nil || req.Payload["span_id"] != nil {
		t.Fatalf("runtime fields leaked into payload: %+v", req.Payload)
	}

	req2, err := ParseRequest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if req2.RequestID == "" || req2.Payload == nil {
		t.Fatalf("empty parse: %+v", req2)
	}
}

func TestParseRequestMergesFlatArgumentsIntoPayload(t *testing.T) {
	req, err := ParseRequest(json.RawMessage(`{"intent":"inspect workspace","progress_summary":"已完成定位，下一步读取文件","callId":"call-1","workspace":"demo","command":"pwd","payload":{"command":"echo legacy"},"execution_task_id":"t1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Payload["command"] != "echo legacy" {
		t.Fatalf("payload should win: %+v", req.Payload)
	}
	if req.ExecutionTaskID != "t1" || req.Payload["execution_task_id"] != "t1" {
		t.Fatalf("execution task argument missing: request=%+v payload=%+v", req, req.Payload)
	}
	if req.Intent != "inspect workspace" {
		t.Fatalf("intent missing: %q", req.Intent)
	}
	if req.ProgressSummary != "已完成定位，下一步读取文件" {
		t.Fatalf("progress summary missing: %q", req.ProgressSummary)
	}
	if req.CallID != "call-1" {
		t.Fatalf("correlation missing: %+v", req)
	}
	if _, exists := req.Payload["intent"]; exists {
		t.Fatalf("intent leaked into payload: %+v", req.Payload)
	}
	if _, exists := req.Payload["progress_summary"]; exists {
		t.Fatalf("progress summary leaked into payload: %+v", req.Payload)
	}
}
