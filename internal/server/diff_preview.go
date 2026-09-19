package server

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"mcpx/internal/edit"
)

const (
	cleanDiffFilePreviewMaxBytes  = 64 << 10
	cleanDiffTotalPreviewMaxBytes = 64 << 10
	cleanDiffPageDefaultBytes     = 64 << 10
	cleanDiffPageMaxBytes         = 256 << 10
)

type diffPreviewResult struct {
	Text       string
	Bytes      int
	NextOffset int
	Truncated  bool
}

func boundedDiffPreview(diff string, maxBytes int) diffPreviewResult {
	if diff == "" || maxBytes <= 0 {
		return diffPreviewResult{}
	}
	if len(diff) <= maxBytes {
		return diffPreviewResult{Text: diff, Bytes: len(diff), NextOffset: len(diff)}
	}
	limit := maxBytes
	if limit > len(diff) {
		limit = len(diff)
	}
	// Keep the preview at a complete UTF-8 and diff-line boundary. A very
	// long single line falls back to the last valid UTF-8 boundary.
	cut := limit
	for cut > 0 && !utf8.ValidString(diff[:cut]) {
		cut--
	}
	if newline := strings.LastIndexByte(diff[:cut], '\n'); newline >= 0 {
		cut = newline + 1
	}
	if cut == 0 {
		return diffPreviewResult{Text: "[diff preview unavailable; request observe(view=diff)]\n", Bytes: len("[diff preview unavailable; request observe(view=diff)]\n"), Truncated: true}
	}
	text := diff[:cut]
	marker := "... [diff truncated; request observe(view=diff) for the next page]\n"
	if len(text)+len(marker) <= maxBytes {
		text += marker
	}
	return diffPreviewResult{Text: text, Bytes: len(text), NextOffset: cut, Truncated: true}
}

func editResponseData(remoteSessionID, editID string, result edit.BatchResult, replay bool) map[string]any {
	files := make([]map[string]any, 0, len(result.Results))
	remaining := cleanDiffTotalPreviewMaxBytes
	for _, item := range result.Results {
		file := map[string]any{
			"path":            item.Path,
			"new_path":        item.NewPath,
			"operation":       item.Operation,
			"original_sha256": item.OriginalSHA256,
			"new_sha256":      item.NewSHA256,
			"changed_lines":   item.ChangedLines,
			"deleted":         item.Deleted,
			"diff_bytes":      len(item.Diff),
		}
		budget := cleanDiffFilePreviewMaxBytes
		if item.Readback != nil {
			file["readback"] = item.Readback
		}
		if remaining < budget {
			budget = remaining
		}
		preview := boundedDiffPreview(item.Diff, budget)
		if preview.Text != "" {
			file["diff"] = preview.Text
			file["preview_bytes"] = preview.Bytes
			remaining -= preview.Bytes
		}
		if preview.Truncated || remaining == 0 && len(item.Diff) > preview.Bytes {
			file["diff_truncated"] = true
		}
		files = append(files, file)
	}
	aggregate := boundedDiffPreview(result.DiffSummary, cleanDiffTotalPreviewMaxBytes)
	data := map[string]any{
		"status":  "succeeded",
		"summary": "updated " + itoa(len(result.Results)) + " path(s), total changed lines " + itoa(result.TotalChangedLines),
		"edit_id": editID,

		"diff_bytes":          len(result.DiffSummary),
		"preview_bytes":       cleanDiffTotalPreviewMaxBytes - remaining,
		"diff_truncated":      aggregate.Truncated,
		"total_changed_lines": result.TotalChangedLines,
		"results":             files,
	}
	if aggregate.Truncated {
		data["next_action"] = map[string]any{
			"tool":   "observe",
			"reason": "read the remaining edit diff by byte offset",
			"arguments": map[string]any{
				"remote_session_id": remoteSessionID,
				"view":              "diff",
				"edit_id":           editID,
				"offset":            aggregate.NextOffset,
				"limit":             cleanDiffPageDefaultBytes,
			},
		}
	}
	if replay {
		data["idempotent_replay"] = true
	}
	return data
}

func editDiffPage(diff string, offset, limit int) (string, int, bool, error) {
	if offset < 0 {
		return "", 0, false, fmt.Errorf("offset must be non-negative")
	}
	if limit <= 0 {
		limit = cleanDiffPageDefaultBytes
	}
	if limit > cleanDiffPageMaxBytes {
		limit = cleanDiffPageMaxBytes
	}
	for offset > 0 && offset < len(diff) && !utf8.RuneStart(diff[offset]) {
		offset--
	}
	if offset >= len(diff) {
		return "", len(diff), true, nil
	}
	end := offset + limit
	if end > len(diff) {
		end = len(diff)
	}
	page := diff[offset:end]
	if !utf8.ValidString(page) {
		for len(page) > 0 && !utf8.ValidString(page) {
			page = page[:len(page)-1]
		}
	}
	if end < len(diff) {
		if newline := strings.LastIndexByte(page, '\n'); newline >= 0 {
			page = page[:newline+1]
		}
	}
	if page == "" && end < len(diff) {
		return "", offset, false, fmt.Errorf("diff page limit does not contain a complete UTF-8 line")
	}
	next := offset + len(page)
	return page, next, next >= len(diff), nil
}

func itoa(value int) string {
	// Keep the response builder allocation-light without pulling formatting into
	// every per-file preview call.
	return strconv.Itoa(value)
}
