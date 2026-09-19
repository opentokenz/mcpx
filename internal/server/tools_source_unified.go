package server

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"

	"mcpx/internal/config"
	"mcpx/internal/envelope"
	"mcpx/internal/file"
	"mcpx/internal/instruction"
	"mcpx/internal/remotesession"
	"mcpx/internal/security"
	"mcpx/internal/source"
)

func nextAction(tool string, arguments map[string]any) map[string]any {
	return nextActionWithReason(tool, "continue with the returned operation", arguments)
}

// toolFileReadUnified is the sole public source-read entry. A single path
// retains the concise shape, while items[] runs the same bounded batch reader
// and preserves per-item failures rather than failing the whole call.
func (r *Runtime) toolFileReadUnified(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, session, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	mode := sourceReadMode(envReq.Payload)
	if mode != "window" && mode != "full" {
		return r.sourceError(envReq, session.ID, session.WorkspaceName, fmt.Errorf("unsupported read mode %q", mode))
	}
	if raw, ok := envReq.Payload["items"].([]any); ok && len(raw) > 0 {
		mixedFull := mode == "full"
		for _, value := range raw {
			if item, ok := value.(map[string]any); ok {
				itemMode, _ := item["mode"].(string)
				if strings.EqualFold(strings.TrimSpace(itemMode), "full") {
					mixedFull = true
				}
			}
		}
		if mixedFull {
			return r.toolFileReadMixedBatch(ctx, envReq, session, raw, mode)
		}
		if len(raw) > MaxReadItems {
			return r.sourceError(envReq, session.ID, session.WorkspaceName, &source.LimitError{Resource: "read.items", Actual: len(raw), Max: MaxReadItems})
		}
		items := make([]source.BatchReadRequest, 0, len(raw))
		for _, value := range raw {
			item, ok := value.(map[string]any)
			if !ok {
				return r.sourceError(envReq, session.ID, session.WorkspaceName, fmt.Errorf("items must contain objects"))
			}
			path, _ := item["path"].(string)
			if strings.TrimSpace(path) == "" {
				return r.sourceError(envReq, session.ID, session.WorkspaceName, fmt.Errorf("item path is required"))
			}
			items = append(items, source.BatchReadRequest{Path: path, Offset: intPayload(item, "offset"), LineByteOffset: intPayload(item, "line_byte_offset"), Limit: intPayload(item, "limit")})
		}
		effective := r.effectiveConfig(session.WorkspacePath)
		budget := intPayload(envReq.Payload, "max_total_bytes")
		if budget <= 0 {
			budget = config.MaxResultBytes(effective.Limits)
		}
		batch := source.ReadBatch(session.WorkspacePath, items, effective.Security.Files.MaxReadBytes, budget, r.sourcePathAllowed(session.WorkspacePath))
		results := make([]map[string]any, 0, len(batch.Results))
		for _, item := range batch.Results {
			entry := map[string]any{
				"path": item.Path, "ok": item.OK, "content": item.Content, "rev": compactFileRevision(item.SHA256), "line_ending": item.LineEnding,
				"format": formatMap(item.Format),
				"offset": item.Offset, "limit": item.Limit, "total_lines": item.TotalLines, "truncated": item.Truncated,
			}
			if item.LineByteOffset > 0 {
				entry["line_byte_offset"] = item.LineByteOffset
			}
			if item.Truncated {
				setNextReadCursor(entry, item.Offset, item.LineByteOffset, item.Content)
			}
			if item.Error != "" {
				code, category := "READ_FAILED", "runtime"
				if strings.Contains(item.Error, "denied") {
					code, category = "FILE_DENIED", "security"
				} else if strings.Contains(strings.ToLower(item.Error), "not exist") || strings.Contains(strings.ToLower(item.Error), "no such file") || strings.Contains(strings.ToLower(item.Error), "not found") {
					code, category = "NOT_FOUND", "not_found"
				} else if strings.Contains(item.Error, "budget") {
					code, category = "RESULT_BUDGET_EXCEEDED", "validation"
				}
				details := map[string]any{}
				if code == "NOT_FOUND" {
					details["next_action"] = nextActionWithReason("read", "locate this path before retrying read", map[string]any{
						"remote_session_id": session.ID,
						"view":              "list",
					})
				}
				entry["error"] = map[string]any{"code": code, "message": item.Error, "category": category, "retryable": code == "RESULT_BUDGET_EXCEEDED", "details": details}
			}
			results = append(results, entry)
		}
		data := map[string]any{
			"results": results, "total_bytes": batch.TotalBytes, "budget_bytes": batch.BudgetBytes, "truncated": batch.Truncated,
		}
		if batch.Truncated {
			data["continue_from"] = batch.ContinueFrom
			items := make([]map[string]any, 0, len(batch.ContinueRequests))
			for _, item := range batch.ContinueRequests {
				arguments := map[string]any{"path": item.Path, "offset": item.Offset, "limit": item.Limit}
				if item.LineByteOffset > 0 {
					arguments["line_byte_offset"] = item.LineByteOffset
				}
				items = append(items, arguments)
			}
			data["next_action"] = nextAction("read", map[string]any{
				"remote_session_id": session.ID, "items": items, "max_total_bytes": readBatchContinuationBudget(batch, budget),
			})
		}
		summary := fmt.Sprintf("Read %d source item(s); %d bytes returned.", len(results), batch.TotalBytes)
		return compactToolResult(data, sourceReadDisplay(data, summary)), nil
	}

	path, _ := envReq.Payload["path"].(string)
	if path == "" {
		return r.sourceError(envReq, session.ID, session.WorkspaceName, fmt.Errorf("path or items is required"))
	}
	if mode == "full" {
		return r.toolFileReadFull(envReq, session.ID, session.WorkspaceName, session.WorkspacePath, path)
	}
	if security.MatchFile(r.effectiveConfig(session.WorkspacePath).Security.Files, path) != security.Allow {
		response := envelope.Fail(envelope.StatusDenied, envReq.RequestID, session.WorkspaceName, map[string]any{"path": path}, "FILE_DENIED", "file denied by policy")
		response.RemoteSessionID = session.ID
		return r.resultJSON(response)
	}
	effective := r.effectiveConfig(session.WorkspacePath)
	maxBytes := readWindowMaxBytes(effective, intPayload(envReq.Payload, "max_bytes_per_file"))
	read, err := source.ReadWindow(session.WorkspacePath, path, intPayload(envReq.Payload, "offset"), intPayload(envReq.Payload, "line_byte_offset"), intPayload(envReq.Payload, "limit"), maxBytes)
	if err != nil {
		return r.sourceError(envReq, session.ID, session.WorkspaceName, err)
	}
	data := map[string]any{
		"path": read.Path, "content": read.Content, "rev": compactFileRevision(read.SHA256), "line_ending": read.LineEnding,
		"format": formatMap(read.Format),
		"offset": read.Offset, "limit": read.Limit, "total_lines": read.TotalLines, "truncated": read.Truncated,
	}
	if read.LineByteOffset > 0 {
		data["line_byte_offset"] = read.LineByteOffset
	}
	if read.Truncated {
		data["next_action"] = nextAction("read", readContinuationArguments(session.ID, path, read.Offset, read.LineByteOffset, read.Content, read.Limit))
	}
	summary := fmt.Sprintf("Read %s (%d lines).", path, read.TotalLines)
	return compactToolResult(data, sourceReadDisplay(data, summary)), nil
}

func (r *Runtime) toolFileReadFull(envReq envelope.Request, remoteSessionID, workspace, workspacePath, path string) (*mcp.CallToolResult, error) {
	if security.MatchFile(r.effectiveConfig(workspacePath).Security.Files, path) != security.Allow {
		response := envelope.Fail(envelope.StatusDenied, envReq.RequestID, workspace, map[string]any{"path": path}, "FILE_DENIED", "file denied by policy")
		response.RemoteSessionID = remoteSessionID
		return r.resultJSON(response)
	}
	read, err := file.ReadFull(file.FullReadOptions{
		WorkspaceRoot: workspacePath,
		Path:          path,
		MaxBytes:      file.MaxSourceBytes,
	})
	if err != nil {
		return r.sourceError(envReq, remoteSessionID, workspace, err)
	}
	data := fullFileReadData(read)
	summary := fmt.Sprintf("Read %s in full (%d bytes, %s).", path, read.Size, read.MIMEType)
	result := compactToolResult(data, fullFileReadDisplay(read, data, summary))
	if strings.HasPrefix(read.MIMEType, "image/") {
		result.Content = append(result.Content, mcpresult.NewImage(read.Content, read.MIMEType))
	}
	return result, nil
}

func sourceReadMode(payload map[string]any) string {
	mode, _ := payload["mode"].(string)
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return "window"
	}
	return mode
}

// toolFileReadMixedBatch is used only when an items[] request contains a full
// read. The common window-only path above remains source.ReadBatch's bounded,
// continuation-aware fast path.
func (r *Runtime) toolFileReadMixedBatch(_ context.Context, envReq envelope.Request, session remotesession.Session, raw []any, defaultMode string) (*mcp.CallToolResult, error) {
	if len(raw) > MaxReadItems {
		return r.sourceError(envReq, session.ID, session.WorkspaceName, &source.LimitError{Resource: "read.items", Actual: len(raw), Max: MaxReadItems})
	}
	effective := r.effectiveConfig(session.WorkspacePath)
	budget := intPayload(envReq.Payload, "max_total_bytes")
	if budget <= 0 {
		budget = config.MaxResultBytes(effective.Limits)
	}
	if budget <= 0 {
		budget = 1 << 20
	}
	used := 0
	truncated := false
	results := make([]map[string]any, 0, len(raw))
	for _, value := range raw {
		item, ok := value.(map[string]any)
		if !ok {
			return r.sourceError(envReq, session.ID, session.WorkspaceName, fmt.Errorf("items must contain objects"))
		}
		path, _ := item["path"].(string)
		if strings.TrimSpace(path) == "" {
			return r.sourceError(envReq, session.ID, session.WorkspaceName, fmt.Errorf("item path is required"))
		}
		itemMode, _ := item["mode"].(string)
		itemMode = strings.ToLower(strings.TrimSpace(itemMode))
		if itemMode == "" {
			itemMode = defaultMode
		}
		if itemMode != "full" && itemMode != "window" {
			return r.sourceError(envReq, session.ID, session.WorkspaceName, fmt.Errorf("unsupported item mode %q", itemMode))
		}
		if security.MatchFile(effective.Security.Files, path) != security.Allow {
			results = append(results, map[string]any{
				"path": path, "ok": false,
				"error": map[string]any{"code": "FILE_DENIED", "message": "file denied by policy"},
			})
			continue
		}
		remaining := budget - used
		if remaining <= 0 {
			truncated = true
			results = append(results, map[string]any{
				"path": path, "ok": false,
				"error": map[string]any{"code": "RESULT_BUDGET_EXCEEDED", "message": "batch result budget exhausted"},
			})
			continue
		}
		if itemMode == "full" {
			read, err := file.ReadFull(file.FullReadOptions{WorkspaceRoot: session.WorkspacePath, Path: path, MaxBytes: file.MaxSourceBytes})
			if err != nil {
				results = append(results, map[string]any{
					"path": path, "ok": false,
					"error": sourceReadItemError(err, path, session.ID),
				})
				continue
			}
			if len(read.Content) > remaining {
				truncated = true
				results = append(results, map[string]any{
					"path": path, "ok": false,
					"error": map[string]any{"code": "RESULT_BUDGET_EXCEEDED", "message": "full item exceeds the remaining batch result budget", "category": "validation", "retryable": true, "details": map[string]any{"remaining_bytes": remaining, "size_bytes": len(read.Content)}},
				})
				continue
			}
			data := fullFileReadData(read)
			data["ok"] = true
			results = append(results, data)
			used += len(read.Content)
			continue
		}

		maxBytes := effective.Security.Files.MaxReadBytes
		if maxBytes <= 0 || maxBytes > int64(remaining) {
			maxBytes = int64(remaining)
		}
		read, err := source.ReadWindow(session.WorkspacePath, path, intPayload(item, "offset"), intPayload(item, "line_byte_offset"), intPayload(item, "limit"), maxBytes)
		if err != nil {
			results = append(results, map[string]any{
				"path": path, "ok": false,
				"error": map[string]any{"code": "READ_FAILED", "message": err.Error()},
			})
			continue
		}
		entry := map[string]any{
			"path": read.Path, "mode": "window", "ok": true, "content": read.Content,
			"rev": compactFileRevision(read.SHA256), "line_ending": read.LineEnding, "format": formatMap(read.Format),
			"offset": read.Offset, "limit": read.Limit, "total_lines": read.TotalLines, "truncated": read.Truncated,
		}
		if read.LineByteOffset > 0 {
			entry["line_byte_offset"] = read.LineByteOffset
		}
		if read.Truncated {
			truncated = true
			setNextReadCursor(entry, read.Offset, read.LineByteOffset, read.Content)
		}
		results = append(results, entry)
		used += len(read.Content)
	}
	data := map[string]any{
		"results": results, "total_bytes": used, "budget_bytes": budget, "truncated": truncated,
	}
	return compactToolResult(data, sourceReadDisplay(data, fmt.Sprintf("Read %d source item(s); %d bytes returned.", len(results), used))), nil
}

func readBatchContinuationBudget(batch source.BatchReadResult, budget int) int {
	if budget >= utf8.UTFMax {
		return budget
	}
	for _, item := range batch.Results {
		if item.Truncated && item.Error == "" && item.Content == "" {
			return utf8.UTFMax
		}
	}
	return budget
}

func setNextReadCursor(target map[string]any, offset, lineByteOffset int, content string) {
	nextOffset, nextLineByteOffset := source.AdvanceReadCursor(offset, lineByteOffset, content)
	target["next_offset"] = nextOffset
	if nextLineByteOffset > 0 {
		target["next_line_byte_offset"] = nextLineByteOffset
	}
}

func readWindowMaxBytes(effective config.Config, requested int) int64 {
	maxBytes := effective.Security.Files.MaxReadBytes
	resultBudget := int64(config.MaxResultBytes(effective.Limits))
	if resultBudget > 0 && (maxBytes <= 0 || resultBudget < maxBytes) {
		maxBytes = resultBudget
	}
	if requested > 0 && (maxBytes <= 0 || int64(requested) < maxBytes) {
		maxBytes = int64(requested)
	}
	return maxBytes
}

func readContinuationArguments(remoteSessionID, path string, offset, lineByteOffset int, content string, limit int) map[string]any {
	nextOffset, nextLineByteOffset := source.AdvanceReadCursor(offset, lineByteOffset, content)
	arguments := map[string]any{
		"remote_session_id": remoteSessionID,
		"view":              "file",
		"path":              path,
		"offset":            nextOffset,
		"limit":             limit,
	}
	if nextLineByteOffset > 0 {
		arguments["line_byte_offset"] = nextLineByteOffset
	}
	return arguments
}

func fullFileReadData(read file.FullReadResult) map[string]any {
	data := map[string]any{
		"path":        read.Path,
		"mode":        "full",
		"mime_type":   read.MIMEType,
		"size_bytes":  read.Size,
		"line_ending": read.LineEnding,
		"format":      formatMap(read.Format),
		"rev":         compactFileRevision(read.SHA256),
	}
	if strings.HasPrefix(read.MIMEType, "image/") && read.MIMEType != "image/svg+xml" {
		data["encoding"] = "base64"
		return data
	}
	if fullReadIsText(read.MIMEType, read.Content) {
		content, decodedFormat, decodeErr := file.DecodeText(read.Content)
		if decodeErr != nil {
			data["content"] = base64.StdEncoding.EncodeToString(read.Content)
			data["encoding"] = "base64"
			data["encoding_error"] = "UNSUPPORTED_ENCODING"
			return data
		}
		data["line_ending"] = decodedFormat.LineEnding
		data["format"] = formatMap(decodedFormat)
		data["content"] = content
		data["encoding"] = "utf-8"
		data["total_lines"] = fullReadLineCount(content)
		return data
	}
	data["content"] = base64.StdEncoding.EncodeToString(read.Content)
	data["encoding"] = "base64"
	return data
}

func fullReadIsText(mimeType string, content []byte) bool {
	isTextMIME := strings.HasPrefix(mimeType, "text/") ||
		strings.Contains(mimeType, "json") ||
		strings.Contains(mimeType, "xml") ||
		strings.Contains(mimeType, "yaml") ||
		strings.Contains(mimeType, "javascript")
	format := file.DetectFormat(content)
	if format.Charset == "utf-16le" || format.Charset == "utf-16be" {
		return true
	}
	return isTextMIME
}

func fullReadLineCount(content string) int {
	if content == "" {
		return 0
	}
	lines := strings.Count(content, "\n")
	if !strings.HasSuffix(content, "\n") {
		lines++
	}
	return lines
}

func formatMap(format file.Format) map[string]any {
	return map[string]any{
		"charset":     format.Charset,
		"bom":         format.BOM,
		"line_ending": format.LineEnding,
		"line_ending_counts": map[string]any{
			"lf":   format.LineEndingCounts.LF,
			"crlf": format.LineEndingCounts.CRLF,
			"cr":   format.LineEndingCounts.CR,
		},
		"final_newline": format.FinalNewline,
	}
}

func fullFileReadDisplay(read file.FullReadResult, data map[string]any, summary string) string {
	if read.MIMEType == "text/html" {
		content, _, err := file.DecodeText(read.Content)
		if err != nil {
			return summary
		}
		fence := "```"
		if strings.Contains(content, fence) {
			fence = "````"
		}
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		// format/line_ending are structured fields only; text keeps Revision for copy.
		return fence + "html\n" + content + fence + "\n\nRevision: `" + read.SHA256 + "`"
	}
	return sourceReadDisplay(data, summary)
}

// sourceReadDisplay is the host/model-facing representation of source_read.
// The public ARC wrapper keeps the complete machine data in response metadata;
// the first text content remains useful to a terminal agent without requiring
// it to decode a protocol envelope before it can inspect source code.
func sourceReadDisplay(data map[string]any, summary string) string {
	var builder strings.Builder
	if summary != "" {
		builder.WriteString(summary)
	}
	for _, item := range sourceReadItems(data) {
		path, _ := item["path"].(string)
		content, _ := item["content"].(string)
		if path == "" {
			continue
		}
		if content == "" {
			if errValue, ok := item["error"].(map[string]any); ok {
				if message, _ := errValue["message"].(string); message != "" {
					fmt.Fprintf(&builder, "\n\n`%s`: %s", path, message)
				}
			}
			if revision, _ := item["rev"].(string); strings.TrimSpace(revision) != "" {
				fmt.Fprintf(&builder, "\n\nRevision: `%s`", revision)
			}
			continue
		}

		start := sourceReadNumber(item["offset"]) + 1
		shownLines := strings.Count(content, "\n")
		if shownLines == 0 {
			shownLines = 1
		}
		end := start + shownLines - 1
		lineLabel := fmt.Sprintf("lines %d-%d", start, end)
		if total := sourceReadNumber(item["total_lines"]); total > 0 {
			lineLabel += fmt.Sprintf(" of %d", total)
		}

		fmt.Fprintf(&builder, "\n\n### `%s` (%s)\n\n```%s\n", path, lineLabel, sourceReadLanguage(path))
		builder.WriteString(content)
		if !strings.HasSuffix(content, "\n") {
			builder.WriteByte('\n')
		}
		builder.WriteString("```")
		if truncated, _ := item["truncated"].(bool); truncated {
			builder.WriteString("\n\n> 内容已截断；请继续调用 `read(view=file)` 读取后续内容。")
		}
		// Keep Revision in text so terminal agents that only read content can
		// copy rev. format/line_ending/charset live only in structured
		// fields (data.format / data.line_ending) — do not restate as prose.
		if revision, _ := item["rev"].(string); strings.TrimSpace(revision) != "" {
			fmt.Fprintf(&builder, "\n\nRevision: `%s`", revision)
		}
	}
	return builder.String()
}

func sourceReadItems(data map[string]any) []map[string]any {
	if raw, ok := data["results"]; ok {
		switch items := raw.(type) {
		case []map[string]any:
			return items
		case []any:
			result := make([]map[string]any, 0, len(items))
			for _, rawItem := range items {
				if item, ok := rawItem.(map[string]any); ok {
					result = append(result, item)
				}
			}
			return result
		}
	}
	return []map[string]any{data}
}

func sourceReadNumber(value any) int {
	switch number := value.(type) {
	case int:
		return number
	case int64:
		return int(number)
	case float64:
		return int(number)
	default:
		return 0
	}
}

func sourceReadLanguage(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".js", ".jsx":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".vue":
		return "vue"
	case ".java":
		return "java"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".md":
		return "markdown"
	case ".css", ".scss":
		return "css"
	case ".html", ".htm":
		return "html"
	default:
		return "text"
	}
}

func (r *Runtime) toolContextQueryUnified(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	action := toolAction(req)
	if action == "query" {
		return r.toolContextQueryAction(ctx, req)
	}
	if action == "search" {
		return r.toolContextSearchAction(ctx, req)
	}
	if action == "list" {
		return r.toolContextListAction(ctx, req)
	}
	return r.invalidAction(ctx, req, "context_query", action)
}

func (r *Runtime) toolContextQueryAction(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, session, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	query, _ := envReq.Payload["query"].(string)
	if strings.TrimSpace(query) == "" {
		return r.sourceError(envReq, session.ID, session.WorkspaceName, fmt.Errorf("query is required"))
	}
	mode := sourcePayloadString(envReq.Payload, "mode")
	if mode == "" {
		mode = "smart"
	}
	parallel := true
	if _, exists := envReq.Payload["parallel"]; exists {
		parallel = boolPayload(envReq.Payload, "parallel")
	}
	maxResults := intPayload(envReq.Payload, "max_results")
	seeds := sourcePayloadPaths(envReq.Payload)
	include, _ := envReq.Payload["include_glob"].(string)
	exclude, _ := envReq.Payload["exclude_glob"].(string)
	allowed := r.sourcePathAllowedWithGlobs(session.WorkspacePath, include, exclude)
	maxBytes := r.effectiveConfig(session.WorkspacePath).Security.Files.MaxReadBytes
	if requested := intPayload(envReq.Payload, "max_bytes_per_file"); requested > 0 && int64(requested) < maxBytes {
		maxBytes = int64(requested)
	}
	data, err := source.SmartQueryPage(session.WorkspacePath, source.SmartQueryOptions{
		Query: query, Mode: mode, Parallel: parallel, MaxResults: maxResults,
		Cursor: sourcePayloadString(envReq.Payload, "cursor"), Pattern: include, ExcludePattern: exclude,
		ContextBefore: intPayload(envReq.Payload, "context_before"), ContextAfter: intPayload(envReq.Payload, "context_after"),
		MaxBytesPerFile: maxBytes, IncludeSHA256: true, ScopePaths: seeds, Allowed: allowed,
	})
	if err != nil {
		return r.sourceError(envReq, session.ID, session.WorkspaceName, err)
	}
	if include, _ := envReq.Payload["include_instructions"].(bool); include {
		anchor := ""
		if len(seeds) > 0 {
			anchor = seeds[0]
		}
		docs := instruction.DiscoverAt(r.cfg.Discovery.Instructions.GlobalAgentsPath, session.WorkspacePath, anchor, r.effectiveConfig(session.WorkspacePath).Security.Files.MaxReadBytes)
		data["instructions"] = docs
	}
	if truncated, _ := data["truncated"].(bool); truncated {
		data["next_action"] = nextAction("context_query", map[string]any{
			"remote_session_id": session.ID, "action": "query", "query": query, "paths": seeds,
			"mode": mode, "parallel": parallel, "max_results": maxResults, "cursor": data["next_cursor"],
			"max_bytes_per_file": intPayload(envReq.Payload, "max_bytes_per_file"),
			"include_glob":       sourcePayloadString(envReq.Payload, "include_glob"),
			"exclude_glob":       sourcePayloadString(envReq.Payload, "exclude_glob"),
		})
	}
	files, _ := data["files"].([]map[string]any)
	data, _ = compactRevisionPayload(data).(map[string]any)
	return compactToolResult(data, fmt.Sprintf("Context query returned %d file(s).", len(files))), nil
}

func (r *Runtime) toolContextSearchAction(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, session, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	query, _ := envReq.Payload["query"].(string)
	pattern, _ := envReq.Payload["include_glob"].(string)
	regex, _ := envReq.Payload["regex"].(bool)
	caseSensitive, setCase := envReq.Payload["case_sensitive"].(bool)
	if !setCase {
		caseSensitive = true // retain existing source search behaviour by default.
	}
	seeds := sourcePayloadPaths(envReq.Payload)
	resultData, err := source.SearchWith(session.WorkspacePath, source.SearchOptions{
		Query: query, Pattern: pattern, ExcludePattern: sourcePayloadString(envReq.Payload, "exclude_glob"), ScopePaths: seeds, Cursor: sourcePayloadString(envReq.Payload, "cursor"), Regex: regex,
		CaseSensitive: caseSensitive, Limit: intPayload(envReq.Payload, "limit"), ContextBefore: intPayload(envReq.Payload, "context_before"), ContextAfter: intPayload(envReq.Payload, "context_after"), IncludeSHA256: true,
	}, r.sourcePathAllowed(session.WorkspacePath))
	if err != nil {
		return r.sourceError(envReq, session.ID, session.WorkspaceName, err)
	}
	data := map[string]any{"matches": compactRevisionPayload(resultData.Matches), "truncated": resultData.Truncated}
	if resultData.NextCursor != "" {
		data["next_cursor"] = resultData.NextCursor
		data["next_action"] = nextAction("context_query", map[string]any{
			"remote_session_id": session.ID, "action": "search", "query": query,
			"paths":  seeds,
			"cursor": resultData.NextCursor, "limit": intPayload(envReq.Payload, "limit"),
			"include_glob": pattern, "exclude_glob": sourcePayloadString(envReq.Payload, "exclude_glob"),
			"regex": regex, "case_sensitive": caseSensitive,
			"context_before": intPayload(envReq.Payload, "context_before"),
			"context_after":  intPayload(envReq.Payload, "context_after"),
		})
	}
	return compactToolResult(data, fmt.Sprintf("Source search returned %d match(es).", len(resultData.Matches))), nil
}

func sourcePayloadPaths(payload map[string]any) []string {
	raw, _ := payload["paths"].([]any)
	paths := make([]string, 0, len(raw))
	for _, value := range raw {
		if path, ok := value.(string); ok && strings.TrimSpace(path) != "" {
			paths = append(paths, strings.TrimSpace(path))
		}
	}
	return paths
}

func (r *Runtime) toolContextListAction(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, session, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	pattern := sourcePayloadString(envReq.Payload, "include_glob")
	baseAllowed := r.sourcePathAllowed(session.WorkspacePath)
	scopeAllowed, err := hardListScope(session.WorkspacePath, sourcePayloadString(envReq.Payload, "path"))
	if err != nil {
		return r.sourceError(envReq, session.ID, session.WorkspaceName, err)
	}
	allowed := baseAllowed
	if scopeAllowed != nil {
		allowed = func(candidate string) bool { return baseAllowed(candidate) && scopeAllowed(candidate) }
	}
	directLimit := intPayload(envReq.Payload, "entries_limit")
	if directLimit <= 0 || directLimit > source.MaxDirectListEntries {
		directLimit = source.DefaultDirectListEntries
	}
	direct, err := source.ListDirect(session.WorkspacePath, sourcePayloadString(envReq.Payload, "path"), sourcePayloadString(envReq.Payload, "entries_cursor"), directLimit, allowed)
	if err != nil {
		return r.sourceError(envReq, session.ID, session.WorkspaceName, err)
	}
	list, err := source.ListWith(session.WorkspacePath, pattern, sourcePayloadString(envReq.Payload, "exclude_glob"), sourcePayloadString(envReq.Payload, "cursor"), intPayload(envReq.Payload, "limit"), false, allowed)
	if err != nil {
		return r.sourceError(envReq, session.ID, session.WorkspaceName, err)
	}
	directScope := strings.TrimSpace(sourcePayloadString(envReq.Payload, "path"))
	if directScope == "" || directScope == "." {
		directScope = "."
	}
	data := map[string]any{
		"entries":                 direct.Entries,
		"entries_scope":           directScope,
		"entries_total":           direct.Total,
		"entries_limit":           directLimit,
		"entries_complete":        direct.NextCursor == "" && !direct.PolicyFiltered,
		"entries_policy_filtered": direct.PolicyFiltered,
		"files":                   list.Files,
		"total":                   list.Total,
	}
	if list.NextCursor != "" {
		data["next_cursor"] = list.NextCursor
		data["next_action"] = nextAction("read", map[string]any{
			"remote_session_id": session.ID, "view": "list", "path": sourcePayloadString(envReq.Payload, "path"), "include_glob": pattern,
			"exclude_glob": sourcePayloadString(envReq.Payload, "exclude_glob"), "cursor": list.NextCursor,
			"limit": intPayload(envReq.Payload, "limit"),
		})
	}
	if direct.NextCursor != "" {
		data["entries_next_cursor"] = direct.NextCursor
		data["entries_next_action"] = nextAction("read", map[string]any{
			"remote_session_id": session.ID,
			"view":              "list",
			"path":              sourcePayloadString(envReq.Payload, "path"),
			"entries_cursor":    direct.NextCursor,
			"entries_limit":     directLimit,
		})
	}
	return compactToolResult(data, fmt.Sprintf("Source list returned %d direct entry(s) and %d of %d recursive file(s).", direct.Total, len(list.Files), list.Total)), nil
}

// hardListScope turns read(list).path into an actual boundary. include_glob
// remains an additional filter; it can narrow this scope but can never widen
// it. The scope is lexical after file.Resolve has rejected traversal and
// symlink escapes, so a directory named "src" cannot leak sibling files.
func hardListScope(workspaceRoot, requested string) (func(string) bool, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return nil, nil
	}
	absolute, err := file.Resolve(workspaceRoot, requested)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, err
	}
	relative := filepath.ToSlash(filepath.Clean(requested))
	if relative == "." {
		relative = ""
	}
	if info.IsDir() {
		return func(candidate string) bool {
			candidate = filepath.ToSlash(filepath.Clean(candidate))
			return relative == "" || candidate == relative || strings.HasPrefix(candidate, relative+"/")
		}, nil
	}
	return func(candidate string) bool {
		return filepath.ToSlash(filepath.Clean(candidate)) == relative
	}, nil
}

func (r *Runtime) sourcePathAllowedWithGlobs(workspacePath, include, exclude string) func(string) bool {
	base := r.sourcePathAllowed(workspacePath)
	include = strings.TrimSpace(include)
	exclude = strings.TrimSpace(exclude)
	return func(path string) bool {
		if !base(path) {
			return false
		}
		if include != "" {
			matched, err := source.MatchGlob(include, path)
			if err != nil || !matched {
				return false
			}
		}
		if exclude != "" {
			matched, err := source.MatchGlob(exclude, path)
			if err == nil && matched {
				return false
			}
		}
		return true
	}
}

func sourcePayloadString(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}

func boolPayload(payload map[string]any, key string) bool {
	value, _ := payload[key].(bool)
	return value
}
