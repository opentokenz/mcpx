package server

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"

	"mcpx/internal/arc"
	"mcpx/internal/envelope"
	"mcpx/internal/file"
)

func TestSourceErrorClassifiesMissingPathWithoutStatatLeak(t *testing.T) {
	result, err := (&Runtime{}).sourceError(envelope.Request{RequestID: "req_missing"}, "session", "demo", fmt.Errorf("statat missing.txt: no such file or directory"))
	if err != nil {
		t.Fatal(err)
	}
	wire, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("source error structured content=%T", result.StructuredContent)
	}
	errorBody, ok := wire["error"].(map[string]any)
	if !ok || errorBody["code"] != "FILE_NOT_FOUND" || errorBody["category"] != "not_found" {
		t.Fatalf("missing-path error=%+v", wire["error"])
	}
	if strings.Contains(strings.ToLower(result.Content[0].(*mcp.TextContent).Text), "statat") {
		t.Fatalf("source error leaked statat typo: %+v", result.Content[0])
	}
}

func TestReadFullLimitIsStructuredAndWindowReadsLargeSource(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	content := append(bytes.Repeat([]byte("x\n"), int(file.MaxSourceBytes/2)), 'z')
	if int64(len(content)) <= file.MaxSourceBytes {
		content = append(content, bytes.Repeat([]byte("x"), int(file.MaxSourceBytes-int64(len(content))+1))...)
	}
	path := filepath.Join(workspace.Path, "large-window.txt")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	full := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{
		"remote_session_id": remoteID, "view": "file", "path": "large-window.txt", "mode": "full",
	})
	if statusOK(full) || errorCode(full) != "file_too_large" {
		t.Fatalf("large full read=%+v", full)
	}
	errorBody, _ := full["error"].(map[string]any)
	if errorBody["category"] != "capacity" {
		t.Fatalf("large full taxonomy=%+v", errorBody)
	}
	details, _ := errorBody["details"].(map[string]any)
	if details["max_source_bytes"] != float64(file.MaxSourceBytes) {
		t.Fatalf("large full details=%+v", details)
	}
	window := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{
		"remote_session_id": remoteID, "view": "file", "path": "large-window.txt", "mode": "window", "offset": 0, "limit": 1,
	})
	if !statusOK(window) {
		t.Fatalf("large window read=%+v", window)
	}
	windowData, _ := window["data"].(map[string]any)
	if windowData["truncated"] != true || windowData["content"] == "" {
		t.Fatalf("large window data=%+v", windowData)
	}
}

func TestReadItemsLimitAndListPathAreStructured(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	if err := os.MkdirAll(filepath.Join(workspace.Path, "scoped"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Path, "scoped", "inside.txt"), []byte("inside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Path, "outside.txt"), []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkAvailable := true
	if err := os.Symlink("scoped", filepath.Join(workspace.Path, "scoped-link")); err != nil {
		symlinkAvailable = false
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)
	items := make([]any, MaxReadItems+1)
	for index := range items {
		items[index] = map[string]any{"path": "scoped/inside.txt", "mode": "window", "limit": 1}
	}
	tooMany := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{
		"remote_session_id": remoteID, "view": "file", "items": items,
	})
	if statusOK(tooMany) || errorCode(tooMany) != "limit_exceeded" {
		t.Fatalf("too many read items=%+v", tooMany)
	}
	errorBody, _ := tooMany["error"].(map[string]any)
	if errorBody["category"] != "validation" {
		t.Fatalf("items taxonomy=%+v", errorBody)
	}
	list := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{
		"remote_session_id": remoteID, "view": "list", "path": "scoped",
	})
	if !statusOK(list) {
		t.Fatalf("scoped list=%+v", list)
	}
	data, _ := list["data"].(map[string]any)
	files, _ := data["files"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["path"] != "scoped/inside.txt" {
		t.Fatalf("path scope leaked: %+v", data)
	}
	rootList := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{
		"remote_session_id": remoteID, "view": "list", "entries_limit": 100,
	})
	if !statusOK(rootList) {
		t.Fatalf("root list=%+v", rootList)
	}
	rootData, _ := rootList["data"].(map[string]any)
	if rootData["entries_scope"] != "." || rootData["entries_complete"] != true || rootData["entries_policy_filtered"] != false {
		t.Fatalf("root direct inventory metadata=%+v", rootData)
	}
	entries, _ := rootData["entries"].([]any)
	kinds := map[string]string{}
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		kinds[entry["path"].(string)] = entry["kind"].(string)
	}
	if kinds["scoped"] != "directory" || kinds["outside.txt"] != "file" || (symlinkAvailable && kinds["scoped-link"] != "symlink") {
		t.Fatalf("root direct inventory types=%+v", kinds)
	}
	pagedRootList := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{
		"remote_session_id": remoteID, "view": "list", "entries_limit": 1,
	})
	pagedData, _ := pagedRootList["data"].(map[string]any)
	if pagedData["entries_complete"] != false || pagedData["entries_next_cursor"] == "" {
		t.Fatalf("paged direct inventory must require continuation: %+v", pagedData)
	}
	next, _ := pagedData["entries_next_action"].(map[string]any)
	arguments, _ := next["arguments"].(map[string]any)
	if next["tool"] != "read" || arguments["view"] != "list" || arguments["entries_cursor"] != pagedData["entries_next_cursor"] {
		t.Fatalf("direct inventory continuation must use public read tool: %+v", next)
	}
}

func TestSingleReadWindowUsesModelResultBudget(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	rt.cfg.Limits.MaxResultBytes = 4
	workspace, _ := rt.reg.Get("demo")
	if err := os.WriteFile(filepath.Join(workspace.Path, "single-budget.txt"), []byte("abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	first := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{
		"remote_session_id": remoteID, "view": "file", "path": "single-budget.txt", "mode": "window", "offset": 0, "limit": 1,
	})
	data, _ := first["data"].(map[string]any)
	firstNext, _ := data["next_action"].(map[string]any)
	firstArgs, _ := firstNext["arguments"].(map[string]any)
	if data["content"] != "abcd" || data["truncated"] != true || firstArgs["line_byte_offset"] != float64(4) {
		t.Fatalf("single read must obey max_result_bytes: %+v", data)
	}

	stricter := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{
		"remote_session_id": remoteID, "view": "file", "path": "single-budget.txt", "mode": "window", "offset": 0, "limit": 1, "max_bytes_per_file": 2,
	})
	strictData, _ := stricter["data"].(map[string]any)
	strictNext, _ := strictData["next_action"].(map[string]any)
	strictArgs, _ := strictNext["arguments"].(map[string]any)
	if strictData["content"] != "ab" || strictArgs["line_byte_offset"] != float64(2) {
		t.Fatalf("max_bytes_per_file must further tighten single read budget: %+v", strictData)
	}
}

func TestReadLongLineNextActionAdvancesWithinLine(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	if err := os.WriteFile(filepath.Join(workspace.Path, "long-line.txt"), []byte("abcdef\nnext\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	first := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{
		"remote_session_id": remoteID,
		"view":              "file",
		"items":             []any{map[string]any{"path": "long-line.txt", "offset": 0, "limit": 2}},
		"max_total_bytes":   3,
	})
	if !statusOK(first) {
		t.Fatalf("first long-line read=%+v", first)
	}
	firstData, _ := first["data"].(map[string]any)
	firstResults, _ := firstData["results"].([]any)
	firstItem, _ := firstResults[0].(map[string]any)
	if firstItem["content"] != "abc" || firstItem["next_offset"] != float64(0) || firstItem["next_line_byte_offset"] != float64(3) {
		t.Fatalf("first long-line fragment=%+v", firstItem)
	}
	next, _ := firstData["next_action"].(map[string]any)
	arguments, _ := next["arguments"].(map[string]any)
	items, _ := arguments["items"].([]any)
	nextItem, _ := items[0].(map[string]any)
	if next["tool"] != "read" || nextItem["offset"] != float64(0) || nextItem["line_byte_offset"] != float64(3) {
		t.Fatalf("long-line next action=%+v", next)
	}

	second := callEnvelope(t, rt.toolRead, context.Background(), arguments)
	if !statusOK(second) {
		t.Fatalf("second long-line read=%+v", second)
	}
	secondData, _ := second["data"].(map[string]any)
	secondResults, _ := secondData["results"].([]any)
	secondItem, _ := secondResults[0].(map[string]any)
	if secondItem["content"] != "def" || secondItem["line_byte_offset"] != float64(3) || secondItem["next_line_byte_offset"] != float64(6) {
		t.Fatalf("second long-line fragment=%+v", secondItem)
	}
}

func TestReadBatchRaisesContinuationBudgetForUTF8Rune(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	workspace, _ := rt.reg.Get("demo")
	if err := os.WriteFile(filepath.Join(workspace.Path, "unicode-line.txt"), []byte("你a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	remoteID := opened["remote_session_id"].(string)

	first := callEnvelope(t, rt.toolRead, context.Background(), map[string]any{
		"remote_session_id": remoteID,
		"view":              "file",
		"items":             []any{map[string]any{"path": "unicode-line.txt", "offset": 0, "limit": 1}},
		"max_total_bytes":   1,
	})
	firstData, _ := first["data"].(map[string]any)
	firstResults, _ := firstData["results"].([]any)
	firstItem, _ := firstResults[0].(map[string]any)
	if firstItem["content"] != "" || firstItem["truncated"] != true {
		t.Fatalf("first unicode fragment=%+v", firstItem)
	}
	firstNext, _ := firstData["next_action"].(map[string]any)
	firstArgs, _ := firstNext["arguments"].(map[string]any)
	if firstArgs["max_total_bytes"] != float64(4) {
		t.Fatalf("unicode recovery budget=%+v", firstNext)
	}

	second := callEnvelope(t, rt.toolRead, context.Background(), firstArgs)
	secondData, _ := second["data"].(map[string]any)
	secondResults, _ := secondData["results"].([]any)
	secondItem, _ := secondResults[0].(map[string]any)
	if secondItem["content"] != "你a" || secondItem["next_line_byte_offset"] != float64(4) {
		t.Fatalf("second unicode fragment=%+v", secondItem)
	}
	secondNext, _ := secondData["next_action"].(map[string]any)
	secondArgs, _ := secondNext["arguments"].(map[string]any)
	third := callEnvelope(t, rt.toolRead, context.Background(), secondArgs)
	thirdData, _ := third["data"].(map[string]any)
	thirdResults, _ := thirdData["results"].([]any)
	thirdItem, _ := thirdResults[0].(map[string]any)
	if thirdItem["content"] != "\n" || thirdItem["truncated"] != false {
		t.Fatalf("final unicode fragment=%+v", thirdItem)
	}
}

func TestSourceReadDisplayIncludesMarkdownSourceBlock(t *testing.T) {
	data := map[string]any{
		"path":        "src/Supplier.vue",
		"content":     "<template>\n  <div />\n</template>\n",
		"rev":         "123456789012345678901234",
		"line_ending": "CRLF",
		"format": map[string]any{
			"charset": "utf-8", "bom": "none", "line_ending": "CRLF",
			"line_ending_counts": map[string]any{"lf": 0, "crlf": 2, "cr": 0},
			"final_newline":      true,
		},
		"offset":      4,
		"limit":       3,
		"total_lines": 10,
	}

	display := sourceReadDisplay(data, "Read src/Supplier.vue (10 lines).")
	for _, want := range []string{"Read src/Supplier.vue", "Revision: `123456789012345678901234`", "### `src/Supplier.vue` (lines 5-7 of 10)", "```vue", "<template>"} {
		if !strings.Contains(display, want) {
			t.Fatalf("source display missing %q: %s", want, display)
		}
	}
	// format/line_ending live in structured fields only — not restated as prose.
	for _, ban := range []string{"换行：", "字符集", "末尾换行", "格式："} {
		if strings.Contains(display, ban) {
			t.Fatalf("source display must not prose-format metadata %q: %s", ban, display)
		}
	}
}

func TestFileReadResultExposesSourceInHostTextAndKeepsStructuredData(t *testing.T) {
	data := map[string]any{
		"path":        "src/Supplier.vue",
		"content":     "<template>\n  <div />\n</template>\n",
		"rev":         "123456789012345678901234",
		"offset":      0,
		"limit":       3,
		"total_lines": 3,
	}
	raw := mcpresult.NewStructured(data, sourceReadDisplay(data, "Read src/Supplier.vue (3 lines)."))
	wrapped := arc.WrapToolResult("file_read", arc.ResultContext{}, raw)
	text, ok := wrapped.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("first content type = %T", wrapped.Content[0])
	}
	for _, want := range []string{"Read src/Supplier.vue", "Revision: `123456789012345678901234`", "```vue", "<template>"} {
		if !strings.Contains(text.Text, want) {
			t.Fatalf("host text missing %q: %s", want, text.Text)
		}
	}
	if _, ok := decodeARCEnvelope(t, wrapped)["mcpx"]; !ok {
		t.Fatalf("ARC metadata was dropped: %#v", wrapped.Meta)
	}
}

func TestSourceReadDisplayIncludesRevisionForEmptyFile(t *testing.T) {
	display := sourceReadDisplay(map[string]any{
		"path": "empty.txt",
		"rev":  "123456789012345678901235",
	}, "Read empty.txt (0 lines).")
	if !strings.Contains(display, "Revision: `123456789012345678901235`") {
		t.Fatalf("empty-file revision missing: %s", display)
	}
}

func TestFileReadFullReturnsHTMLAndDirectImageContent(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	created := callEnvelope(t, rt.toolSessionOpen, context.Background(), map[string]any{"workspace": "demo"})
	remoteSessionID, _ := created["remote_session_id"].(string)
	if remoteSessionID == "" {
		t.Fatalf("session=%+v", created)
	}
	registered, ok := rt.reg.Get("demo")
	if !ok {
		t.Fatal("demo workspace was not registered")
	}

	html := []byte("<!doctype html>\n<html>\n<body>\n" + strings.Repeat("  <p>Preview content</p>\n", 24) + "</body>\n</html>\n")
	if err := os.WriteFile(registered.Path+"/preview.html", html, 0o644); err != nil {
		t.Fatal(err)
	}
	var imageBuffer bytes.Buffer
	preview := image.NewRGBA(image.Rect(0, 0, 2, 2))
	preview.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&imageBuffer, preview); err != nil {
		t.Fatal(err)
	}
	imageBytes := imageBuffer.Bytes()
	if err := os.WriteFile(registered.Path+"/preview.png", imageBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	read := func(path string) *mcp.CallToolResult {
		t.Helper()
		request := mcpresult.Request(map[string]any{
			"intent":            "读取客户端预览文件",
			"remote_session_id": remoteSessionID,
			"path":              path,
			"mode":              "full",
		})

		result, err := rt.toolFileReadUnified(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	htmlResult := read("preview.html")
	htmlText, ok := htmlResult.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(htmlText.Text, "```html\n"+string(html)+"```") || !strings.Contains(htmlText.Text, "Revision: `") {
		t.Fatalf("full HTML was not returned directly: %#v", htmlResult.Content)
	}
	htmlData := structuredBusinessData(htmlResult)
	if htmlData["mode"] != "full" || htmlData["mime_type"] != "text/html" || htmlData["encoding"] != "utf-8" {
		t.Fatalf("HTML metadata=%+v", htmlResult.StructuredContent)
	}
	wrappedHTML := arc.WrapToolResult("file_read", arc.ResultContext{}, htmlResult)
	wrappedHTMLText, ok := wrappedHTML.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(wrappedHTMLText.Text, string(html)) || !strings.Contains(wrappedHTMLText.Text, "</html>\n```") {
		t.Fatalf("ARC dropped full HTML content: %#v", wrappedHTML.Content)
	}

	imageResult := read("preview.png")
	if len(imageResult.Content) != 2 {
		t.Fatalf("image result content=%#v", imageResult.Content)
	}
	imageContent, ok := imageResult.Content[1].(*mcp.ImageContent)
	if !ok || imageContent.MIMEType != "image/png" {
		t.Fatalf("image content=%T %#v", imageResult.Content[1], imageResult.Content[1])
	}
	decoded := imageContent.Data
	if !bytes.Equal(decoded, imageBytes) {
		t.Fatalf("image bytes changed: size=%d/%d", len(decoded), len(imageBytes))
	}

	wrapped := arc.WrapToolResult("file_read", arc.ResultContext{}, imageResult)
	if len(wrapped.Content) != 2 {
		t.Fatalf("wrapped image content=%#v", wrapped.Content)
	}
	if _, ok := wrapped.Content[1].(*mcp.ImageContent); !ok {
		t.Fatalf("ARC dropped direct image content: %T", wrapped.Content[1])
	}
}

func TestFileReadDecodesUTF16ForModelAndWindow(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	created := callEnvelope(t, rt.toolSessionOpen, context.Background(), map[string]any{"workspace": "demo"})
	remoteSessionID, _ := created["remote_session_id"].(string)
	registered, ok := rt.reg.Get("demo")
	if !ok {
		t.Fatal("demo workspace was not registered")
	}
	content := []byte{0xff, 0xfe, 'o', 0, 'n', 0, 'e', 0, '\n', 0, 't', 0, 'w', 0, 'o', 0, '\n', 0}
	if err := os.WriteFile(registered.Path+"/utf16.txt", content, 0o644); err != nil {
		t.Fatal(err)
	}
	fullResult, err := rt.toolFileReadUnified(context.Background(), mcpresult.Request(map[string]any{
		"remote_session_id": remoteSessionID, "path": "utf16.txt", "mode": "full",
	}))
	if err != nil {
		t.Fatal(err)
	}
	full := structuredBusinessData(fullResult)
	if full["content"] != "one\ntwo\n" || full["encoding"] != "utf-8" {
		t.Fatalf("UTF-16 full result=%+v", full)
	}
	format, _ := full["format"].(map[string]any)
	if format["charset"] != "utf-16le" || format["bom"] != "utf-16le" {
		t.Fatalf("UTF-16 format=%+v", format)
	}
	windowResult, err := rt.toolFileReadUnified(context.Background(), mcpresult.Request(map[string]any{
		"remote_session_id": remoteSessionID, "path": "utf16.txt", "mode": "window", "offset": 1, "limit": 1,
	}))
	if err != nil {
		t.Fatal(err)
	}
	window := structuredBusinessData(windowResult)
	if window["content"] != "two\n" || window["rev"] != full["rev"] {
		t.Fatalf("UTF-16 window result=%+v full=%+v", window, full)
	}
}

func TestSourceReadWindowAndBatchExposeSameFormat(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	created := callEnvelope(t, rt.toolSessionOpen, context.Background(), map[string]any{"workspace": "demo"})
	remoteSessionID, _ := created["remote_session_id"].(string)
	registered, ok := rt.reg.Get("demo")
	if !ok {
		t.Fatal("demo workspace was not registered")
	}
	content := []byte("one\r\ntwo\r\n")
	if err := os.WriteFile(registered.Path+"/format.txt", content, 0o644); err != nil {
		t.Fatal(err)
	}

	read := func(arguments map[string]any) map[string]any {
		t.Helper()
		request := mcpresult.Request(map[string]any{})
		request = mcpresult.Request(arguments)
		result, err := rt.toolFileReadUnified(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		data := structuredBusinessData(result)
		if data == nil {
			t.Fatalf("structured data=%T", result.StructuredContent)
		}
		return data
	}
	base := map[string]any{"remote_session_id": remoteSessionID, "path": "format.txt", "mode": "window", "limit": 10}
	window := read(base)
	windowFormat, ok := window["format"].(map[string]any)
	if !ok {
		t.Fatalf("window format=%T %+v", window["format"], window)
	}
	if windowFormat["line_ending"] != "CRLF" || windowFormat["charset"] != "utf-8" {
		t.Fatalf("window format=%+v", windowFormat)
	}

	batch := read(map[string]any{
		"remote_session_id": remoteSessionID,
		"mode":              "window",
		"items":             []any{map[string]any{"path": "format.txt", "limit": 10}},
	})
	results := asMapSlice(batch["results"])
	if len(results) != 1 {
		t.Fatalf("batch results=%T %+v", batch["results"], batch)
	}
	if !reflect.DeepEqual(windowFormat, results[0]["format"]) {
		t.Fatalf("window/batch format mismatch: window=%+v batch=%+v", windowFormat, results[0]["format"])
	}

	full := read(map[string]any{"remote_session_id": remoteSessionID, "path": "format.txt", "mode": "full"})
	if !reflect.DeepEqual(windowFormat, full["format"]) {
		t.Fatalf("window/full format mismatch: window=%+v full=%+v", windowFormat, full["format"])
	}
}
