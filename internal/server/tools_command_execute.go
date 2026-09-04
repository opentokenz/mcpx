package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/approval"
	"mcpx/internal/audit"
	"mcpx/internal/auth"
	"mcpx/internal/config"
	"mcpx/internal/envelope"
	workspacefile "mcpx/internal/file"
	"mcpx/internal/projecttask"
	"mcpx/internal/remotesession"
	"mcpx/internal/security"
	"mcpx/internal/sqlitequery"
	"mcpx/internal/terminal"
)

const defaultCommandYield = 10 * time.Second

// toolCommandExecute uses one Task implementation for both ordinary commands
// and discovered project tasks. It waits for short commands, but only exposes
// an execution_task_id to clients when the process still runs after the yield window.
func (r *Runtime) toolCommandExecute(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, remote, fail := r.changeRequest(ctx, req, true)
	if fail != nil {
		return fail, nil
	}
	purpose, scope, intentErr := commandIntent(envReq)
	if intentErr != nil {
		return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "bad_request", intentErr.Error())
	}
	runtimeSpec, runtimeErr := ephemeralRuntimeSpecFromPayload(envReq.Payload)
	if runtimeErr != nil {
		return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "bad_request", runtimeErr.Error())
	}
	if runtimeSpec != nil && !isCleanCoreRequest(ctx) {
		return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "bad_request", "runtime+script is available only through clean-core execute")
	}
	command := strings.TrimSpace(stringPayload(envReq.Payload, "command"))
	taskName := strings.TrimSpace(stringPayload(envReq.Payload, "task"))
	if runtimeSpec != nil {
		command = runtimeSpec.Command
	} else if taskName != "" {
		if command != "" {
			return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "bad_request", "command and task are mutually exclusive")
		}
		discovered, ok := projecttask.Find(remote.WorkspacePath, taskName)
		if !ok {
			return r.terminalErrorForContext(ctx, envReq, remote.ID, remote.WorkspaceName, "task_not_found", fmt.Sprintf("project task %q not found", taskName))
		}
		command = discovered.Command
	}
	if command == "" {
		return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "bad_request", "command, task, or runtime+script is required")
	}
	payloadDigest := ""
	if runtimeSpec != nil {
		payloadDigest = runtimeSpec.ScriptSHA256
	}
	commandDigest := commandRequestDigestWithPayload(envReq.RequestID, remote.ID, remote.WorkspaceName, command, purpose, scope, payloadDigest)
	effective := r.effectiveConfig(remote.WorkspacePath)
	if !effective.Terminal.Enabled {
		return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "disabled", "terminal tools are disabled")
	}
	analysis := security.AnalyzeCommand(effective.Security.Commands, command)
	if runtimeSpec != nil && runtimeSpec.Runtime == "sqlite" {
		normalizedDatabase, normalizeErr := normalizeSQLiteDatabasePath(remote.WorkspacePath, runtimeSpec.Database)
		if normalizeErr != nil {
			return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "SQLITE_QUERY_ERROR", normalizeErr.Error())
		}
		runtimeSpec.Database = normalizedDatabase
		if security.MatchFile(effective.Security.Files, normalizedDatabase) != security.Allow {
			response := envelope.Fail(envelope.StatusDenied, envReq.RequestID, remote.WorkspaceName,
				map[string]any{"database": normalizedDatabase}, "FILE_DENIED", "sqlite database denied by file policy")
			response.RemoteSessionID = remote.ID
			return r.resultJSON(response)
		}
		analysis = readonlySQLiteAnalysis(command)
	}
	decision := analysis.Decision
	yieldForRequest := commandYield(envReq.Payload)
	if runtimeSpec != nil {
		yieldForRequest = ephemeralRuntimeWait(envReq.Payload)
	}
	executeApproved := func(yield time.Duration) (*mcp.CallToolResult, error) {
		if runtimeSpec == nil {
			return r.executeApprovedCommandTask(ctx, envReq, principal, remote, command, yield, purpose, scope, commandDigest, analysis)
		}
		detail := runtimeExecutionDetail(purpose, scope, commandDigest, runtimeSpec, analysis)
		if err := r.writeAudit(audit.Event{RequestID: envReq.RequestID, RemoteSessionID: remote.ID, Workspace: remote.WorkspaceName, Tool: "execute", Command: command, Status: "preflight_approved", Detail: detail}); err != nil {
			return r.terminalErrorForContext(ctx, envReq, remote.ID, remote.WorkspaceName, "audit_write_failed", "runtime preflight audit could not be persisted; script was not executed")
		}
		if runtimeSpec.Runtime == "sqlite" {
			return r.executeSQLiteRuntime(ctx, envReq, remote, runtimeSpec, purpose, scope, commandDigest, analysis)
		}
		return r.executeRuntimeTask(ctx, envReq, principal, remote, runtimeSpec, purpose, scope, commandDigest, analysis)
	}
	switch decision {
	case security.Deny:
		r.logAudit(audit.Event{RequestID: envReq.RequestID, RemoteSessionID: remote.ID, Workspace: remote.WorkspaceName, Tool: "command_execute", Command: command, Status: "denied", Detail: runtimeExecutionDetail(purpose, scope, commandDigest, runtimeSpec, analysis)})
		message := "command denied by policy after auditing all command segments"
		if containsUnsafeShellFeature(command) {
			message += "；命令包含无法独立审计的 shell 特性。&&、|| 和 ; 会拆分后逐段审计；quoted heredoc（如 <<'PY'）会作为 literal stdin 随所属命令一起审计。管道、普通重定向、单个 &、任意多行 shell、$() 和反引号命令替换仍会拒绝；遇到这些情况请改用可独立审计的简单命令，例如 git fetch && git rev-parse HEAD && git status。"
		}
		return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "denied", message)
	case security.Confirm:
		yield := yieldForRequest
		confirmationToken := stringPayload(envReq.Payload, "confirmation_token")
		if isCleanCoreRequest(ctx) {
			userConfirmed := boolPayload(envReq.Payload, "user_confirmed")
			pending, pendingOK := r.pendingCommandConfirmation(remote.ID, principal.ID, command, scope, commandDigest)
			if !userConfirmed || !pendingOK {
				if !pendingOK {
					var confirmationErr error
					pending, confirmationErr = r.approvals.PutPending(approval.Pending{
						Tool: "command_execute", Summary: command, Command: command,
						CommandYieldMs: int(yield / time.Millisecond), Purpose: purpose, Scope: scope,
						CommandDigest: commandDigest, WorkDir: remote.WorkspacePath,
						RequestID: envReq.RequestID, Workspace: remote.WorkspaceName,
						RemoteSessionID: remote.ID, PrincipalID: principal.ID,
						ContentKey: cleanCommandConfirmationContentKey(principal.ID, commandDigest),
					})
					if confirmationErr != nil {
						return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "confirmation_store_error", confirmationErr.Error())
					}
				}
				confirmationData := map[string]any{
					"command": command, "purpose": purpose, "scope": scope,
					"command_digest": commandDigest, "pending_digest": commandDigest,
					"command_policy":        commandPolicyData(analysis),
					"confirmation_required": true, "user_confirmed_required": true,
					"summary": "执行已完成策略预检；请向用户展示命令或临时脚本摘要及用途，确认后将 user_confirmed=true 原样重试。",
				}
				addRuntimeConfirmationData(confirmationData, runtimeSpec)
				response := envelope.Fail(envelope.StatusNeedConfirmation, envReq.RequestID, remote.WorkspaceName,
					confirmationData, "USER_CONFIRMATION_REQUIRED", "命令执行等待用户语义确认")
				response.RemoteSessionID = remote.ID
				retryArguments := map[string]any{
					"remote_session_id": remote.ID, "action": "run", "command": command,
					"purpose": purpose, "scope": scope, "user_confirmed": true,
				}
				if runtimeSpec != nil {
					retryArguments = runtimeConfirmationRetryArguments(remote.ID, purpose, scope, runtimeSpec)
				}
				addRecoveryAction(&response, "execute", "用户确认后使用相同 command/task 或 runtime+script、purpose 和 remote_session_id 重试，并设置 user_confirmed=true", retryArguments)
				return r.resultJSON(response)
			}
			result, executeErr := executeApproved(yield)
			if executeErr == nil {
				if _, consumed := r.approvals.Consume(pending.ID); !consumed {
					return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "confirmation_state_error", "confirmed command approval could not be consumed")
				}
			}
			return result, executeErr
		}
		if !r.hasPendingCommandConfirmation(remote.ID, principal.ID, command, purpose, scope, confirmationToken) {
			pending, confirmationErr := r.approvals.PutPending(approval.Pending{
				Tool: "command_execute", Summary: command, Command: command,
				CommandYieldMs: int(yield / time.Millisecond), Purpose: purpose, Scope: scope,
				CommandDigest: commandDigest, WorkDir: remote.WorkspacePath,
				RequestID: envReq.RequestID, Workspace: remote.WorkspaceName,
				RemoteSessionID: remote.ID, PrincipalID: principal.ID,
			})
			if confirmationErr != nil {
				return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "confirmation_store_error", confirmationErr.Error())
			}
			// Struct field order puts confirmation_token first in the JSON
			// text, so host previews that truncate long tool output still show
			// the full token to the model.
			confirmationMessage := "confirmation_token: " + pending.ConfirmationToken + "；请向用户展示命令及用途，获得明确语义确认后，使用相同 command 和该 confirmation_token 重试。该 token 仅绑定本次操作，不承担认证职责。"
			if confirmationToken != "" {
				confirmationMessage = "你提供的 confirmation_token 未匹配当前待确认项；请使用本响应 data.confirmation_token 中的完整 token 原样重试：" + pending.ConfirmationToken + "（相同 command、remote_session_id 和 scope）。"
			}
			confirmationData := commandConfirmationData{
				ConfirmationToken:    pending.ConfirmationToken,
				Command:              command,
				Purpose:              purpose,
				Scope:                scope,
				CommandDigest:        commandDigest,
				CommandPolicy:        commandPolicyData(analysis),
				ConfirmationRequired: true,
				ConfirmationMessage:  confirmationMessage,
			}
			response := envelope.Fail(envelope.StatusNeedConfirmation, envReq.RequestID, remote.WorkspaceName,
				confirmationData, "USER_CONFIRMATION_REQUIRED", "命令执行等待用户语义确认")
			response.RemoteSessionID = remote.ID
			return r.resultJSON(response)
		}
		result, executeErr := executeApproved(yield)
		if executeErr == nil {
			r.consumePendingCommandConfirmation(remote.ID, principal.ID, command, purpose, scope, confirmationToken)
		}
		return result, executeErr
	}

	return executeApproved(yieldForRequest)
}

// commandConfirmationData keeps confirmation_token first in the serialized
// JSON payload so truncated host previews cannot hide it.
type commandConfirmationData struct {
	ConfirmationToken    string         `json:"confirmation_token"`
	Command              string         `json:"command"`
	Purpose              string         `json:"purpose"`
	Scope                string         `json:"scope"`
	CommandDigest        string         `json:"command_digest"`
	CommandPolicy        map[string]any `json:"command_policy,omitempty"`
	ConfirmationRequired bool           `json:"confirmation_required"`
	ConfirmationMessage  string         `json:"confirmation_message"`
}

func (r *Runtime) executeApprovedCommandTask(ctx context.Context, envReq envelope.Request, principal auth.Principal, remote remotesession.Session, command string, yield time.Duration, purpose, scope, commandDigest string, analysis security.CommandAnalysis) (*mcp.CallToolResult, error) {
	detail := commandExecutionDetail(purpose, scope, commandDigest, analysis)
	if err := r.writeAudit(audit.Event{
		RequestID: envReq.RequestID, RemoteSessionID: remote.ID, Workspace: remote.WorkspaceName,
		Tool: "command_execute", Command: command, Status: "preflight_approved", Detail: detail,
	}); err != nil {
		return r.terminalErrorForContext(ctx, envReq, remote.ID, remote.WorkspaceName, "audit_write_failed", "command preflight audit could not be persisted; no command segment was executed")
	}
	return r.executeCommandTask(ctx, envReq, principal, remote, command, yield, purpose, scope, commandDigest, analysis)
}

func (r *Runtime) executeCommandTask(ctx context.Context, envReq envelope.Request, principal auth.Principal, remote remotesession.Session, command string, yield time.Duration, purpose, scope, commandDigest string, analysis security.CommandAnalysis) (*mcp.CallToolResult, error) {
	originTool := toolInvocationName(ctx)
	if originTool == "" {
		originTool = "command_execute"
	}
	task, err := r.tasks.StartRemoteWithObservationContext(ctx, envReq.RequestID, observationCallID(envReq), originTool, remote.ID, remote.WorkspaceName, remote.WorkspacePath, command)
	if err != nil {
		return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "start_error", err.Error())
	}
	_ = r.remote.AddEvent(ctx, principal, remotesession.Event{RemoteSessionID: remote.ID, Type: "command.started", OperationID: task.ID, Summary: command, Metadata: commandExecutionDetail(purpose, scope, commandDigest, analysis)})
	waitCtx, cancel := context.WithTimeout(ctx, yield)
	completed := task.Wait(waitCtx)
	cancel()
	data := r.taskResultData(task, 0, 0, true)
	data["purpose"] = purpose
	data["scope"] = scope
	data["command_digest"] = commandDigest
	data["command_policy"] = commandPolicyData(analysis)
	data["command"] = command
	data["working_directory"] = remote.WorkspacePath
	data["workspace_scoped"] = scope == "workspace"
	capTaskExecutionOutput(data, config.MaxResultBytes(r.cfg.Limits))
	if completed {
		data["completed_in_call"] = true
		delete(data, "execution_task_id")
		detail := commandExecutionDetail(purpose, scope, commandDigest, analysis)
		detail["exit_code"] = data["exit_code"]
		if code, message := annotateExecutionOutcome(data); code != "" {
			response := envelope.Fail(envelope.StatusError, envReq.RequestID, remote.WorkspaceName, data, code, message)
			response.RemoteSessionID = remote.ID
			return r.resultJSON(response)
		}
		r.logAudit(audit.Event{RequestID: envReq.RequestID, RemoteSessionID: remote.ID, Workspace: remote.WorkspaceName, Tool: "command_execute", Command: command, Status: "ok", Detail: detail})
		result := compactToolResult(data, commandOutputText(ctx, data, fmt.Sprintf("Command completed with exit code %v.", data["exit_code"])))
		return result, nil
	}
	data["completed_in_call"] = false
	nextTool := "task_manage"
	if isCleanCoreRequest(ctx) {
		nextTool = "execute"
	}
	data["next_action"] = nextAction(nextTool, map[string]any{
		"remote_session_id": remote.ID, "action": "attach", "execution_task_id": task.ID,
		"stdout_offset": data["stdout_next_offset"], "stderr_offset": data["stderr_next_offset"],
		"yield_time_ms": int(yield / time.Millisecond),
	})
	data["summary"] = fmt.Sprintf("Command is running as Task %s.", task.ID)
	detail := commandExecutionDetail(purpose, scope, commandDigest, analysis)
	detail["execution_task_id"] = task.ID
	r.logAudit(audit.Event{RequestID: envReq.RequestID, RemoteSessionID: remote.ID, Workspace: remote.WorkspaceName, Tool: "command_execute", Command: command, Status: "running", Detail: detail})
	response := envelope.Accepted(envReq.RequestID, remote.WorkspaceName, data)
	response.RemoteSessionID = remote.ID
	responseData, responseErr := r.resultJSON(response)
	if responseErr != nil {
		return nil, responseErr
	}
	return responseData, nil
}

// commandOutputText renders the model-facing summary with any stdout/stderr
// inline, so completed command output is readable directly in the conversation.
// Truncated streams state the truncation and point to task_read/task for more.
func commandOutputText(ctx context.Context, data map[string]any, summary string) string {
	var builder strings.Builder
	builder.WriteString(summary)
	for _, stream := range []string{"stdout", "stderr"} {
		text, _ := data[stream].(string)
		if text == "" {
			continue
		}
		builder.WriteString("\n\n")
		builder.WriteString(stream)
		builder.WriteString(":\n")
		builder.WriteString(text)
	}
	if truncated, _ := data["output_truncated"].(bool); truncated {
		builder.WriteString("\n\n")
		if isCleanCoreRequest(ctx) {
			builder.WriteString("Output truncated; call observe(view=logs) or execute(action=attach) to read more.")
		} else {
			builder.WriteString("Output truncated; call task(operation=attach) or task_read(view=logs) to read more.")
		}
	}
	return builder.String()
}

func capTaskExecutionOutput(data map[string]any, maxBytes int) {
	const hardCap = 256 << 10 // 256 KiB inline budget
	if maxBytes <= 0 || maxBytes > hardCap {
		maxBytes = hardCap
	}
	for _, stream := range []string{"stdout", "stderr"} {
		value, _ := data[stream].(string)
		trimmed, truncated := TruncateUTF8(value, maxBytes)
		if !truncated {
			continue
		}
		data[stream] = trimmed
		data[stream+"_truncated"] = true
		offset, _ := data[stream+"_offset"].(int)
		data[stream+"_next_offset"] = offset + len(trimmed)
		data["output_truncated"] = true
	}
}

func commandIntent(req envelope.Request) (purpose, scope string, err error) {
	purpose = strings.TrimSpace(req.Intent)
	if purpose == "" {
		return "", "", fmt.Errorf("purpose is required and must describe the user's requested development action")
	}
	if len(purpose) > 512 {
		return "", "", fmt.Errorf("purpose exceeds 512 bytes")
	}
	scope, _ = req.Payload["scope"].(string)
	scope = strings.TrimSpace(scope)
	if scope == "" {
		scope = "workspace"
	}
	if scope != "workspace" {
		return "", "", fmt.Errorf("unsupported execution scope %q; only workspace is allowed", scope)
	}
	return purpose, scope, nil
}

func commandFailureCode(exitCode int, _ string) string {
	// POSIX shells reserve 127 for command lookup failure. Do not inspect the
	// shell's diagnostic text here: modern Bash localizes that message, so an
	// English substring check makes protocol error taxonomy locale-dependent.
	if exitCode == 127 {
		return "COMMAND_NOT_FOUND"
	}
	return "PROCESS_EXIT"
}

func commandFailureMessage(code string, exitCode int) string {
	if code == "COMMAND_NOT_FOUND" {
		return fmt.Sprintf("command executable was not found (exit code %d)", exitCode)
	}
	return fmt.Sprintf("command exited with code %d", exitCode)
}

func commandRequestDigest(requestID, remoteSessionID, workspace, command, purpose, scope string) string {
	return commandRequestDigestWithPayload(requestID, remoteSessionID, workspace, command, purpose, scope, "")
}

func commandRequestDigestWithPayload(requestID, remoteSessionID, workspace, command, purpose, scope, payloadDigest string) string {
	// Request IDs identify transport attempts. They must not change the
	// semantic operation digest used to bind confirmation across retry.
	_ = requestID
	parts := []string{remoteSessionID, workspace, command, purpose, scope}
	if payloadDigest != "" {
		parts = append(parts, payloadDigest)
	}
	value := strings.Join(parts, "\x00")
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func commandPolicyData(analysis security.CommandAnalysis) map[string]any {
	segments := make([]map[string]any, 0, len(analysis.Segments))
	for index, segment := range analysis.Segments {
		item := map[string]any{
			"index": index + 1, "command": segment.Command, "decision": segment.Decision.String(),
		}
		if segment.Operator != "" {
			item["operator_after"] = segment.Operator
		}
		segments = append(segments, item)
	}
	return map[string]any{
		"decision": analysis.Decision.String(), "segments": segments, "unsafe": analysis.Unsafe,
		"all_segments_preflighted": !analysis.Unsafe && len(segments) > 0,
		"atomic_policy_gate":       true,
		"execute_original_once":    true,
	}
}

func commandExecutionDetail(purpose, scope, commandDigest string, analysis security.CommandAnalysis) map[string]any {
	return map[string]any{
		"purpose": purpose, "scope": scope, "command_digest": commandDigest,
		"workspace_scoped": scope == "workspace", "command_policy": commandPolicyData(analysis),
	}
}

const (
	ephemeralScriptMaxBytes      = 64 << 10
	ephemeralRuntimeDefaultWait  = 15 * time.Second
	ephemeralRuntimeMaxWait      = 15 * time.Second
	ephemeralRuntimeWallLimit    = 45 * time.Second
	ephemeralRuntimeCPUTimeLimit = 15 * time.Second
)

type ephemeralRuntimeSpec struct {
	Runtime      string
	Executable   string
	Args         []string
	Command      string
	Database     string
	Script       string
	ScriptSHA256 string
	ScriptBytes  int
}

func ephemeralRuntimeSpecFromPayload(payload map[string]any) (*ephemeralRuntimeSpec, error) {
	runtimeName := strings.ToLower(strings.TrimSpace(stringPayload(payload, "runtime")))
	rawScript, scriptPresent := payload["script"]
	if runtimeName == "" && !scriptPresent {
		return nil, nil
	}
	if runtimeName == "" {
		return nil, fmt.Errorf("runtime is required when script is provided")
	}
	script, ok := rawScript.(string)
	if !scriptPresent || !ok || script == "" {
		return nil, fmt.Errorf("script is required and must be a non-empty string when runtime is provided")
	}
	if len(script) > ephemeralScriptMaxBytes {
		return nil, fmt.Errorf("script exceeds %d bytes; create a workspace file with edit instead", ephemeralScriptMaxBytes)
	}
	if strings.TrimSpace(stringPayload(payload, "command")) != "" || strings.TrimSpace(stringPayload(payload, "task")) != "" {
		return nil, fmt.Errorf("runtime+script is mutually exclusive with command and task")
	}
	database := strings.TrimSpace(stringPayload(payload, "database"))
	spec := &ephemeralRuntimeSpec{Runtime: runtimeName, Script: script, ScriptBytes: len(script)}
	switch runtimeName {
	case "python":
		if database != "" {
			return nil, fmt.Errorf("database is supported only by sqlite runtime")
		}
		spec.Executable, spec.Args, spec.Command = "python3", []string{"-"}, "python3 -"
	case "node":
		if database != "" {
			return nil, fmt.Errorf("database is supported only by sqlite runtime")
		}
		spec.Executable, spec.Args, spec.Command = "node", []string{"-"}, "node -"
	case "sqlite":
		if database == "" {
			return nil, fmt.Errorf("database is required for sqlite runtime")
		}
		spec.Database = database
		spec.Command = "sqlite readonly query " + database
	default:
		return nil, fmt.Errorf("unsupported runtime %q; use python, node, or sqlite", runtimeName)
	}
	digest := sha256.Sum256([]byte(script))
	spec.ScriptSHA256 = "sha256:" + hex.EncodeToString(digest[:])
	return spec, nil
}

func ephemeralRuntimeWait(payload map[string]any) time.Duration {
	yield := intPayload(payload, "yield_time_ms")
	if yield <= 0 {
		return ephemeralRuntimeDefaultWait
	}
	wait := time.Duration(yield) * time.Millisecond
	if wait > ephemeralRuntimeMaxWait {
		return ephemeralRuntimeMaxWait
	}
	return wait
}

func runtimeExecutionDetail(purpose, scope, commandDigest string, spec *ephemeralRuntimeSpec, analysis security.CommandAnalysis) map[string]any {
	detail := commandExecutionDetail(purpose, scope, commandDigest, analysis)
	if spec == nil {
		return detail
	}
	detail["runtime"] = spec.Runtime
	detail["script_sha256"] = spec.ScriptSHA256
	detail["script_bytes"] = spec.ScriptBytes
	if spec.Runtime == "sqlite" {
		detail["database"] = spec.Database
		detail["readonly"] = true
		return detail
	}
	detail["wall_limit_ms"] = ephemeralRuntimeWallLimit.Milliseconds()
	detail["cpu_time_limit_ms"] = ephemeralRuntimeCPUTimeLimit.Milliseconds()
	return detail
}

func addRuntimeConfirmationData(data map[string]any, spec *ephemeralRuntimeSpec) {
	if spec == nil {
		return
	}
	data["runtime"] = spec.Runtime
	data["script_sha256"] = spec.ScriptSHA256
	data["script_bytes"] = spec.ScriptBytes
	if spec.Runtime == "sqlite" {
		data["database"] = spec.Database
		data["readonly"] = true
		return
	}
	data["wall_limit_ms"] = ephemeralRuntimeWallLimit.Milliseconds()
	data["cpu_time_limit_ms"] = ephemeralRuntimeCPUTimeLimit.Milliseconds()
}

func runtimeConfirmationRetryArguments(remoteID, purpose, scope string, spec *ephemeralRuntimeSpec) map[string]any {
	arguments := map[string]any{
		"remote_session_id": remoteID,
		"action":            "run",
		"runtime":           spec.Runtime,
		"purpose":           purpose,
		"scope":             scope,
		"user_confirmed":    true,
		"note":              "reuse the exact original script; confirmation is bound to its SHA-256 and script source is not persisted",
	}
	if spec.Runtime == "sqlite" {
		arguments["database"] = spec.Database
	}
	return arguments
}

func normalizeSQLiteDatabasePath(workspaceRoot, database string) (string, error) {
	database = strings.TrimSpace(database)
	if filepath.IsAbs(database) {
		return "", fmt.Errorf("sqlite database must be a workspace-relative path")
	}
	absolute, err := workspacefile.Resolve(workspaceRoot, database)
	if err != nil {
		return "", fmt.Errorf("resolve sqlite database: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	relative, err := filepath.Rel(resolvedRoot, absolute)
	if err != nil {
		return "", fmt.Errorf("make sqlite database workspace-relative: %w", err)
	}
	return filepath.ToSlash(filepath.Clean(relative)), nil
}

func readonlySQLiteAnalysis(command string) security.CommandAnalysis {
	return security.CommandAnalysis{
		Decision: security.Allow,
		Segments: []security.CommandSegmentDecision{{Command: command, Decision: security.Allow}},
	}
}

func (r *Runtime) executeSQLiteRuntime(ctx context.Context, envReq envelope.Request, remote remotesession.Session, spec *ephemeralRuntimeSpec, purpose, scope, commandDigest string, analysis security.CommandAnalysis) (*mcp.CallToolResult, error) {
	queryCtx, cancel := context.WithTimeout(ctx, ephemeralRuntimeMaxWait)
	defer cancel()
	result, err := sqlitequery.Query(queryCtx, remote.WorkspacePath, spec.Database, spec.Script, sqlitequery.DefaultMaxRows, config.MaxResultBytes(r.cfg.Limits))
	detail := runtimeExecutionDetail(purpose, scope, commandDigest, spec, analysis)
	if err != nil {
		detail["error"] = err.Error()
		r.logAudit(audit.Event{RequestID: envReq.RequestID, RemoteSessionID: remote.ID, Workspace: remote.WorkspaceName, Tool: "execute", Command: spec.Command, Status: "error", Detail: detail})
		data := map[string]any{
			"runtime": spec.Runtime, "database": spec.Database, "readonly": true,
			"script_sha256": spec.ScriptSHA256, "script_bytes": spec.ScriptBytes,
			"working_directory": remote.WorkspacePath, "workspace_scoped": true,
		}
		response := envelope.Fail(envelope.StatusError, envReq.RequestID, remote.WorkspaceName, data, "SQLITE_QUERY_ERROR", err.Error())
		response.RemoteSessionID = remote.ID
		return r.resultJSON(response)
	}
	data := map[string]any{
		"runtime": spec.Runtime, "database": spec.Database, "readonly": true,
		"columns": result.Columns, "rows": result.Rows, "row_count": result.RowCount, "truncated": result.Truncated,
		"script_sha256": spec.ScriptSHA256, "script_bytes": spec.ScriptBytes,
		"purpose": purpose, "scope": scope, "command_digest": commandDigest,
		"working_directory": remote.WorkspacePath, "workspace_scoped": true, "completed_in_call": true,
	}
	detail["row_count"] = result.RowCount
	detail["truncated"] = result.Truncated
	r.logAudit(audit.Event{RequestID: envReq.RequestID, RemoteSessionID: remote.ID, Workspace: remote.WorkspaceName, Tool: "execute", Command: spec.Command, Status: "ok", Detail: detail})
	return compactToolResult(data, fmt.Sprintf("SQLite query returned %d row(s).", result.RowCount)), nil
}

func (r *Runtime) executeRuntimeTask(ctx context.Context, envReq envelope.Request, principal auth.Principal, remote remotesession.Session, spec *ephemeralRuntimeSpec, purpose, scope, commandDigest string, analysis security.CommandAnalysis) (*mcp.CallToolResult, error) {
	originTool := toolInvocationName(ctx)
	if originTool == "" {
		originTool = "execute"
	}
	task, err := r.tasks.StartRemoteProcessWithObservationContext(
		envReq.RequestID, observationCallID(envReq), originTool, remote.ID, remote.WorkspaceName, remote.WorkspacePath, spec.Command,
		terminal.ProcessSpec{
			Executable: spec.Executable, Args: spec.Args, Stdin: spec.Script,
			WallLimit: ephemeralRuntimeWallLimit, CPUTimeLimit: ephemeralRuntimeCPUTimeLimit,
		},
	)
	if err != nil {
		return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "RUNTIME_START_ERROR", err.Error())
	}
	detail := runtimeExecutionDetail(purpose, scope, commandDigest, spec, analysis)
	_ = r.remote.AddEvent(ctx, principal, remotesession.Event{
		RemoteSessionID: remote.ID, Type: "command.started", OperationID: task.ID,
		Summary: fmt.Sprintf("%s ephemeral script", spec.Runtime), Metadata: detail,
	})

	waitCtx, cancel := context.WithTimeout(ctx, ephemeralRuntimeWait(envReq.Payload))
	completed := task.Wait(waitCtx)
	cancel()
	data := r.taskResultData(task, 0, 0, true)
	data["purpose"] = purpose
	data["scope"] = scope
	data["command_digest"] = commandDigest
	data["command_policy"] = commandPolicyData(analysis)
	data["command"] = spec.Command
	data["runtime"] = spec.Runtime
	data["script_sha256"] = spec.ScriptSHA256
	data["script_bytes"] = spec.ScriptBytes
	data["working_directory"] = remote.WorkspacePath
	data["workspace_scoped"] = true
	data["wall_limit_ms"] = ephemeralRuntimeWallLimit.Milliseconds()
	data["cpu_time_limit_ms"] = ephemeralRuntimeCPUTimeLimit.Milliseconds()
	if runtime.GOOS == "windows" {
		data["cpu_limit_enforcement"] = "unavailable_windows"
	} else {
		data["cpu_limit_enforcement"] = "best_effort_primary_process"
	}
	capTaskExecutionOutput(data, config.MaxResultBytes(r.cfg.Limits))

	if completed {
		data["completed_in_call"] = true
		detail["execution_task_id"] = task.ID
		detail["exit_code"] = data["exit_code"]
		if reason, _ := data["limit_reason"].(string); reason != "" {
			detail["limit_reason"] = reason
		}
		if code, message := annotateExecutionOutcome(data); code != "" {
			r.logAudit(audit.Event{RequestID: envReq.RequestID, RemoteSessionID: remote.ID, Workspace: remote.WorkspaceName, Tool: "execute", Command: spec.Command, Status: "error", Detail: detail})
			response := envelope.Fail(envelope.StatusError, envReq.RequestID, remote.WorkspaceName, data, code, message)
			response.RemoteSessionID = remote.ID
			return r.resultJSON(response)
		}
		r.logAudit(audit.Event{RequestID: envReq.RequestID, RemoteSessionID: remote.ID, Workspace: remote.WorkspaceName, Tool: "execute", Command: spec.Command, Status: "ok", Detail: detail})
		return compactToolResult(data, commandOutputText(ctx, data, fmt.Sprintf("%s runtime completed with exit code %v.", spec.Runtime, data["exit_code"]))), nil
	}

	data["completed_in_call"] = false
	data["next_action"] = nextAction("observe", map[string]any{
		"remote_session_id": remote.ID, "view": "task", "execution_task_id": task.ID,
	})
	data["logs_action"] = nextAction("observe", map[string]any{
		"remote_session_id": remote.ID, "view": "logs", "execution_task_id": task.ID,
		"stdout_offset": data["stdout_next_offset"], "stderr_offset": data["stderr_next_offset"],
	})
	data["summary"] = fmt.Sprintf("%s runtime is running as Task %s.", spec.Runtime, task.ID)
	detail["execution_task_id"] = task.ID
	r.logAudit(audit.Event{RequestID: envReq.RequestID, RemoteSessionID: remote.ID, Workspace: remote.WorkspaceName, Tool: "execute", Command: spec.Command, Status: "running", Detail: detail})
	response := envelope.Accepted(envReq.RequestID, remote.WorkspaceName, data)
	response.RemoteSessionID = remote.ID
	return r.resultJSON(response)
}

func isEphemeralRuntimeArguments(args map[string]any) bool {
	if strings.TrimSpace(stringPayload(args, "runtime")) != "" {
		return true
	}
	_, exists := args["script"]
	return exists
}

func observationArguments(name string, args map[string]any) map[string]any {
	if name != "execute" || !isEphemeralRuntimeArguments(args) {
		return args
	}
	clone := make(map[string]any, len(args)+2)
	for key, value := range args {
		clone[key] = value
	}
	if script, ok := args["script"].(string); ok {
		digest := sha256.Sum256([]byte(script))
		clone["script"] = "[redacted ephemeral script]"
		clone["script_sha256"] = "sha256:" + hex.EncodeToString(digest[:])
		clone["script_bytes"] = len(script)
	}
	return clone
}

func commandYield(payload map[string]any) time.Duration {
	yield := intPayload(payload, "yield_time_ms")
	if yield <= 0 {
		return defaultCommandYield
	}
	if yield > 60_000 {
		yield = 60_000
	}
	return time.Duration(yield) * time.Millisecond
}

func (r *Runtime) hasPendingCommandConfirmation(remoteSessionID, principalID, command, purpose, scope, confirmationToken string) bool {
	confirmationToken = strings.TrimSpace(confirmationToken)
	if confirmationToken == "" {
		return false
	}
	for _, pending := range r.approvals.ListRemoteSession(remoteSessionID) {
		if pending.Tool == "command_execute" && pending.PrincipalID == principalID && pending.Command == command && pending.Scope == scope && pending.ConfirmationToken == confirmationToken {
			return true
		}
	}
	return false
}

func (r *Runtime) pendingCommandConfirmation(remoteSessionID, principalID, command, scope, digest string) (approval.Pending, bool) {
	for _, pending := range r.approvals.ListRemoteSession(remoteSessionID) {
		if pending.Tool == "command_execute" && pending.PrincipalID == principalID &&
			pending.Command == command && pending.Scope == scope && pending.CommandDigest == digest {
			return pending, true
		}
	}
	return approval.Pending{}, false
}

// cleanCommandConfirmationContentKey binds clean-core user confirmation to the
// exact semantic command digest. Legacy confirmation-token clients retain the
// broader command/scope dedup behavior in approval.contentKey.
func cleanCommandConfirmationContentKey(principalID, digest string) string {
	return strings.Join([]string{"clean-command", principalID, digest}, "\x00")
}

func (r *Runtime) consumePendingCommandConfirmation(remoteSessionID, principalID, command, purpose, scope, confirmationToken string) {
	for _, pending := range r.approvals.ListRemoteSession(remoteSessionID) {
		if pending.Tool == "command_execute" && pending.PrincipalID == principalID && pending.Command == command && pending.Scope == scope && pending.ConfirmationToken == confirmationToken {
			_, _ = r.approvals.Consume(pending.ID)
			return
		}
	}
}

// containsUnsafeShellFeature mirrors the security matcher's rejection reason
// so the model-facing error can explain why a compound verification command
// was denied instead of leaving the model to guess.
func containsUnsafeShellFeature(command string) bool {
	return security.HasUnsafeShellOperator(command)
}

func (r *Runtime) toolTaskManage(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	action := toolAction(req)
	edit := action == "stop" || action == "stdin"
	envReq, principal, remote, fail := r.changeRequest(ctx, req, edit)
	if fail != nil {
		return fail, nil
	}
	if action == "list" {
		items, err := r.tasks.List(remote.ID, intPayload(envReq.Payload, "limit"))
		if err != nil {
			return r.terminalErrorForContext(ctx, envReq, remote.ID, remote.WorkspaceName, "task_list_error", err.Error())
		}
		digest := taskListDigest(items)
		knownDigest := strings.TrimSpace(stringPayload(envReq.Payload, "known_task_digest"))
		data := map[string]any{"task_list_digest": digest, "not_modified": knownDigest != "" && knownDigest == digest}
		if data["not_modified"] == true {
			data["tasks"] = []map[string]any{}
			data["message"] = "Task list unchanged; reuse the previously returned Task IDs."
		} else {
			data["tasks"] = items
		}
		return r.remoteResult(envReq, remote.ID, remote.WorkspaceName, data)
	}
	executionTaskID := strings.TrimSpace(stringPayload(envReq.Payload, "execution_task_id"))
	if executionTaskID == "" {
		return r.terminalErrorForContext(ctx, envReq, remote.ID, remote.WorkspaceName, "EXECUTION_TASK_ID_REQUIRED", "execution_task_id is required")
	}
	if strings.HasPrefix(executionTaskID, "pt_") || strings.HasPrefix(executionTaskID, "pl_") {
		return r.terminalErrorForContext(ctx, envReq, remote.ID, remote.WorkspaceName, "EXECUTION_TASK_ID_INVALID", "execution_task_id must identify a terminal execution Task; use observe(view=plan) for plan_task_id")
	}
	task, err := r.tasks.Get(remote.ID, executionTaskID)
	if err != nil {
		return r.terminalErrorForContext(ctx, envReq, remote.ID, remote.WorkspaceName, "EXECUTION_TASK_NOT_FOUND", "execution_task_id does not belong to this Remote Session")
	}
	switch action {
	case "status":
		data := task.StatusView()
		annotateExecutionOutcome(data)
		return r.remoteResult(envReq, remote.ID, remote.WorkspaceName, data)
	case "logs":
		data := r.taskResultData(task, intPayload(envReq.Payload, "stdout_offset"), intPayload(envReq.Payload, "stderr_offset"), false)
		addTaskObservationContext(data, task, envReq.Intent)
		if int64(data["stdout_next_offset"].(int)) < task.LogStreamSize("stdout") || int64(data["stderr_next_offset"].(int)) < task.LogStreamSize("stderr") {
			nextTool := "task_manage"
			if isCleanCoreRequest(ctx) {
				nextTool = "observe"
			}
			data["next_action"] = nextAction(nextTool, map[string]any{"remote_session_id": remote.ID, "view": "logs", "execution_task_id": task.ID, "stdout_offset": data["stdout_next_offset"], "stderr_offset": data["stderr_next_offset"]})
		}
		result := compactToolResult(data, fmt.Sprintf("Task %s log chunk returned; use structuredContent for the bounded log window.", task.ID))
		return result, nil
	case "attach":
		waitCtx, cancel := context.WithTimeout(ctx, commandYield(envReq.Payload))
		task.Wait(waitCtx)
		cancel()
		data := r.taskResultData(task, intPayload(envReq.Payload, "stdout_offset"), intPayload(envReq.Payload, "stderr_offset"), false)
		addTaskObservationContext(data, task, envReq.Intent)
		stdoutNext := data["stdout_next_offset"].(int)
		stderrNext := data["stderr_next_offset"].(int)
		if fmt.Sprint(task.StatusView()["status"]) == string(terminal.TaskRunning) || int64(stdoutNext) < task.LogStreamSize("stdout") || int64(stderrNext) < task.LogStreamSize("stderr") {
			nextTool := "task_manage"
			if isCleanCoreRequest(ctx) {
				nextTool = "execute"
			}
			data["next_action"] = nextAction(nextTool, map[string]any{"remote_session_id": remote.ID, "action": "attach", "execution_task_id": task.ID, "stdout_offset": stdoutNext, "stderr_offset": stderrNext, "yield_time_ms": int(commandYield(envReq.Payload) / time.Millisecond)})
		}
		if code, message := annotateExecutionOutcome(data); code != "" {
			response := envelope.Fail(envelope.StatusError, envReq.RequestID, remote.WorkspaceName, data, code, message)
			response.RemoteSessionID = remote.ID
			return r.resultJSON(response)
		}
		result := compactToolResult(data, fmt.Sprintf("Task %s attached; use structuredContent for the bounded log window.", task.ID))
		return result, nil
	case "stop":
		if err := task.Kill(); err != nil {
			return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "stop_error", err.Error())
		}
		_ = r.remote.AddEvent(ctx, principal, remotesession.Event{RemoteSessionID: remote.ID, Type: "task.stopped", OperationID: task.ID, Summary: task.Command})
		return r.remoteResult(envReq, remote.ID, remote.WorkspaceName, task.StatusView())
	case "ports":
		ports, err := terminal.ListeningPorts(ctx, task.PID)
		if err != nil {
			return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "port_inspection_unavailable", err.Error())
		}
		return r.remoteResult(envReq, remote.ID, remote.WorkspaceName, map[string]any{"execution_task_id": task.ID, "pid": task.PID, "ports": ports})
	case "diagnostics":
		log, next := task.Logs(0)
		return r.remoteResult(envReq, remote.ID, remote.WorkspaceName, map[string]any{"execution_task_id": task.ID, "diagnostics": projecttask.ParseDiagnostics(log, intPayload(envReq.Payload, "limit")), "parsed_log_bytes": next})
	case "stdin":
		input, _ := envReq.Payload["input"].(string)
		if err := task.WriteStdin(input); err != nil {
			return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "stdin_unavailable", err.Error())
		}
		return r.remoteResult(envReq, remote.ID, remote.WorkspaceName, map[string]any{"execution_task_id": task.ID, "accepted_bytes": len(input)})
	default:
		return r.invalidAction(ctx, req, "task_manage", action)
	}
}

func (r *Runtime) taskResultData(task *terminal.Task, stdoutOffset, stderrOffset int, includeCommand bool) map[string]any {
	const inlineLogBytes = 4 << 10
	stdout, stdoutNext := task.LogsForLimit("stdout", stdoutOffset, inlineLogBytes/2)
	stderr, stderrNext := task.LogsForLimit("stderr", stderrOffset, inlineLogBytes/2)
	data := task.StatusView()
	if !includeCommand {
		delete(data, "command")
	}
	data["execution_task_id"] = task.ID
	data["stdout"] = stdout
	data["stderr"] = stderr
	data["stdout_offset"] = stdoutOffset
	data["stderr_offset"] = stderrOffset
	data["stdout_next_offset"] = stdoutNext
	data["stderr_next_offset"] = stderrNext
	data["resource_uri"] = fmt.Sprintf("mcpx://remote-sessions/%s/tasks/%s/logs", task.RemoteSessionID, task.ID)
	truncated := int64(stdoutNext) < task.LogStreamSize("stdout") || int64(stderrNext) < task.LogStreamSize("stderr")
	if logTruncated, _ := data["log_truncated"].(bool); logTruncated {
		truncated = true
	}
	data["truncated"] = truncated
	data["output_truncated"] = truncated
	annotateExecutionOutcome(data)
	return data
}

func addTaskObservationContext(data map[string]any, task *terminal.Task, purpose string) {
	digest := sha256.Sum256([]byte(task.Command))
	data["command_digest"] = "sha256:" + hex.EncodeToString(digest[:])
	if purpose = strings.TrimSpace(purpose); purpose != "" {
		data["purpose"], _ = TruncateUTF8(purpose, 256)
	}
}

// annotateExecutionOutcome gives every execution Task one canonical business
// outcome regardless of whether it completed in the initial execute call or
// after detaching. Observation tools expose these fields while remaining
// successful queries; execute(action=attach) converts terminal failures into
// the corresponding error envelope.
func annotateExecutionOutcome(data map[string]any) (code, message string) {
	status := strings.TrimSpace(fmt.Sprint(data["status"]))
	if status == string(terminal.TaskRunning) {
		data["outcome"] = "running"
		delete(data, "error_code")
		return "", ""
	}
	if reason, _ := data["limit_reason"].(string); strings.TrimSpace(reason) != "" {
		data["outcome"] = "error"
		data["error_code"] = "RUNTIME_LIMIT_EXCEEDED"
		return "RUNTIME_LIMIT_EXCEEDED", fmt.Sprintf("execution exceeded %s", reason)
	}
	if status == string(terminal.TaskKilled) {
		data["outcome"] = "stopped"
		delete(data, "error_code")
		return "", ""
	}
	if exitCode, ok := data["exit_code"].(int); ok {
		if exitCode != 0 {
			stderr, _ := data["stderr"].(string)
			code := commandFailureCode(exitCode, stderr)
			data["outcome"] = "error"
			data["error_code"] = code
			return code, commandFailureMessage(code, exitCode)
		}
		data["outcome"] = "succeeded"
		delete(data, "error_code")
		return "", ""
	}
	if status == "" {
		status = "unknown"
	}
	data["outcome"] = status
	delete(data, "error_code")
	return "", ""
}

func taskListDigest(items []map[string]any) string {
	stableItems := make([]map[string]any, 0, len(items))
	for _, item := range items {
		stable := make(map[string]any, 7)
		for _, key := range []string{"execution_task_id", "status", "pid", "command", "exit_code", "log_truncated", "finished_at"} {
			if value, ok := item[key]; ok {
				stable[key] = value
			}
		}
		stableItems = append(stableItems, stable)
	}
	encoded, _ := json.Marshal(stableItems)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}
