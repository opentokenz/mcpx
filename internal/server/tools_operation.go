package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"

	"mcpx/internal/arc"
	"mcpx/internal/envelope"
	"mcpx/internal/operation"
	"mcpx/internal/remotesession"
)

func (r *Runtime) toolOperationBatch(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, session, fail := r.changeRequest(ctx, req, true)
	if fail != nil {
		return fail, nil
	}
	if session.Role != "owner" && session.Role != "editor" {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "forbidden", "operation_batch requires an owner or editor session")
	}
	for _, key := range []string{"run_id", "operation_id"} {
		if raw, present := envReq.Payload[key]; present {
			value, ok := raw.(string)
			if !ok || value == "" || strings.TrimSpace(value) != value || len(value) > 200 {
				return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", key+" must be a non-empty stable string of at most 200 bytes")
			}
		}
	}
	items, ok := envReq.Payload["operations"].([]any)
	if !ok || len(items) == 0 {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", "operations is required")
	}
	if len(items) > operation.MaxSteps {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", fmt.Sprintf("operations exceeds %d steps", operation.MaxSteps))
	}
	steps := make([]operation.StepSpec, 0, len(items))
	for index, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", fmt.Sprintf("operations[%d] must be an object", index))
		}
		stepID, _ := item["id"].(string)
		toolName, _ := item["tool"].(string)
		arguments, _ := item["arguments"].(map[string]any)
		stepID = strings.TrimSpace(stepID)
		toolName = strings.TrimSpace(toolName)
		if stepID == "" || toolName == "" || arguments == nil {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", fmt.Sprintf("operations[%d] requires id, tool and arguments", index))
		}
		if isCleanCoreRequest(ctx) && !isCleanPublicTool(toolName) {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", fmt.Sprintf("tool %q is not available in the clean-core operation catalog", toolName))
		}
		if toolName == "operation_batch" || toolName == "operation_manage" || toolName == "secret_provide" {
			message := fmt.Sprintf("tool %q cannot be nested in operation_batch", toolName)
			if toolName == "operation_manage" {
				message += "; use operation_manage with action=status/result and operation_ids for batch queries"
			}
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", message)
		}
		dependsOn, err := parseStringSliceValue(item["depends_on"])
		if err != nil {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", fmt.Sprintf("step %q: %v", stepID, err))
		}
		if err := r.validateOperationToolArguments(toolName, arguments, session.ID, envReq.Intent); err != nil {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", fmt.Sprintf("step %q: %v", stepID, err))
		}
		r.toolIndexMu.RLock()
		meta, exists := r.toolMeta[toolName]
		r.toolIndexMu.RUnlock()
		if !exists {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", fmt.Sprintf("tool %q is not registered", toolName))
		}
		steps = append(steps, operation.StepSpec{
			ID: stepID, Tool: toolName, Arguments: cloneArguments(arguments), DependsOn: dependsOn,
			Exclusive: !meta.ReadOnly || meta.OpenWorld,
		})
	}
	record, err := r.operations.Submit(ctx, operation.SubmitSpec{
		ID: stringPayload(envReq.Payload, "operation_id"), RunID: stringPayload(envReq.Payload, "run_id"),
		RemoteSessionID: session.ID, WorkspaceName: session.WorkspaceName,
		RequestID: envReq.RequestID, Purpose: envReq.Intent, Steps: steps,
	}, r.executeOperationStep)
	if err != nil {
		if errors.Is(err, operation.ErrResultExpired) {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "operation_result_expired", err.Error())
		}
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "operation_submit_error", err.Error())
	}
	response := envelope.Accepted(envReq.RequestID, session.WorkspaceName, operationView(record, false))
	response.RemoteSessionID = session.ID
	return r.resultJSON(response)
}

func (r *Runtime) toolOperationManage(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, fail := r.remoteRequest(ctx, req)
	if fail != nil {
		return fail, nil
	}
	remoteID, err := requireRemoteSessionID(envReq)
	if err != nil {
		return r.remoteError(envReq, "", "", err)
	}
	session, err := r.remote.Get(ctx, principal, remoteID)
	if err != nil {
		return r.remoteError(envReq, remoteID, "", err)
	}
	action := strings.ToLower(stringPayload(envReq.Payload, "action"))
	if action == "" {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", "action is required")
	}
	if (action == "cancel" || action == "resume") && session.Role != "owner" && session.Role != "editor" {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "forbidden", "operation mutation requires an owner or editor session")
	}
	if r.operations == nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "operation_unavailable", "asynchronous operations are unavailable")
	}
	operationIDs, batch, err := parseOperationTargets(envReq.Payload, action)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", err.Error())
	}
	records := make([]operation.Record, len(operationIDs))
	for index, operationID := range operationIDs {
		record, err := r.operations.Get(ctx, operationID)
		if err != nil {
			return r.operationError(envReq, session, err)
		}
		if record.RemoteSessionID != session.ID {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "forbidden", "operation belongs to another Remote Session")
		}
		records[index] = record
	}
	if batch {
		return r.operationBatchManageResponse(ctx, envReq, session, action, records)
	}
	operationID := operationIDs[0]
	record := records[0]
	switch action {
	case "status":
		return r.operationResponse(envReq, session, record, operationView(record, false), false)
	case "wait":
		timeout := time.Duration(intPayload(envReq.Payload, "timeout_ms")) * time.Millisecond
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		if timeout > 60*time.Second {
			timeout = 60 * time.Second
		}
		record, timedOut, err := r.operations.Wait(ctx, operationID, timeout)
		if err != nil {
			return r.operationError(envReq, session, err)
		}
		return r.operationResponse(envReq, session, record, operationView(record, !timedOut), timedOut)
	case "result":
		page, err := r.operations.Result(ctx, operationID, stringPayload(envReq.Payload, "step_id"), stringPayload(envReq.Payload, "cursor"), intPayload(envReq.Payload, "limit"))
		if err != nil {
			return r.operationError(envReq, session, err)
		}
		data := operationResultView(page)
		data["step_id"] = page.StepID
		return r.operationResponse(envReq, session, page.Operation, data, false)
	case "cancel":
		record, err := r.operations.Cancel(ctx, operationID)
		if err != nil {
			return r.operationError(envReq, session, err)
		}
		return r.operationResponse(envReq, session, record, operationView(record, false), false)
	case "resume":
		stepID := stringPayload(envReq.Payload, "step_id")
		token := stringPayload(envReq.Payload, "confirmation_token")
		record, err := r.operations.Resume(ctx, operationID, stepID, token, r.executeOperationStep)
		if err != nil {
			return r.operationError(envReq, session, err)
		}
		return r.operationResponse(envReq, session, record, operationView(record, false), false)
	default:
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", fmt.Sprintf("unsupported operation action %q", action))
	}
}

func parseOperationTargets(payload map[string]any, action string) ([]string, bool, error) {
	operationID := strings.TrimSpace(stringPayload(payload, "operation_id"))
	rawIDs, hasIDs := payload["operation_ids"]
	if operationID != "" && hasIDs {
		return nil, false, errors.New("operation_id and operation_ids are mutually exclusive")
	}
	if hasIDs {
		ids, err := parseNonEmptyStringSlice(rawIDs, "operation_ids")
		if err != nil {
			return nil, false, err
		}
		if len(ids) == 0 || len(ids) > operation.MaxBatchQueries {
			return nil, false, fmt.Errorf("operation_ids must contain 1-%d IDs", operation.MaxBatchQueries)
		}
		seen := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			if _, exists := seen[id]; exists {
				return nil, false, fmt.Errorf("operation_ids contains duplicate ID %q", id)
			}
			seen[id] = struct{}{}
		}
		if action != "status" && action != "result" {
			return nil, false, errors.New("batch operation_ids supports only action=status or action=result")
		}
		_, hasStepID := payload["step_id"]
		_, hasCursor := payload["cursor"]
		if hasStepID || hasCursor {
			return nil, false, errors.New("step_id and cursor require a single operation_id")
		}
		return ids, true, nil
	}
	if operationID == "" {
		return nil, false, errors.New("exactly one of operation_id or operation_ids is required")
	}
	return []string{operationID}, false, nil
}

func parseNonEmptyStringSlice(value any, field string) ([]string, error) {
	var items []any
	switch typed := value.(type) {
	case []any:
		items = typed
	case []string:
		items = make([]any, len(typed))
		for index, item := range typed {
			items[index] = item
		}
	default:
		return nil, fmt.Errorf("%s must be an array of strings", field)
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		text = strings.TrimSpace(text)
		if !ok || text == "" {
			return nil, fmt.Errorf("%s must contain only non-empty strings", field)
		}
		result = append(result, text)
	}
	return result, nil
}

func (r *Runtime) validateOperationToolArguments(toolName string, arguments map[string]any, sessionID, purpose string) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// Tool schemas are server-owned data, but a malformed schema must remain
			// a request error and must never be able to crash the MCP process.
			err = fmt.Errorf("invalid schema for tool %q: %v", toolName, recovered)
		}
	}()
	r.toolIndexMu.RLock()
	tool, exists := r.toolIndex[toolName]
	r.toolIndexMu.RUnlock()
	if !exists {
		return fmt.Errorf("tool %q is not registered", toolName)
	}
	rawSchema := mcpresult.ToolSchemaJSON(tool)
	if len(rawSchema) == 0 || string(rawSchema) == `{"type":"object"}` {
		// still validate against object if present
	}
	var schema map[string]any
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		return fmt.Errorf("invalid schema for tool %q", toolName)
	}
	merged := cloneArguments(arguments)
	merged["remote_session_id"] = sessionID
	merged["purpose"] = purpose
	return validateOperationSchemaValue(merged, schema, "arguments")
}

func (r *Runtime) operationResponse(envReq envelope.Request, session remotesession.Session, record operation.Record, data map[string]any, timedOut bool) (*mcp.CallToolResult, error) {
	if timedOut || record.State == operation.StateQueued || record.State == operation.StateRunning {
		response := envelope.Accepted(envReq.RequestID, session.WorkspaceName, data)
		response.RemoteSessionID = session.ID
		return r.resultJSON(response)
	}
	if record.State == operation.StateWaitingConfirmation {
		response := envelope.Fail(envelope.StatusNeedConfirmation, envReq.RequestID, session.WorkspaceName, data, "USER_CONFIRMATION_REQUIRED", "操作等待语义确认")
		response.RemoteSessionID = session.ID
		return r.resultJSON(response)
	}
	if record.State == operation.StateInterrupted || record.State == operation.StateCancelled {
		response := envelope.Interrupted(envReq.RequestID, session.WorkspaceName, data)
		response.RemoteSessionID = session.ID
		return r.resultJSON(response)
	}
	if record.State == operation.StateFailed {
		response := envelope.Fail(envelope.StatusError, envReq.RequestID, session.WorkspaceName, data, "OPERATION_FAILED", "异步操作执行失败")
		response.RemoteSessionID = session.ID
		return r.resultJSON(response)
	}
	response := envelope.OK(envReq.RequestID, session.WorkspaceName, data)
	response.RemoteSessionID = session.ID
	return r.resultJSON(response)
}

func (r *Runtime) operationBatchManageResponse(ctx context.Context, envReq envelope.Request, session remotesession.Session, action string, records []operation.Record) (*mcp.CallToolResult, error) {
	items := make([]map[string]any, len(records))
	switch action {
	case "status":
		for index, record := range records {
			items[index] = operationView(record, false)
		}
	case "result":
		limit := intPayload(envReq.Payload, "limit")
		if limit <= 0 {
			// Batch results are usually read for their content, not for a
			// compact summary; default to the maximum single-result budget so
			// a source_read batch is not truncated into unusable chunks.
			limit = operation.MaxResultBytes
		}
		for index, record := range records {
			page, err := r.operations.Result(ctx, record.ID, "", "", limit)
			if err != nil {
				return r.operationError(envReq, session, err)
			}
			records[index] = page.Operation
			items[index] = operationResultView(page)
		}
	default:
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", fmt.Sprintf("unsupported batch operation action %q", action))
	}
	data := map[string]any{"action": action, "items": items}
	status := operationBatchStatus(records)
	var response envelope.Response
	switch status {
	case envelope.StatusAccepted:
		response = envelope.Accepted(envReq.RequestID, session.WorkspaceName, data)
	case envelope.StatusNeedConfirmation:
		response = envelope.Fail(envelope.StatusNeedConfirmation, envReq.RequestID, session.WorkspaceName, data, "USER_CONFIRMATION_REQUIRED", "批量操作中存在等待语义确认的操作")
	case envelope.StatusInterrupted:
		response = envelope.Interrupted(envReq.RequestID, session.WorkspaceName, data)
	case envelope.StatusError:
		response = envelope.Fail(envelope.StatusError, envReq.RequestID, session.WorkspaceName, data, "OPERATION_FAILED", "批量操作中存在执行失败的操作")
	default:
		response = envelope.OK(envReq.RequestID, session.WorkspaceName, data)
	}
	response.RemoteSessionID = session.ID
	return r.resultJSON(response)
}

func operationBatchStatus(records []operation.Record) envelope.Status {
	if len(records) == 0 {
		return envelope.StatusError
	}
	var hasRunning, hasFailed, hasInterrupted bool
	for _, record := range records {
		switch record.State {
		case operation.StateWaitingConfirmation:
			return envelope.StatusNeedConfirmation
		case operation.StateQueued, operation.StateRunning:
			hasRunning = true
		case operation.StateFailed:
			hasFailed = true
		case operation.StateInterrupted, operation.StateCancelled:
			hasInterrupted = true
		case operation.StateSucceeded:
		default:
			return envelope.StatusError
		}
	}
	if hasRunning {
		return envelope.StatusAccepted
	}
	if hasFailed {
		return envelope.StatusError
	}
	if hasInterrupted {
		return envelope.StatusInterrupted
	}
	return envelope.StatusOK
}

func (r *Runtime) operationError(envReq envelope.Request, session remotesession.Session, err error) (*mcp.CallToolResult, error) {
	code := "operation_error"
	if errors.Is(err, operation.ErrNotFound) {
		code = "operation_not_found"
	} else if errors.Is(err, operation.ErrAlreadyCompleted) {
		code = "operation_already_completed"
	} else if errors.Is(err, operation.ErrConfirmation) {
		code = "confirmation_invalid"
	}
	return r.terminalError(envReq, session.ID, session.WorkspaceName, code, err.Error())
}

func operationView(record operation.Record, includeResults bool) map[string]any {
	data := map[string]any{
		"operation_id": record.ID, "remote_session_id": record.RemoteSessionID, "workspace": record.WorkspaceName,
		"run_id": record.RunID,
		"state":  record.State, "purpose": record.Purpose,
		"state_sequence": record.StateSequence, "state_event_id": record.StateEventID,
	}
	if record.State == operation.StateSucceeded || record.State == operation.StateFailed || record.State == operation.StateInterrupted || record.State == operation.StateCancelled {
		data["terminal_event_id"] = record.StateEventID
	}
	steps := make([]map[string]any, 0, len(record.Steps))
	for _, step := range record.Steps {
		view := map[string]any{"id": step.ID, "tool": step.Tool, "state": step.State, "depends_on": step.DependsOn}
		if step.ConfirmationToken != "" {
			view["confirmation_token"] = step.ConfirmationToken
		}
		if includeResults {
			view["result"] = operationResultValue(step.Result)
			view["error"] = decodeJSONValue(step.Error)
		}
		steps = append(steps, view)
	}
	data["steps"] = steps
	data["stats"] = operationStats(record)
	if record.State == operation.StateQueued || record.State == operation.StateRunning {
		data["next_action"] = nextActionWithReason("operation_manage", "操作仍在执行；使用一次 wait 等待结果，不要重复轮询 status", map[string]any{
			"remote_session_id": record.RemoteSessionID,
			"operation_id":      record.ID,
			"action":            "wait",
			"timeout_ms":        30000,
		})
	}
	// A completed wait already exposes each machine-readable result under
	// steps[].result. Do not also copy record.Result here: for multi-step
	// operations it is an aggregate of the raw stored MCP envelopes and would
	// duplicate the same payload at the top level. Callers that need the
	// aggregate or paged bytes use operation_manage(action=result).
	return data
}

func operationStats(record operation.Record) map[string]any {
	stats := map[string]any{
		"step_count":         len(record.Steps),
		"success_count":      0,
		"failure_count":      0,
		"scheduling_wait_ms": int64(0),
		"server_duration_ms": int64(0),
		"max_concurrency":    0,
	}
	if record.StartedAt != nil {
		stats["scheduling_wait_ms"] = nonNegativeDurationMillis(record.StartedAt.Sub(record.CreatedAt))
	}
	if record.StartedAt != nil && record.CompletedAt != nil {
		stats["server_duration_ms"] = nonNegativeDurationMillis(record.CompletedAt.Sub(*record.StartedAt))
	}

	type concurrencyEvent struct {
		at    time.Time
		delta int
	}
	events := make([]concurrencyEvent, 0, len(record.Steps)*2)
	durations := make([]int64, 0, len(record.Steps))
	successCount, failureCount := 0, 0
	for _, step := range record.Steps {
		switch step.State {
		case operation.StateSucceeded:
			successCount++
		case operation.StateFailed, operation.StateInterrupted, operation.StateCancelled:
			failureCount++
		}
		if step.StartedAt == nil || step.CompletedAt == nil {
			continue
		}
		start := *step.StartedAt
		end := *step.CompletedAt
		if !end.After(start) {
			end = start.Add(time.Millisecond)
		}
		durations = append(durations, nonNegativeDurationMillis(end.Sub(start)))
		events = append(events, concurrencyEvent{at: start, delta: 1}, concurrencyEvent{at: end, delta: -1})
	}
	stats["success_count"] = successCount
	stats["failure_count"] = failureCount

	sort.Slice(events, func(i, j int) bool {
		if events[i].at.Equal(events[j].at) {
			return events[i].delta > events[j].delta
		}
		return events[i].at.Before(events[j].at)
	})
	current, maximum := 0, 0
	for _, event := range events {
		current += event.delta
		if current > maximum {
			maximum = current
		}
	}
	stats["max_concurrency"] = maximum

	if len(durations) > 0 {
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		stats["step_duration_ms"] = map[string]any{
			"p50": durationPercentile(durations, 50),
			"p95": durationPercentile(durations, 95),
		}
	}
	return stats
}

func nonNegativeDurationMillis(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return duration.Milliseconds()
}

func durationPercentile(sorted []int64, percentile int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	index := (len(sorted)*percentile + 99) / 100
	if index < 1 {
		index = 1
	}
	if index > len(sorted) {
		index = len(sorted)
	}
	return sorted[index-1]
}

func operationResultView(page operation.ResultPage) map[string]any {
	data := operationView(page.Operation, false)
	data["result"] = operationResultValue(page.Result)
	data["next_cursor"] = page.NextCursor
	if page.NextCursor != "" {
		arguments := map[string]any{
			"remote_session_id": page.Operation.RemoteSessionID,
			"operation_id":      page.Operation.ID,
			"action":            "result",
			"cursor":            page.NextCursor,
		}
		if page.StepID != "" {
			arguments["step_id"] = page.StepID
		}
		data["next_action"] = map[string]any{
			"tool":      "operation_manage",
			"reason":    "结果超过单次返回上限，使用返回的 cursor 继续读取剩余内容",
			"arguments": arguments,
		}
	}
	return data
}

// operationResultValue unwraps the mcp.CallToolResult envelope stored by an
// operation step. Prefer machine data (ARC _meta / structuredContent) over the
// human text summary so nested source_read matches, sha256, etc. stay fields.
func operationResultValue(raw json.RawMessage) any {
	value := decodeJSONValue(raw)
	wrapper, ok := value.(map[string]any)
	if !ok {
		return value
	}
	result := wrapper
	if nested, hasNested := wrapper["result"].(map[string]any); hasNested {
		result = nested
	}
	return unwrapToolContent(result, value)
}

func unwrapToolContent(result map[string]any, fallback any) any {
	if data := extractMachineToolData(result); data != nil {
		return data
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		return fallback
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		return fallback
	}
	text, ok := first["text"].(string)
	if !ok || strings.TrimSpace(text) == "" {
		return fallback
	}
	var decoded any
	if err := json.Unmarshal([]byte(text), &decoded); err == nil {
		return decoded
	}
	return text
}

// extractMachineToolData prefers structured fields over human prose text.
// Stored step results look like: {_meta: {mcpx.result: {mcpx: {result: {data}}}}, content:[{text}]}.
func extractMachineToolData(result map[string]any) any {
	if sc := result["structuredContent"]; sc != nil {
		return sc
	}
	if sc := result["structured_content"]; sc != nil {
		return sc
	}
	meta, _ := result["_meta"].(map[string]any)
	if meta == nil {
		meta, _ = result["meta"].(map[string]any)
	}
	if meta == nil {
		return nil
	}
	// mcp-go may nest additional fields under additionalFields or flatten them.
	candidates := []any{meta[arc.ResultMetadataKey], meta["mcpx.result"]}
	if additional, ok := meta["additionalFields"].(map[string]any); ok {
		candidates = append(candidates, additional[arc.ResultMetadataKey], additional["mcpx.result"])
	}
	if additional, ok := meta["AdditionalFields"].(map[string]any); ok {
		candidates = append(candidates, additional[arc.ResultMetadataKey], additional["mcpx.result"])
	}
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		if data := arcResultData(candidate); data != nil {
			return data
		}
	}
	return nil
}

func arcResultData(value any) any {
	asMap, ok := value.(map[string]any)
	if !ok {
		// envelope may have been stored as typed JSON round-trip only
		raw, err := json.Marshal(value)
		if err != nil {
			return nil
		}
		if json.Unmarshal(raw, &asMap) != nil {
			return nil
		}
	}
	// shapes: {mcpx:{result:{data}}} or {result:{data}} or raw data map
	if mcpx, ok := asMap["mcpx"].(map[string]any); ok {
		if result, ok := mcpx["result"].(map[string]any); ok {
			if data, ok := result["data"]; ok && data != nil {
				// Keep type/status for consumers that branch on result kind.
				return map[string]any{
					"type":    result["type"],
					"status":  result["status"],
					"summary": result["summary"],
					"data":    data,
				}
			}
		}
	}
	if result, ok := asMap["result"].(map[string]any); ok {
		if data, ok := result["data"]; ok && data != nil {
			return map[string]any{
				"type":    result["type"],
				"status":  result["status"],
				"summary": result["summary"],
				"data":    data,
			}
		}
	}
	if _, hasData := asMap["data"]; hasData {
		return asMap
	}
	return nil
}

func decodeJSONValue(raw json.RawMessage) any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return string(raw)
	}
	return value
}

func parseStringSliceValue(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, errors.New("depends_on must be an array of strings")
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, errors.New("depends_on must contain only non-empty strings")
		}
		result = append(result, strings.TrimSpace(text))
	}
	return result, nil
}

func validateOperationSchemaValue(value any, schema map[string]any, path string) error {
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		matched := false
		for _, item := range enum {
			if fmt.Sprint(item) == fmt.Sprint(value) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s has an unsupported value", path)
		}
	}
	if constant, exists := schema["const"]; exists && fmt.Sprint(constant) != fmt.Sprint(value) {
		return fmt.Errorf("%s has an unsupported value", path)
	}
	schemaType, _ := schema["type"].(string)
	if schemaType == "" {
		// JSON Schema permits type-less constraint objects. The generated MCP
		// schemas use this shape for some nested values; infer object only when
		// object constraints are present and otherwise leave the value open.
		if _, hasProperties := schema["properties"]; hasProperties {
			schemaType = "object"
		}
	}
	switch schemaType {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be an object", path)
		}
		properties, _ := schema["properties"].(map[string]any)
		if required, ok := schema["required"].([]any); ok {
			for _, raw := range required {
				key, _ := raw.(string)
				if _, exists := object[key]; !exists {
					return fmt.Errorf("missing required field %q", key)
				}
			}
		}
		for key, item := range object {
			rawSchema, known := properties[key].(map[string]any)
			if !known {
				if isOperationInjectedField(key) {
					continue
				}
				if additional, explicit := schema["additionalProperties"].(bool); explicit && !additional {
					return fmt.Errorf("unknown field %q", key)
				}
				continue
			}
			if err := validateOperationSchemaValue(item, rawSchema, path+"."+key); err != nil {
				return err
			}
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s must be an array", path)
		}
		itemSchema, _ := schema["items"].(map[string]any)
		for index, item := range items {
			if err := validateOperationSchemaValue(item, itemSchema, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s must be a string", path)
		}
	case "number", "integer":
		switch value.(type) {
		case float64, float32, int, int64, int32, uint, uint64, uint32:
		default:
			return fmt.Errorf("%s must be a number", path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be a boolean", path)
		}
	}
	return nil
}

func isOperationInjectedField(key string) bool {
	switch key {
	case "session_id", "remote_session_id", "goal", "purpose", "intent", "progress_summary", "execution_mode":
		return true
	default:
		return false
	}
}
