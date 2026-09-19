package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMoveOutPrepareInfersWorkspaceKindAndIdempotency(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	fileContent := []byte("remove me\n")
	if err := os.WriteFile(filepath.Join(workspace.Path, "remove.txt"), fileContent, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace.Path, "remove-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace.Path, "remove-link")); err != nil {
		t.Skipf("symlink creation unavailable on this platform/account: %v", err)
	}
	remoteID := openMoveOutSession(t, rt)

	prepared := callMoveOut(t, rt.toolMoveOut, map[string]any{
		"action":            "prepare",
		"remote_session_id": remoteID,
		"purpose":           "prepare three user-requested workspace entries for safe removal",
		"targets": []any{
			map[string]any{"path": "remove.txt", "expected_sha256": digestForTest(fileContent)},
			map[string]any{"path": "remove-dir"},
			map[string]any{"path": "remove-link"},
		},
	})
	if !statusOK(prepared) {
		t.Fatalf("simplified prepare failed: %+v", prepared)
	}
	data := prepared["data"].(map[string]any)
	if data["target_count"] != float64(3) || data["file_count"] != float64(1) || data["directory_count"] != float64(1) || data["symlink_count"] != float64(1) {
		t.Fatalf("inferred target kinds are wrong: %+v", data)
	}
	if data["confirmation_uuid"] == "" || data["idempotency_key"] == "" {
		t.Fatalf("Runtime must freeze confirmation and generated idempotency facts: %+v", data)
	}
	if _, err := os.Stat(filepath.Join(workspace.Path, "remove.txt")); err != nil {
		t.Fatalf("prepare mutated file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace.Path, "remove-dir")); err != nil {
		t.Fatalf("prepare mutated directory: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(workspace.Path, "remove-link")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("prepare mutated symlink: %v", err)
	}
}

func TestMoveOutSimplifiedSchemaKeepsFileRevisionGuard(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	tool := rt.toolIndex["move_out"]
	encoded, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(encoded, &schema); err != nil {
		t.Fatal(err)
	}
	branch := schemaActionBranch(schema, "prepare")
	if branch == nil {
		t.Fatalf("prepare branch missing: %s", encoded)
	}
	properties := branch["properties"].(map[string]any)
	if properties["workspace"] != nil || schemaRequires(branch, "idempotency_key") {
		t.Fatalf("prepare still exposes duplicate workspace or requires idempotency key: %s", encoded)
	}
	targetItems := properties["targets"].(map[string]any)["items"].(map[string]any)
	targetProperties := targetItems["properties"].(map[string]any)
	if targetProperties["kind"] != nil || targetProperties["expected_sha256"] == nil || !schemaRequires(targetItems, "path") {
		t.Fatalf("move_out target schema must infer kind and retain expected_sha256: %s", encoded)
	}

	workspace, _ := rt.reg.Get("demo")
	if err := os.WriteFile(filepath.Join(workspace.Path, "guard.txt"), []byte("guard\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	remoteID := openMoveOutSession(t, rt)
	missingSHA := callMoveOut(t, rt.toolMoveOut, map[string]any{
		"action": "prepare", "remote_session_id": remoteID, "purpose": "prepare file without revision to exercise guard",
		"targets": []any{map[string]any{"path": "guard.txt"}},
	})
	if statusOK(missingSHA) || errorCode(missingSHA) != "invalid_request" {
		t.Fatalf("regular file without expected_sha256 must be rejected: %+v", missingSHA)
	}
}

var _ = context.Background
