package envelope

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	MaxIntentBytes        = 512
	MaxResultSummaryBytes = 8192
)

// Status is the unified response status.
type Status string

const (
	StatusOK               Status = "succeeded"
	StatusAccepted         Status = "accepted"
	StatusNeedConfirmation Status = "waiting_confirmation"
	StatusInterrupted      Status = "interrupted"
	StatusNeedSecret       Status = "failed"
	StatusDenied           Status = "failed"
	StatusUnauthorized     Status = "failed"
	StatusError            Status = "failed"
)

// ActivityInput is the model-authored semantic trajectory carried by a normal
// MCP tool call. Runtime owns lifecycle fields such as turn, sequence, state,
// and related call identity; clients only provide public semantic summaries.
type ActivityInput struct {
	Intent     string `json:"intent,omitempty"`
	Hypothesis string `json:"hypothesis,omitempty"`
	Evidence   string `json:"evidence,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	Next       string `json:"next,omitempty"`
	Status     string `json:"status,omitempty"`
}

// Request is the common tool arguments envelope.
type Request struct {
	// RequestID and StartedAtMs are populated by the Gateway Runtime Context.
	// StartedAtMs is derived by the server instrumentation boundary from an
	// optional MCP client metadata timestamp or the server receive time.
	RequestID         string         `json:"-"`
	OperationID       string         `json:"-"`
	ParentOperationID string         `json:"-"`
	StepID            string         `json:"-"`
	Purpose           string         `json:"purpose,omitempty"`
	Intent            string         `json:"intent,omitempty"`
	Activity          ActivityInput  `json:"activity,omitempty"`
	ReasoningSummary  string         `json:"reasoning_summary,omitempty"`
	ProgressSummary   string         `json:"progress_summary,omitempty"`
	NextStep          string         `json:"next_step,omitempty"`
	PlanID            string         `json:"plan_id,omitempty"`
	PlanTaskID        string         `json:"plan_task_id,omitempty"`
	ExecutionTaskID   string         `json:"execution_task_id,omitempty"`
	CallID            string         `json:"call_id,omitempty"`
	RemoteSessionID   string         `json:"remote_session_id,omitempty"`
	Workspace         string         `json:"workspace,omitempty"`
	StartedAtMs       int64          `json:"-"`
	Payload           map[string]any `json:"payload"`
}

// ErrorBody is the machine-readable error contract returned by every failed
// tool call. Clients must branch on category/code rather than parsing Message.
type ErrorBody struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Category  string         `json:"category"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details"`
	Recovery  *Recovery      `json:"recovery,omitempty"`
}

// Recovery describes the next public operation that can resolve an error.
// Arguments are suggestions only; authorization and policy are rechecked.
type Recovery struct {
	Action    string         `json:"action"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// Response is the internal handler response assembled before the server's
// public ARC instrumentation boundary.
type Response struct {
	// OK is retained as an internal convenience for handlers and tests. It is
	// deliberately not serialized: Status is the only public outcome field.
	OK               bool       `json:"-"`
	Status           Status     `json:"-"`
	RequestID        string     `json:"-"`
	OperationID      string     `json:"-"`
	RemoteSessionID  string     `json:"-"`
	Workspace        string     `json:"-"`
	StartedAtMs      int64      `json:"-"`
	ReceivedAtMs     int64      `json:"-"`
	CompletedAtMs    int64      `json:"-"`
	NetworkLatencyMs int64      `json:"-"`
	ProcessingMs     int64      `json:"-"`
	ServerElapsedMs  int64      `json:"-"`
	Data             any        `json:"-"`
	Error            *ErrorBody `json:"-"`
}

type responseMeta struct {
	RequestID       string `json:"request_id,omitempty"`
	OperationID     string `json:"operation_id,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	Workspace       string `json:"workspace,omitempty"`
	StartedAtMs     int64  `json:"started_at_ms,omitempty"`
	CompletedAtMs   int64  `json:"completed_at_ms,omitempty"`
	ProcessingMs    int64  `json:"processing_ms,omitempty"`
	ServerElapsedMs int64  `json:"server_elapsed_ms,omitempty"`
}

type publicResponse struct {
	Status Status       `json:"status"`
	Data   any          `json:"data,omitempty"`
	Meta   responseMeta `json:"meta"`
	Error  *ErrorBody   `json:"error,omitempty"`
}

// MarshalJSON exposes the compact public response contract. Transport and
// timing fields live under meta; OK and the legacy top-level fields never
// cross the public boundary.
func (r Response) MarshalJSON() ([]byte, error) {
	operationID := r.OperationID
	if operationID == "" && r.RequestID != "" {
		operationID = "op_" + strings.TrimPrefix(r.RequestID, "req_")
	}
	return json.Marshal(publicResponse{
		Status: r.Status,
		Data:   publicData(r.Data),
		Meta: responseMeta{
			RequestID: r.RequestID, OperationID: operationID, SessionID: r.RemoteSessionID,
			Workspace: r.Workspace, StartedAtMs: r.StartedAtMs, CompletedAtMs: r.CompletedAtMs,
			ProcessingMs: r.ProcessingMs, ServerElapsedMs: r.ServerElapsedMs,
		},
		Error: r.Error,
	})
}

// UnmarshalJSON accepts the compact public response so Go clients and tests
// can inspect the same status/meta contract that MarshalJSON emits.
func (r *Response) UnmarshalJSON(data []byte) error {
	var wire publicResponse
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	r.OK = wire.Status == StatusOK
	r.Status = wire.Status
	r.Data = wire.Data
	r.Error = wire.Error
	r.RequestID = wire.Meta.RequestID
	r.OperationID = wire.Meta.OperationID
	r.RemoteSessionID = wire.Meta.SessionID
	r.Workspace = wire.Meta.Workspace
	r.StartedAtMs = wire.Meta.StartedAtMs
	r.CompletedAtMs = wire.Meta.CompletedAtMs
	r.ProcessingMs = wire.Meta.ProcessingMs
	r.ServerElapsedMs = wire.Meta.ServerElapsedMs
	return nil
}

func publicData(value any) any {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var decoded any
	if json.Unmarshal(encoded, &decoded) != nil {
		return value
	}
	return normalizePublicData(decoded)
}

func normalizePublicData(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[key] = normalizePublicData(item)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = normalizePublicData(item)
		}
		return result
	default:
		return value
	}
}

// Accepted builds a response for work handed to a durable Task.
func Accepted(requestID, workspace string, data any) Response {
	return Response{Status: StatusAccepted, RequestID: requestID, Workspace: workspace, Data: data}
}

// Interrupted builds a response when execution ended before completion was
// observed. The caller must provide a queryable Task or operation reference.
func Interrupted(requestID, workspace string, data any) Response {
	return Response{Status: StatusInterrupted, RequestID: requestID, Workspace: workspace, Data: data}
}

// OK builds a successful response.
func OK(requestID, workspace string, data any) Response {
	return Response{
		OK:        true,
		Status:    StatusOK,
		RequestID: requestID,
		Workspace: workspace,
		Data:      data,
		Error:     nil,
	}
}

// Fail builds an internal non-OK response with a normalized error body.
// The public MCP boundary converts this payload to an ARC error result.
func Fail(status Status, requestID, workspace string, data any, code, msg string) Response {
	if code == "" {
		if status == StatusNeedConfirmation {
			code = "USER_CONFIRMATION_REQUIRED"
		} else if status == StatusNeedSecret && strings.Contains(strings.ToUpper(msg), "SECRET") {
			code = "SECRET_REQUIRED"
		} else if strings.Contains(strings.ToUpper(msg), "UNAUTHORIZED") {
			code = "UNAUTHORIZED"
		} else {
			code = "INTERNAL_ERROR"
		}
	}
	if msg == "" {
		msg = strings.ToLower(strings.ReplaceAll(code, "_", " "))
	}
	code = strings.ToUpper(code)
	category, retryable, hint := classifyError(status, code)
	details := map[string]any{}
	if status == StatusNeedConfirmation {
		switch {
		case usesBooleanConfirmation(data):
			hint = "Ask the user for explicit confirmation, then retry the original tool with the same business arguments and user_confirmed=true."
		case usesKeyConfirmation(data):
			hint = "Ask the user for explicit confirmation, then retry the original tool with the same business arguments and the confirmation_key returned by waiting_confirmation."
		}
	}
	if hint != "" {
		details["retry_hint"] = hint
	}
	if exitCode, ok := exitCodeFromData(data, 0); ok {
		details["exit_code"] = exitCode
	}
	return Response{
		OK:        false,
		Status:    status,
		RequestID: requestID,
		Workspace: workspace,
		Data:      data,
		Error: &ErrorBody{
			Code: code, Message: msg, Category: category, Retryable: retryable, Details: details,
		},
	}
}

func usesBooleanConfirmation(data any) bool {
	value, ok := data.(map[string]any)
	if !ok || value["user_confirmed_required"] != true {
		return false
	}
	_, hasToken := value["confirmation_token"]
	return !hasToken
}

func usesKeyConfirmation(data any) bool {
	value, ok := data.(map[string]any)
	if !ok {
		return false
	}
	key, _ := value["confirmation_key"].(string)
	return strings.TrimSpace(key) != ""
}

func classifyError(status Status, code string) (category string, retryable bool, retryHint string) {
	if code == "CONFIRMATION_REQUIRED" {
		return "confirmation", true, "Ask the user to confirm the frozen manifest in the web conversation, then call move_out(action=submit) with the confirmation_uuid returned by move_out(action=prepare)."
	}
	if code == "CONFIRMATION_MISMATCH" {
		return "conflict", false, "Use the confirmation_uuid returned by move_out(action=prepare) for this move-out request; do not create a new UUID."
	}
	if status == StatusNeedConfirmation || strings.Contains(code, "CONFIRMATION") {
		return "confirmation", true, "Ask the user for explicit confirmation, then retry the original tool with the same business arguments and confirmation_token."
	}
	if strings.Contains(code, "UNAUTHORIZED") || strings.Contains(code, "FORBIDDEN") || strings.Contains(code, "DENIED") || strings.Contains(code, "SECRET") {
		return "permission", false, "Request the required permission or provide the required secret."
	}
	switch code {
	case "COMMAND_NOT_FOUND":
		return "execution", false, "Check the executable name and the workspace toolchain, then retry with a command that exists."
	case "PROCESS_EXIT", "COMMAND_FAILED":
		return "execution", false, "Inspect exit_code, stdout and stderr; correct the command or its inputs before retrying."
	case "OPERATION_FAILED":
		return "execution", false, "Inspect the failed operation step and its result before deciding whether to retry."
	case "BROWSER_NODE_STALE":
		return "conflict", true, "Refresh the browser snapshot and retry with a current DOM node id."
	case "BROWSER_ATTACHMENT_STALE":
		return "conflict", false, "Refresh the browser tab list; this tab's debugger attachment is stale. Reopen or create a controlled tab, or reconnect the browser extension, before retrying."
	case "BROWSER_TAB_NOT_FOUND":
		return "not_found", false, "Refresh the browser tab list and retry with a current tab_id."
	}
	if strings.Contains(code, "NOT_FOUND") || strings.Contains(code, "WORKSPACE_NOT_FOUND") {
		return "not_found", false, "Check the identifier and refresh the relevant list."
	}
	if code == "SYMLINK_NOT_ALLOWED" || code == "MOVE_OUT_FILE_ONLY" || code == "MOVE_OUT_DIRECTORY_ONLY" || code == "MOVE_OUT_SYMLINK_ONLY" || code == "PATH_ESCAPE" || code == "PATH_STAT_FAILED" || code == "FILE_READ_FAILED" || code == "DIRECTORY_READ_FAILED" || code == "WORKSPACE_MISMATCH" || code == "MOVE_OUT_REQUIRED" {
		return "validation", false, "Correct the explicit Workspace target and retry; safe move-out never follows symlinks or accepts shell paths."
	}
	if strings.Contains(code, "STALE") || strings.Contains(code, "CONFLICT") || strings.Contains(code, "VERSION") || strings.Contains(code, "PATCH_CONTEXT") || strings.Contains(code, "PATCH_HUNKS") || strings.Contains(code, "PATCH_APPLY") || strings.Contains(code, "ROLLBACK") || code == "DIRECTORY_CHANGED" {
		return "conflict", true, "Read the current revision and regenerate the operation."
	}
	if code == "TOO_MANY_CHANGES" {
		return "validation", true, "Split the edit into smaller batches and retry; keep total changed lines within the request limit."
	}
	if code == "LIMIT_EXCEEDED" {
		return "validation", false, "Reduce the request to the advertised limit and retry."
	}
	if code == "MOVE_OUT_IN_PROGRESS" {
		return "runtime", true, "Wait for the current move_out(action=submit) request, then retry with the same confirmation_uuid."
	}
	if code == "FILE_TOO_LARGE" {
		return "capacity", false, "Use a bounded window read, or reduce the requested full-read source size."
	}
	if code == "MOVE_OUT_REQUEST_EXPIRED" || code == "MOVE_OUT_MANIFEST_MISMATCH" || code == "MOVE_OUT_PURPOSE_MISMATCH" {
		return "validation", false, "Prepare a new move-out manifest and ask the web user to confirm it again."
	}
	if code == "MOVE_OUT_FAILED" || code == "MOVE_OUT_STATE_IN_DOUBT" || code == "MOVE_OUT_STORE_ERROR" || code == "MOVE_OUT_QUARANTINE_ERROR" {
		return "runtime", true, "Inspect the durable move-out request and audit event before retrying."
	}
	if strings.Contains(code, "INVALID") || strings.Contains(code, "BAD_REQUEST") || strings.Contains(code, "VALIDATION") || strings.Contains(code, "UNSUPPORTED") || strings.Contains(code, "REQUIRED") || strings.Contains(code, "AMBIGUOUS") {
		return "validation", false, "Correct the request arguments and retry."
	}
	if strings.Contains(code, "START") || strings.Contains(code, "EXEC") || strings.Contains(code, "RUNTIME") || strings.Contains(code, "TIMEOUT") || strings.Contains(code, "INTERRUPTED") {
		return "runtime", true, "Retry when the runtime is available or inspect the Task logs."
	}
	return "internal", false, "Inspect the server log and retry with a new request id if appropriate."
}

// exitCodeFromData extracts an execution exit code from a response payload so
// errors remain actionable even when a failed command is wrapped by an async
// operation result.
func exitCodeFromData(value any, depth int) (int, bool) {
	if depth > 8 || value == nil {
		return 0, false
	}
	switch typed := value.(type) {
	case map[string]any:
		if raw, exists := typed["exit_code"]; exists {
			switch code := raw.(type) {
			case int:
				return code, true
			case int8:
				return int(code), true
			case int16:
				return int(code), true
			case int32:
				return int(code), true
			case int64:
				return int(code), true
			case float64:
				return int(code), true
			}
		}
		for _, child := range typed {
			if code, ok := exitCodeFromData(child, depth+1); ok {
				return code, true
			}
		}
	case []any:
		for _, child := range typed {
			if code, ok := exitCodeFromData(child, depth+1); ok {
				return code, true
			}
		}
	case []map[string]any:
		for _, child := range typed {
			if code, ok := exitCodeFromData(child, depth+1); ok {
				return code, true
			}
		}
	}
	return 0, false
}

// EnsureRequestID returns id if non-empty, otherwise generates one.
func EnsureRequestID(id string) string {
	if id != "" {
		return id
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("req_%s_%s", time.Now().UTC().Format("20060102"), hex.EncodeToString(b[:]))
}

// ParseRequest decodes business tool arguments into Request.
// Runtime metadata is intentionally ignored here and injected by the server
// instrumentation boundary before handlers consume the normalized request.
func ParseRequest(raw json.RawMessage) (Request, error) {
	var req Request
	if len(raw) == 0 || string(raw) == "null" {
		req.Payload = map[string]any{}
		req.RequestID = EnsureRequestID("")
		req.OperationID = "op_" + strings.TrimPrefix(req.RequestID, "req_")
		return req, nil
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return Request{}, err
	}
	if req.Payload == nil {
		req.Payload = map[string]any{}
	}
	// purpose is the public semantic field. Intent remains an internal
	// observation name so existing handlers do not need to duplicate the value.
	if strings.TrimSpace(req.Purpose) != "" {
		req.Intent = req.Purpose
	}
	req.Purpose = strings.TrimSpace(req.Purpose)
	req.ReasoningSummary = strings.TrimSpace(req.ReasoningSummary)
	req.ProgressSummary = strings.TrimSpace(req.ProgressSummary)
	req.NextStep = strings.TrimSpace(req.NextStep)
	req.PlanID = strings.TrimSpace(req.PlanID)
	req.PlanTaskID = strings.TrimSpace(req.PlanTaskID)
	req.ExecutionTaskID = strings.TrimSpace(req.ExecutionTaskID)
	// Runtime metadata is deliberately discarded from the business payload.
	// The server repopulates it from Gateway Runtime Context after parsing.
	for key := range req.Payload {
		if isRuntimeField(key) {
			delete(req.Payload, key)
		}
	}
	// goal was a legacy model-supplied context field. Keep accepting old wire
	// payloads, but discard it before any business handler can consume it.
	delete(req.Payload, "goal")
	// MCP tool schemas expose flat arguments. Normalize them into Payload so
	// handlers share one request representation; explicit Payload values win.
	var flat map[string]any
	if err := json.Unmarshal(raw, &flat); err != nil {
		return Request{}, err
	}
	if req.RemoteSessionID == "" {
		if sessionID, ok := flat["session_id"].(string); ok {
			req.RemoteSessionID = strings.TrimSpace(sessionID)
		}
	}
	if req.CallID == "" {
		req.CallID = firstStringValue(flat, "call_id", "callId")
	}
	for key, value := range flat {
		if key == "goal" || key == "purpose" || key == "intent" || key == "activity" || key == "reasoning_summary" || key == "progress_summary" || key == "call_id" || key == "callId" || key == "session_id" || key == "remote_session_id" || key == "workspace" || key == "payload" || key == "execution_mode" || isRuntimeField(key) {
			continue
		}
		if _, exists := req.Payload[key]; !exists {
			req.Payload[key] = value
		}
	}
	req.RequestID = EnsureRequestID("")
	req.OperationID = "op_" + strings.TrimPrefix(req.RequestID, "req_")
	req.StartedAtMs = 0
	return req, nil
}

func firstStringValue(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func isRuntimeField(key string) bool {
	switch key {
	case "request_id", "trace_id", "span_id", "started_at_ms", "received_at_ms", "completed_at_ms", "network_latency_ms", "processing_ms", "server_elapsed_ms", "client_info", "execution_mode":
		return true
	default:
		return false
	}
}

// Marshal serializes a Response to JSON bytes.
func Marshal(resp Response) ([]byte, error) {
	return json.Marshal(resp)
}
