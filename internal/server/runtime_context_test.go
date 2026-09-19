package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"
)

func TestRuntimeContextComesFromTransportHeaders(t *testing.T) {
	now := time.UnixMilli(1_785_486_000_100)
	headers := http.Header{
		"X-Request-ID":         []string{"req_header"},
		"Traceparent":          []string{"00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"},
		"X-MCPX-Started-At-Ms": []string{"1785486000000"},
		"Mcp-Session-Id":       []string{"mcp-transport-1"},
	}
	ctx, runtime := ensureRuntimeContext(context.Background(), headers, now)
	if runtime.RequestID != "req_header" || runtime.TraceID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("runtime identity = %+v", runtime)
	}
	if runtime.ParentSpanID != "0123456789abcdef" || runtime.SpanID == runtime.ParentSpanID || runtime.SpanID == "" {
		t.Fatalf("span context = %+v", runtime)
	}
	if runtime.StartedAtMs != 1785486000000 || runtime.ReceivedAtMs != now.UnixMilli() {
		t.Fatalf("runtime timing = %+v", runtime)
	}
	if runtime.TransportSessionID != "mcp-transport-1" {
		t.Fatalf("transport session = %+v", runtime)
	}
	if got, ok := runtimeContextFrom(ctx); !ok || got.RequestID != runtime.RequestID {
		t.Fatalf("context value = %+v, %v", got, ok)
	}
}

func TestRuntimeContextGeneratesValuesWithoutHeaders(t *testing.T) {
	now := time.UnixMilli(1_785_486_000_100)
	_, runtime := ensureRuntimeContext(context.Background(), nil, now)
	if runtime.RequestID == "" || runtime.TraceID == "" || runtime.SpanID == "" {
		t.Fatalf("runtime IDs were not generated: %+v", runtime)
	}
	if runtime.StartedAtMs != now.UnixMilli() || runtime.ReceivedAtMs != now.UnixMilli() {
		t.Fatalf("generated timing = %+v", runtime)
	}
}

func TestRegisteredToolSchemasExcludeClientTimestampAndServerRuntimeContext(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "demo")
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	runtime.registerTools(protocol)
	for name, registered := range runtime.listedToolMap() {
		encoded, err := json.Marshal(registered)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		var tool map[string]any
		if err := json.Unmarshal(encoded, &tool); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		inputSchema, _ := tool["inputSchema"].(map[string]any)
		if schemaContainsProperty(inputSchema, "started_at_ms") {
			t.Fatalf("%s input schema must not expose started_at_ms: %s", name, encoded)
		}
		for _, field := range []string{"request_id", "trace_id", "span_id", "received_at_ms", "completed_at_ms", "network_latency_ms", "processing_ms", "server_elapsed_ms"} {
			if schemaContainsProperty(inputSchema, field) {
				t.Fatalf("%s input schema contains server runtime field %q: %s", name, field, encoded)
			}
		}
	}
}

func TestInstrumentToolUsesServerReceiveTimeWithoutClientTimestamp(t *testing.T) {
	before := time.Now().UnixMilli()
	ctx := withRuntimeContext(context.Background(), RuntimeContext{
		RequestID: "req_context", TraceID: "trace_context", SpanID: "span_context", StartedAtMs: 1,
	})
	called := false
	var handlerStarted int64
	handler := func(handlerCtx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		called = true
		runtime, ok := runtimeContextFrom(handlerCtx)
		if !ok || runtime.RequestID != "req_context" {
			t.Fatalf("handler runtime context = %+v, %v", runtime, ok)
		}
		handlerStarted = runtime.StartedAtMs
		return mcpresult.NewStructured(map[string]any{"value": "ok"}, "ok"), nil
	}
	request := mcpresult.Request(map[string]any{"action": "list"})

	result, err := (&Runtime{}).instrumentTool("runtime_context_test", handler)(ctx, request)
	if err != nil || !called {
		t.Fatalf("instrumented call err=%v called=%v", err, called)
	}
	after := time.Now().UnixMilli()
	if handlerStarted < before || handlerStarted > after {
		t.Fatalf("handler started_at_ms = %d, want server receive time in [%d,%d]", handlerStarted, before, after)
	}
	envelope := decodeARCEnvelope(t, result)
	mcpx, _ := envelope["mcpx"].(map[string]any)
	trace, _ := mcpx["trace"].(map[string]any)
	if trace["request_id"] != "req_context" || trace["trace_id"] != "trace_context" || trace["span_id"] != "span_context" {
		t.Fatalf("ARC did not project runtime identity: %+v", trace)
	}
	if trace["started_at_ms"] != trace["received_at_ms"] || trace["network_latency_ms"] != float64(0) {
		t.Fatalf("ARC did not use server receive time fallback: %+v", trace)
	}
	if trace["completed_at_ms"] == nil {
		t.Fatalf("ARC metadata trace missing completion time: %+v", trace)
	}
	duration, _ := trace["duration"].(map[string]any)
	if duration["server_ms"] == nil {
		t.Fatalf("ARC metadata trace missing server duration: %+v", trace)
	}
	structured, _ := result.StructuredContent.(map[string]any)
	if structured["timing"] != nil {
		t.Fatalf("model structured content must not repeat timing: %+v", structured["timing"])
	}
	semantic, _ := structured["context"].(map[string]any)
	if semantic["operation_id"] != nil {
		t.Fatalf("ordinary call correlation operation_id leaked to model context: %+v", semantic)
	}
}

func TestTransportSessionBindingAllowsImplicitRemoteSession(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, ok := rt.reg.Get("demo")
	if !ok {
		t.Fatal("demo workspace was not registered")
	}
	if err := os.WriteFile(filepath.Join(workspace.Path, "bound.txt"), []byte("bound-session\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctxA := withRuntimeContext(context.Background(), RuntimeContext{RequestID: "req-mcp-a", TransportSessionID: "mcp-a"})
	ctxB := withRuntimeContext(context.Background(), RuntimeContext{RequestID: "req-mcp-b", TransportSessionID: "mcp-b"})
	wrappedSession := rt.instrumentTool("session", rt.toolSession)

	openedA := callEnvelope(t, wrappedSession, ctxA, map[string]any{"action": "open", "workspace": "demo"})
	idA, _ := openedA["remote_session_id"].(string)
	openedB := callEnvelope(t, wrappedSession, ctxB, map[string]any{"action": "open", "workspace": "demo"})
	idB, _ := openedB["remote_session_id"].(string)
	if idA == "" || idB == "" || idA == idB {
		t.Fatalf("bound sessions=%q/%q", idA, idB)
	}
	principal, err := rt.principalFromContext(ctxA)
	if err != nil {
		t.Fatal(err)
	}
	if got := rt.boundRemoteSessionID(ctxA, principal); got != idA {
		t.Fatalf("transport A binding=%q want=%q", got, idA)
	}
	if got := rt.boundRemoteSessionID(ctxB, principal); got != idB {
		t.Fatalf("transport B binding=%q want=%q", got, idB)
	}

	wrapped := rt.instrumentTool("read", rt.toolRead)
	result, err := wrapped(ctxA, mcpresult.Request(map[string]any{"view": "file", "path": "bound.txt"}))
	if err != nil || result.IsError {
		t.Fatalf("implicit bound read failed: err=%v result=%+v", err, result)
	}
	structured, _ := result.StructuredContent.(map[string]any)
	data, _ := structured["data"].(map[string]any)
	if data["content"] != "bound-session\n" || data["rev"] == nil || data["sha256"] != nil {
		t.Fatalf("implicit bound read data=%+v", data)
	}
	encoded, _ := json.Marshal(structured)
	if strings.Contains(string(encoded), "remote_session_id") {
		t.Fatalf("implicit bound model payload repeated remote_session_id: %s", encoded)
	}
	semantic, _ := structured["context"].(map[string]any)
	if semantic["operation_id"] != nil {
		t.Fatalf("ordinary read leaked correlation operation_id: %+v", semantic)
	}

	asyncResult, err := wrapped(ctxA, mcpresult.Request(map[string]any{
		"view": "list", "path": ".", "purpose": "verify bound async read", "execution_mode": "async", "limit": 5,
	}))
	if err != nil || asyncResult.IsError {
		t.Fatalf("implicit bound async read failed: err=%v result=%+v", err, asyncResult)
	}
	asyncStructured, _ := asyncResult.StructuredContent.(map[string]any)
	asyncData, _ := asyncStructured["data"].(map[string]any)
	operationID, _ := asyncData["operation_id"].(string)
	asyncContext, _ := asyncStructured["context"].(map[string]any)
	if asyncStructured["status"] != "accepted" || operationID == "" || asyncContext["operation_id"] != operationID {
		t.Fatalf("persistent async operation identity missing: %+v", asyncStructured)
	}
	asyncEncoded, _ := json.Marshal(asyncStructured)
	if strings.Contains(string(asyncEncoded), "remote_session_id") {
		t.Fatalf("implicit async model payload repeated remote_session_id: %s", asyncEncoded)
	}
	if final, timeout, waitErr := rt.operations.Wait(context.Background(), operationID, 5*time.Second); waitErr != nil || timeout || final.State != "succeeded" {
		t.Fatalf("implicit async operation did not complete: state=%s timeout=%v err=%v", final.State, timeout, waitErr)
	}

	envReq, _, fail := rt.remoteRequest(ctxA, mcpresult.Request(map[string]any{"remote_session_id": idB}))
	if fail != nil || envReq.RemoteSessionID != idB {
		t.Fatalf("explicit session override failed: session=%q fail=%+v", envReq.RemoteSessionID, fail)
	}

	openedA2 := callEnvelope(t, wrappedSession, ctxA, map[string]any{"action": "open", "workspace": "demo"})
	idA2, _ := openedA2["remote_session_id"].(string)
	if idA2 == "" || idA2 == idA || rt.boundRemoteSessionID(ctxA, principal) != idA2 {
		t.Fatalf("session open must create/rebind explicitly: old=%q new=%q bound=%q", idA, idA2, rt.boundRemoteSessionID(ctxA, principal))
	}
}

func schemaRequiresProperty(value any, wanted string) bool {
	item, ok := value.(map[string]any)
	if !ok {
		return false
	}
	required, _ := item["required"].([]any)
	for _, field := range required {
		if field == wanted {
			return true
		}
	}
	return false
}

func schemaContainsProperty(value any, wanted string) bool {
	switch item := value.(type) {
	case map[string]any:
		if properties, ok := item["properties"].(map[string]any); ok {
			if _, exists := properties[wanted]; exists {
				return true
			}
		}
		for _, nested := range item {
			if schemaContainsProperty(nested, wanted) {
				return true
			}
		}
	case []any:
		for _, nested := range item {
			if schemaContainsProperty(nested, wanted) {
				return true
			}
		}
	}
	return false
}
