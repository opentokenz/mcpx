package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"mcpx/internal/mcpresult"
)

func TestReadCanonicalizesUniquelyImpliedView(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "batch file", args: map[string]any{"items": []any{map[string]any{"path": "a.go"}}}, want: "file"},
		{name: "file mode", args: map[string]any{"path": "a.go", "mode": "full"}, want: "file"},
		{name: "search", args: map[string]any{"query": "needle"}, want: "search"},
		{name: "context", args: map[string]any{"query": "needle", "search_mode": "smart"}, want: "context"},
		{name: "list", args: map[string]any{"entries_limit": 20}, want: "list"},
		{name: "ambiguous path", args: map[string]any{"path": "internal"}, want: ""},
		{name: "conflicting hints", args: map[string]any{"mode": "full", "entries_limit": 20}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := mcpresult.Request(tt.args)
			_, got := canonicalReadRequest(request)
			if got != tt.want {
				t.Fatalf("view=%q want=%q", got, tt.want)
			}
		})
	}
}

func TestReadWithoutViewExecutesWhenViewIsInferable(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	if err := os.WriteFile(filepath.Join(workspace.Path, "inferred.txt"), []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	result := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{
		"remote_session_id": remoteID,
		"items":             []any{map[string]any{"path": "inferred.txt", "mode": "full"}},
	})
	if !statusOK(result) {
		t.Fatalf("inferred batch read=%+v", result)
	}
}

func TestReadPublicSchemaDoesNotRequireView(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	tool, ok := rt.toolIndex["read"]
	if !ok {
		t.Fatal("read tool not registered")
	}
	encoded, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(encoded, &schema); err != nil {
		t.Fatal(err)
	}
	required, _ := schema["required"].([]any)
	for _, field := range required {
		if field == "view" {
			t.Fatalf("read public schema still requires view: %s", encoded)
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	if properties["sections"] != nil || properties["snapshot_id"] != nil {
		t.Fatalf("read must not duplicate environment_read fields: %s", encoded)
	}
	if properties["include_sha256"] != nil {
		t.Fatalf("content-bearing reads always return revision; include_sha256 must not be client bookkeeping: %s", encoded)
	}
	view, _ := properties["view"].(map[string]any)
	for _, raw := range view["enum"].([]any) {
		if raw == "environment" {
			t.Fatalf("read must not expose environment view: %s", encoded)
		}
	}
}

func TestReadSearchAndContextReturnRevisionByDefault(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	if err := os.WriteFile(filepath.Join(workspace.Path, "revision.txt"), []byte("alpha needle omega\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	search := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{
		"remote_session_id": remoteID, "query": "needle",
	})
	searchData := search["data"].(map[string]any)
	matches := searchData["matches"].([]any)
	if len(matches) == 0 || matches[0].(map[string]any)["rev"] == "" {
		t.Fatalf("search result must carry revision: %+v", searchData)
	}

	contextResult := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{
		"remote_session_id": remoteID, "query": "needle", "search_mode": "smart",
	})
	contextData := contextResult["data"].(map[string]any)
	files := contextData["files"].([]any)
	if len(files) == 0 || files[0].(map[string]any)["rev"] == "" {
		t.Fatalf("context result must carry revision: %+v", contextData)
	}
}

func TestReadPathOnlyReturnsCompactAmbiguityError(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	result := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{
		"remote_session_id": remoteID,
		"path":              "internal",
	})
	if statusOK(result) || errorCode(result) != "ambiguous_request" {
		t.Fatalf("ambiguous read=%+v", result)
	}
}
