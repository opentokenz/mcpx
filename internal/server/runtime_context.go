package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/auth"
	"mcpx/internal/envelope"
	"mcpx/internal/remotesession"
)

const (
	requestIDHeader           = "X-Request-ID"
	mcpxRequestIDHeader       = "X-MCPX-Request-ID"
	traceparentHeader         = "Traceparent"
	mcpxTraceIDHeader         = "X-MCPX-Trace-ID"
	mcpxSpanIDHeader          = "X-MCPX-Span-ID"
	mcpxStartedAtHeader       = "X-MCPX-Started-At-Ms"
	mcpTransportSessionHeader = "Mcp-Session-Id"
)

type runtimeContextKey struct{}
type toolInvocationNameKey struct{}
type operationChildKey struct{}
type cleanCoreRequestKey struct{}

var opaqueIDFallbackCounter atomic.Uint64

// RuntimeContext is server-owned lifecycle metadata. Identity and trace fields
// come from the Gateway; instrumentTool overrides StartedAtMs from the required
// tool argument before invoking business handlers.
type RuntimeContext struct {
	RequestID          string
	OperationID        string
	ParentOperationID  string
	StepID             string
	TraceID            string
	SpanID             string
	ParentSpanID       string
	StartedAtMs        int64
	ReceivedAtMs       int64
	CompletedAtMs      int64
	NetworkLatency     int64
	ProcessingMs       int64
	ServerElapsed      int64
	ClientName         string
	ClientVersion      string
	TransportSessionID string
}

func withRuntimeContext(ctx context.Context, value RuntimeContext) context.Context {
	return context.WithValue(ctx, runtimeContextKey{}, value)
}

func runtimeContextFrom(ctx context.Context) (RuntimeContext, bool) {
	value, ok := ctx.Value(runtimeContextKey{}).(RuntimeContext)
	return value, ok && value.RequestID != ""
}

func ensureRuntimeContext(ctx context.Context, headers http.Header, received time.Time) (context.Context, RuntimeContext) {
	if current, ok := runtimeContextFrom(ctx); ok {
		current.ReceivedAtMs = received.UnixMilli()
		if current.TransportSessionID == "" {
			current.TransportSessionID = firstHeader(headers, mcpTransportSessionHeader)
		}
		return withRuntimeContext(ctx, current), current
	}
	current := runtimeContextFromHeaders(headers, received)
	return withRuntimeContext(ctx, current), current
}

func runtimeContextFromHeaders(headers http.Header, received time.Time) RuntimeContext {
	receivedAtMs := received.UnixMilli()
	startedAtMs := receivedAtMs
	if value, err := strconv.ParseInt(firstHeader(headers, mcpxStartedAtHeader), 10, 64); err == nil && value > 0 && value <= receivedAtMs {
		startedAtMs = value
	}
	requestID := firstHeader(headers, requestIDHeader, mcpxRequestIDHeader)
	if requestID == "" {
		requestID = envelope.EnsureRequestID("")
	}
	traceID, parentSpanID := traceFromHeaders(headers)
	if traceID == "" {
		traceID = newRuntimeID("tr", 16)
	}
	return RuntimeContext{
		RequestID: requestID, TraceID: traceID, SpanID: newRuntimeID("sp", 8), ParentSpanID: parentSpanID,
		StartedAtMs: startedAtMs, ReceivedAtMs: receivedAtMs,
		TransportSessionID: firstHeader(headers, mcpTransportSessionHeader),
	}
}

func firstHeader(headers http.Header, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
		for key, values := range headers {
			if !strings.EqualFold(key, name) || len(values) == 0 {
				continue
			}
			if value := strings.TrimSpace(values[0]); value != "" {
				return value
			}
		}
	}
	return ""
}

func traceFromHeaders(headers http.Header) (traceID, parentSpanID string) {
	traceID = firstHeader(headers, mcpxTraceIDHeader)
	traceparent := strings.Split(firstHeader(headers, traceparentHeader), "-")
	if len(traceparent) == 4 && len(traceparent[1]) == 32 && len(traceparent[2]) == 16 {
		if _, err := hex.DecodeString(traceparent[1]); err == nil {
			if _, err := hex.DecodeString(traceparent[2]); err == nil {
				if traceID == "" {
					traceID = traceparent[1]
				}
				parentSpanID = traceparent[2]
			}
		}
	}
	if parentSpanID == "" {
		parentSpanID = firstHeader(headers, mcpxSpanIDHeader)
	}
	return traceID, parentSpanID
}

func newRuntimeID(prefix string, bytes int) string {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return prefix + "_unavailable"
	}
	return prefix + "_" + hex.EncodeToString(raw)
}

func newOpaqueID(prefix string, bytes int) string {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err == nil {
		return prefix + "_" + base64.RawURLEncoding.EncodeToString(raw)
	}
	fallback := make([]byte, 16)
	binary.BigEndian.PutUint64(fallback[:8], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint64(fallback[8:], opaqueIDFallbackCounter.Add(1))
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(fallback)
}

func runtimeContextWithClient(value RuntimeContext, name, version string) RuntimeContext {
	value.ClientName = name
	value.ClientVersion = version
	return value
}

func runtimeContextWithTiming(value RuntimeContext, timing interactionTiming) RuntimeContext {
	value.StartedAtMs = timing.StartedAtMs
	value.ReceivedAtMs = timing.ReceivedAtMs
	value.CompletedAtMs = timing.CompletedAtMs
	value.NetworkLatency = timing.NetworkLatencyMs
	value.ProcessingMs = timing.ProcessingMs
	value.ServerElapsed = timing.ServerElapsedMs
	return value
}

func withToolInvocationName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, toolInvocationNameKey{}, name)
}

func toolInvocationName(ctx context.Context) string {
	value, _ := ctx.Value(toolInvocationNameKey{}).(string)
	return strings.TrimSpace(value)
}

func withCleanCoreRequest(ctx context.Context) context.Context {
	return context.WithValue(ctx, cleanCoreRequestKey{}, true)
}

func isCleanCoreRequest(ctx context.Context) bool {
	value, _ := ctx.Value(cleanCoreRequestKey{}).(bool)
	return value
}

func withOperationChild(ctx context.Context) context.Context {
	return context.WithValue(ctx, operationChildKey{}, true)
}

func isOperationChild(ctx context.Context) bool {
	value, _ := ctx.Value(operationChildKey{}).(bool)
	return value
}

func remoteSessionBindingKey(ctx context.Context, principal auth.Principal) string {
	runtime, ok := runtimeContextFrom(ctx)
	if !ok || strings.TrimSpace(runtime.TransportSessionID) == "" || strings.TrimSpace(principal.ID) == "" {
		return ""
	}
	return principal.ID + "\x00" + strings.TrimSpace(runtime.TransportSessionID)
}

func (r *Runtime) boundRemoteSessionID(ctx context.Context, principal auth.Principal) string {
	if r == nil {
		return ""
	}
	key := remoteSessionBindingKey(ctx, principal)
	if key == "" {
		return ""
	}
	r.sessionBindingMu.RLock()
	defer r.sessionBindingMu.RUnlock()
	return strings.TrimSpace(r.sessionBindings[key])
}

func (r *Runtime) bindRemoteSession(ctx context.Context, principal auth.Principal, remoteSessionID string) bool {
	if r == nil {
		return false
	}
	key := remoteSessionBindingKey(ctx, principal)
	remoteSessionID = strings.TrimSpace(remoteSessionID)
	if key == "" || remoteSessionID == "" {
		return false
	}
	r.sessionBindingMu.Lock()
	defer r.sessionBindingMu.Unlock()
	if r.sessionBindings == nil {
		r.sessionBindings = map[string]string{}
	}
	r.sessionBindings[key] = remoteSessionID
	return true
}

func (r *Runtime) unbindRemoteSession(ctx context.Context, principal auth.Principal, remoteSessionID string) {
	if r == nil {
		return
	}
	key := remoteSessionBindingKey(ctx, principal)
	if key == "" {
		return
	}
	r.sessionBindingMu.Lock()
	defer r.sessionBindingMu.Unlock()
	if strings.TrimSpace(remoteSessionID) == "" || r.sessionBindings[key] == strings.TrimSpace(remoteSessionID) {
		delete(r.sessionBindings, key)
	}
}

// changeRequest resolves the authenticated remote session for tools that need
// workspace-scoped access. The edit flag requires an explicit purpose and an
// owner/editor role; read-only callers can pass false.
func (r *Runtime) changeRequest(ctx context.Context, req *mcp.CallToolRequest, edit bool) (envelope.Request, auth.Principal, remotesession.Session, *mcp.CallToolResult) {
	envReq, principal, fail := r.remoteRequest(ctx, req)
	if fail != nil {
		return envReq, principal, remotesession.Session{}, fail
	}
	if edit {
		if err := validatePurpose(envReq.Intent); err != nil {
			response := envelope.Fail(envelope.StatusError, envReq.RequestID, envReq.Workspace, nil, "PURPOSE_REQUIRED", err.Error())
			result, _ := r.resultJSON(response)
			return envReq, principal, remotesession.Session{}, result
		}
	}
	remoteSessionID, err := requireRemoteSessionID(envReq)
	if err != nil {
		result, _ := r.remoteError(envReq, "", "", err)
		return envReq, principal, remotesession.Session{}, result
	}
	session, err := r.remote.Get(ctx, principal, remoteSessionID)
	if err != nil {
		result, _ := r.remoteError(envReq, remoteSessionID, "", err)
		return envReq, principal, remotesession.Session{}, result
	}
	if edit && session.Role != "owner" && session.Role != "editor" {
		result, _ := r.remoteError(envReq, remoteSessionID, session.WorkspaceName, remotesession.ErrForbidden)
		return envReq, principal, remotesession.Session{}, result
	}
	return envReq, principal, session, nil
}
