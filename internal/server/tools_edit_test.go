package server

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"mcpx/internal/edit"
	"mcpx/internal/envelope"
	"mcpx/internal/observation"
	"mcpx/internal/remotesession"
)

func TestTooManyChangesResponseCarriesStructuredRecovery(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, ok := rt.reg.Get("demo")
	if !ok {
		t.Fatal("demo workspace was not registered")
	}
	result, err := rt.editToolError(envelope.Request{RequestID: "req_too_many"}, remotesession.Session{
		ID: "session-1", WorkspaceName: "demo", WorkspacePath: workspace.Path,
	}, &edit.ApplyError{Code: "TOO_MANY_CHANGES", Path: "large.txt", ChangedLines: edit.MaxChangedLines + 1})
	if err != nil {
		t.Fatal(err)
	}
	response := decodeToolResult(t, result)
	errorBody, _ := response["error"].(map[string]any)
	if errorBody["category"] != "validation" || errorBody["retryable"] != true {
		t.Fatalf("error classification=%+v", errorBody)
	}
	details, _ := errorBody["details"].(map[string]any)
	recovery, _ := details["recovery"].(map[string]any)
	if recovery["action"] != "split_edit" || recovery["max_changed_lines"] != float64(edit.MaxChangedLines) {
		t.Fatalf("details recovery=%+v", details["recovery"])
	}
	if details["suggested_next"] != nil {
		t.Fatalf("too-many-changes requires replanning, not a fake executable action: %+v", details["suggested_next"])
	}
	if errorBody["recovery"] != nil {
		t.Fatalf("too-many-changes must not publish an incomplete public recovery call: %+v", errorBody["recovery"])
	}
}

func TestStaleEditRecoveryIsExecutableReadRefresh(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	result, err := rt.editToolError(envelope.Request{RequestID: "req_stale"}, remotesession.Session{
		ID: "session-1", WorkspaceName: "demo", WorkspacePath: workspace.Path,
	}, &edit.ApplyError{Code: "STALE_REVISION", Path: "demo.go", Current: "sha256:current"})
	if err != nil {
		t.Fatal(err)
	}
	response := decodeToolResult(t, result)
	errorBody := response["error"].(map[string]any)
	details := errorBody["details"].(map[string]any)
	next := details["suggested_next"].(map[string]any)
	assertSuggestedActionFitsPublicSchema(t, rt, next)
	args := next["arguments"].(map[string]any)
	if next["tool"] != "read" || args["view"] != "file" || args["mode"] != "window" || args["limit"] != float64(1) {
		t.Fatalf("stale refresh action=%+v", next)
	}
	publicRecovery := errorBody["recovery"].(map[string]any)
	if publicRecovery["tool"] != "read" {
		t.Fatalf("stale public recovery=%+v", publicRecovery)
	}
}

func TestCleanCoreReadSupportsMixedFullAndWindowBatch(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, ok := rt.reg.Get("demo")
	if !ok {
		t.Fatal("demo workspace missing")
	}
	if err := os.WriteFile(filepath.Join(workspace.Path, "full.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Path, "window.txt"), []byte("zero\none\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	response := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{
		"remote_session_id": remoteID,
		"view":              "file",
		"items": []map[string]any{
			{"path": "full.txt", "mode": "full"},
			{"path": "window.txt", "mode": "window", "offset": 1, "limit": 1},
		},
	})
	if !statusOK(response) {
		t.Fatalf("read failed: %+v", response)
	}
	items := asMapSlice(response["data"].(map[string]any)["results"])
	if len(items) != 2 {
		t.Fatalf("read results=%+v", items)
	}
	if items[0]["mode"] != "full" || items[0]["content"] != "one\ntwo\n" {
		t.Fatalf("full item=%+v", items[0])
	}
	if items[1]["mode"] != "window" || items[1]["content"] != "one\n" {
		t.Fatalf("window item=%+v", items[1])
	}
}

func TestCleanCoreSessionDefaultsToOpenAndResumesSuppliedID(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"workspace": "demo"})
	if !statusOK(opened) {
		t.Fatalf("default session open failed: %+v", opened)
	}
	remoteID := opened["remote_session_id"].(string)
	resumed := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"remote_session_id": remoteID})
	if resumed["remote_session_id"] != remoteID {
		t.Fatalf("resume changed remote id: %+v", resumed)
	}
}

func TestCleanCoreEditAppliesIdempotentlyAndReportsStale(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	path := filepath.Join(workspace.Path, "edit.txt")
	original := []byte("title: old\ncolor: red\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	request := map[string]any{
		"remote_session_id": remoteID,
		"purpose":           "update the file labels",
		"idempotency_key":   "edit-test-1",
		"edits": []map[string]any{{
			"path":      "edit.txt",
			"operation": "update",
			"rev":       compactFileRevision(digestForTest(original)),
			"replacements": []map[string]any{
				{"match": "title: old", "replacement": "title: new"},
				{"match": "color: red", "replacement": "color: blue"},
			},
		}},
	}
	first := callEnvelope(t, rt.toolEdit, context.Background(), request)
	if !statusOK(first) {
		t.Fatalf("edit failed: %+v", first)
	}
	data := first["data"].(map[string]any)
	if data["total_changed_lines"] != float64(4) && data["total_changed_lines"] != 4 {
		t.Fatalf("edit changed lines=%v", data["total_changed_lines"])
	}
	if data["diff_summary"] != nil {
		t.Fatalf("edit response must not expose duplicate diff_summary: %+v", data)
	}
	results, _ := data["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("edit results=%+v", data["results"])
	}
	if diff, _ := results[0].(map[string]any)["diff"].(string); !strings.Contains(diff, "+title: new") {
		t.Fatalf("per-file diff=%q", diff)
	}
	content, _ := os.ReadFile(path)
	if string(content) != "title: new\ncolor: blue\n" {
		t.Fatalf("edited content=%q", content)
	}

	replay := callEnvelope(t, rt.toolEdit, context.Background(), request)
	replayData := replay["data"].(map[string]any)
	if replayData["idempotent_replay"] != true {
		t.Fatalf("replay did not use idempotency cache: %+v", replayData)
	}

	stale := callEnvelope(t, rt.toolEdit, context.Background(), map[string]any{
		"remote_session_id": remoteID,
		"purpose":           "test stale revision",
		"edits": []map[string]any{{
			"path": "edit.txt", "operation": "update", "rev": compactFileRevision(digestForTest([]byte("stale"))),
			"replacements": []map[string]any{{"match": "title: new", "replacement": "title: newest"}},
		}},
	})
	if statusOK(stale) || errorCode(stale) != "stale_revision" {
		t.Fatalf("stale response=%+v", stale)
	}
	if details, _ := stale["error"].(map[string]any)["details"].(map[string]any); details["current_rev"] == nil || details["current_sha256"] != nil {
		t.Fatalf("stale response missing compact current_rev or leaking sha256: %+v", stale)
	}
}

func TestCleanCoreObserveRejectsChangesView(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	payload, err := json.Marshal(map[string]any{
		"diff_summary":        "--- a/a.txt\n+++ b/a.txt\n-old\n+new\n",
		"total_changed_lines": 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.observation.Record(context.Background(), observation.Event{
		Workspace: "demo", RemoteSessionID: remoteID, Tool: "edit", Type: observation.TypeFileChanged,
		Status: "succeeded", Path: "a.txt", Output: payload,
	}); err != nil {
		t.Fatal(err)
	}
	response := callEnvelope(t, rt.toolObserve, context.Background(), map[string]any{
		"remote_session_id": remoteID, "view": "changes",
	})
	if statusOK(response) || errorCode(response) != "invalid_action" {
		t.Fatalf("observe changes must be rejected: %+v", response)
	}
	// Runtime may still record file_change events internally for audit/history;
	// removing observe(changes) only removes the ambiguous public view.
	events, _, err := rt.observation.store.Query(context.Background(), observation.HistoryQuery{
		Workspace: "demo", SessionID: remoteID, Kinds: []string{observation.TypeFileChanged}, Limit: 20,
	})
	if err != nil || len(events) != 1 || !strings.Contains(string(events[0].Output), "+new") {
		t.Fatalf("internal file_change observation missing: events=%+v err=%v", events, err)
	}
}

func TestCleanCoreEditUsesContextualDiffForSmallChangeInLargeFile(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	lines := make([]string, 724)
	for index := range lines {
		lines[index] = fmt.Sprintf("line-%03d", index+1)
	}
	old := strings.Join(lines, "\n") + "\n"
	path := filepath.Join(workspace.Path, "large-small-change.txt")
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	response := callEnvelope(t, rt.toolEdit, context.Background(), map[string]any{
		"remote_session_id": remoteID,
		"purpose":           "preview one line in a large file",
		"apply":             false,
		"edits": []map[string]any{{
			"path": "large-small-change.txt", "operation": "update", "rev": compactFileRevision(digestForTest([]byte(old))),
			"replacements": []map[string]any{{"match": "line-400", "replacement": "line-400 changed"}},
		}},
	})
	if !statusOK(response) {
		t.Fatalf("dry-run edit failed: %+v", response)
	}
	data := response["data"].(map[string]any)
	if data["diff_summary"] != nil {
		t.Fatalf("small edit must not duplicate diff_summary: %+v", data)
	}
	results, _ := data["results"].([]any)
	preview, _ := results[0].(map[string]any)["diff"].(string)
	if data["diff_truncated"] == true || len(preview) > 1024 {
		t.Fatalf("small edit should have a compact contextual diff: bytes=%d data=%+v", len(preview), data)
	}
	if diffBytes, _ := data["diff_bytes"].(float64); diffBytes > 1024 {
		t.Fatalf("stored diff should be contextual for a small edit: %v", data["diff_bytes"])
	}
	if !strings.Contains(preview, "-line-400") || !strings.Contains(preview, "+line-400 changed") || strings.Contains(preview, "line-001") || strings.Contains(preview, "line-724") {
		t.Fatalf("contextual diff content is incorrect: %s", preview)
	}
}

func TestCleanCoreEditBoundsLargeDiffAndPaginatesFullDiff(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	oldText := strings.Repeat("old-", 200) + "\n"
	newText := strings.Repeat("new-", 200) + "\n"
	old := strings.Repeat(oldText, 400)
	updated := strings.Repeat(newText, 400)
	path := filepath.Join(workspace.Path, "large.txt")
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	response := callEnvelope(t, rt.toolEdit, context.Background(), map[string]any{
		"remote_session_id": remoteID,
		"purpose":           "replace a large generated block",
		"idempotency_key":   "large-diff-1",
		"edits": []map[string]any{{
			"path": "large.txt", "operation": "update", "rev": compactFileRevision(digestForTest([]byte(old))), "content": updated,
		}},
	})
	if !statusOK(response) {
		t.Fatalf("large edit failed: %+v", response)
	}
	data := response["data"].(map[string]any)
	if data["diff_summary"] != nil {
		t.Fatalf("large edit must not duplicate diff_summary: %+v", data)
	}
	results, _ := data["results"].([]any)
	preview, _ := results[0].(map[string]any)["diff"].(string)
	if data["diff_truncated"] != true || len(preview) > cleanDiffTotalPreviewMaxBytes || len(preview) <= 32<<10 || data["edit_id"] == "" {
		t.Fatalf("large single-file diff must retain up to the 64 KiB total preview budget: bytes=%d data=%+v", len(preview), data)
	}
	if diffBytes, _ := data["diff_bytes"].(float64); diffBytes <= float64(cleanDiffTotalPreviewMaxBytes) {
		t.Fatalf("large diff byte count=%v", data["diff_bytes"])
	}
	editID := data["edit_id"].(string)
	diffPage := callEnvelope(t, rt.toolObserve, context.Background(), map[string]any{
		"remote_session_id": remoteID, "view": "diff", "edit_id": editID, "offset": 0, "limit": 1024,
	})
	if !statusOK(diffPage) {
		t.Fatalf("observe diff failed: %+v", diffPage)
	}
	diffPageData, _ := diffPage["data"].(map[string]any)
	if diffPageData["edit_id"] != editID || diffPageData["diff"] == "" || diffPageData["next_offset"] == nil {
		t.Fatalf("observe diff page incomplete: %+v", diffPageData)
	}
	if diffPageData["diff_summary"] != nil {
		t.Fatalf("observe diff must expose a single authoritative diff body: %+v", diffPageData)
	}

	events, _, err := rt.observation.store.Query(context.Background(), observation.HistoryQuery{
		Workspace: "demo", SessionID: remoteID, Kinds: []string{observation.TypeFileChanged}, Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	var observed map[string]any
	for _, event := range events {
		var payload map[string]any
		if json.Unmarshal(event.Output, &payload) == nil && payload["edit_id"] == editID {
			observed = payload
			if event.Truncated {
				t.Fatalf("file.changed observation was truncated: bytes=%d", len(event.Output))
			}
			break
		}
	}
	if observed == nil {
		t.Fatalf("file.changed observation for %s not found", editID)
	}
	observedResults, _ := observed["results"].([]any)
	if len(observedResults) != 1 {
		t.Fatalf("observed results=%+v", observed["results"])
	}
	file, _ := observedResults[0].(map[string]any)
	fullDiff, _ := file["diff"].(string)
	if len(fullDiff) <= cleanDiffFilePreviewMaxBytes || !strings.Contains(fullDiff, strings.TrimSpace(newText)) {
		t.Fatalf("observation did not retain full diff: bytes=%d", len(fullDiff))
	}
}

func TestCleanCoreEditRejectsIdempotencyFingerprintConflict(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	path := filepath.Join(workspace.Path, "conflict.txt")
	original := []byte("old\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	base := compactFileRevision(digestForTest(original))
	first := map[string]any{
		"remote_session_id": remoteID, "purpose": "first", "idempotency_key": "same-key",
		"edits": []map[string]any{{"path": "conflict.txt", "operation": "update", "rev": base, "replacements": []map[string]any{{"match": "old", "replacement": "new"}}}},
	}
	if response := callEnvelope(t, rt.toolEdit, context.Background(), first); !statusOK(response) {
		t.Fatalf("first edit failed: %+v", response)
	}
	conflict := map[string]any{
		"remote_session_id": remoteID, "purpose": "different", "idempotency_key": "same-key",
		"edits": []map[string]any{{"path": "conflict.txt", "operation": "update", "rev": base, "replacements": []map[string]any{{"match": "old", "replacement": "other"}}}},
	}
	response := callEnvelope(t, rt.toolEdit, context.Background(), conflict)
	if statusOK(response) || errorCode(response) != "idempotency_conflict" {
		t.Fatalf("expected idempotency conflict: %+v", response)
	}
}

func TestCleanCoreUTF16ReadEditRoundTripPreservesRawBytes(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	logical := "第一行\r\nemoji 😀\r\n第三行\r\n"
	original := append([]byte{0xff, 0xfe}, encodeUTF16ForServerTest(logical, binary.LittleEndian)...)
	path := filepath.Join(workspace.Path, "utf16-edit.txt")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	read := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{
		"remote_session_id": remoteID, "view": "file", "path": "utf16-edit.txt", "mode": "full",
	})
	if !statusOK(read) {
		t.Fatalf("UTF-16 read failed: %+v", read)
	}
	readData := read["data"].(map[string]any)
	item := readData
	if item["content"] != logical || item["encoding"] != "utf-8" {
		t.Fatalf("decoded UTF-16 payload=%+v", item)
	}
	rev, _ := item["rev"].(string)
	if rev == "" || item["sha256"] != nil {
		t.Fatalf("UTF-16 read must return compact rev only: %+v", item)
	}
	response := callEnvelope(t, rt.toolEdit, context.Background(), map[string]any{
		"remote_session_id": remoteID, "purpose": "update UTF-16 text", "idempotency_key": "utf16-roundtrip-1",
		"edits": []map[string]any{{
			"path": "utf16-edit.txt", "operation": "update", "rev": rev,
			"replacements": []map[string]any{{"match": "第三行", "replacement": "第三行-已改"}},
		}},
	})
	if !statusOK(response) {
		t.Fatalf("UTF-16 edit failed: %+v", response)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte{0xff, 0xfe}, encodeUTF16ForServerTest("第一行\r\nemoji 😀\r\n第三行-已改\r\n", binary.LittleEndian)...)
	if string(updated) != string(want) {
		t.Fatalf("UTF-16 raw bytes changed unexpectedly: got %x want %x", updated, want)
	}
	if compactFileRevision(digestForTest(updated)) == rev {
		t.Fatal("UTF-16 edit did not advance compact revision")
	}
}

func encodeUTF16ForServerTest(text string, order binary.ByteOrder) []byte {
	units := utf16.Encode([]rune(text))
	encoded := make([]byte, len(units)*2)
	for index, unit := range units {
		order.PutUint16(encoded[index*2:], unit)
	}
	return encoded
}

func digestForTest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestCleanEditApplyFalseNeverMutatesFilesystem(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	path := filepath.Join(workspace.Path, "dry-run.txt")
	original := []byte("before\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	response := callEnvelope(t, rt.toolEdit, context.Background(), map[string]any{
		"remote_session_id": remoteID,
		"purpose":           "preview a change without applying it",
		"apply":             false,
		"edits": []any{map[string]any{
			"path": "dry-run.txt", "operation": "update", "rev": compactFileRevision(digestForTest(original)),
			"replacements": []any{map[string]any{"match": "before", "replacement": "after"}},
		}},
	})
	if !statusOK(response) {
		t.Fatalf("dry-run edit failed: %+v", response)
	}
	data, _ := response["data"].(map[string]any)
	if data["apply"] != false || data["preview_only"] != true || data["edit_id"] != "" {
		t.Fatalf("dry-run response indicates mutation: %+v", data)
	}
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != string(original) {
		t.Fatalf("dry-run changed file: %q", actual)
	}
	deletePreview := callEnvelope(t, rt.toolEdit, context.Background(), map[string]any{
		"remote_session_id": remoteID, "purpose": "ensure delete preview cannot mutate",
		"apply": false,
		"edits": []any{map[string]any{"path": "dry-run.txt", "operation": "delete"}},
	})
	if statusOK(deletePreview) || errorCode(deletePreview) != "move_out_required" {
		t.Fatalf("edit delete must route to move_out(action=prepare): %+v", deletePreview)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("rejected delete preview changed file: %v", err)
	}
}

func anyStrings(values []any) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			result = append(result, text)
		}
	}
	return result
}
