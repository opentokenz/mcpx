package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/arc"
	"mcpx/internal/mcpresult"
)

type serializedByteMeasurement struct {
	Name   string
	Before int
	After  int
}

func TestContextOptimizationSerializedBytes(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	rt.cfg.Security.Commands.Confirm = append(rt.cfg.Security.Commands.Confirm, "^echo\\b")
	workspace, _ := rt.reg.Get("demo")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	mustWrite := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(workspace.Path, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("normal.txt", "alpha\nbeta\n")
	mustWrite("long.txt", strings.Repeat("界", 400000)+"\n")
	mustWrite("edit.txt", "before\n")

	callRaw := func(handler mcp.ToolHandler, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		if _, ok := args["intent"]; !ok {
			args["intent"] = "measure serialized response"
		}
		result, err := handler(context.Background(), mcpresult.Request(args))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	measure := func(name, tool string, raw *mcp.CallToolResult, mutateLegacy func(map[string]any, map[string]any)) serializedByteMeasurement {
		t.Helper()
		rawEnvelope := decodeToolResult(t, raw)
		current := arc.WrapToolResult(tool, arc.ResultContext{}, raw)
		currentMap, ok := current.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("%s current structuredContent=%T", name, current.StructuredContent)
		}
		legacy := legacyStructuredForMeasurement(rawEnvelope, currentMap)
		if mutateLegacy != nil {
			mutateLegacy(legacy, rawEnvelope)
		}
		beforeJSON, err := json.Marshal(legacy)
		if err != nil {
			t.Fatal(err)
		}
		afterJSON, err := json.Marshal(currentMap)
		if err != nil {
			t.Fatal(err)
		}
		m := serializedByteMeasurement{Name: name, Before: len(beforeJSON), After: len(afterJSON)}
		t.Logf("serialized_bytes sample=%s before=%d after=%d delta=%d", m.Name, m.Before, m.After, m.Before-m.After)
		return m
	}

	var measurements []serializedByteMeasurement
	normal := callRaw(rt.toolRead, map[string]any{
		"remote_session_id": remoteID, "view": "file", "path": "normal.txt", "mode": "full",
	})
	measurements = append(measurements, measure("normal_read", "read", normal, nil))

	truncated := callRaw(rt.toolRead, map[string]any{
		"remote_session_id": remoteID, "view": "file", "path": "long.txt", "mode": "window",
		"offset": 0, "limit": 1,
	})
	measurements = append(measurements, measure("truncated_read_continuation", "read", truncated, nil))

	baseBytes, err := os.ReadFile(filepath.Join(workspace.Path, "edit.txt"))
	if err != nil {
		t.Fatal(err)
	}
	base := compactFileRevision(fmt.Sprintf("sha256:%x", sha256.Sum256(baseBytes)))
	editRaw := callRaw(rt.toolEdit, map[string]any{
		"remote_session_id": remoteID, "purpose": "measure edit response",
		"edits": []map[string]any{{
			"operation": "update", "path": "edit.txt", "rev": base,
			"replacements": []map[string]any{{"match": "before", "replacement": "after"}},
		}},
	})
	editEnvelope := decodeToolResult(t, editRaw)
	editData := editEnvelope["data"].(map[string]any)
	editID, _ := editData["edit_id"].(string)
	measurements = append(measurements, measure("edit_success", "edit", editRaw, func(legacy, _ map[string]any) {
		data, _ := legacy["data"].(map[string]any)
		data["diff_summary"] = combinedMeasurementDiff(data["results"])
	}))

	diffRaw := callRaw(rt.toolObserve, map[string]any{
		"remote_session_id": remoteID, "view": "diff", "edit_id": editID, "offset": 0, "limit": 4096,
	})
	measurements = append(measurements, measure("observe_diff", "observe", diffRaw, func(legacy, _ map[string]any) {
		data, _ := legacy["data"].(map[string]any)
		data["diff_summary"] = data["diff"]
	}))

	staleRaw := callRaw(rt.toolEdit, map[string]any{
		"remote_session_id": remoteID, "purpose": "measure stale edit",
		"edits": []map[string]any{{
			"operation": "update", "path": "edit.txt", "rev": base,
			"replacements": []map[string]any{{"match": "after", "replacement": "newer"}},
		}},
	})
	measurements = append(measurements, measure("stale_edit_error", "edit", staleRaw, nil))

	runRaw := callRaw(rt.toolExecute, map[string]any{
		"action": "run", "remote_session_id": remoteID, "purpose": "measure running task",
		"command": testSleepCommand(2 * time.Second), "yield_time_ms": 1,
	})
	runEnvelope := decodeToolResult(t, runRaw)
	runData := runEnvelope["data"].(map[string]any)
	taskID, _ := runData["execution_task_id"].(string)
	if taskID == "" {
		t.Fatalf("running sample did not return a task: %+v", runEnvelope)
	}
	measurements = append(measurements, measure("execute_running_task", "execute", runRaw, func(legacy, _ map[string]any) {
		addLegacyYield(legacy, 1)
	}))

	attachRaw := callRaw(rt.toolExecute, map[string]any{
		"action": "attach", "remote_session_id": remoteID, "execution_task_id": taskID,
		"stdout_offset": runData["stdout_next_offset"], "stderr_offset": runData["stderr_next_offset"], "yield_time_ms": 1,
	})
	measurements = append(measurements, measure("execute_attach", "execute", attachRaw, func(legacy, _ map[string]any) {
		addLegacyYield(legacy, 1)
	}))

	confirmationRaw := callRaw(rt.toolExecute, map[string]any{
		"action": "run", "remote_session_id": remoteID, "purpose": "measure command confirmation",
		"command": "echo confirmed",
	})
	confirmationEnvelope := decodeToolResult(t, confirmationRaw)
	if confirmationEnvelope["status"] != "waiting_confirmation" {
		t.Fatalf("confirmation sample=%+v", confirmationEnvelope)
	}
	measurements = append(measurements, measure("confirmation_response", "execute", confirmationRaw, nil))

	for _, m := range measurements {
		if m.After > m.Before {
			t.Fatalf("%s serialized bytes increased: before=%d after=%d", m.Name, m.Before, m.After)
		}
	}
}

func legacyStructuredForMeasurement(rawEnvelope, current map[string]any) map[string]any {
	legacy := cloneJSONMapForMeasurement(current)
	status, _ := rawEnvelope["status"].(string)
	rawData, _ := rawEnvelope["data"].(map[string]any)
	if rawData == nil {
		rawData = map[string]any{}
	}
	if rawEnvelope["error"] != nil || status == "failed" || status == "waiting_confirmation" || status == "need_confirmation" {
		legacyData := cloneJSONMapForMeasurement(rawData)
		if rawEnvelope["error"] != nil {
			legacyData["error"] = rawEnvelope["error"]
			legacy["error"] = rawEnvelope["error"]
		}
		if status != "" {
			legacyData["status"] = status
		}
		legacy["data"] = legacyData
	} else {
		legacy["data"] = cloneJSONMapForMeasurement(rawData)
	}
	if actions, ok := legacy["actions"].([]any); ok && len(actions) > 1 {
		legacy["actions"] = actions[:1]
	}
	if legacy["actions"] == nil {
		if action := firstLegacyActionForMeasurement(rawEnvelope); action != nil {
			legacy["actions"] = []any{action}
		}
	}
	return legacy
}

func firstLegacyActionForMeasurement(raw map[string]any) map[string]any {
	var find func(map[string]any) map[string]any
	find = func(source map[string]any) map[string]any {
		if source == nil {
			return nil
		}
		if action, ok := source["next_action"].(map[string]any); ok {
			return measurementAction(action)
		}
		if inner, ok := source["data"].(map[string]any); ok {
			if action := find(inner); action != nil {
				return action
			}
		}
		if errBody, ok := source["error"].(map[string]any); ok {
			if details, ok := errBody["details"].(map[string]any); ok {
				if action, ok := details["next_action"].(map[string]any); ok {
					return measurementAction(action)
				}
				if rawActions, ok := details["next_actions"].([]any); ok && len(rawActions) > 0 {
					if action, ok := rawActions[0].(map[string]any); ok {
						return measurementAction(action)
					}
				}
			}
		}
		return nil
	}
	return find(raw)
}

func measurementAction(next map[string]any) map[string]any {
	tool, _ := next["tool"].(string)
	if tool == "" {
		return nil
	}
	args, _ := next["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	return map[string]any{"id": tool, "type": "continue", "label": "Continue with " + tool, "confirm": false, "arguments": args}
}

func addLegacyYield(legacy map[string]any, yield int) {
	data, _ := legacy["data"].(map[string]any)
	next, _ := data["next_action"].(map[string]any)
	args, _ := next["arguments"].(map[string]any)
	if args != nil {
		args["yield_time_ms"] = yield
	}
}

func combinedMeasurementDiff(raw any) string {
	results, _ := raw.([]any)
	var builder strings.Builder
	for _, item := range results {
		file, _ := item.(map[string]any)
		diff, _ := file["diff"].(string)
		if diff == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(diff)
	}
	return builder.String()
}

func cloneJSONMapForMeasurement(input map[string]any) map[string]any {
	encoded, _ := json.Marshal(input)
	var result map[string]any
	_ = json.Unmarshal(encoded, &result)
	return result
}
