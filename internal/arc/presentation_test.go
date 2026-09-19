package arc

import (
	"strings"
	"testing"
)

func TestRenderContentRendersCodeChangeDiff(t *testing.T) {
	diff := "diff --git a/demo.go b/demo.go\nindex 111..222 100644\n--- a/demo.go\n+++ b/demo.go\n@@ -1,3 +1,3 @@\n const Value = 1\n-const Value = 2\n+const Value = 3\n"
	data := map[string]any{
		"edit_id": "edit_1",
		"results": []any{map[string]any{"path": "demo.go", "operation": "update", "diff": diff}},
	}
	text, ok := RenderContent("code_change", "diff", "summary text", data)
	if !ok {
		t.Fatal("code_change diff renderer must produce a dedicated view")
	}
	for _, want := range []string{"### Edit edit_1 · +1 −1", "#### `demo.go` · update · [-1,+1]", "```diff", "-const Value = 2", "+const Value = 3"} {
		if !strings.Contains(text, want) {
			t.Fatalf("rendered diff missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "diff --git") || strings.Contains(text, "@@") || strings.Count(text, "demo.go") != 1 {
		t.Fatalf("rendered diff leaked headers or repeated path:\n%s", text)
	}
}

func TestRenderContentFallsBackForNonCodeChange(t *testing.T) {
	text, ok := RenderContent("search_result", "table", "search summary", map[string]any{"files": []any{}})
	if ok || text != "search summary" {
		t.Fatalf("non-code-change render = (%q, %v)", text, ok)
	}
}

func TestRenderContentTrimsLargeDiff(t *testing.T) {
	var lines []string
	for index := 0; index < 250; index++ {
		lines = append(lines, "+line "+strings.Repeat("x", 40))
	}
	data := map[string]any{
		"edit_id":      "edit_big",
		"results":      []any{map[string]any{"path": "big.go", "operation": "update"}},
		"diff_summary": strings.Join(lines, "\n"),
	}
	text, _ := RenderContent("code_change", "diff", "x", data)
	if !strings.Contains(text, "```diff") || !strings.Contains(text, "Diff 已截断") || !strings.Contains(text, `observe(view=diff, edit_id="edit_big")`) {
		t.Fatalf("large diff must be truncated with a continuation hint:\n%s", text)
	}
}

func TestPresentationDefaultsCoverStableResultTypes(t *testing.T) {
	for _, resultType := range []string{
		"text", "markdown", "search_result", "code_change", "table", "file_tree", "log", "error",
		"diagram", "diagram_collection", "plan", "plan_task", "delivery", "unknown",
	} {
		presentation := DefaultPresentation(resultType)
		if presentation.Default == "" || len(presentation.Available) == 0 || len(presentation.Fallback) == 0 {
			t.Fatalf("default presentation for %q is incomplete: %+v", resultType, presentation)
		}
		if !contains(presentation.Available, presentation.Default) || !contains(presentation.Available, "text") {
			t.Fatalf("default presentation for %q is not safely available: %+v", resultType, presentation)
		}
	}
}

func TestNormalizePresentationAndSelectRendererUseSafeFallbacks(t *testing.T) {
	presentation := NormalizePresentation(Presentation{
		Default:   "not-advertised",
		Available: []string{"table", "table", "unknown", ""},
		Fallback:  []string{"not-advertised", "text", "table", "text"},
	})
	if presentation.Default != "table" || len(presentation.Available) != 2 || len(presentation.Fallback) != 2 || presentation.Fallback[0] != "table" || presentation.Fallback[1] != "text" {
		t.Fatalf("normalized presentation = %+v", presentation)
	}
	textOnly := NormalizePresentation(Presentation{Default: "custom", Available: []string{"custom"}, Fallback: []string{"custom"}})
	if textOnly.Default != "text" || len(textOnly.Available) != 1 || textOnly.Available[0] != "text" || len(textOnly.Fallback) != 1 || textOnly.Fallback[0] != "text" {
		t.Fatalf("invalid presentation should degrade to text: %+v", textOnly)
	}
	if got := SelectRenderer(*DefaultPresentation("search_result"), PresentationPreference{Preferred: "not-advertised"}, HostCapabilities{Renderers: []string{"markdown"}}); got != "markdown" {
		t.Fatalf("renderer = %q, want markdown", got)
	}
	if got := SelectRenderer(*DefaultPresentation("search_result"), PresentationPreference{Preferred: "table"}, HostCapabilities{Renderers: []string{"text"}}); got != "text" {
		t.Fatalf("renderer = %q, want text", got)
	}
}

func TestRenderContentReportsPerFileDiffStats(t *testing.T) {
	diff := "diff --git a/one.go b/one.go\n--- a/one.go\n+++ b/one.go\n@@ -1 +1,2 @@\n old\n+new\n" +
		"diff --git a/two.go b/two.go\n--- a/two.go\n+++ b/two.go\n@@ -1 +1 @@\n-old\n+new\n"
	data := map[string]any{
		"edit_id": "edit_stats",
		"results": []any{
			map[string]any{"path": "one.go", "operation": "update"},
			map[string]any{"path": "two.go", "operation": "update"},
		},
		"diff_summary": diff,
	}
	text, ok := RenderContent("code_change", "diff", "summary", data)
	if !ok || !strings.Contains(text, "| `one.go` | update | +1 −0 |") || !strings.Contains(text, "| `two.go` | update | +1 −1 |") {
		t.Fatalf("per-file diff stats = (%q, %v)", text, ok)
	}
}

func TestRenderContentShowsPerFileDiffDetails(t *testing.T) {
	path := "ChatGPT-互联网医院小程序修复.txt"
	fileDiff := "--- a/" + path + "\n+++ b/" + path + "\n@@ -1 +1 @@\n-旧消息链路\n+新消息链路\n"
	data := map[string]any{
		"edit_id": "edit_detail",
		"results": []map[string]any{{
			"path":      path,
			"operation": "update",
			"diff":      fileDiff,
		}},
		"diff_summary": fileDiff,
	}

	text, ok := RenderContent("code_change", "diff", "summary", data)
	if !ok {
		t.Fatal("code_change diff renderer must produce a dedicated view")
	}
	for _, want := range []string{"#### `" + path + "`", "-旧消息链路", "+新消息链路"} {
		if !strings.Contains(text, want) {
			t.Fatalf("per-file diff detail missing %q:\n%s", want, text)
		}
	}
}

func TestRenderContentRendersCleanEditResultsDiff(t *testing.T) {
	data := map[string]any{
		"edit_id": "edit_clean_1",
		"results": []any{map[string]any{
			"path":      "created.txt",
			"operation": "create",
			"diff":      "--- /dev/null\n+++ b/created.txt\n@@ -0,0 +1 @@\n+created\n",
		}},
		"diff_summary": "--- /dev/null\n+++ b/created.txt\n@@ -0,0 +1 @@\n+created\n",
	}
	text, ok := RenderContent("code_change", "diff", "edit summary", data)
	if !ok || !strings.Contains(text, "### Edit edit_clean_1") || !strings.Contains(text, "created.txt") || !strings.Contains(text, "+created") {
		t.Fatalf("clean edit diff rendering=%q, dedicated=%v", text, ok)
	}
}

func TestRenderToolContentUsesBooleanConfirmationForCleanDelete(t *testing.T) {
	text, ok := RenderToolContent("edit", "error", "text", "delete blocked", map[string]any{
		"confirmation_required":   true,
		"user_confirmed_required": true,
		"confirmation_digest":     "sha256:delete-request",
		"deletions": []any{map[string]any{
			"path": "tmp/fixture.txt", "sha256": "sha256:file", "size": int64(12),
		}},
	})
	if !ok || !strings.Contains(text, "user_confirmed=true") || !strings.Contains(text, "confirmation_digest") {
		t.Fatalf("clean delete confirmation text=%q rendered=%v", text, ok)
	}
	if strings.Contains(text, "confirmation_token") {
		t.Fatalf("clean delete confirmation leaked token terminology: %q", text)
	}
}

func TestDetectMermaidRequiresCompleteDiagramBlocks(t *testing.T) {
	tests := []struct {
		name string
		text string
		kind string
		ok   bool
	}{
		{name: "single", text: "```mermaid\nflowchart TD\n  A --> B\n```", kind: "diagram", ok: true},
		{name: "collection", text: "```mermaid\nsequenceDiagram\n  A->>B: ping\n```\n\n```mermaid\ngraph LR\n  C-->D\n```", kind: "diagram_collection", ok: true},
		{name: "keyword only", text: "```mermaid\ngraph\n```", ok: false},
		{name: "directive only", text: "```mermaid\nsequenceDiagram\n```", ok: false},
		{name: "truncated", text: "```mermaid\nflowchart TD\n  A --> B", ok: false},
		{name: "non mermaid fence", text: "```text\nflowchart TD\n```", ok: false},
		{name: "prose around block", text: "Here is a graph:\n\n```mermaid\nflowchart TD\n  A --> B\n```", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, source, ok := DetectMermaid(tt.text)
			if ok != tt.ok || kind != tt.kind {
				t.Fatalf("DetectMermaid() = (%q, %q, %v), want kind=%q ok=%v", kind, source, ok, tt.kind, tt.ok)
			}
			if ok && source == "" {
				t.Fatal("successful detection returned empty source")
			}
		})
	}
}
