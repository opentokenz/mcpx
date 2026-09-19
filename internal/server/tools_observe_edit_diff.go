package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/envelope"
)

func (r *Runtime) toolObserveEditDiff(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, session, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	editID := strings.TrimSpace(stringPayload(envReq.Payload, "edit_id"))
	if editID == "" {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "EDIT_ID_REQUIRED", "edit_id is required for observe(view=diff)")
	}
	record, state, err := r.loadCleanEditRecord(ctx, session.ID, principal.ID, editID)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "EDIT_DIFF_NOT_FOUND", err.Error())
	}
	if state != "succeeded" {
		response := envelope.Fail(envelope.StatusError, envReq.RequestID, session.WorkspaceName, nil,
			"EDIT_DIFF_UNAVAILABLE", fmt.Sprintf("edit %s is not in a completed state", editID))
		response.RemoteSessionID = session.ID
		response.Error.Details["edit_id"] = editID
		response.Error.Details["state"] = state
		response.Error.Details["next_action"] = map[string]any{
			"tool":              "read",
			"remote_session_id": session.ID,
			"view":              "file",
			"note":              "reconcile the affected files before requesting the diff again",
		}
		return r.resultJSON(response)
	}
	offset := intPayload(envReq.Payload, "offset")
	limit := intPayload(envReq.Payload, "limit")
	page, nextOffset, eof, err := editDiffPage(record.Result.DiffSummary, offset, limit)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "EDIT_DIFF_PAGE_INVALID", err.Error())
	}
	data := map[string]any{
		"edit_id":           editID,
		"remote_session_id": session.ID,
		"diff":              page,

		"encoding":            "utf-8",
		"offset":              offset,
		"next_offset":         nextOffset,
		"limit":               len(page),
		"eof":                 eof,
		"diff_bytes":          len(record.Result.DiffSummary),
		"total_changed_lines": record.Result.TotalChangedLines,
	}
	if !eof {
		data["next_action"] = map[string]any{
			"tool":   "observe",
			"reason": "continue reading the edit diff",
			"arguments": map[string]any{
				"remote_session_id": session.ID,
				"view":              "diff",
				"edit_id":           editID,
				"offset":            nextOffset,
				"limit":             cleanDiffPageDefaultBytes,
			},
		}
	}
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, data)
}
