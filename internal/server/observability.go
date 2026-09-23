package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"

	"mcpx/internal/arc"
	"mcpx/internal/envelope"
	"mcpx/internal/logging"
)

func (r *Runtime) addTool(s *mcp.Server, tool mcp.Tool, handler mcp.ToolHandler) {
	tool = withEmbeddedActivitySchema(tool)
	// OutputSchema describes structuredContent, not the larger ARC metadata
	// envelope.
	if output := outputSchemaForTool(tool.Name); len(output) > 0 {
		tool.OutputSchema = output
	} else {
		tool.OutputSchema = nil
	}
	instrumented := r.instrumentTool(tool.Name, handler)
	if r.toolHandlers == nil {
		r.toolHandlers = map[string]mcp.ToolHandler{}
	}
	if r.toolMeta == nil {
		r.toolMeta = map[string]toolAnnotation{}
	}
	if r.toolIndex == nil {
		r.toolIndex = map[string]mcp.Tool{}
	}
	ann := toolAnnotation{}
	if tool.Annotations != nil {
		ann = toolAnnotation{
			ReadOnly:    tool.Annotations.ReadOnlyHint,
			Destructive: boolPointerValue(tool.Annotations.DestructiveHint),
			Idempotent:  tool.Annotations.IdempotentHint,
			OpenWorld:   boolPointerValue(tool.Annotations.OpenWorldHint),
		}
	}
	r.toolHandlers[tool.Name] = instrumented
	r.toolMeta[tool.Name] = ann
	r.toolIndexMu.Lock()
	r.toolIndex[tool.Name] = tool
	r.toolIndexMu.Unlock()
	tt := tool
	s.AddTool(&tt, instrumented)
}

func outputSchemaForTool(toolName string) json.RawMessage {
	if toolName == "mcp_tool" {
		return nil
	}
	limits, hasLimits := publishedLimits()[toolName]
	base := arc.OutputSchema()
	if !hasLimits {
		return base
	}
	var schema map[string]any
	if err := json.Unmarshal(base, &schema); err != nil || schema == nil {
		return base
	}
	schema["x-mcpx-limits"] = limits
	encoded, err := json.Marshal(schema)
	if err != nil {
		return base
	}
	return json.RawMessage(encoded)
}

func boolPointerValue(value *bool) bool {
	return value != nil && *value
}

type interactionTiming struct {
	StartedAtMs      int64
	ReceivedAtMs     int64
	CompletedAtMs    int64
	NetworkLatencyMs int64
	ProcessingMs     int64
	ServerElapsedMs  int64
}

const (
	clientStartedAtMetaKey      = "mcpx/started_at_ms"
	progressNotificationTimeout = 2 * time.Second
)

var toolProgressHeartbeatInterval = 25 * time.Second

type progressPulseContextKey struct{}

type progressPulse struct {
	reset chan struct{}
}

func withProgressPulse(ctx context.Context) (context.Context, *progressPulse) {
	pulse := &progressPulse{reset: make(chan struct{}, 1)}
	return context.WithValue(ctx, progressPulseContextKey{}, pulse), pulse
}

func signalProgressPulse(ctx context.Context) {
	pulse, _ := ctx.Value(progressPulseContextKey{}).(*progressPulse)
	if pulse == nil {
		return
	}
	select {
	case pulse.reset <- struct{}{}:
	default:
	}
}

func (r *Runtime) instrumentTool(name string, handler mcp.ToolHandler) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (result *mcp.CallToolResult, err error) {
		// Keep the entire instrumentation boundary defensive. Handler calls use
		// callToolSafely below so normal panics retain ARC wrapping; this outer
		// guard also covers malformed observation metadata or renderer changes.
		defer func() {
			if recovered := recover(); recovered != nil {
				logging.With("component", "mcp_tool").Error("instrumentation panic recovered", "tool", name, "panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
				result = mcpresult.NewError("EXECUTION_RUNTIME_ERROR: tool execution failed")
				err = nil
			}
		}()
		received := time.Now()
		callCtx, runtime := ensureRuntimeContext(ctx, mcpresult.Header(req), received)
		runtime.StartedAtMs = toolRequestStartedAtMs(req, received)
		clientName, clientVersion := clientInfoFromContext(callCtx)
		if clientName != "" && clientName != "unknown" {
			runtime = runtimeContextWithClient(runtime, clientName, clientVersion)
		}
		callCtx = withRuntimeContext(callCtx, runtime)
		callCtx = withToolInvocationName(callCtx, name)
		callCtx, progressPulse := withProgressPulse(callCtx)
		stopProgressHeartbeat := startToolProgressHeartbeat(callCtx, req, name, received, progressPulse)
		defer stopProgressHeartbeat()
		if isCleanPublicTool(name) {
			callCtx = withCleanCoreRequest(callCtx)
		}
		internalOperationStep := isOperationChild(callCtx)
		observationRequest, observationParseErr := r.parseEnv(callCtx, req)
		if observationParseErr == nil && name != "session" && strings.TrimSpace(observationRequest.RemoteSessionID) == "" {
			if principal, principalErr := r.principalFromContext(callCtx); principalErr == nil {
				observationRequest.RemoteSessionID = r.boundRemoteSessionID(callCtx, principal)
			}
		}
		if observationParseErr == nil {
			r.touchRemoteSessionActivity(callCtx, observationRequest)
		}
		arguments := mcpresult.Arguments(req)
		observedArguments := observationArguments(name, arguments)
		var embeddedActivityErr error
		if !internalOperationStep && observationParseErr == nil {
			embeddedActivityErr = r.recordEmbeddedAgentActivity(callCtx, observationRequest, runtime, received.UTC())
		}
		if !internalOperationStep && observationParseErr == nil && embeddedActivityErr == nil && r.observation != nil {
			// Activity is recorded synchronously before this async lifecycle event,
			// preserving semantic -> tool ordering in the observer stream. Ephemeral
			// scripts are hashed/redacted before reaching the durable store.
			_ = r.observation.RecordToolStarted(callCtx, name, observationRequest, observedArguments)
		}

		if embeddedActivityErr != nil {
			result = mcpresult.NewError("INVALID_ACTIVITY: " + embeddedActivityErr.Error())
			err = nil
		} else if !isOperationChild(callCtx) && r.operations != nil && asyncEligibleTool(name) && executionMode(req) == "async" && !isEphemeralRuntimeArguments(arguments) && observationParseErr == nil {
			result, err = callToolSafely(name, func() (*mcp.CallToolResult, error) {
				return r.submitAsyncTool(callCtx, name, req, observationRequest)
			})
		} else {
			result, err = callToolSafely(name, func() (*mcp.CallToolResult, error) {
				return handler(callCtx, req)
			})
		}
		completed := time.Now()
		timing := makeInteractionTiming(runtime.StartedAtMs, received, completed)
		runtime = runtimeContextWithTiming(runtime, timing)
		status := "ok"
		if err != nil || result == nil || result.IsError {
			status = "error"
		}
		if err != nil {
			if result == nil {
				result = mcpresult.NewError(err.Error())
			} else {
				result.IsError = true
			}
		}
		// When this MCP transport already carries the Remote Session binding, keep
		// the session identifier out of model-facing business data and continuation
		// arguments. Explicit remote_session_id calls keep their original payload.
		if name != "session" && runtime.TransportSessionID != "" {
			if _, explicit := arguments["remote_session_id"]; !explicit && result != nil {
				stripModelRemoteSessionIDs(result.StructuredContent)
			}
		}
		// Wrap first so host-visible content is the human summary; observation
		// then snapshots that text only (never full structuredContent dump).
		// ARC V2 semantic narration comes only from the durable Activity channel;
		// legacy request/result bookkeeping cannot synthesize Activity.
		if !transparentMCPToolResult(name, req, result) {
			activity := r.arcActivityContext(callCtx, observationRequest.RemoteSessionID)
			result = arc.WrapToolResult(name, arc.ResultContext{
				RequestID: runtime.RequestID, TraceID: runtime.TraceID, SpanID: runtime.SpanID,
				Context: arc.Context{
					Purpose: firstSemanticPurpose(observationRequest), Activity: activity,
					PlanID: observationRequest.PlanID, PlanTaskID: observationRequest.PlanTaskID, ExecutionTaskID: observationRequest.ExecutionTaskID,
				},
				Timing: arc.Timing{
					StartedAtMs: timing.StartedAtMs, ReceivedAtMs: timing.ReceivedAtMs,
					CompletedAtMs: timing.CompletedAtMs, NetworkLatencyMs: timing.NetworkLatencyMs,
					ProcessingMs: timing.ProcessingMs, ServerElapsedMs: timing.ServerElapsedMs,
				},
			}, result)
		}
		if !internalOperationStep && observationParseErr == nil && r.observation != nil {
			_ = r.observation.RecordToolCompleted(callCtx, name, observationRequest, observedArguments, result, err, timing)
		}
		if !internalOperationStep {
			logToolCall(name, runtime, status, timing)
		}
		return result, err
	}
}

func stripModelRemoteSessionIDs(value any) {
	switch typed := value.(type) {
	case map[string]any:
		delete(typed, "remote_session_id")
		for _, child := range typed {
			stripModelRemoteSessionIDs(child)
		}
	case []any:
		for _, child := range typed {
			stripModelRemoteSessionIDs(child)
		}
	case []map[string]any:
		for _, child := range typed {
			stripModelRemoteSessionIDs(child)
		}
	}
}

func transparentMCPToolResult(name string, req *mcp.CallToolRequest, result *mcp.CallToolResult) bool {
	if name != "mcp_tool" || toolAction(req) != "call" || result == nil || result.Meta == nil {
		return false
	}
	serverName, _ := result.Meta[mcpMetaServer].(string)
	toolName, _ := result.Meta[mcpMetaTool].(string)
	return strings.TrimSpace(serverName) != "" && strings.TrimSpace(toolName) != ""
}

func firstSemanticPurpose(req envelope.Request) string {
	if strings.TrimSpace(req.Purpose) != "" {
		return req.Purpose
	}
	return req.Intent
}

func (r *Runtime) arcActivityContext(ctx context.Context, remoteSessionID string) *arc.Activity {
	state, err := r.currentAgentActivity(ctx, remoteSessionID)
	if err != nil {
		logging.With("component", "arc").Error("activity snapshot unavailable", "remote_session_id", remoteSessionID, "error", err)
		return nil
	}
	if state == nil {
		return nil
	}
	return &arc.Activity{
		TurnID: state.TurnID, Sequence: state.Sequence, State: state.State, Kind: state.Kind,
		Summary: state.Summary, RelatedCallID: state.RelatedCallID,
	}
}

// callToolSafely keeps a handler panic inside the MCP tool error contract. A
// malformed request, task race, or future handler regression must not take
// down the shared MCP server process or its other sessions.
func callToolSafely(name string, call func() (*mcp.CallToolResult, error)) (result *mcp.CallToolResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logging.With("component", "mcp_tool").Error("panic recovered", "tool", name, "panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
			result = mcpresult.NewError("EXECUTION_RUNTIME_ERROR: tool execution failed")
			err = nil
		}
	}()
	return call()
}

func toolRequestStartedAtMs(req *mcp.CallToolRequest, received time.Time) int64 {
	if req != nil && req.Params != nil && req.Params.Meta != nil {
		if started, ok := positiveInt64(req.Params.Meta[clientStartedAtMetaKey]); ok {
			return started
		}
	}
	return received.UnixMilli()
}

func positiveInt64(value any) (int64, bool) {
	switch number := value.(type) {
	case int:
		if number > 0 {
			return int64(number), true
		}
	case int64:
		if number > 0 {
			return number, true
		}
	case float64:
		if number > 0 {
			return int64(number), true
		}
	case json.Number:
		if parsed, err := number.Int64(); err == nil && parsed > 0 {
			return parsed, true
		}
	}
	return 0, false
}

func startToolProgressHeartbeat(ctx context.Context, req *mcp.CallToolRequest, name string, started time.Time, pulse *progressPulse) func() {
	if toolProgressHeartbeatInterval <= 0 || req == nil || req.Params == nil || req.Session == nil || req.Params.GetProgressToken() == nil || pulse == nil {
		return func() {}
	}
	heartbeatCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(toolProgressHeartbeatInterval)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				elapsed := time.Since(started)
				notifyRequestProgress(heartbeatCtx, req,
					fmt.Sprintf("MCPX %s is still running; elapsed %ds", name, int(elapsed.Seconds())),
					elapsed.Seconds(), 0,
				)
				timer.Reset(toolProgressHeartbeatInterval)
			case <-pulse.reset:
				resetToolProgressTimer(timer, toolProgressHeartbeatInterval)
			case <-heartbeatCtx.Done():
				return
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func notifyRequestProgress(ctx context.Context, req *mcp.CallToolRequest, message string, progress, total float64) bool {
	if req == nil || req.Params == nil || req.Session == nil {
		return false
	}
	token := req.Params.GetProgressToken()
	if token == nil {
		return false
	}
	notifyCtx, cancel := context.WithTimeout(ctx, progressNotificationTimeout)
	defer cancel()
	if err := req.Session.NotifyProgress(notifyCtx, &mcp.ProgressNotificationParams{
		ProgressToken: token,
		Progress:      progress,
		Total:         total,
		Message:       strings.TrimSpace(message),
	}); err != nil {
		return false
	}
	signalProgressPulse(ctx)
	return true
}

func resetToolProgressTimer(timer *time.Timer, interval time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(interval)
}

func makeInteractionTiming(startedAtMs int64, received, completed time.Time) interactionTiming {
	receivedAtMs := received.UnixMilli()
	completedAtMs := completed.UnixMilli()
	networkLatencyMs := receivedAtMs - startedAtMs
	if networkLatencyMs < 0 {
		networkLatencyMs = 0
	}
	processingMs := completedAtMs - receivedAtMs
	if processingMs < 0 {
		processingMs = 0
	}
	return interactionTiming{
		StartedAtMs: startedAtMs, ReceivedAtMs: receivedAtMs, CompletedAtMs: completedAtMs,
		NetworkLatencyMs: networkLatencyMs,
		ProcessingMs:     processingMs,
		ServerElapsedMs:  networkLatencyMs + processingMs,
	}
}

func logToolCall(name string, runtime RuntimeContext, status string, timing interactionTiming) {
	fields := []any{
		"tool", name, "status", status,
		"request_id", runtime.RequestID, "trace_id", runtime.TraceID, "span_id", runtime.SpanID,
		"started_at_ms", timing.StartedAtMs, "received_at_ms", timing.ReceivedAtMs,
		"completed_at_ms", timing.CompletedAtMs, "network_latency_ms", timing.NetworkLatencyMs,
		"processing_ms", timing.ProcessingMs, "server_elapsed_ms", timing.ServerElapsedMs,
	}
	if runtime.ClientName != "" {
		fields = append(fields, "client_name", runtime.ClientName, "client_version", runtime.ClientVersion)
	}
	logging.With("component", "mcp_tool").Info("call", fields...)
}

type accessLogResponseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (w *accessLogResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *accessLogResponseWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += n
	return n, err
}

func (w *accessLogResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *accessLogResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (g *Gateway) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestContext, runtime := ensureRuntimeContext(r.Context(), r.Header, started)
		r = r.WithContext(requestContext)
		w.Header().Set("X-Request-ID", runtime.RequestID)
		w.Header().Set("X-MCPX-Trace-ID", runtime.TraceID)
		w.Header().Set("X-MCPX-Span-ID", runtime.SpanID)
		w.Header().Add("Trailer", "Server-Timing")
		w.Header().Add("Trailer", "X-MCPX-Processing-Ms")
		logged := &accessLogResponseWriter{ResponseWriter: w}
		next.ServeHTTP(logged, r)
		status := logged.status
		if status == 0 {
			status = http.StatusOK
		}
		processingMs := time.Since(started).Milliseconds()
		logged.Header().Set("Server-Timing", fmt.Sprintf("mcpx;dur=%d", processingMs))
		logged.Header().Set("X-MCPX-Processing-Ms", strconv.FormatInt(processingMs, 10))
		logging.With("component", "mcp_http").Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"request_id", runtime.RequestID,
			"trace_id", runtime.TraceID,
			"span_id", runtime.SpanID,
			"duration_ms", processingMs,
			"response_bytes", logged.bytes,
			"mcp_session_id", r.Header.Get("Mcp-Session-Id"),
			"remote_addr", r.RemoteAddr,
		)
	})
}
