// Package arc implements the MCPX Agent Result Contract.
package arc

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"
)

const Version = "2.0"

// ResultMetadataKey identifies the hidden response metadata that carries the
// ARC envelope without duplicating business data. Models must read
// structuredContent; _meta keeps trace identifiers and result type/status.
const ResultMetadataKey = "mcpx.result"

const (
	SchemaText              = "mcpx.text.v1"
	SchemaMarkdown          = "mcpx.markdown.v1"
	SchemaSearchResult      = "mcpx.search_result.v1"
	SchemaCodeChange        = "mcpx.code_change.v1"
	SchemaTable             = "mcpx.table.v1"
	SchemaFileTree          = "mcpx.file_tree.v1"
	SchemaLog               = "mcpx.log.v1"
	SchemaError             = "mcpx.error.v1"
	SchemaDiagram           = "mcpx.diagram.v1"
	SchemaDiagramCollection = "mcpx.diagram_collection.v1"
	SchemaPlan              = "mcpx.plan.v1"
	SchemaPlanTask          = "mcpx.plan_task.v1"
	SchemaDelivery          = "mcpx.delivery.v1"
)

type Timing struct {
	StartedAtMs      int64
	ReceivedAtMs     int64
	CompletedAtMs    int64
	NetworkLatencyMs int64
	ProcessingMs     int64
	ServerElapsedMs  int64
}

type ResultContext struct {
	RequestID string
	TraceID   string
	SpanID    string
	Context   Context
	Timing    Timing
}

// Activity is the latest accepted Client Protocol Activity V2 snapshot for the
// Remote Session at tool-result time. It is server-sourced only: handlers and
// raw result data must never synthesize or override it.
type Activity struct {
	TurnID        string `json:"turn_id"`
	Sequence      int64  `json:"sequence"`
	State         string `json:"state"`
	Kind          string `json:"kind"`
	Summary       string `json:"summary"`
	RelatedCallID string `json:"related_call_id,omitempty"`
}

// Context is the concise public execution context for one ARC result. Purpose
// describes the effect of this tool call; Activity carries semantic narration.
// Planning/execution IDs remain correlation fields, not narration.
type Context struct {
	Purpose         string    `json:"purpose,omitempty"`
	Activity        *Activity `json:"activity,omitempty"`
	PlanID          string    `json:"plan_id,omitempty"`
	PlanTaskID      string    `json:"plan_task_id,omitempty"`
	ExecutionTaskID string    `json:"execution_task_id,omitempty"`
	OperationID     string    `json:"operation_id,omitempty"`
}

type Trace struct {
	TraceID          string `json:"trace_id"`
	SpanID           string `json:"span_id,omitempty"`
	RequestID        string `json:"request_id,omitempty"`
	Source           string `json:"source"`
	Tool             string `json:"tool"`
	StartedAtMs      int64  `json:"started_at_ms"`
	ReceivedAtMs     int64  `json:"received_at_ms"`
	CompletedAtMs    int64  `json:"completed_at_ms"`
	NetworkLatencyMs int64  `json:"network_latency_ms"`
	Duration         struct {
		ServerMs int64 `json:"server_ms"`
	} `json:"duration"`
}

type Hints struct {
	PreferredBehavior string `json:"preferred_behavior,omitempty"`
}

type Action struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Label     string         `json:"label"`
	Confirm   bool           `json:"confirm"`
	Arguments map[string]any `json:"arguments"`
}

type Result struct {
	Type         string        `json:"type"`
	Schema       string        `json:"schema"`
	Status       string        `json:"status"`
	Summary      string        `json:"summary,omitempty"`
	Context      Context       `json:"context"`
	Data         any           `json:"data"`
	Presentation *Presentation `json:"presentation,omitempty"`
	Hints        Hints         `json:"hints,omitempty"`
	Actions      []Action      `json:"actions,omitempty"`
}

type Envelope struct {
	MCPX struct {
		Version string `json:"version"`
		Trace   Trace  `json:"trace"`
		Result  Result `json:"result"`
	} `json:"mcpx"`
}

// WrapToolResult converts an internal handler result to the public MCPX result.
//
// Contract:
//   - content[0].text — human-facing summary only (Markdown / short status / diffs)
//   - structuredContent — model-facing fields {status, type, data, error?, actions?, hints?}
//   - _meta[mcpx.result] — ARC envelope without business data (trace + type)
//
// Models must consume structuredContent (or ARC data), not parse prose text.
func WrapToolResult(tool string, runtime ResultContext, raw *mcp.CallToolResult) *mcp.CallToolResult {
	if raw == nil {
		raw = mcpresult.NewError("tool returned no result")
	}
	data, summary := extractResult(raw)
	resultType, resultData, hints, actions := classify(tool, raw.IsError, data, summary)
	semanticContext := contextFrom(runtime, data, resultData)
	if summary == "" {
		summary = fmt.Sprintf("%s result returned.", tool)
	}
	renderer := "text"
	if resultType == "code_change" {
		renderer = "diff"
	}
	display, _ := RenderToolContent(tool, resultType, renderer, summary, resultData)
	status := publicResultStatus(raw, data)

	var envelope Envelope
	envelope.MCPX.Version = Version
	envelope.MCPX.Trace = buildTrace(tool, runtime, runtime.Timing)
	envelope.MCPX.Result = Result{
		Type: resultType, Schema: schemaForType(resultType), Status: status, Summary: summary,
		Context: semanticContext,
		Data:    resultData, Presentation: DefaultPresentation(resultType), Hints: hints, Actions: actions,
	}
	display = renderContextBlock(display, semanticContext)
	content := []mcp.Content{&mcp.TextContent{Text: display}}
	for _, item := range raw.Content {
		if _, isText := item.(*mcp.TextContent); isText {
			continue // replaced by display summary
		}
		content = append(content, item)
	}
	raw.Content = content
	raw.StructuredContent = modelStructuredContent(status, resultType, resultData, data, semanticContext, hints, actions, raw.IsError)
	setMetadata(raw, envelope, resultType)
	return raw
}

// modelStructuredContent is the machine contract for models.
func modelStructuredContent(status, resultType string, resultData any, rawData map[string]any, semanticContext Context, hints Hints, actions []Action, isError bool) map[string]any {
	payload := map[string]any{
		"status":  status,
		"type":    resultType,
		"context": contextData(semanticContext),
		"data":    modelBusinessData(resultData, rawData, isError),
	}
	if errBody := modelErrorData(resultData, rawData, isError); errBody != nil {
		payload["error"] = errBody
	}
	if modelNeedsHint(status, hints.PreferredBehavior) {
		payload["hints"] = hints
	}
	if len(actions) > 0 {
		payload["actions"] = actions
	}
	normalized, _ := normalizePublicData(payload).(map[string]any)
	return normalized
}

func modelBusinessData(resultData any, rawData map[string]any, isError bool) any {
	if isError || isErrorData(rawData) {
		if inner, ok := rawData["data"].(map[string]any); ok {
			return stripRecoveryFields(inner)
		}
		return map[string]any{}
	}
	if asMap, ok := resultData.(map[string]any); ok {
		return stripRecoveryFields(asMap)
	}
	return resultData
}

func modelErrorData(resultData any, rawData map[string]any, isError bool) map[string]any {
	if rawData != nil {
		if errBody, ok := rawData["error"].(map[string]any); ok && errBody != nil {
			return stripErrorRecoveryFields(errBody)
		}
	}
	if isError {
		if asMap, ok := resultData.(map[string]any); ok {
			if _, hasCode := asMap["code"]; hasCode {
				return stripErrorRecoveryFields(asMap)
			}
		}
	}
	return nil
}

func stripRecoveryFields(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	result := make(map[string]any, len(input))
	for key, value := range input {
		switch key {
		case "next_action", "next_actions", "suggested_next", "recovery":
			continue
		default:
			result[key] = value
		}
	}
	return result
}

func stripErrorRecoveryFields(input map[string]any) map[string]any {
	result := stripRecoveryFields(input)
	if details, ok := result["details"].(map[string]any); ok {
		result["details"] = stripRecoveryFields(details)
	}
	return result
}

func modelNeedsHint(status, behavior string) bool {
	switch behavior {
	case "ask_confirm", "continue":
		return true
	case "summarize":
		return status == "failed" || status == "interrupted"
	default:
		return false
	}
}

func contextData(context Context) map[string]any {
	context = normalizeContext(context)
	data := map[string]any{}
	for key, value := range map[string]string{
		"purpose":           context.Purpose,
		"plan_id":           context.PlanID,
		"plan_task_id":      context.PlanTaskID,
		"execution_task_id": context.ExecutionTaskID,
		"operation_id":      context.OperationID,
	} {
		if value != "" {
			data[key] = value
		}
	}
	return data
}

func activityData(activity *Activity) map[string]any {
	activity = normalizeActivity(activity)
	if activity == nil {
		return nil
	}
	data := map[string]any{
		"turn_id":  activity.TurnID,
		"sequence": activity.Sequence,
		"state":    activity.State,
		"kind":     activity.Kind,
		"summary":  activity.Summary,
	}
	if activity.RelatedCallID != "" {
		data["related_call_id"] = activity.RelatedCallID
	}
	return data
}

func contextFrom(runtime ResultContext, rawData map[string]any, resultData any) Context {
	result := runtime.Context
	mergeContextMap(&result, rawData)
	if nested, ok := rawData["context"].(map[string]any); ok {
		mergeContextMap(&result, nested)
	}
	if nested, ok := rawData["data"].(map[string]any); ok {
		mergeContextMap(&result, nested)
		if nestedContext, contextOK := nested["context"].(map[string]any); contextOK {
			mergeContextMap(&result, nestedContext)
		}
	}
	if nested, ok := resultData.(map[string]any); ok {
		mergeContextMap(&result, nested)
	}
	return normalizeContext(result)
}

func mergeContextMap(context *Context, values map[string]any) {
	if context == nil {
		return
	}
	if context.Purpose == "" {
		context.Purpose = stringValue(values, "purpose")
	}
	if context.PlanID == "" {
		context.PlanID = stringValue(values, "plan_id")
	}
	if context.PlanTaskID == "" {
		context.PlanTaskID = stringValue(values, "plan_task_id")
	}
	if context.ExecutionTaskID == "" {
		context.ExecutionTaskID = stringValue(values, "execution_task_id")
	}
	if context.OperationID == "" {
		context.OperationID = stringValue(values, "operation_id")
	}
}

func normalizeContext(context Context) Context {
	context.Purpose = strings.TrimSpace(context.Purpose)
	context.Activity = normalizeActivity(context.Activity)
	context.PlanID = strings.TrimSpace(context.PlanID)
	context.PlanTaskID = strings.TrimSpace(context.PlanTaskID)
	context.ExecutionTaskID = strings.TrimSpace(context.ExecutionTaskID)
	context.OperationID = strings.TrimSpace(context.OperationID)
	return context
}

func normalizeActivity(activity *Activity) *Activity {
	if activity == nil {
		return nil
	}
	normalized := *activity
	normalized.TurnID = strings.TrimSpace(normalized.TurnID)
	normalized.State = strings.ToLower(strings.TrimSpace(normalized.State))
	normalized.Kind = strings.ToLower(strings.TrimSpace(normalized.Kind))
	normalized.Summary = strings.TrimSpace(normalized.Summary)
	normalized.RelatedCallID = strings.TrimSpace(normalized.RelatedCallID)
	if normalized.TurnID == "" || normalized.Sequence <= 0 || normalized.Summary == "" || !validActivityState(normalized.State) || !validActivityKind(normalized.Kind) {
		return nil
	}
	return &normalized
}

func validActivityState(state string) bool {
	switch state {
	case "turn_started", "thinking", "preparing_action", "waiting_tool", "reviewing_result", "responding", "waiting_user", "blocked", "turn_completed", "turn_failed":
		return true
	default:
		return false
	}
}

func validActivityKind(kind string) bool {
	switch kind {
	case "intent", "hypothesis", "evidence", "conclusion", "next", "status":
		return true
	default:
		return false
	}
}

func buildTrace(tool string, runtime ResultContext, timing Timing) Trace {
	traceID := runtime.TraceID
	if traceID == "" {
		traceID = newTraceID()
	}
	spanID := runtime.SpanID
	if spanID == "" {
		spanID = newTraceID()
	}
	trace := Trace{
		TraceID: traceID, SpanID: spanID, RequestID: runtime.RequestID, Source: "mcpx", Tool: tool,
		StartedAtMs: timing.StartedAtMs, ReceivedAtMs: timing.ReceivedAtMs,
		CompletedAtMs: timing.CompletedAtMs, NetworkLatencyMs: timing.NetworkLatencyMs,
	}
	trace.Duration.ServerMs = timing.ServerElapsedMs
	return trace
}

func setMetadata(result *mcp.CallToolResult, envelope Envelope, resultType string) {
	trace := envelope.MCPX.Trace
	if result.Meta == nil {
		result.Meta = mcp.Meta{}
	}
	result.Meta["mcpx.version"] = Version
	result.Meta["mcpx.trace_id"] = trace.TraceID
	result.Meta["mcpx.span_id"] = trace.SpanID
	result.Meta["mcpx.request_id"] = trace.RequestID
	result.Meta["mcpx.result_type"] = resultType
	result.Meta["mcpx.server_timestamp_ms"] = trace.CompletedAtMs
	result.Meta["mcpx.network_latency_ms"] = trace.NetworkLatencyMs
	result.Meta["mcpx.tool_duration_ms"] = trace.Duration.ServerMs - trace.NetworkLatencyMs
	result.Meta["mcpx.processing_ms"] = trace.Duration.ServerMs - trace.NetworkLatencyMs
	result.Meta["mcpx.server_elapsed_ms"] = trace.Duration.ServerMs
	compact := envelope
	compact.MCPX.Result.Data = nil
	result.Meta[ResultMetadataKey] = compact
}

func extractResult(raw *mcp.CallToolResult) (map[string]any, string) {
	if data, ok := toMap(raw.StructuredContent); ok {
		data, _ = normalizePublicData(data).(map[string]any)
		return data, resultSummary(data, firstText(raw))
	}
	text := firstText(raw)
	if text == "" {
		return nil, ""
	}
	var data map[string]any
	if json.Unmarshal([]byte(text), &data) == nil {
		data, _ = normalizePublicData(data).(map[string]any)
		return data, resultSummary(data, text)
	}
	return map[string]any{"text": text}, text
}

// normalizePublicData preserves the clean-core business identifier names in
// the model-facing ARC payload. In particular, remote_session_id is the
// stable cross-client session key and must not be rewritten to session_id.
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

func classify(tool string, isError bool, data map[string]any, summary string) (string, any, Hints, []Action) {
	if data == nil {
		return "error", map[string]any{"message": summary}, Hints{PreferredBehavior: "summarize"}, nil
	}
	if isError || isErrorData(data) {
		behavior := "summarize"
		status, _ := data["status"].(string)
		if status == "waiting_confirmation" || status == "need_confirmation" {
			behavior = "ask_confirm"
		}
		errorResult := errorData(data)
		return "error", errorResult, Hints{PreferredBehavior: behavior}, actionsFrom(data)
	}

	inner := nestedData(data)
	if diagramType, diagramData, ok := detectDiagramResult(tool, inner, summary); ok {
		return diagramType, diagramData, Hints{PreferredBehavior: "show_directly"}, actionsFrom(data)
	}
	resultType := "text"
	switch {
	case (tool == "plan_manage" || tool == "plan_create" || tool == "plan_read" || tool == "plan_transition") && hasAnyKey(inner, "ready", "checks", "blockers"):
		resultType = "delivery"
	case (tool == "plan_manage" || tool == "plan_create" || tool == "plan_read" || tool == "plan_transition" || tool == "plan") && hasAnyKey(inner, "plan_task_id", "task"):
		resultType = "plan_task"
	case tool == "plan_manage" || tool == "plan_create" || tool == "plan_read" || tool == "plan_transition":
		resultType = "plan"
	case tool == "edit" && hasAnyKey(inner, "edit_id", "results", "diff_summary"):
		// Clean-core edit results use the code-change renderer.
		resultType = "code_change"
	case tool == "context_query" || tool == "source_read":
		resultType = "search_result"
	case hasAnyKey(inner, "files", "matches"):
		resultType = "search_result"
	case tool == "command_execute" || tool == "command_run" || tool == "task_manage" || tool == "task_read" || tool == "task_control" || hasAnyKey(inner, "stdout", "stderr", "exit_code"):
		resultType = "log"
	case hasAnyKey(inner, "columns", "rows"):
		resultType = "table"
	case hasAnyKey(inner, "tree", "entries"):
		resultType = "file_tree"
	}
	behavior := "show_directly"
	if completed, ok := inner["completed_in_call"].(bool); ok && !completed {
		behavior = "continue"
	}
	if data["status"] == "waiting_confirmation" {
		behavior = "ask_confirm"
	}
	return resultType, inner, Hints{PreferredBehavior: behavior}, actionsFrom(data)
}

func publicResultStatus(raw *mcp.CallToolResult, data map[string]any) string {
	if status, ok := data["status"].(string); ok {
		switch status {
		case "succeeded", "accepted", "waiting_confirmation", "interrupted", "failed":
			return status
		}
	}
	if raw != nil && (raw.IsError || isErrorData(data)) {
		return "failed"
	}
	return "succeeded"
}

func isErrorData(data map[string]any) bool {
	status, _ := data["status"].(string)
	return data["error"] != nil || status == "failed" || status == "waiting_confirmation" || status == "need_confirmation" || status == "need_secret" || status == "denied" || status == "unauthorized" || status == "error"
}

func errorData(data map[string]any) map[string]any {
	result := map[string]any{}
	if inner, ok := data["data"].(map[string]any); ok {
		for key, value := range inner {
			result[key] = value
		}
	}
	if value, ok := data["error"]; ok && value != nil {
		result["error"] = value
	}
	if status, ok := data["status"].(string); ok && status != "" {
		result["status"] = status
	}
	if len(result) == 0 {
		for key, value := range data {
			result[key] = value
		}
	}
	return result
}

func nestedData(data map[string]any) map[string]any {
	if inner, ok := data["data"].(map[string]any); ok {
		return inner
	}
	return data
}

func detectDiagramResult(tool string, inner map[string]any, summary string) (string, map[string]any, bool) {
	if tool != "context_query" || boolValue(inner, "truncated") {
		return "", nil, false
	}
	for _, candidate := range []string{stringValue(inner, "markdown"), stringValue(inner, "content"), stringValue(inner, "text"), summary} {
		blocks := extractMermaidBlocks(strings.TrimSpace(candidate))
		if len(blocks) == 0 {
			continue
		}
		if len(blocks) == 1 {
			result := map[string]any{"source": blocks[0], "mermaid": blocks[0]}
			if candidate != summary {
				result["markdown"] = candidate
			}
			return "diagram", result, true
		}
		diagrams := make([]map[string]any, 0, len(blocks))
		for index, source := range blocks {
			diagrams = append(diagrams, map[string]any{"id": fmt.Sprintf("diagram_%d", index+1), "source": source, "mermaid": source})
		}
		result := map[string]any{"diagrams": diagrams}
		if candidate != summary {
			result["markdown"] = candidate
		}
		return "diagram_collection", result, true
	}
	return "", nil, false
}

func stringValue(data map[string]any, key string) string {
	value, _ := data[key].(string)
	return value
}

func boolValue(data map[string]any, key string) bool {
	value, _ := data[key].(bool)
	return value
}

func actionsFrom(data map[string]any) []Action {
	if semanticConfirmation(data) {
		return nil
	}
	var candidates []map[string]any
	collect := func(source map[string]any) {
		if source == nil {
			return
		}
		for _, key := range []string{"next_action", "suggested_next", "recovery"} {
			if action, ok := source[key].(map[string]any); ok {
				candidates = append(candidates, action)
			}
		}
		if rawActions, ok := source["next_actions"].([]any); ok {
			for _, raw := range rawActions {
				if action, ok := raw.(map[string]any); ok {
					candidates = append(candidates, action)
				}
			}
		}
	}
	collect(data)
	if inner, ok := data["data"].(map[string]any); ok {
		collect(inner)
	}
	if rawError, ok := data["error"].(map[string]any); ok {
		collect(rawError)
		if details, ok := rawError["details"].(map[string]any); ok {
			collect(details)
		}
	}

	seen := map[string]bool{}
	actions := make([]Action, 0, len(candidates))
	for _, candidate := range candidates {
		action, ok := canonicalAction(candidate)
		if !ok {
			continue
		}
		encoded, _ := json.Marshal([]any{action.ID, action.Arguments})
		key := string(encoded)
		if seen[key] {
			continue
		}
		seen[key] = true
		if len(actions) > 0 {
			action.ID = fmt.Sprintf("%s_%d", action.ID, len(actions)+1)
		}
		actions = append(actions, action)
	}
	return actions
}

func semanticConfirmation(data map[string]any) bool {
	status, _ := data["status"].(string)
	if status == "waiting_confirmation" || status == "need_confirmation" {
		return true
	}
	if inner, ok := data["data"].(map[string]any); ok {
		confirmationRequired, _ := inner["confirmation_required"].(bool)
		return confirmationRequired
	}
	return false
}

func canonicalAction(candidate map[string]any) (Action, bool) {
	tool, _ := candidate["tool"].(string)
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return Action{}, false
	}
	args, _ := candidate["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
		for key, value := range candidate {
			switch key {
			case "tool", "reason", "note", "label", "type", "confirm", "id":
				continue
			case "action":
				if fmt.Sprint(value) == tool {
					continue
				}
			}
			args[key] = value
		}
	}
	label := "Continue with " + tool
	if reason, _ := candidate["reason"].(string); strings.TrimSpace(reason) != "" {
		label = reason
	}
	return Action{ID: tool, Type: "continue", Label: label, Confirm: false, Arguments: args}, true
}

func hasAnyKey(data map[string]any, keys ...string) bool {
	for _, key := range keys {
		if _, ok := data[key]; ok {
			return true
		}
	}
	return false
}

func resultSummary(data map[string]any, fallback string) string {
	if summary, ok := data["summary"].(string); ok && strings.TrimSpace(summary) != "" {
		return summary
	}
	if inner, ok := data["data"].(map[string]any); ok {
		if summary, ok := inner["summary"].(string); ok && strings.TrimSpace(summary) != "" {
			return summary
		}
		if message, ok := inner["message"].(string); ok && strings.TrimSpace(message) != "" {
			return message
		}
	}
	if message, ok := data["message"].(string); ok && message != "" {
		return message
	}
	if errData, ok := data["error"].(map[string]any); ok {
		if message, ok := errData["message"].(string); ok && message != "" {
			return message
		}
	}
	status, _ := data["status"].(string)
	if status == "failed" || status == "waiting_confirmation" {
		return strings.ReplaceAll(status, "_", " ")
	}
	if strings.TrimSpace(fallback) != "" {
		var encoded any
		if json.Unmarshal([]byte(fallback), &encoded) == nil {
			// Envelope.Response and legacy structured payloads must never leak
			// into host-visible text as raw JSON. RenderToolContent receives the
			// decoded data and will provide the useful Markdown representation.
			if status != "" && status != "succeeded" {
				return strings.ReplaceAll(status, "_", " ")
			}
			return "succeeded"
		}
		return fallback
	}
	if status != "" && status != "succeeded" {
		return strings.ReplaceAll(status, "_", " ")
	}
	return ""
}

func toMap(value any) (map[string]any, bool) {
	if data, ok := value.(map[string]any); ok {
		return data, true
	}
	if value == nil {
		return nil, false
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var data map[string]any
	if json.Unmarshal(encoded, &data) != nil {
		return nil, false
	}
	return data, true
}

func firstText(result *mcp.CallToolResult) string {
	return mcpresult.FirstText(result)
}

func newTraceID() string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return "tr_" + time.Now().UTC().Format("20060102") + "_" + hex.EncodeToString(raw[:])
}

func schemaForType(resultType string) string {
	switch resultType {
	case "markdown":
		return SchemaMarkdown
	case "search_result":
		return SchemaSearchResult
	case "code_change":
		return SchemaCodeChange
	case "table":
		return SchemaTable
	case "file_tree":
		return SchemaFileTree
	case "log":
		return SchemaLog
	case "error":
		return SchemaError
	case "diagram":
		return SchemaDiagram
	case "diagram_collection":
		return SchemaDiagramCollection
	case "plan":
		return SchemaPlan
	case "plan_task":
		return SchemaPlanTask
	case "delivery":
		return SchemaDelivery
	default:
		return SchemaText
	}
}

var publicSchemaNames = []string{
	SchemaText, SchemaMarkdown, SchemaSearchResult, SchemaCodeChange, SchemaTable,
	SchemaFileTree, SchemaLog, SchemaError, SchemaDiagram, SchemaDiagramCollection,
	SchemaPlan, SchemaPlanTask, SchemaDelivery,
}

var schemaRegistryCache struct {
	sync.Once
	value map[string]json.RawMessage
}

// SchemaRegistry returns a copy of the public ARC result schemas.
func SchemaRegistry() map[string]json.RawMessage {
	schemaRegistryCache.Do(func() {
		registry := make(map[string]json.RawMessage, len(publicSchemaNames))
		for _, name := range publicSchemaNames {
			registry[name] = resultSchema(name)
		}
		schemaRegistryCache.value = registry
	})
	registry := make(map[string]json.RawMessage, len(schemaRegistryCache.value))
	for name, schema := range schemaRegistryCache.value {
		registry[name] = append(json.RawMessage(nil), schema...)
	}
	return registry
}

func resultSchema(name string) json.RawMessage {
	schema := map[string]any{
		"$id": name, "type": "object", "additionalProperties": true,
		"properties": map[string]any{"data": resultDataSchema(name)},
	}
	encoded, _ := json.Marshal(schema)
	return encoded
}

func resultDataSchema(name string) map[string]any {
	object := func(properties map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "additionalProperties": true}
	}
	stringArray := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	switch name {
	case SchemaSearchResult:
		return object(map[string]any{
			"query": map[string]any{"type": "string"}, "mode": map[string]any{"type": "string"},
			"analysis": map[string]any{"type": "object"}, "files": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			"matches": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}, "total_bytes": map[string]any{"type": "integer"},
			"truncated": map[string]any{"type": "boolean"}, "next_cursor": map[string]any{"type": "string"},
		})
	case SchemaCodeChange:
		return object(map[string]any{
			"edit_id": map[string]any{"type": "string"}, "status": map[string]any{"type": "string"},
			"results": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
				"path": map[string]any{"type": "string"}, "new_path": map[string]any{"type": "string"},
				"operation": map[string]any{"type": "string"}, "diff": map[string]any{"type": "string"},
				"diff_truncated": map[string]any{"type": "boolean"}, "new_sha256": map[string]any{"type": "string"},
			}}},
			"total_changed_lines": map[string]any{"type": "integer"},
			"diff_truncated":      map[string]any{"type": "boolean"}, "applied": map[string]any{"type": "boolean"},
			"preview_only": map[string]any{"type": "boolean"}, "idempotent_replay": map[string]any{"type": "boolean"},
		})
	case SchemaTable:
		return object(map[string]any{
			"columns": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			"rows":    map[string]any{"type": "array", "items": map[string]any{}},
		})
	case SchemaFileTree:
		return object(map[string]any{"tree": map[string]any{"type": "object"}, "entries": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}})
	case SchemaLog:
		return object(map[string]any{
			"execution_task_id": map[string]any{"type": "string"}, "status": map[string]any{"type": "string"},
			"stdout": map[string]any{"type": "string"}, "stderr": map[string]any{"type": "string"},
			"stdout_next_offset": map[string]any{"type": "integer"}, "stderr_next_offset": map[string]any{"type": "integer"},
		})
	case SchemaError:
		return object(map[string]any{
			"status": map[string]any{"type": "string"}, "error": map[string]any{"type": "object"},
			"code": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"},
		})
	case SchemaText:
		return object(map[string]any{"text": map[string]any{"type": "string"}})
	case SchemaMarkdown:
		return object(map[string]any{
			"markdown": map[string]any{"type": "string"}, "text": map[string]any{"type": "string"},
		})
	case SchemaDiagram:
		return object(map[string]any{
			"source": map[string]any{"type": "string"}, "mermaid": map[string]any{"type": "string"},
			"title": map[string]any{"type": "string"},
		})
	case SchemaDiagramCollection:
		return object(map[string]any{
			"diagrams": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		})
	case SchemaPlan:
		return object(map[string]any{
			"plan_id": map[string]any{"type": "string"}, "summary": map[string]any{"type": "string"},
			"status": map[string]any{"type": "string"}, "tasks": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			"progress": map[string]any{"type": "object"},
		})
	case SchemaPlanTask:
		return object(map[string]any{
			"plan_id": map[string]any{"type": "string"}, "plan_task_id": map[string]any{"type": "string"},
			"status": map[string]any{"type": "string"}, "task": map[string]any{"type": "object"},
			"evidence": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		})
	case SchemaDelivery:
		return object(map[string]any{
			"plan_id": map[string]any{"type": "string"}, "status": map[string]any{"type": "string"},
			"summary": map[string]any{"type": "string"}, "checks": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		})
	default:
		return object(map[string]any{"values": stringArray})
	}
}

var outputSchemaCache struct {
	sync.Once
	value json.RawMessage
}

func OutputSchema() json.RawMessage {
	outputSchemaCache.Do(func() {
		outputSchemaCache.value = buildOutputSchema()
	})
	return append(json.RawMessage(nil), outputSchemaCache.value...)
}

func buildOutputSchema() json.RawMessage {
	resultTypes := []string{
		"text", "markdown", "search_result", "code_change", "table", "file_tree", "log", "error",
		"diagram", "diagram_collection", "plan", "plan_task", "delivery",
	}
	typeValues := make([]any, 0, len(resultTypes))
	for _, resultType := range resultTypes {
		typeValues = append(typeValues, resultType)
	}
	schema := map[string]any{
		// This schema is repeated once per tool in tools/list. Keep the shared
		// result contract explicit while leaving tool-specific data open; the
		// result type and the actual data fields are the stable discriminator.
		"$id": "mcpx.structured_content.v" + Version, "type": "object",
		"required":             []string{"status", "type", "context", "data"},
		"additionalProperties": false,
		"properties": map[string]any{
			"status": map[string]any{"type": "string", "enum": []string{"succeeded", "accepted", "waiting_confirmation", "interrupted", "failed"}},
			"type":   map[string]any{"type": "string", "enum": typeValues},
			"context": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"purpose": map[string]any{"type": "string"},
					"plan_id": map[string]any{"type": "string"}, "plan_task_id": map[string]any{"type": "string"},
					"execution_task_id": map[string]any{"type": "string"}, "operation_id": map[string]any{"type": "string"},
				},
			},
			"data":    map[string]any{"type": "object", "additionalProperties": true},
			"error":   map[string]any{"type": "object", "additionalProperties": true},
			"hints":   map[string]any{"type": "object"},
			"actions": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		},
	}
	encoded, _ := json.Marshal(schema)
	return encoded
}
