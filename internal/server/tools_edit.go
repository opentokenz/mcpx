package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/audit"
	"mcpx/internal/edit"
	"mcpx/internal/envelope"
	"mcpx/internal/file"
	"mcpx/internal/idempotency"
	"mcpx/internal/observation"
	"mcpx/internal/remotesession"
	"mcpx/internal/security"
)

const cleanEditIdempotencyOperation = "edit"

type storedEditError struct {
	Code         string `json:"code"`
	Message      string `json:"message"`
	Path         string `json:"path,omitempty"`
	Index        int    `json:"index"`
	Current      string `json:"current_sha256,omitempty"`
	ChangedLines int    `json:"changed_lines,omitempty"`
}

type storedEditResult struct {
	EditID string           `json:"edit_id"`
	Result edit.BatchResult `json:"result"`
	Error  *storedEditError `json:"error,omitempty"`
}

func (s storedEditError) applyError() *edit.ApplyError {
	return &edit.ApplyError{Code: s.Code, Message: s.Message, Path: s.Path, Index: s.Index, Current: s.Current, ChangedLines: s.ChangedLines}
}

// toolEdit is the clean-core unified file edit entry.
func (r *Runtime) toolEdit(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, session, fail := r.changeRequest(ctx, req, true)
	if fail != nil {
		return fail, nil
	}

	edits, err := parseCleanEdits(envReq.Payload)
	if err != nil {
		return r.editToolError(envReq, session, err)
	}
	for index, item := range edits {
		if item.Operation == edit.OpDelete {
			return r.editToolError(envReq, session, &edit.ApplyError{
				Code:    "MOVE_OUT_REQUIRED",
				Message: "file removal requires move_out(action=prepare), user confirmation, then move_out(action=submit)",
				Path:    item.Path,
				Index:   index,
				Err:     edit.ErrUnsupportedOp,
			})
		}
	}
	effective := r.effectiveConfig(session.WorkspacePath)
	physicalRoot, err := file.Resolve(session.WorkspacePath, ".")
	if err != nil {
		return r.editToolError(envReq, session, err)
	}
	validatePath := func(absolute string) error {
		relative, err := filepath.Rel(physicalRoot, absolute)
		if err != nil {
			return err
		}
		for _, path := range []string{filepath.ToSlash(relative), filepath.ToSlash(absolute)} {
			if security.MatchFile(effective.Security.Files, path) == security.Deny {
				return &edit.ApplyError{Code: "FILE_DENIED", Message: "file denied by policy", Path: relative, Index: -1}
			}
		}
		return nil
	}
	for _, item := range edits {
		for _, path := range []string{item.Path, item.NewPath} {
			if path != "" && security.MatchFile(effective.Security.Files, path) == security.Deny {
				response := envelope.Fail(envelope.StatusDenied, envReq.RequestID, session.WorkspaceName,
					nil, "FILE_DENIED", "file denied by policy")
				response.Error.Details["path"] = path
				response.RemoteSessionID = session.ID
				return r.resultJSON(response)
			}
		}
	}
	idempotencyKey := strings.TrimSpace(stringPayload(envReq.Payload, "idempotency_key"))
	fingerprint := cleanEditFingerprint(envReq, edits)
	apply := true
	if value, exists := envReq.Payload["apply"].(bool); exists {
		apply = value
	}
	var idemKey idempotency.Key
	var claim idempotency.Claim
	claimed := apply && idempotencyKey != "" && r.idempotency != nil
	if claimed {
		idemKey = idempotency.Key{RemoteSessionID: session.ID, PrincipalID: principal.ID, Operation: cleanEditIdempotencyOperation, Value: idempotencyKey}
		var claimErr error
		claim, claimErr = r.idempotency.Claim(ctx, idemKey, fingerprint, cleanEditRecordTTL)
		if claimErr != nil {
			return r.editToolError(envReq, session, claimErr)
		}
		switch claim.Kind {
		case idempotency.ClaimConflict:
			return r.editIdempotencyConflict(envReq, session, fingerprint, claim.Record.Fingerprint)
		case idempotency.ClaimReplay:
			return r.replayStoredEdit(envReq, session, claim.Record.Response)
		case idempotency.ClaimInDoubt:
			if reconciled, ok := r.reconcilePendingEdit(ctx, envReq, session, principal.ID, claim, idemKey, fingerprint); ok {
				return reconciled, nil
			}
			return r.editIdempotencyInDoubt(envReq, session, claim.Record)
		case idempotency.ClaimWait:
			record, waitErr := r.idempotency.Wait(ctx, claim, idemKey)
			if waitErr != nil {
				return r.editIdempotencyPending(envReq, session, waitErr.Error())
			}
			return r.replayStoredEdit(envReq, session, record.Response)
		case idempotency.ClaimPending:
			return r.editIdempotencyPending(envReq, session, "the same idempotency request is still running")
		}
		if claim.Kind == idempotency.ClaimOwner {
			if reconciled, ok := r.reconcilePendingEdit(ctx, envReq, session, principal.ID, claim, idemKey, fingerprint); ok {
				return reconciled, nil
			}
		}
	}

	if err := resolveEditRevisions(session.WorkspacePath, edits); err != nil {
		if claimed && claim.Kind == idempotency.ClaimOwner {
			stored := storedEditResult{Error: storedApplyError(err)}
			encoded, _ := json.Marshal(stored)
			_ = r.idempotency.Complete(ctx, idemKey, fingerprint, idempotency.StateFailed, encoded, nil)
		}
		return r.editToolError(envReq, session, err)
	}
	if !apply {
		result, dryRunErr := edit.ApplyBatch(edit.BatchRequest{WorkspaceRoot: session.WorkspacePath, Edits: edits, DryRun: true, ValidatePath: validatePath})
		if dryRunErr != nil {
			return r.editToolError(envReq, session, dryRunErr)
		}
		data := editResponseData(session.ID, "", result, false)
		data["applied"] = false
		data["apply"] = false
		data["preview_only"] = true
		data["remote_session_id"] = session.ID
		return r.remoteResult(envReq, session.ID, session.WorkspaceName, data)
	}

	editID := newOpaqueID("edit", 12)
	preparedPersisted := false
	var preparedResult edit.BatchResult
	result, err := edit.ApplyBatchWithHook(edit.BatchRequest{
		WorkspaceRoot: session.WorkspacePath,
		Edits:         edits,
		ValidatePath:  validatePath,
	}, func(prepared edit.BatchResult) error {
		preparedResult = prepared
		stored := storedEditResult{EditID: editID, Result: prepared}
		if err := r.saveCleanEditRecord(ctx, session.ID, principal.ID, editID, "pending", prepared); err != nil {
			return err
		}
		if claimed {
			encoded, encodeErr := json.Marshal(stored)
			if encodeErr != nil {
				return encodeErr
			}
			metadata, _ := json.Marshal(map[string]any{"edit_id": editID, "paths": editPaths(prepared)})
			if err := r.idempotency.UpdatePending(ctx, idemKey, fingerprint, encoded, metadata); err != nil {
				return err
			}
		}
		preparedPersisted = true
		return nil
	})
	if err != nil {
		if preparedPersisted {
			_ = r.saveCleanEditRecord(ctx, session.ID, principal.ID, editID, "in_doubt", preparedResult)
			if claimed {
				_ = r.idempotency.MarkInDoubt(ctx, idemKey, fingerprint, []byte(`{"recovery":"read current file SHA values before retrying"}`))
			}
			return r.editIdempotencyInDoubt(envReq, session, idempotency.Record{Key: idemKey, Fingerprint: fingerprint, State: idempotency.StateInDoubt})
		}
		if claimed {
			stored := storedEditResult{Error: storedApplyError(err)}
			encoded, _ := json.Marshal(stored)
			_ = r.idempotency.Complete(ctx, idemKey, fingerprint, idempotency.StateFailed, encoded, nil)
		}
		return r.editToolError(envReq, session, err)
	}
	if err := r.saveCleanEditRecord(ctx, session.ID, principal.ID, editID, "succeeded", result); err != nil {
		if claimed {
			_ = r.idempotency.MarkInDoubt(ctx, idemKey, fingerprint, []byte(`{"recovery":"edit record persistence failed; inspect file SHA values"}`))
		}
		return r.editIdempotencyInDoubt(envReq, session, idempotency.Record{Key: idemKey, Fingerprint: fingerprint, State: idempotency.StateInDoubt})
	}
	if claimed {
		stored := storedEditResult{EditID: editID, Result: result}
		encoded, _ := json.Marshal(stored)
		metadata, _ := json.Marshal(map[string]any{"edit_id": editID, "paths": editPaths(result)})
		if err := r.idempotency.Complete(ctx, idemKey, fingerprint, idempotency.StateSucceeded, encoded, metadata); err != nil {
			recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = r.idempotency.MarkInDoubt(recoveryCtx, idemKey, fingerprint, nil)
			cancel()
			return r.editIdempotencyInDoubt(envReq, session, idempotency.Record{Key: idemKey, Fingerprint: fingerprint, State: idempotency.StateInDoubt})
		}
	}
	_ = r.remote.AddEvent(ctx, principal, remotesession.Event{
		RemoteSessionID: session.ID,
		Type:            "edit.applied",
		Summary:         fmt.Sprintf("edit %d files, %d changed lines", len(result.Results), result.TotalChangedLines),
	})
	r.observeCleanEdit(ctx, envReq, session, editID, result)
	r.logAudit(audit.Event{
		RequestID: envReq.RequestID, RemoteSessionID: session.ID, Workspace: session.WorkspaceName,
		Tool: "edit", Status: "ok",
		Detail: map[string]any{"files": len(result.Results), "deleted_files": 0, "total_changed_lines": result.TotalChangedLines},
	})
	return r.editToolSuccess(envReq, session, editID, result, false)
}

func (r *Runtime) editToolSuccess(envReq envelope.Request, session remotesession.Session, editID string, result edit.BatchResult, replay bool) (*mcp.CallToolResult, error) {
	data := editResponseData(session.ID, editID, result, replay)
	data["remote_session_id"] = session.ID
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, data)
}

func (r *Runtime) editToolError(envReq envelope.Request, session remotesession.Session, err error) (*mcp.CallToolResult, error) {
	var ae *edit.ApplyError
	if errors.As(err, &ae) {
		details := map[string]any{}
		if ae.Path != "" {
			details["path"] = ae.Path
		}
		if ae.Index >= 0 {
			details["replacement_index"] = ae.Index
		}
		if ae.Current != "" {
			if rev := compactFileRevision(ae.Current); rev != "" {
				details["current_rev"] = rev
			}
		}
		if ae.ChangedLines > 0 {
			details["total_changed_lines"] = ae.ChangedLines
		}
		code := ae.Code
		if code == "" {
			code = "EDIT_FAILED"
		}
		msg := ae.Error()
		details["recovery"] = editRecovery(code, ae)
		suggestedNext := editSuggestedNext(code, session.ID, ae)
		if suggestedNext != nil {
			details["suggested_next"] = suggestedNext
		}
		response := envelope.Fail(envelope.StatusError, envReq.RequestID, session.WorkspaceName, nil, code, msg)
		for key, value := range details {
			response.Error.Details[key] = value
		}
		if suggestedNext != nil {
			tool, _ := suggestedNext["tool"].(string)
			arguments, _ := suggestedNext["arguments"].(map[string]any)
			response.Error.Recovery = &envelope.Recovery{Action: tool, Tool: tool, Arguments: argumentsOrEmpty(arguments)}
		}
		response.RemoteSessionID = session.ID
		return r.resultJSON(response)
	}
	response := envelope.Fail(envelope.StatusError, envReq.RequestID, session.WorkspaceName, nil, "EDIT_FAILED", err.Error())
	response.RemoteSessionID = session.ID
	return r.resultJSON(response)
}

func editRecovery(code string, ae *edit.ApplyError) any {
	switch code {
	case "STALE_REVISION":
		return "re-read the file to get the latest rev and retry with a new idempotency_key; the old key remains bound to the stale request"
	case "MATCH_NOT_FOUND", "MATCH_AMBIGUOUS":
		return "re-read the file and use a longer unique match snippet, or use a revision-guarded line range when the target lines are known"
	case "RANGE_OUT_OF_BOUNDS":
		return "re-read the file to refresh its current line layout and rev, then retry the range against that revision"
	case "TOO_MANY_CHANGES":
		return map[string]any{
			"action":            "split_edit",
			"max_changed_lines": edit.MaxChangedLines,
			"message":           fmt.Sprintf("split the edit into smaller batches; max total changed lines is %d", edit.MaxChangedLines),
		}
	case "SYMLINK_NOT_ALLOWED", "DELETE_FILE_ONLY":
		return "move out only an explicit regular file, directory or symlink entry; do not follow symlink paths"
	case "MOVE_OUT_REQUIRED":
		return "use move_out(action=prepare), ask the web user to confirm the frozen manifest, then move_out(action=submit) with confirmation_uuid; edit never removes files"
	case "FILE_DENIED", "POLICY_DENIED":
		return "adjust the path or obtain policy approval"
	default:
		if ae != nil && ae.Message != "" {
			return ae.Message
		}
		return "inspect error details and retry with corrected parameters"
	}
}

func editSuggestedNext(code, remoteSessionID string, ae *edit.ApplyError) map[string]any {
	if ae == nil || strings.TrimSpace(ae.Path) == "" {
		return nil
	}
	refreshWindow := func(reason string) map[string]any {
		return nextActionWithReason("read", reason, map[string]any{
			"remote_session_id": remoteSessionID,
			"view":              "file",
			"path":              ae.Path,
			"mode":              "window",
			"offset":            0,
			"limit":             1,
		})
	}
	switch code {
	case "STALE_REVISION":
		return refreshWindow("refresh the current file rev before regenerating the edit")
	case "MATCH_NOT_FOUND", "MATCH_AMBIGUOUS", "RANGE_OUT_OF_BOUNDS":
		return nextActionWithReason("read", "refresh the current file content before regenerating the update", map[string]any{
			"remote_session_id": remoteSessionID,
			"view":              "file",
			"path":              ae.Path,
		})
	case "MOVE_OUT_REQUIRED":
		return refreshWindow("read the current file sha256 before preparing move_out")
	default:
		return nil
	}
}

func (r *Runtime) observeCleanEdit(ctx context.Context, envReq envelope.Request, session remotesession.Session, editID string, result edit.BatchResult) {
	if r == nil || r.observation == nil {
		return
	}
	paths := make([]string, 0, len(result.Results))
	for _, item := range result.Results {
		paths = append(paths, item.Path)
	}
	observationResults := make([]map[string]any, 0, len(result.Results))
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
		if item.Diff != "" {
			file["diff"] = item.Diff
		}
		observationResults = append(observationResults, file)
	}
	payload, _ := json.Marshal(map[string]any{
		"edit_id":             editID,
		"diff_summary":        boundedDiffPreview(result.DiffSummary, cleanDiffTotalPreviewMaxBytes).Text,
		"diff_bytes":          len(result.DiffSummary),
		"diff_truncated":      len(result.DiffSummary) > cleanDiffTotalPreviewMaxBytes,
		"total_changed_lines": result.TotalChangedLines,
		"paths":               paths,
		"results":             observationResults,
	})
	summary := fmt.Sprintf("edit %d file(s), %d changed lines", len(result.Results), result.TotalChangedLines)
	_ = r.observation.Record(ctx, observation.Event{
		Workspace:       session.WorkspaceName,
		RemoteSessionID: session.ID,
		RequestID:       envReq.RequestID,
		CallID:          observationCallID(envReq),
		Tool:            "edit",
		Type:            observation.TypeFileChanged,
		Status:          "succeeded",
		Purpose:         envReq.Intent,
		Intent:          envReq.Intent,
		Output:          payload,
		Summary:         summary,
		Path:            strings.Join(paths, ","),
	})
}

func parseCleanEdits(payload map[string]any) ([]edit.FileEdit, error) {
	if payload == nil {
		return nil, &edit.ApplyError{Code: "INVALID_INPUT", Message: "edits required", Index: -1, Err: edit.ErrInvalidInput}
	}
	raw, ok := payload["edits"]
	if !ok || raw == nil {
		// Single-file shorthand: top-level path + replacements/content.
		if path, _ := payload["path"].(string); strings.TrimSpace(path) != "" {
			raw = []any{payload}
		} else {
			return nil, &edit.ApplyError{Code: "INVALID_INPUT", Message: "edits required", Index: -1, Err: edit.ErrInvalidInput}
		}
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var edits []edit.FileEdit
	var rawEdits []map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &rawEdits); err != nil {
		return nil, &edit.ApplyError{Code: "INVALID_INPUT", Message: "invalid edits payload", Index: -1, Err: edit.ErrInvalidInput}
	}
	for index, item := range rawEdits {
		if _, exact := item["content_base64"]; !exact {
			continue
		}
		for _, key := range []string{"content", "replacements", "range"} {
			if _, present := item[key]; present {
				return nil, &edit.ApplyError{Code: "INVALID_INPUT", Message: "content_base64 cannot be combined with logical edit fields", Index: index, Err: edit.ErrInvalidInput}
			}
		}
	}
	if err := json.Unmarshal(encoded, &edits); err != nil {
		return nil, &edit.ApplyError{Code: "INVALID_INPUT", Message: "invalid edits payload", Index: -1, Err: edit.ErrInvalidInput}
	}
	if len(edits) == 0 {
		return nil, &edit.ApplyError{Code: "INVALID_INPUT", Message: "edits required", Index: -1, Err: edit.ErrInvalidInput}
	}
	for i := range edits {
		if strings.TrimSpace(edits[i].Operation) == "" {
			return nil, &edit.ApplyError{Code: "INVALID_INPUT", Message: fmt.Sprintf("edits[%d].operation required", i), Index: i, Err: edit.ErrInvalidInput}
		}
		if strings.TrimSpace(edits[i].BaseSHA256) != "" {
			return nil, &edit.ApplyError{Code: "INVALID_INPUT", Message: fmt.Sprintf("edits[%d].base_sha256 is not supported; use rev from read", i), Path: edits[i].Path, Index: i, Err: edit.ErrInvalidInput}
		}
		if strings.TrimSpace(edits[i].Operation) == edit.OpUpdate || strings.TrimSpace(edits[i].Operation) == edit.OpRename {
			if strings.TrimSpace(edits[i].Revision) == "" {
				return nil, &edit.ApplyError{Code: "INVALID_INPUT", Message: fmt.Sprintf("edits[%d].rev required for update/rename", i), Path: edits[i].Path, Index: i, Err: edit.ErrInvalidInput}
			}
		}
	}
	return edits, nil
}
