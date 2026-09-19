package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/audit"
	"mcpx/internal/auth"
	"mcpx/internal/config"
	"mcpx/internal/deletion"
	"mcpx/internal/envelope"
	"mcpx/internal/file"
	"mcpx/internal/mcpresult"
	"mcpx/internal/observation"
	"mcpx/internal/remotesession"
	"mcpx/internal/security"
)

const moveOutRequestMaxTargets = MaxMoveOutTargets

var workspaceMoveOutSafetyMeta = mcp.Meta{
	"mcpx/safety": map[string]any{
		"classification":                      "constrained_workspace_quarantine_move",
		"approval":                            "web_model_user_confirmation_required",
		"filesystem_only":                     true,
		"registered_workspace":                true,
		"explicit_files_directories_symlinks": true,
		"revision_guarded":                    true,
		"no_shell_execution":                  true,
		"operation":                           "atomic_move_to_workspace_sibling_quarantine",
		"destination":                         "server_managed_workspace_sibling_quarantine",
		"quarantine_path_scope":               "relative_to_workspace_parent",
		"reversible":                          true,
		"directory_prepare":                   "root_only_no_descendant_scan",
		"no_symlink_following":                true,
		"symlink_entry_move":                  true,
		"durable_audit":                       true,
		"response_preview_max_targets":        MaxMoveOutResponsePreviewTargets,
		"confirmation_credential":             "server_generated_confirmation_uuid",
	},
}

var workspaceMoveOutToolAnnotation = toolAnnotation{
	ReadOnly: false, Destructive: true, Idempotent: true, OpenWorld: false,
	Title: "Workspace 安全移出", Meta: mcp.Meta{
		"mcpx/safety":                  workspaceMoveOutSafetyMeta["mcpx/safety"],
		"mcpx/strict_action_arguments": []string{"submit"},
		"mcpx/action_risk": map[string]any{
			"prepare": riskDescriptor(true, false, true, false, "workspace_move_out_manifest_prepare"),
			"submit":  riskDescriptor(false, true, true, false, "workspace_move_out_manifest_submit"),
		},
	},
}

func (r *Runtime) toolMoveOut(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	action, _ := mcpresult.Arguments(req)["action"].(string)
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "prepare":
		return r.toolWorkspaceMoveOutPrepare(ctx, req)
	case "submit":
		return r.toolWorkspaceMoveOutCommit(ctx, req)
	default:
		envReq, _, session, fail := r.changeRequest(ctx, req, false)
		if fail != nil {
			return fail, nil
		}
		return r.moveOutError(envReq, session, "INVALID_REQUEST", "action must be prepare or submit", map[string]any{"field": "action"})
	}
}

func (r *Runtime) toolWorkspaceMoveOutPrepare(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, session, fail := r.changeRequest(ctx, req, true)
	if fail != nil {
		return fail, nil
	}
	targets, err := parseMoveOutTargets(envReq.Payload)
	if err != nil {
		return r.moveOutError(envReq, session, "INVALID_REQUEST", err.Error(), nil)
	}
	targets, err = r.inferMoveOutTargetKinds(session, targets)
	if err != nil {
		return r.moveOutError(envReq, session, moveOutErrorCode(err), moveOutErrorMessage(err), moveOutErrorDetails(err))
	}
	key := strings.TrimSpace(stringPayload(envReq.Payload, "idempotency_key"))
	if key == "" {
		key = "auto:" + strings.TrimSpace(envReq.RequestID)
		if key == "auto:" {
			generated, keyErr := deletion.NewUUID()
			if keyErr != nil {
				return r.moveOutError(envReq, session, "MOVE_OUT_ID_ERROR", keyErr.Error(), nil)
			}
			key = "auto:" + generated
		}
	}
	if len(targets) > moveOutRequestMaxTargets {
		return r.moveOutError(envReq, session, "LIMIT_EXCEEDED", fmt.Sprintf("move-out targets exceed maximum %d", moveOutRequestMaxTargets), map[string]any{"max_targets": moveOutRequestMaxTargets})
	}

	if existing, findErr := r.deletions.FindByIdempotency(ctx, session.ID, principal.ID, key); findErr == nil {
		if !sameMoveOutTargetSpec(existing.Manifest.Targets, targets) || strings.TrimSpace(existing.Purpose) != strings.TrimSpace(envReq.Purpose) {
			return r.moveOutError(envReq, session, "IDEMPOTENCY_CONFLICT", "idempotency_key is bound to a different move-out manifest or purpose", map[string]any{"move_request_id": existing.ID, "manifest_sha256": existing.ManifestSHA256})
		}
		return r.moveOutPrepareResult(envReq, session, existing, true)
	} else if !errors.Is(findErr, deletion.ErrNotFound) {
		return r.moveOutError(envReq, session, "MOVE_OUT_STORE_ERROR", findErr.Error(), nil)
	}

	manifest, err := r.freezeMoveOutManifest(session, targets)
	if err != nil {
		return r.moveOutError(envReq, session, moveOutErrorCode(err), moveOutErrorMessage(err), moveOutErrorDetails(err))
	}
	manifestSHA, err := manifest.SHA256()
	if err != nil {
		return r.moveOutError(envReq, session, "MOVE_OUT_STORE_ERROR", err.Error(), nil)
	}
	moveID, idErr := deletion.NewUUID()
	if idErr != nil {
		return r.moveOutError(envReq, session, "MOVE_OUT_ID_ERROR", idErr.Error(), nil)
	}
	now := time.Now().UTC()
	item := deletion.Request{
		ID: moveID, RemoteSessionID: session.ID, PrincipalID: principal.ID,
		Workspace: session.WorkspaceName, WorkspacePath: session.WorkspacePath, Purpose: envReq.Purpose,
		IdempotencyKey: key, Manifest: manifest, ManifestSHA256: manifestSHA,
		CreatedAt: now, ExpiresAt: now.Add(deletion.DefaultTTL), UpdatedAt: now,
	}
	item.ConfirmationUUIDHash = deletion.HashConfirmationUUID(item.ID)
	if err := r.deletions.Create(ctx, item); err != nil {
		if existing, findErr := r.deletions.FindByIdempotency(ctx, session.ID, principal.ID, key); findErr == nil && sameMoveOutTargetSpec(existing.Manifest.Targets, targets) && strings.TrimSpace(existing.Purpose) == strings.TrimSpace(envReq.Purpose) {
			return r.moveOutPrepareResult(envReq, session, existing, true)
		}
		return r.moveOutError(envReq, session, "MOVE_OUT_STORE_ERROR", err.Error(), nil)
	}
	preview, truncated := moveOutTargetPreview(manifest.Targets)
	_ = r.remote.AddEvent(ctx, principal, remotesession.Event{RemoteSessionID: session.ID, Type: "workspace.move_out.prepared", Summary: fmt.Sprintf("freeze %d explicit move-out target(s)", len(manifest.Targets)), Metadata: map[string]any{"manifest_sha256": manifestSHA, "target_count": len(manifest.Targets), "directory_contents_enumerated": false}})
	r.logAudit(audit.Event{RequestID: envReq.RequestID, RemoteSessionID: session.ID, Workspace: session.WorkspaceName, Tool: "move_out", Status: "prepared", Detail: map[string]any{"action": "prepare", "move_request_id": item.ID, "manifest_sha256": manifestSHA, "target_count": len(manifest.Targets), "target_preview": preview, "target_preview_truncated": truncated, "directory_contents_enumerated": false, "purpose": envReq.Purpose}})
	return r.moveOutPrepareResult(envReq, session, item, false)
}

func (r *Runtime) toolWorkspaceMoveOutCommit(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, session, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	if session.Role != "owner" && session.Role != "editor" {
		return r.moveOutError(envReq, session, "FORBIDDEN", "remote session role cannot submit a workspace move-out", nil)
	}
	confirmationUUID := strings.TrimSpace(stringPayload(envReq.Payload, "confirmation_uuid"))
	if confirmationUUID == "" {
		return r.moveOutError(envReq, session, "CONFIRMATION_REQUIRED", "confirmation_uuid from move_out(action=prepare) is required after the web client obtains user confirmation", map[string]any{"field": "confirmation_uuid", "requires_user_confirmation": true})
	}
	item, err := r.deletions.Get(ctx, confirmationUUID)
	if err != nil {
		return r.moveOutError(envReq, session, "MOVE_OUT_REQUEST_NOT_FOUND", err.Error(), nil)
	}
	if item.RemoteSessionID != session.ID || item.PrincipalID != principal.ID || item.WorkspacePath != session.WorkspacePath || item.Workspace != session.WorkspaceName {
		return r.moveOutError(envReq, session, "MOVE_OUT_MANIFEST_MISMATCH", "confirmation_uuid is not bound to this remote session and workspace", map[string]any{"move_request_id": item.ID})
	}
	if previewOnlyMoveOutPurpose(item.Purpose) {
		return r.moveOutError(envReq, session, "MOVE_OUT_PURPOSE_MISMATCH", "a preview-only move_out(action=prepare) purpose cannot be submitted for moving; prepare a new manifest with the actual move-out intent", map[string]any{"move_request_id": item.ID, "prepare_purpose": item.Purpose, "recovery": "prepare again with a purpose that describes the actual move-out"})
	}
	if confirmationUUID != item.ID || item.ConfirmationUUIDHash == "" || deletion.HashConfirmationUUID(confirmationUUID) != item.ConfirmationUUIDHash {
		return r.moveOutError(envReq, session, "CONFIRMATION_MISMATCH", "confirmation_uuid is not the server-issued credential for this move-out request", map[string]any{"move_request_id": item.ID, "manifest_sha256": item.ManifestSHA256})
	}
	if time.Now().UTC().After(item.ExpiresAt) {
		return r.moveOutError(envReq, session, "MOVE_OUT_REQUEST_EXPIRED", "move-out request approval window has expired", map[string]any{"expires_at": item.ExpiresAt.Format(time.RFC3339Nano)})
	}
	envReq.Purpose = item.Purpose
	if strings.TrimSpace(envReq.Intent) == "" {
		envReq.Intent = item.Purpose
	}
	if item.Status == "committed" || item.Status == "partial" || item.Status == "failed" {
		return r.moveOutCommitReplay(envReq, session, item)
	}
	if item.Status == "committing" {
		return r.moveOutError(envReq, session, "MOVE_OUT_IN_PROGRESS", "move-out request is being committed; retry with the same confirmation_uuid", map[string]any{"move_request_id": item.ID, "retry_with_same_confirmation_uuid": true})
	}
	item, err = r.deletions.MarkCommitting(ctx, item.ID)
	if err != nil {
		if errors.Is(err, deletion.ErrInProcess) {
			return r.moveOutError(envReq, session, "MOVE_OUT_IN_PROGRESS", err.Error(), map[string]any{"move_request_id": confirmationUUID, "retry_with_same_confirmation_uuid": true})
		}
		return r.moveOutError(envReq, session, "MOVE_OUT_REQUEST_CONFLICT", err.Error(), nil)
	}
	result := r.commitMoveOutManifest(ctx, envReq, principal, session, item)
	status := "committed"
	if result.FailedCount > 0 {
		status = "partial"
		if result.MovedCount == 0 {
			status = "failed"
		}
	}
	result.Status = status
	encoded, _ := json.Marshal(result)
	if completeErr := r.deletions.Complete(ctx, item.ID, status, encoded); completeErr != nil {
		return r.moveOutError(envReq, session, "MOVE_OUT_STATE_IN_DOUBT", completeErr.Error(), map[string]any{"move_request_id": item.ID, "manifest_sha256": item.ManifestSHA256})
	}
	result.IdempotentReplay = false
	r.logAudit(audit.Event{RequestID: envReq.RequestID, RemoteSessionID: session.ID, Workspace: session.WorkspaceName, Tool: "move_out", Status: status, Detail: map[string]any{"action": "submit", "move_request_id": item.ID, "manifest_sha256": item.ManifestSHA256, "confirmation_uuid_hash": item.ConfirmationUUIDHash, "idempotency_key": item.IdempotencyKey, "target_count": len(result.Targets), "target_preview": moveOutTargetResultPreviewData(result.Targets), "moved_count": result.MovedCount, "failed_count": result.FailedCount, "idempotent_replay": result.IdempotentReplay, "audit_event_id": result.AuditEventID}})
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, moveOutCommitResponseData(result))
}

func previewOnlyMoveOutPurpose(purpose string) bool {
	normalized := strings.ToLower(strings.TrimSpace(purpose))
	for _, marker := range []string{
		"仅 prepare", "仅预览", "仅检查", "仅冻结", "只预览", "只检查",
		"prepare only", "preview only", "dry-run", "dry run",
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

type moveOutCommitResult struct {
	Status             string                `json:"status"`
	MoveRequestID      string                `json:"move_request_id"`
	ManifestSHA256     string                `json:"manifest_sha256"`
	MovedCount         int                   `json:"moved_count"`
	MovedBytes         int64                 `json:"moved_bytes"`
	MovedBytesKnown    bool                  `json:"moved_bytes_known"`
	FailedCount        int                   `json:"failed_count"`
	Targets            []moveOutTargetResult `json:"targets"`
	MoveID             string                `json:"move_id"`
	AuditEventID       string                `json:"audit_event_id"`
	IdempotentReplay   bool                  `json:"idempotent_replay,omitempty"`
	Reversible         bool                  `json:"reversible"`
	QuarantineLocation string                `json:"quarantine_location"`
}

type moveOutTargetResult struct {
	Path           string `json:"path"`
	Kind           string `json:"kind"`
	ExpectedSHA256 string `json:"expected_sha256"`
	Size           int64  `json:"size"`
	Status         string `json:"status"`
	QuarantinePath string `json:"quarantine_path,omitempty"`
	ErrorCode      string `json:"error_code,omitempty"`
}

func (r *Runtime) commitMoveOutManifest(ctx context.Context, envReq envelope.Request, principal auth.Principal, session remotesession.Session, item deletion.Request) moveOutCommitResult {
	result := moveOutCommitResult{
		MoveRequestID: item.ID, ManifestSHA256: item.ManifestSHA256, MoveID: newOpaqueID("move", 12), AuditEventID: newOpaqueID("audit", 12),
		Targets: make([]moveOutTargetResult, 0, len(item.Manifest.Targets)), MovedBytesKnown: true, Reversible: true,
	}

	workspacePath, err := filepath.Abs(session.WorkspacePath)
	if err != nil {
		for _, target := range item.Manifest.Targets {
			result.Targets = append(result.Targets, moveOutTargetResult{Path: target.Path, Kind: target.Kind, ExpectedSHA256: target.ExpectedSHA256, Size: target.Size, Status: "failed", ErrorCode: "WORKSPACE_ROOT_ERROR"})
		}
		result.FailedCount = len(result.Targets)
		return result
	}

	platform := "linux"
	if os.PathSeparator == '\\' {
		platform = "windows"
	} else {
		systemName := strings.ToLower(strings.TrimSpace(boundedCommand(ctx, workspacePath, "uname", "-s")))
		switch {
		case strings.Contains(systemName, "darwin"):
			platform = "darwin"
		case strings.Contains(systemName, "mingw"), strings.Contains(systemName, "msys"), strings.Contains(systemName, "cygwin"):
			platform = "windows"
		}
	}

	for index, target := range item.Manifest.Targets {
		entries, finalKind, inspectErr := r.inspectMoveOutTargetForCommit(session, target)
		if inspectErr != nil {
			result.Targets = append(result.Targets, moveOutTargetResult{Path: target.Path, Kind: target.Kind, ExpectedSHA256: target.ExpectedSHA256, Size: target.Size, Status: "failed", ErrorCode: moveOutErrorCode(inspectErr)})
			result.FailedCount++
			continue
		}

		source := filepath.Join(workspacePath, filepath.FromSlash(target.Path))
		trashPath := "trash://" + filepath.ToSlash(filepath.Base(target.Path))
		trashLocation := "trash://"
		moved := false

		switch platform {
		case "windows":
			script := `$ErrorActionPreference='Stop'; Add-Type -AssemblyName Microsoft.VisualBasic; $p=$args[0]; $entry=Get-Item -LiteralPath $p -Force; if ($entry.PSIsContainer) { [Microsoft.VisualBasic.FileIO.FileSystem]::DeleteDirectory($p,[Microsoft.VisualBasic.FileIO.UIOption]::OnlyErrorDialogs,[Microsoft.VisualBasic.FileIO.RecycleOption]::SendToRecycleBin,[Microsoft.VisualBasic.FileIO.UICancelOption]::ThrowException) } else { [Microsoft.VisualBasic.FileIO.FileSystem]::DeleteFile($p,[Microsoft.VisualBasic.FileIO.UIOption]::OnlyErrorDialogs,[Microsoft.VisualBasic.FileIO.RecycleOption]::SendToRecycleBin,[Microsoft.VisualBasic.FileIO.UICancelOption]::ThrowException) }`
			_ = boundedCommand(ctx, workspacePath, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script, source)
			if _, statErr := os.Lstat(source); errors.Is(statErr, os.ErrNotExist) {
				moved = true
			} else {
				_ = boundedCommand(ctx, workspacePath, "pwsh.exe", "-NoProfile", "-NonInteractive", "-Command", script, source)
				if _, statErr := os.Lstat(source); errors.Is(statErr, os.ErrNotExist) {
					moved = true
				}
			}
			trashPath = "recycle-bin://" + filepath.ToSlash(filepath.Base(target.Path))
			trashLocation = "recycle-bin://"

		case "darwin":
			_ = boundedCommand(ctx, workspacePath, "osascript",
				"-e", "on run argv",
				"-e", `tell application "Finder" to delete POSIX file (item 1 of argv)`,
				"-e", "end run", source,
			)
			if _, statErr := os.Lstat(source); errors.Is(statErr, os.ErrNotExist) {
				moved = true
			} else if home, homeErr := os.UserHomeDir(); homeErr == nil {
				trashDir := filepath.Join(home, ".Trash")
				if mkdirErr := os.MkdirAll(trashDir, 0o700); mkdirErr == nil {
					destination := filepath.Join(trashDir, fmt.Sprintf("%s-%05d-%s", item.ID, index+1, filepath.Base(target.Path)))
					if renameErr := os.Rename(source, destination); renameErr == nil {
						moved = true
						trashPath = filepath.ToSlash(destination)
						trashLocation = filepath.ToSlash(trashDir)
					}
				}
			}

		default:
			_ = boundedCommand(ctx, workspacePath, "gio", "trash", source)
			if _, statErr := os.Lstat(source); errors.Is(statErr, os.ErrNotExist) {
				moved = true
			} else {
				_ = boundedCommand(ctx, workspacePath, "trash-put", source)
				if _, statErr := os.Lstat(source); errors.Is(statErr, os.ErrNotExist) {
					moved = true
				}
			}
			if !moved {
				home, homeErr := os.UserHomeDir()
				if homeErr == nil {
					dataHome := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
					if dataHome == "" {
						dataHome = filepath.Join(home, ".local", "share")
					}
					trashRoot := filepath.Join(dataHome, "Trash")
					filesDir := filepath.Join(trashRoot, "files")
					infoDir := filepath.Join(trashRoot, "info")
					if os.MkdirAll(filesDir, 0o700) == nil && os.MkdirAll(infoDir, 0o700) == nil {
						name := fmt.Sprintf("%s-%05d-%s", result.MoveID, index+1, filepath.Base(target.Path))
						destination := filepath.Join(filesDir, name)
						infoPath := filepath.Join(infoDir, name+".trashinfo")
						var encoded strings.Builder
						for offset := 0; offset < len(source); offset++ {
							ch := source[offset]
							if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || strings.ContainsRune("-._~/", rune(ch)) {
								encoded.WriteByte(ch)
							} else {
								fmt.Fprintf(&encoded, "%%%02X", ch)
							}
						}
						info := "[Trash Info]\nPath=" + encoded.String() + "\nDeletionDate=" + time.Now().Format("2006-01-02T15:04:05") + "\n"
						if infoFile, createErr := os.OpenFile(infoPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); createErr == nil {
							_, writeErr := infoFile.Write([]byte(info))
							closeErr := infoFile.Close()
							if writeErr == nil && closeErr == nil {
								if renameErr := os.Rename(source, destination); renameErr == nil {
									moved = true
									trashPath = filepath.ToSlash(destination)
									trashLocation = filepath.ToSlash(trashRoot)
								} else {
									_ = os.Remove(infoPath)
								}
							} else {
								_ = os.Remove(infoPath)
							}
						}
					}
				}
			}
		}

		entry := entries[0]
		if !moved {
			result.Targets = append(result.Targets, moveOutTargetResult{Path: entry.Path, Kind: finalKind, ExpectedSHA256: entry.ExpectedSHA256, Size: entry.Size, Status: "failed", ErrorCode: "MOVE_OUT_FAILED"})
			result.FailedCount++
			continue
		}

		result.Targets = append(result.Targets, moveOutTargetResult{Path: entry.Path, Kind: entry.Kind, ExpectedSHA256: entry.ExpectedSHA256, Size: entry.Size, Status: "moved", QuarantinePath: trashPath})
		result.MovedCount++
		if entry.Kind == "directory" {
			result.MovedBytesKnown = false
		} else {
			result.MovedBytes += entry.Size
		}
		if result.QuarantineLocation == "" {
			result.QuarantineLocation = trashLocation
		}
	}

	if result.FailedCount == 0 {
		result.Status = "committed"
	} else if result.MovedCount == 0 {
		result.Status = "failed"
	} else {
		result.Status = "partial"
	}
	_ = r.remote.AddEvent(ctx, principal, remotesession.Event{RemoteSessionID: session.ID, Type: "workspace.move_out.committed", OperationID: result.MoveID, Summary: fmt.Sprintf("moved %d explicit target(s) to system trash", result.MovedCount), Metadata: map[string]any{"move_request_id": item.ID, "manifest_sha256": item.ManifestSHA256, "status": result.Status, "moved_bytes": result.MovedBytes, "moved_bytes_known": result.MovedBytesKnown, "reversible": true, "audit_event_id": result.AuditEventID}})
	r.observeWorkspaceMoveOut(ctx, envReq, session, result)
	return result
}

func openWorkspaceSiblingRoot(workspacePath string) (*os.Root, string, error) {
	abs, err := filepath.Abs(workspacePath)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(filepath.Dir(filepath.Clean(abs)))
	if err != nil {
		return nil, "", err
	}
	return root, filepath.Base(filepath.Clean(abs)), nil
}

func moveOutDestination(root *os.Root, moveRequestID string, index int, targetPath string) (string, error) {
	if err := ensureManagedMoveOutDirectory(root, ".mcpx-quarantine"); err != nil {
		return "", err
	}
	requestDirectory := filepath.Join(".mcpx-quarantine", moveRequestID)
	if err := ensureManagedMoveOutDirectory(root, requestDirectory); err != nil {
		return "", err
	}
	destination := filepath.Join(requestDirectory, fmt.Sprintf("%05d-%s", index+1, filepath.Base(targetPath)))
	if _, err := root.Lstat(destination); err == nil {
		return "", errors.New("managed quarantine destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return destination, nil
}

func ensureManagedMoveOutDirectory(root *os.Root, relativePath string) error {
	info, err := root.Lstat(relativePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := root.MkdirAll(relativePath, 0o700); err != nil {
			return err
		}
		info, err = root.Lstat(relativePath)
		if err != nil {
			return err
		}
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("managed quarantine path is not a regular directory")
	}
	return nil
}

func (r *Runtime) observeWorkspaceMoveOut(ctx context.Context, envReq envelope.Request, session remotesession.Session, result moveOutCommitResult) {
	if r.observation == nil {
		return
	}
	payload, _ := json.Marshal(moveOutCommitResponseData(result))
	_ = r.observation.Record(ctx, observation.Event{Workspace: session.WorkspaceName, RemoteSessionID: session.ID, RequestID: envReq.RequestID, CallID: observationCallID(envReq), Tool: "move_out", Type: observation.TypeFileChanged, Status: result.Status, Purpose: envReq.Purpose, Intent: envReq.Intent, Output: payload, Summary: fmt.Sprintf("moved %d explicit target(s) to managed quarantine", result.MovedCount)})
}

func (r *Runtime) moveOutPrepareResult(envReq envelope.Request, session remotesession.Session, item deletion.Request, replay bool) (*mcp.CallToolResult, error) {
	nextAction := map[string]any{
		"tool": "move_out",
		"arguments": map[string]any{
			"action":            "submit",
			"remote_session_id": session.ID,
			"confirmation_uuid": item.ID,
		},
	}
	data := map[string]any{
		"summary":                       "安全移出清单已冻结；网页端模型向用户确认后调用 move_out(action=submit)",
		"move_request_id":               item.ID,
		"confirmation_uuid":             item.ID,
		"manifest_sha256":               item.ManifestSHA256,
		"idempotency_key":               item.IdempotencyKey,
		"workspace":                     item.Workspace,
		"purpose":                       item.Purpose,
		"target_count":                  len(item.Manifest.Targets),
		"target_preview":                moveOutTargetPreviewData(item.Manifest.Targets),
		"target_preview_truncated":      len(item.Manifest.Targets) > MaxMoveOutResponsePreviewTargets,
		"directory_contents_enumerated": false,
		"file_count":                    item.Manifest.FileCount,
		"directory_count":               item.Manifest.DirectoryCount,
		"symlink_count":                 item.Manifest.SymlinkCount,
		"total_bytes":                   item.Manifest.TotalBytes,
		"total_bytes_known":             item.Manifest.TotalBytesKnown,
		"expires_at":                    item.ExpiresAt.Format(time.RFC3339Nano),
		"requires_user_confirmation":    true,
		"approval_surface":              "web_model_user_question",
		"next_action":                   nextAction,
		"filesystem_mutated":            false,
	}
	if replay {
		data["idempotent_replay"] = true
	}
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, data)
}

func moveOutTargetPreview(targets []deletion.Target) ([]deletion.Target, bool) {
	limit := len(targets)
	if limit > MaxMoveOutResponsePreviewTargets {
		limit = MaxMoveOutResponsePreviewTargets
	}
	return append([]deletion.Target(nil), targets[:limit]...), len(targets) > limit
}

func moveOutTargetPreviewData(targets []deletion.Target) []deletion.Target {
	preview, _ := moveOutTargetPreview(targets)
	return preview
}

func (r *Runtime) moveOutCommitReplay(envReq envelope.Request, session remotesession.Session, item deletion.Request) (*mcp.CallToolResult, error) {
	var result moveOutCommitResult
	if err := json.Unmarshal(item.ResultJSON, &result); err != nil {
		return r.moveOutError(envReq, session, "MOVE_OUT_STATE_IN_DOUBT", err.Error(), map[string]any{"move_request_id": item.ID})
	}
	result.IdempotentReplay = true
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, moveOutCommitResponseData(result))
}

func moveOutCommitResponseData(result moveOutCommitResult) map[string]any {
	preview, truncated := moveOutTargetResultPreview(result.Targets)
	return map[string]any{
		"status":                   result.Status,
		"move_request_id":          result.MoveRequestID,
		"manifest_sha256":          result.ManifestSHA256,
		"moved_count":              result.MovedCount,
		"moved_bytes":              result.MovedBytes,
		"moved_bytes_known":        result.MovedBytesKnown,
		"failed_count":             result.FailedCount,
		"target_count":             len(result.Targets),
		"target_preview":           preview,
		"target_preview_truncated": truncated,
		"move_id":                  result.MoveID,
		"audit_event_id":           result.AuditEventID,
		"idempotent_replay":        result.IdempotentReplay,
		"reversible":               result.Reversible,
		"quarantine_location":      result.QuarantineLocation,
		"quarantine_path_scope":    "workspace_parent",
	}
}

func moveOutTargetResultPreview(targets []moveOutTargetResult) ([]moveOutTargetResult, bool) {
	limit := len(targets)
	if limit > MaxMoveOutResponsePreviewTargets {
		limit = MaxMoveOutResponsePreviewTargets
	}
	return append([]moveOutTargetResult(nil), targets[:limit]...), len(targets) > limit
}

func moveOutTargetResultPreviewData(targets []moveOutTargetResult) []moveOutTargetResult {
	preview, _ := moveOutTargetResultPreview(targets)
	return preview
}

func parseMoveOutTargets(payload map[string]any) ([]deletion.Target, error) {
	raw, ok := payload["targets"]
	if !ok {
		return nil, errors.New("targets is required")
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid targets: %w", err)
	}
	var targets []deletion.Target
	if err := json.Unmarshal(b, &targets); err != nil || len(targets) == 0 {
		return nil, errors.New("targets must contain at least one path")
	}
	seen := map[string]bool{}
	for i := range targets {
		canonical, err := canonicalMoveOutPath(targets[i].Path)
		if err != nil {
			return nil, fmt.Errorf("targets[%d]: %w", i, err)
		}
		targets[i].Path = canonical
		targets[i].Kind = ""
		targets[i].ExpectedSHA256 = normalizeSHA(targets[i].ExpectedSHA256)
		targets[i].Revision = strings.TrimSpace(targets[i].Revision)
		if targets[i].Revision != "" && targets[i].ExpectedSHA256 != "" {
			return nil, fmt.Errorf("targets[%d]: rev and expected_sha256 are mutually exclusive", i)
		}
		if seen[canonical] {
			return nil, fmt.Errorf("duplicate move-out target %q", canonical)
		}
		seen[canonical] = true
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Path < targets[j].Path })
	for i := 1; i < len(targets); i++ {
		if strings.HasPrefix(targets[i].Path, targets[i-1].Path+"/") {
			return nil, fmt.Errorf("overlapping move-out targets are not allowed: %q contains %q", targets[i-1].Path, targets[i].Path)
		}
	}
	return targets, nil
}

func (r *Runtime) inferMoveOutTargetKinds(session remotesession.Session, targets []deletion.Target) ([]deletion.Target, error) {
	cfg := r.effectiveConfig(session.WorkspacePath)
	resolved := append([]deletion.Target(nil), targets...)
	for i := range resolved {
		lexical, info, err := lstatMoveOutTarget(session.WorkspacePath, resolved[i].Path, cfg)
		if err != nil {
			return nil, err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			resolved[i].Kind = "symlink"
			if resolved[i].Revision != "" {
				linkTarget, readErr := os.Readlink(lexical)
				if readErr != nil {
					return nil, &moveOutValidationError{Code: "SYMLINK_READ_FAILED", Message: readErr.Error(), Path: resolved[i].Path}
				}
				actual := digestBytes([]byte(linkTarget))
				if compactFileRevision(actual) != resolved[i].Revision {
					return nil, &moveOutValidationError{Code: "STALE_REVISION", Message: "rev does not match current symlink text", Path: resolved[i].Path, CurrentSHA: actual}
				}
				resolved[i].ExpectedSHA256, resolved[i].Revision = actual, ""
			}
		case info.IsDir():
			resolved[i].Kind = "directory"
			if resolved[i].ExpectedSHA256 != "" || resolved[i].Revision != "" {
				return nil, &moveOutValidationError{Code: "INVALID_REQUEST", Message: "revision guard is not supported for directory; directory contents are intentionally not enumerated", Path: resolved[i].Path}
			}
		case info.Mode().IsRegular():
			resolved[i].Kind = "file"
			if resolved[i].Revision != "" {
				content, readErr := os.ReadFile(lexical)
				if readErr != nil {
					return nil, &moveOutValidationError{Code: "FILE_READ_FAILED", Message: readErr.Error(), Path: resolved[i].Path}
				}
				actual := digestBytes(content)
				if compactFileRevision(actual) != resolved[i].Revision {
					return nil, &moveOutValidationError{Code: "STALE_REVISION", Message: "rev does not match current file", Path: resolved[i].Path, CurrentSHA: actual}
				}
				resolved[i].ExpectedSHA256, resolved[i].Revision = actual, ""
			}
			if resolved[i].ExpectedSHA256 == "" {
				return nil, &moveOutValidationError{Code: "INVALID_REQUEST", Message: "rev from read is required for regular files (legacy expected_sha256 also accepted)", Path: resolved[i].Path}
			}
		default:
			return nil, &moveOutValidationError{Code: "MOVE_OUT_FILE_ONLY", Message: "only regular files, directories or symlink entries can be moved out", Path: resolved[i].Path}
		}
	}
	return resolved, nil
}

func (r *Runtime) freezeMoveOutManifest(session remotesession.Session, targets []deletion.Target) (deletion.Manifest, error) {
	manifest := deletion.Manifest{Workspace: session.WorkspaceName, Targets: make([]deletion.Target, 0, len(targets)), TotalBytesKnown: true}
	for _, target := range targets {
		if err := validateFrozenMoveOutTarget(session.WorkspacePath, target, r.effectiveConfig(session.WorkspacePath)); err != nil {
			return deletion.Manifest{}, err
		}
		absolute, err := file.LexicalPath(session.WorkspacePath, target.Path)
		if err != nil {
			return deletion.Manifest{}, &moveOutValidationError{Code: "PATH_ESCAPE", Message: err.Error(), Path: target.Path}
		}
		if target.Kind == "directory" {
			manifest.Targets = append(manifest.Targets, deletion.Target{Path: target.Path, Kind: "directory"})
			manifest.DirectoryCount++
			manifest.TotalBytesKnown = false
			continue
		}
		if target.Kind == "symlink" {
			entry, linkErr := freezeMoveOutSymlink(target.Path, absolute)
			if linkErr != nil {
				return deletion.Manifest{}, linkErr
			}
			if target.ExpectedSHA256 != "" && target.ExpectedSHA256 != entry.LinkSHA256 {
				return deletion.Manifest{}, &moveOutValidationError{Code: "STALE_REVISION", Message: "expected_sha256 does not match current symlink text", Path: target.Path, CurrentSHA: entry.LinkSHA256}
			}
			entry.ExpectedSHA256 = target.ExpectedSHA256
			manifest.Targets = append(manifest.Targets, entry)
			manifest.SymlinkCount++
			manifest.TotalBytes += entry.Size
			continue
		}
		content, readErr := os.ReadFile(absolute)
		if readErr != nil {
			return deletion.Manifest{}, &moveOutValidationError{Code: "FILE_NOT_FOUND", Message: "file not found", Path: target.Path}
		}
		actual := digestBytes(content)
		if actual != target.ExpectedSHA256 {
			return deletion.Manifest{}, &moveOutValidationError{Code: "STALE_REVISION", Message: "expected_sha256 does not match current file", Path: target.Path, CurrentSHA: actual}
		}
		copyTarget := target
		copyTarget.Size = int64(len(content))
		manifest.Targets = append(manifest.Targets, copyTarget)
		manifest.FileCount++
		manifest.TotalBytes += copyTarget.Size
	}
	return manifest, nil
}

func freezeMoveOutSymlink(relative, lexical string) (deletion.Target, error) {
	linkTarget, err := os.Readlink(lexical)
	if err != nil {
		return deletion.Target{}, &moveOutValidationError{Code: "SYMLINK_READ_FAILED", Message: err.Error(), Path: relative}
	}
	linkSHA := digestBytes([]byte(linkTarget))
	return deletion.Target{Path: relative, Kind: "symlink", LinkTarget: linkTarget, LinkSHA256: linkSHA, Size: int64(len(linkTarget))}, nil
}

func validateFrozenMoveOutTarget(workspacePath string, target deletion.Target, cfg config.Config) error {
	_, info, err := lstatMoveOutTarget(workspacePath, target.Path, cfg)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if target.Kind != "symlink" {
			return &moveOutValidationError{Code: "STALE_REVISION", Message: "target changed to a symlink after prepare type inference", Path: target.Path}
		}
		return nil
	}
	if target.Kind == "directory" {
		if !info.IsDir() {
			return &moveOutValidationError{Code: "MOVE_OUT_DIRECTORY_ONLY", Message: "target is not a directory", Path: target.Path}
		}
		return nil
	}
	if target.Kind != "file" || !info.Mode().IsRegular() {
		return &moveOutValidationError{Code: "MOVE_OUT_FILE_ONLY", Message: "only regular files, directories or explicitly requested symlinks can be moved out", Path: target.Path}
	}
	return nil
}

func lstatMoveOutTarget(workspacePath, relativePath string, cfg config.Config) (string, os.FileInfo, error) {
	canonical, err := canonicalMoveOutPath(relativePath)
	if err != nil || canonical != relativePath {
		return "", nil, &moveOutValidationError{Code: "INVALID_PATH", Message: "move-out target must be a canonical workspace-relative path", Path: relativePath}
	}
	if security.MatchFile(cfg.Security.Files, relativePath) == security.Deny {
		return "", nil, &moveOutValidationError{Code: "FILE_DENIED", Message: "file denied by policy", Path: relativePath}
	}
	lexical, err := file.LexicalPath(workspacePath, relativePath)
	if err != nil {
		return "", nil, &moveOutValidationError{Code: "PATH_ESCAPE", Message: err.Error(), Path: relativePath}
	}
	if err := rejectSymlinkParentComponents(workspacePath, relativePath); err != nil {
		return "", nil, err
	}
	info, err := os.Lstat(lexical)
	if err != nil {
		return "", nil, &moveOutValidationError{Code: "FILE_NOT_FOUND", Message: "file not found", Path: relativePath}
	}
	return lexical, info, nil
}

func rejectSymlinkParentComponents(workspacePath, relativePath string) error {
	current := workspacePath
	components := strings.Split(filepath.ToSlash(relativePath), "/")
	for index, component := range components {
		if component == "" || component == "." {
			continue
		}
		if index == len(components)-1 {
			break
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return &moveOutValidationError{Code: "PATH_STAT_FAILED", Message: err.Error(), Path: relativePath}
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return &moveOutValidationError{Code: "SYMLINK_NOT_ALLOWED", Message: "move-out path cannot contain symlink components", Path: relativePath}
		}
	}
	return nil
}

func (r *Runtime) inspectMoveOutTargetForCommit(session remotesession.Session, target deletion.Target) ([]deletion.Target, string, error) {
	lexical, info, err := lstatMoveOutTarget(session.WorkspacePath, target.Path, r.effectiveConfig(session.WorkspacePath))
	if err != nil {
		if moveOutErrorCode(err) == "FILE_NOT_FOUND" {
			return nil, "", &moveOutValidationError{Code: "STALE_REVISION", Message: "move-out target no longer exists", Path: target.Path}
		}
		return nil, "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		entry, linkErr := freezeMoveOutSymlink(target.Path, lexical)
		if linkErr != nil {
			return nil, "", linkErr
		}
		return []deletion.Target{entry}, "symlink", nil
	}
	switch target.Kind {
	case "file":
		if !info.Mode().IsRegular() {
			return nil, "", &moveOutValidationError{Code: "STALE_REVISION", Message: "file target changed type before move-out", Path: target.Path}
		}
		content, readErr := os.ReadFile(lexical)
		if readErr != nil {
			return nil, "", &moveOutValidationError{Code: "FILE_READ_FAILED", Message: readErr.Error(), Path: target.Path}
		}
		actual := digestBytes(content)
		if actual != target.ExpectedSHA256 {
			return nil, "", &moveOutValidationError{Code: "STALE_REVISION", Message: "expected_sha256 does not match current file", Path: target.Path, CurrentSHA: actual}
		}
		return []deletion.Target{{Path: target.Path, Kind: "file", ExpectedSHA256: actual, Size: int64(len(content))}}, "file", nil
	case "symlink":
		return nil, "", &moveOutValidationError{Code: "STALE_REVISION", Message: "symlink target changed type before move-out", Path: target.Path}
	case "directory":
		if !info.IsDir() {
			return nil, "", &moveOutValidationError{Code: "STALE_REVISION", Message: "directory target changed type before move-out", Path: target.Path}
		}
		return []deletion.Target{{Path: target.Path, Kind: "directory"}}, "directory", nil
	default:
		return nil, "", &moveOutValidationError{Code: "STALE_REVISION", Message: "move-out target changed type before move-out", Path: target.Path}
	}
}

type moveOutValidationError struct {
	Code, Message, Path, CurrentSHA string
}

func (e *moveOutValidationError) Error() string { return e.Message }

func moveOutErrorCode(err error) string {
	var validation *moveOutValidationError
	if errors.As(err, &validation) {
		return validation.Code
	}
	return "MOVE_OUT_FAILED"
}

func moveOutErrorMessage(err error) string {
	var validation *moveOutValidationError
	if errors.As(err, &validation) {
		return validation.Message
	}
	return err.Error()
}

func moveOutErrorDetails(err error) map[string]any {
	var validation *moveOutValidationError
	if !errors.As(err, &validation) {
		return nil
	}
	details := map[string]any{"path": validation.Path}
	if validation.CurrentSHA != "" {
		if rev := compactFileRevision(validation.CurrentSHA); rev != "" {
			details["current_rev"] = rev
		}
	}
	return details
}

func (r *Runtime) moveOutError(envReq envelope.Request, session remotesession.Session, code, message string, details map[string]any) (*mcp.CallToolResult, error) {
	response := envelope.Fail(envelope.StatusError, envReq.RequestID, session.WorkspaceName, nil, code, message)
	response.RemoteSessionID = session.ID
	for key, value := range details {
		response.Error.Details[key] = value
	}
	return r.resultJSON(response)
}

func canonicalMoveOutPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || filepath.IsAbs(value) || strings.IndexByte(value, 0) >= 0 {
		return "", errors.New("path must be a non-empty relative path")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("path must remain inside the workspace")
	}
	return clean, nil
}

func normalizeSHA(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if strings.HasPrefix(value, "sha256:") {
		return value
	}
	if len(value) == sha256.Size*2 {
		return "sha256:" + value
	}
	return value
}

func digestBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sameMoveOutTargetSpec(existing, requested []deletion.Target) bool {
	if len(existing) != len(requested) {
		return false
	}
	for i := range existing {
		if existing[i].Path != requested[i].Path || existing[i].Kind != requested[i].Kind {
			return false
		}
		if requested[i].Kind == "directory" && requested[i].ExpectedSHA256 == "" {
			continue
		}
		if existing[i].ExpectedSHA256 != requested[i].ExpectedSHA256 {
			return false
		}
	}
	return true
}
