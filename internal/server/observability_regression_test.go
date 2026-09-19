package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"

	"mcpx/internal/config"
	"mcpx/internal/envelope"
	"mcpx/internal/logging"
	"mcpx/internal/observation"
	"mcpx/internal/remotesession"
)

func TestGatewayAccessLogRecordsDuration(t *testing.T) {
	var output bytes.Buffer
	logging.Init(logging.Options{Level: "info", Format: "text", Out: &output})
	defer logging.Init(logging.Options{Level: "info"})

	cfg := config.DefaultConfig()
	cfg.Auth.Mode = "open"
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	})
	server := httptest.NewServer(NewGateway(cfg, nil, inner).Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.Trailer.Get("Server-Timing") == "" || response.Trailer.Get("X-MCPX-Processing-Ms") == "" {
		t.Fatalf("missing response timing trailers: %+v", response.Trailer)
	}
	log := output.String()
	if !strings.Contains(log, "component=mcp_http") || !strings.Contains(log, "duration_ms=") || !strings.Contains(log, "status=200") {
		t.Fatalf("missing HTTP access log fields: %s", log)
	}
}

func TestToolLogRecordsDuration(t *testing.T) {
	var output bytes.Buffer
	logging.Init(logging.Options{Level: "info", Format: "text", Out: &output})
	defer logging.Init(logging.Options{Level: "info"})

	handler := func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcpresult.NewText("ok"), nil
	}
	instrumented := (&Runtime{}).instrumentTool("observability_test", handler)
	request := mcpresult.Request(map[string]any{})

	result, err := instrumented(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Meta == nil || result.Meta["mcpx.processing_ms"] == nil {
		t.Fatalf("missing tool processing metadata: %+v", result.Meta)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("ARC content type = %T", result.Content[0])
	}
	if text.Text != "ok" {
		t.Fatalf("non-code-change text should stay the summary, got: %q", text.Text)
	}
	payload := decodeARCEnvelope(t, result)
	if payload["mcpx"] == nil || payload["ok"] != nil {
		t.Fatalf("tool result is not ARC-only: %+v", payload)
	}
	log := output.String()
	if !strings.Contains(log, "component=mcp_tool") || !strings.Contains(log, "tool=observability_test") || !strings.Contains(log, "processing_ms=") || !strings.Contains(log, "network_latency_ms=") {
		t.Fatalf("missing tool timing log fields: %s", log)
	}
}

func TestToolRequestStartedAtMsPrefersClientMeta(t *testing.T) {
	received := time.UnixMilli(1786365000200)
	request := mcpresult.Request(map[string]any{})
	request.Params.Meta = mcp.Meta{clientStartedAtMetaKey: int64(1786365000050)}
	if got := toolRequestStartedAtMs(request, received); got != int64(1786365000050) {
		t.Fatalf("started_at_ms = %d, want client meta timestamp", got)
	}
}

func TestInstrumentToolSendsNativeProgressHeartbeat(t *testing.T) {
	old := toolProgressHeartbeatInterval
	toolProgressHeartbeatInterval = 5 * time.Millisecond
	defer func() { toolProgressHeartbeatInterval = old }()

	protocol := mcp.NewServer(&mcp.Implementation{Name: "progress-server", Version: "0.1.0"}, nil)
	handler := (&Runtime{}).instrumentTool("slow_tool", func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		time.Sleep(20 * time.Millisecond)
		return mcpresult.NewText("done"), nil
	})
	protocol.AddTool(&mcp.Tool{Name: "slow_tool", InputSchema: map[string]any{"type": "object"}}, handler)

	progress := make(chan *mcp.ProgressNotificationParams, 4)
	client := mcp.NewClient(&mcp.Implementation{Name: "progress-client", Version: "0.1.0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			if req != nil && req.Params != nil {
				progress <- req.Params
			}
		},
	})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	serverSession, err := protocol.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	params := &mcp.CallToolParams{Name: "slow_tool", Arguments: map[string]any{}}
	params.SetProgressToken("heartbeat-1")
	if _, err := clientSession.CallTool(ctx, params); err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-progress:
		if update.ProgressToken != "heartbeat-1" || !strings.Contains(update.Message, "slow_tool is still running") {
			t.Fatalf("unexpected progress update: %+v", update)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("server did not send native progress heartbeat")
	}
}

func TestInstrumentToolExpandsEmbeddedActivityBeforeBusinessCall(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	called := false
	handler := func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		called = true
		current, err := rt.currentAgentActivity(ctx, remoteID)
		if err != nil {
			t.Fatal(err)
		}
		if current == nil || current.Kind != "next" || current.Sequence != 3 || current.State != "preparing_action" || current.RelatedCallID == "" {
			t.Fatalf("embedded activity was not persisted before handler: %+v", current)
		}
		return mcpresult.NewStructured(map[string]any{"status": "succeeded"}, "ok"), nil
	}
	instrumented := rt.instrumentTool("read", handler)
	result, err := instrumented(context.Background(), mcpresult.Request(map[string]any{
		"remote_session_id": remoteID,
		"activity": map[string]any{
			"intent":   "定位订单支付超时配置入口",
			"evidence": "已确认交易模块持有支付过期配置",
			"next":     "继续读取自动取消任务",
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError || !called {
		t.Fatalf("instrumented call failed: result=%+v called=%v", result, called)
	}

	events, _, err := rt.observation.store.Query(context.Background(), observation.HistoryQuery{
		Workspace: "demo", SessionID: remoteID, Kinds: []string{observation.TypeAgentActivity}, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("activity events=%d want=3 events=%+v", len(events), events)
	}
	bySequence := map[int64]observation.Event{}
	for _, event := range events {
		bySequence[event.ActivitySequence] = event
	}
	wantKinds := map[int64]string{1: "intent", 2: "evidence", 3: "next"}
	turnID := bySequence[1].TurnID
	relatedCallID := bySequence[1].RelatedCallID
	if turnID == "" || relatedCallID == "" {
		t.Fatalf("runtime identifiers missing: %+v", events)
	}
	for sequence, kind := range wantKinds {
		event := bySequence[sequence]
		if event.ActivityKind != kind || event.TurnID != turnID || event.RelatedCallID != relatedCallID || event.Status != "preparing_action" {
			t.Fatalf("activity sequence %d=%+v want kind=%s turn=%s call=%s", sequence, event, kind, turnID, relatedCallID)
		}
	}

	structured, _ := result.StructuredContent.(map[string]any)
	semantic, _ := structured["context"].(map[string]any)
	if semantic["activity"] != nil {
		t.Fatalf("model result repeated activity snapshot: %+v", semantic["activity"])
	}
	envelope := decodeARCEnvelope(t, result)
	mcpx, _ := envelope["mcpx"].(map[string]any)
	arcResult, _ := mcpx["result"].(map[string]any)
	arcContext, _ := arcResult["context"].(map[string]any)
	activity, _ := arcContext["activity"].(map[string]any)
	if activity["kind"] != "next" || fmt.Sprint(activity["sequence"]) != "3" || activity["turn_id"] != turnID || activity["related_call_id"] != relatedCallID {
		t.Fatalf("ARC metadata lost persisted embedded activity snapshot: %+v", activity)
	}
}

func TestEmbeddedActivityPersistsStrictlyIncreasingMillisecondCursor(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	session := remotesession.Session{ID: remoteID, WorkspaceName: "demo"}
	base := time.UnixMilli(1786459500000).UTC()
	if err := rt.acceptEmbeddedAgentActivities(context.Background(), session, []embeddedActivityUpdate{{Kind: "intent", Summary: "first"}}, true, "req_first", base); err != nil {
		t.Fatal(err)
	}
	if err := rt.acceptEmbeddedAgentActivities(context.Background(), session, []embeddedActivityUpdate{{Kind: "intent", Summary: "second"}}, true, "req_second", base.Add(500*time.Microsecond)); err != nil {
		t.Fatal(err)
	}
	current, err := rt.currentAgentActivity(context.Background(), remoteID)
	if err != nil || current == nil {
		t.Fatalf("current activity unavailable: state=%+v err=%v", current, err)
	}
	if current.TurnID != "turn_second" || current.Summary != "second" {
		t.Fatalf("latest activity selected old millisecond row: %+v", current)
	}
}

func TestEmbeddedActivityIntentStartsNewRuntimeTurn(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	instrumented := rt.instrumentTool("read", func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcpresult.NewStructured(map[string]any{"status": "succeeded"}, "ok"), nil
	})
	call := func(activity map[string]any) agentActivityState {
		t.Helper()
		result, err := instrumented(context.Background(), mcpresult.Request(map[string]any{
			"remote_session_id": remoteID,
			"activity":          activity,
		}))
		if err != nil || result == nil || result.IsError {
			t.Fatalf("activity call failed: result=%+v err=%v", result, err)
		}
		current, err := rt.currentAgentActivity(context.Background(), remoteID)
		if err != nil || current == nil {
			t.Fatalf("current activity unavailable: state=%+v err=%v", current, err)
		}
		return *current
	}

	first := call(map[string]any{"intent": "开始第一轮定位", "hypothesis": "配置可能位于交易模块"})
	if first.Sequence != 2 || first.Kind != "hypothesis" || first.TurnID == "" {
		t.Fatalf("first turn=%+v", first)
	}
	continued := call(map[string]any{"evidence": "已找到交易模块配置"})
	if continued.TurnID != first.TurnID || continued.Sequence != 3 || continued.Kind != "evidence" {
		t.Fatalf("continued turn=%+v first=%+v", continued, first)
	}
	second := call(map[string]any{"intent": "开始第二轮验证", "status": "正在核对新问题"})
	if second.TurnID == first.TurnID || second.Sequence != 2 || second.Kind != "status" {
		t.Fatalf("new intent did not start a fresh turn: second=%+v first=%+v", second, first)
	}
}

func TestInstrumentToolRejectsOversizedEmbeddedActivityBeforeHandler(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	called := false
	instrumented := rt.instrumentTool("read", func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		called = true
		return mcpresult.NewText("unexpected"), nil
	})
	result, err := instrumented(context.Background(), mcpresult.Request(map[string]any{
		"remote_session_id": remoteID,
		"activity":          map[string]any{"intent": strings.Repeat("x", envelope.MaxIntentBytes+1)},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !result.IsError || called {
		t.Fatalf("oversized activity should fail before handler: result=%+v called=%v", result, called)
	}
}

func TestInstrumentToolCarriesEmbeddedActivitySnapshotToARCV2(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	handler := func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcpresult.NewStructured(map[string]any{"status": "succeeded"}, "ok"), nil
	}
	instrumented := rt.instrumentTool("observability_context", handler)
	request := mcpresult.Request(map[string]any{
		"remote_session_id": remoteID,
		"purpose":           "验证 ARC V2 Activity bridge",
		"activity":          map[string]any{"intent": "验证工具内 Activity", "evidence": "已确认服务端读取真实 Activity snapshot"},
		"reasoning_summary": "legacy request field",
		"progress_summary":  "legacy request progress",
		"next_step":         "legacy request next",
		"plan_id":           "pl_context",
		"plan_task_id":      "pt_context",
		"execution_task_id": "task_context",
	})
	result, err := instrumented(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent=%T", result.StructuredContent)
	}
	semantic, ok := structured["context"].(map[string]any)
	if !ok {
		t.Fatalf("context=%#v", structured["context"])
	}
	if semantic["activity"] != nil {
		t.Fatalf("model context must not repeat Activity snapshot: %+v", semantic)
	}
	envelope := decodeARCEnvelope(t, result)
	mcpx, _ := envelope["mcpx"].(map[string]any)
	arcResult, _ := mcpx["result"].(map[string]any)
	metadataContext, _ := arcResult["context"].(map[string]any)
	activity, ok := metadataContext["activity"].(map[string]any)
	if !ok || activity["kind"] != "evidence" || activity["summary"] != "已确认服务端读取真实 Activity snapshot" || activity["related_call_id"] == "" {
		t.Fatalf("ARC metadata activity=%+v context=%+v", activity, metadataContext)
	}
	for _, forbidden := range []string{"goal", "task_id", "reasoning_summary", "progress_summary", "next_step"} {
		if _, exists := semantic[forbidden]; exists {
			t.Fatalf("ARC V2 context leaked %q: %+v", forbidden, semantic)
		}
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if strings.Contains(text, "legacy request") || !strings.Contains(text, "activity: Evidence 已确认服务端读取真实 Activity snapshot") {
		t.Fatalf("human ARC text=%q", text)
	}
}

func TestCallToolSafelyConvertsHandlerPanic(t *testing.T) {
	result, err := callToolSafely("panic_test", func() (*mcp.CallToolResult, error) {
		panic("synthetic handler panic")
	})
	if err != nil {
		t.Fatalf("panic should be converted to a tool result, got error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("panic result=%+v, want structured MCP tool error", result)
	}
	if len(result.Content) != 1 || result.Content[0].(*mcp.TextContent).Text != "EXECUTION_RUNTIME_ERROR: tool execution failed" {
		t.Fatalf("panic result content=%+v", result.Content)
	}

	result, err = callToolSafely("recovery_test", func() (*mcp.CallToolResult, error) {
		return mcpresult.NewText("alive"), nil
	})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("safe call did not remain usable after panic: result=%+v err=%v", result, err)
	}
}

func TestStartupSkillDetailsUseDebugAndInfoOnlyCounts(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo", "fyy", "codex")
	skillRoot := t.TempDir()
	skillDir := filepath.Join(skillRoot, "sample")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: sample\ndescription: sample skill\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt.cfg.Discovery.Skills.Dirs = []string{skillRoot}
	recorder := &startupLogRecorder{}
	rt.logStartupInventory(recorder)
	if !strings.Contains(recorder.info.String(), "skills_summary count 1") {
		t.Fatalf("info log should contain only skill count: %s", recorder.info.String())
	}
	if strings.Contains(recorder.info.String(), "sample") {
		t.Fatalf("skill detail leaked into info log: %s", recorder.info.String())
	}
	if !strings.Contains(recorder.debug.String(), "sample") {
		t.Fatalf("debug log should contain skill detail: %s", recorder.debug.String())
	}
}

type startupLogRecorder struct {
	info  bytes.Buffer
	debug bytes.Buffer
}

func (r *startupLogRecorder) Info(msg string, args ...any) {
	r.info.WriteString(msg)
	for _, arg := range args {
		r.info.WriteString(" ")
		r.info.WriteString(fmt.Sprint(arg))
	}
	r.info.WriteByte('\n')
}

func (r *startupLogRecorder) Debug(msg string, args ...any) {
	r.debug.WriteString(msg)
	for _, arg := range args {
		r.debug.WriteString(" ")
		r.debug.WriteString(fmt.Sprint(arg))
	}
	r.debug.WriteByte('\n')
}
