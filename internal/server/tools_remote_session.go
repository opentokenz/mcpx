package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"

	"mcpx/internal/audit"
	"mcpx/internal/auth"
	"mcpx/internal/config"
	"mcpx/internal/envelope"
	"mcpx/internal/remotesession"
	"mcpx/internal/winproc"
	"mcpx/internal/workspacechanges"
)

var (
	errWorkspaceNotFound     = errors.New("workspace not found")
	errRemoteSessionRequired = errors.New("remote session id required")
	errRemoteSessionRunning  = errors.New("remote session has running tasks")
)

func (r *Runtime) principalFromContext(ctx context.Context) (auth.Principal, error) {
	issuer, resource := r.authAudience()
	credentials := auth.ValidateHTTP(
		bearerFromCtx(ctx), config.EffectiveAuthMode(r.cfg.Auth), r.cfg.Auth.Token,
		r.oauth, issuer, resource,
	)
	if !credentials.OK {
		return auth.Principal{}, fmt.Errorf("unauthorized")
	}
	return auth.PrincipalFromCredentials(credentials, bearerFromCtx(ctx)), nil
}

func clientInfoFromContext(ctx context.Context) (name, version string) {
	// Official go-sdk does not expose client info via the same context helpers as
	// mark3labs; keep a stable fallback until session-level client metadata is wired.
	_ = ctx
	return "unknown", ""
}

func (r *Runtime) touchRemoteSessionActivity(ctx context.Context, req envelope.Request) {
	if r == nil || r.remote == nil {
		return
	}
	sessionID := strings.TrimSpace(req.RemoteSessionID)
	principal, err := r.principalFromContext(ctx)
	if err != nil {
		return
	}
	if sessionID == "" {
		sessionID = r.boundRemoteSessionID(ctx, principal)
	}
	if sessionID == "" {
		return
	}
	name, version := clientInfoFromContext(ctx)
	r.remote.Touch(ctx, principal, sessionID, name, version)
}

func (r *Runtime) remoteRequest(ctx context.Context, req *mcp.CallToolRequest) (envelope.Request, auth.Principal, *mcp.CallToolResult) {
	envReq, err := r.parseEnv(ctx, req)
	if err != nil {
		return envReq, auth.Principal{}, mcpresult.NewError(err.Error())
	}
	principal, err := r.principalFromContext(ctx)
	if err != nil {
		resp := envelope.Fail(envelope.StatusUnauthorized, envReq.RequestID, envReq.Workspace, nil, "unauthorized", "invalid or missing token")
		out, _ := r.resultJSON(resp)
		return envReq, auth.Principal{}, out
	}
	if strings.TrimSpace(envReq.RemoteSessionID) == "" && toolInvocationName(ctx) != "session" {
		envReq.RemoteSessionID = r.boundRemoteSessionID(ctx, principal)
	}
	return envReq, principal, nil
}

func validatePurpose(purpose string) error {
	purpose = strings.TrimSpace(purpose)
	if purpose == "" {
		return fmt.Errorf("purpose is required")
	}
	if len(purpose) > envelope.MaxIntentBytes {
		return fmt.Errorf("purpose exceeds %d bytes", envelope.MaxIntentBytes)
	}
	return nil
}

// validateIntent is kept for internal callers that still use the observation
// field name; public schemas expose purpose.
func validateIntent(intent string) error { return validatePurpose(intent) }

func (r *Runtime) remoteResult(envReq envelope.Request, remoteSessionID, workspace string, data any) (*mcp.CallToolResult, error) {
	resp := envelope.OK(envReq.RequestID, workspace, data)
	resp.RemoteSessionID = remoteSessionID
	return r.resultJSON(resp)
}

func (r *Runtime) remoteError(envReq envelope.Request, remoteSessionID, workspace string, err error) (*mcp.CallToolResult, error) {
	status, code := envelope.StatusError, "remote_session_error"
	switch {
	case errors.Is(err, remotesession.ErrNotFound):
		code = "not_found"
	case errors.Is(err, remotesession.ErrForbidden):
		status, code = envelope.StatusDenied, "forbidden"
	case errors.Is(err, remotesession.ErrConflict):
		code = "version_conflict"
	case errors.Is(err, errRemoteSessionRunning):
		code = "running_task"
	case errors.Is(err, remotesession.ErrInvalidInput):
		code = "invalid_request"
	case errors.Is(err, errWorkspaceNotFound):
		code = "workspace_not_found"
	case errors.Is(err, errRemoteSessionRequired):
		code = "remote_session_required"
	}
	message := err.Error()
	if code == "not_found" {
		message = "remote session not found：remote_session_id 必须原样复制 session 返回的完整值。如果 ID 已丢失或不确定，先调用 session(action=list) 发现已有会话，再用返回的完整 remote_session_id 调用 session(action=open) 恢复；不要直接创建新 Session。"
	}
	resp := envelope.Fail(status, envReq.RequestID, workspace, nil, code, message)
	resp.RemoteSessionID = remoteSessionID
	switch code {
	case "workspace_not_found":
		addRecoveryAction(&resp, "workspace", "select a valid workspace before retrying session", map[string]any{})
		addRecoveryActions(&resp,
			nextActionWithReason("workspace", "refresh the available workspace names", map[string]any{}),
			nextActionWithReason("session", "open a Remote Session after selecting a workspace", map[string]any{"workspace": workspace}),
		)
	case "not_found":
		if remoteSessionID != "" {
			arguments := map[string]any{"action": "list"}
			if workspace = strings.TrimSpace(workspace); workspace != "" {
				arguments["workspace"] = workspace
			}
			addRecoveryAction(&resp, "session", "discover an existing Remote Session before opening a new one", arguments)
		}
	case "remote_session_required":
		if workspace != "" {
			addRecoveryAction(&resp, "session", "open or resume a Remote Session before using this tool", map[string]any{"workspace": workspace})
		} else {
			addRecoveryAction(&resp, "workspace", "select a workspace before opening a Remote Session", map[string]any{})
		}
	}
	return r.resultJSON(resp)
}

func (r *Runtime) createRemoteSession(ctx context.Context, principal auth.Principal, envReq envelope.Request, workspaceName string) (remotesession.CreateResult, error) {
	workspaceName = strings.TrimSpace(workspaceName)
	ws, ok := r.reg.Get(workspaceName)
	if !ok {
		return remotesession.CreateResult{}, fmt.Errorf("%w: %q", errWorkspaceNotFound, workspaceName)
	}
	label, _ := envReq.Payload["label"].(string)
	description, _ := envReq.Payload["description"].(string)
	clientRequestID, _ := envReq.Payload["client_request_id"].(string)
	clientName, clientVersion := clientInfoFromContext(ctx)
	gitHead, treeDigest := workspaceRevision(ctx, ws.Path)
	result, err := r.remote.Create(ctx, principal, remotesession.CreateInput{
		WorkspaceName: workspaceName, WorkspacePath: ws.Path, Label: label,
		Description: description, BaseGitHead: gitHead, BaseTreeDigest: treeDigest,
		ClientRequestID: clientRequestID, ClientName: clientName, ClientVersion: clientVersion,
	})
	if err != nil {
		return remotesession.CreateResult{}, err
	}
	if err := r.ensureSessionEnvironment(ctx, principal, &result); err != nil {
		r.logAudit(audit.Event{RequestID: envReq.RequestID, RemoteSessionID: result.Session.ID, Workspace: workspaceName, Tool: "environment_snapshot", Status: "error", Detail: map[string]any{"error": err.Error()}})
	}
	if result.Replayed {
		return result, nil
	}
	if err := r.workspaceDiff.CaptureBaseline(ctx, result.Session.ID, result.Session.WorkspacePath); err != nil {
		r.logAudit(audit.Event{RequestID: envReq.RequestID, RemoteSessionID: result.Session.ID, Workspace: workspaceName, Tool: "workspace_baseline", Status: "error", Detail: map[string]any{"error": err.Error()}})
	}
	return result, nil
}

func (r *Runtime) toolRemoteSessionList(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, fail := r.remoteRequest(ctx, req)
	if fail != nil {
		return fail, nil
	}
	workspaceName := strings.TrimSpace(envReq.Workspace)
	if workspaceName == "" {
		workspaceName, _ = envReq.Payload["workspace"].(string)
	}
	query, _ := envReq.Payload["query"].(string)
	status, _ := envReq.Payload["status"].(string)
	cursor, _ := envReq.Payload["cursor"].(string)
	limit := intPayload(envReq.Payload, "limit")
	var statuses []string
	for _, value := range strings.Split(status, ",") {
		if value = strings.TrimSpace(value); value != "" {
			statuses = append(statuses, value)
		}
	}
	result, err := r.remote.List(ctx, principal, remotesession.ListInput{
		Workspace: workspaceName, Query: query, Statuses: statuses, Limit: limit, Cursor: cursor,
	})
	if err != nil {
		return r.remoteError(envReq, "", workspaceName, err)
	}
	return r.remoteResult(envReq, "", workspaceName, result)
}

func (r *Runtime) toolRemoteSessionClose(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, fail := r.remoteRequest(ctx, req)
	if fail != nil {
		return fail, nil
	}
	remoteSessionID, err := requireRemoteSessionID(envReq)
	if err != nil {
		return r.remoteError(envReq, "", "", err)
	}
	if tasks, taskErr := r.tasks.List(remoteSessionID, 100); taskErr == nil {
		for _, task := range tasks {
			if fmt.Sprint(task["status"]) == "running" {
				return r.remoteError(envReq, remoteSessionID, "", fmt.Errorf("%w: task %v must be stopped before closing", errRemoteSessionRunning, task["execution_task_id"]))
			}
		}
	}
	mode, _ := envReq.Payload["mode"].(string)
	session, err := r.remote.Close(ctx, principal, remoteSessionID, mode)
	if err != nil {
		return r.remoteError(envReq, remoteSessionID, "", err)
	}
	r.discoveryMu.Lock()
	for id, observed := range r.discoveries {
		if observed.RemoteSessionID == remoteSessionID {
			delete(r.discoveries, id)
		}
	}
	r.discoveryMu.Unlock()
	r.unbindRemoteSession(ctx, principal, remoteSessionID)
	return r.remoteResult(envReq, remoteSessionID, session.WorkspaceName, session)
}

func remoteSessionID(req envelope.Request) string {
	if req.RemoteSessionID != "" {
		return req.RemoteSessionID
	}
	value, _ := req.Payload["remote_session_id"].(string)
	return strings.TrimSpace(value)
}

func requireRemoteSessionID(req envelope.Request) (string, error) {
	id := remoteSessionID(req)
	if id == "" {
		return "", errRemoteSessionRequired
	}
	return id, nil
}

func intPayload(payload map[string]any, key string) int {
	switch value := payload[key].(type) {
	case int:
		return value
	case float64:
		return int(value)
	default:
		return 0
	}
}

func workspaceRevision(parent context.Context, path string) (head, digest string) {
	ctx, cancel := context.WithTimeout(parent, 4*time.Second)
	defer cancel()
	roots, err := workspacechanges.DiscoverGitRoots(path)
	if err != nil || len(roots) == 0 {
		return "", ""
	}
	heads := make([]string, 0, len(roots))
	var statusParts []string
	for _, root := range roots {
		rootPath := path
		label := ""
		if root != "" {
			rootPath = filepath.Join(path, root)
			label = root + ":"
		}
		headCommand := exec.CommandContext(ctx, "git", "-C", rootPath, "rev-parse", "HEAD")
		winproc.ConfigureNoWindow(headCommand)
		if out, err := headCommand.Output(); err == nil {
			heads = append(heads, label+strings.TrimSpace(string(out)))
		}
		statusCommand := exec.CommandContext(ctx, "git", "-C", rootPath, "status", "--porcelain=v2", "-z")
		winproc.ConfigureNoWindow(statusCommand)
		if out, err := statusCommand.Output(); err == nil {
			statusParts = append(statusParts, label+string(out))
		}
	}
	if len(heads) == 0 {
		return "", ""
	}
	head = strings.Join(heads, " ")
	digest = remotesessionDigest(head + "\x00" + strings.Join(statusParts, "\x00"))
	return head, digest
}

func remotesessionDigest(value string) string {
	// Reuse the standard library without exposing workspace contents.
	return fmt.Sprintf("sha256:%x", sha256Sum([]byte(value)))
}

func sha256Sum(value []byte) [32]byte {
	return sha256.Sum256(value)
}
