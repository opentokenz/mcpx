package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanCoreEditRangeUpdatePreservesFormat(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	original := []byte("one\r\ntwo\r\nthree\r\nfour\r\n")
	path := filepath.Join(workspace.Path, "range.txt")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	response := callEnvelope(t, rt.toolEdit, context.Background(), map[string]any{
		"remote_session_id": remoteID,
		"purpose":           "replace two known lines without copying their previous contents",
		"edits": []any{map[string]any{
			"path": "range.txt", "operation": "update", "rev": compactFileRevision(digestForTest(original)),
			"range": map[string]any{"start_line": 2, "end_line": 3, "replacement": "TWO\nTHREE"},
		}},
	})
	if !statusOK(response) {
		t.Fatalf("range edit=%+v", response)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(updated), "one\r\nTWO\r\nTHREE\r\nfour\r\n"; got != want {
		t.Fatalf("range edit bytes=%q want=%q", got, want)
	}
}

func TestEditRangeSchemaAndOutOfBoundsRecovery(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	tool := rt.toolIndex["edit"]
	encoded, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(encoded, &schema); err != nil {
		t.Fatal(err)
	}
	items := schema["properties"].(map[string]any)["edits"].(map[string]any)["items"].(map[string]any)
	itemProperties := items["properties"].(map[string]any)
	rangeSchema, _ := itemProperties["range"].(map[string]any)
	if rangeSchema == nil || rangeSchema["required"] == nil {
		t.Fatalf("edit range schema missing: %s", encoded)
	}

	workspace, _ := rt.reg.Get("demo")
	original := []byte("one\ntwo\n")
	if err := os.WriteFile(filepath.Join(workspace.Path, "bounds.txt"), original, 0o644); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	response := callEnvelope(t, rt.toolEdit, context.Background(), map[string]any{
		"remote_session_id": remoteID,
		"purpose":           "exercise range bounds recovery",
		"edits": []any{map[string]any{
			"path": "bounds.txt", "operation": "update", "rev": compactFileRevision(digestForTest(original)),
			"range": map[string]any{"start_line": 2, "end_line": 3, "replacement": "x"},
		}},
	})
	if statusOK(response) || errorCode(response) != "range_out_of_bounds" {
		t.Fatalf("out-of-bounds response=%+v", response)
	}
	errorBody := response["error"].(map[string]any)
	details := errorBody["details"].(map[string]any)
	next := details["suggested_next"].(map[string]any)
	assertSuggestedActionFitsPublicSchema(t, rt, next)
	if next["tool"] != "read" {
		t.Fatalf("range recovery=%+v", next)
	}
}
