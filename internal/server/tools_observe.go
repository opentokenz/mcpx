package server

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"
	"mcpx/internal/observation"
)

// toolObserve is the clean-core observation entry point. The Runtime infers a
// view only when the target is unique; otherwise the client must choose one.
func (r *Runtime) toolObserve(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	req, view := canonicalObserveRequest(req)
	args := mcpresult.Arguments(req)
	planTaskID := stringPayload(args, "plan_task_id")
	executionTaskID := stringPayload(args, "execution_task_id")
	if planTaskID != "" && executionTaskID != "" && view != "history" {
		return r.observeTaskIDError(ctx, req, "OBSERVE_TASK_ID_CONFLICT", "plan_task_id and execution_task_id cannot both select a single observe target")
	}
	switch view {
	case "session":
		return r.toolObserveStatus(ctx, req)
	case "task":
		if planTaskID != "" {
			return r.observeTaskIDError(ctx, req, "EXECUTION_TASK_ID_REQUIRED", "view=task requires execution_task_id; use view=plan for plan_task_id")
		}
		return r.toolObserveTask(ctx, req, "status")
	case "plan":
		if executionTaskID != "" {
			return r.observeTaskIDError(ctx, req, "PLAN_TASK_ID_REQUIRED", "view=plan requires plan_task_id; use view=task for execution_task_id")
		}
		return r.toolObservePlan(ctx, req)
	case "history":
		return r.toolWorkspaceHistoryRead(ctx, req)
	case "logs":
		if planTaskID != "" {
			return r.observeTaskIDError(ctx, req, "EXECUTION_TASK_ID_REQUIRED", "view=logs requires execution_task_id; Plan Tasks do not own terminal logs")
		}
		return r.toolObserveTask(ctx, req, "logs")
	case "diff":
		return r.toolObserveEditDiff(ctx, req)
	default:
		if view == "" {
			envReq, _, fail := r.remoteRequest(ctx, req)
			if fail != nil {
				return fail, nil
			}
			return r.terminalError(envReq, envReq.RemoteSessionID, envReq.Workspace, "ambiguous_request", "observe view cannot be inferred uniquely; choose session, task, plan, history, logs, or diff")
		}
		return r.invalidAction(ctx, req, "observe", view)
	}
}

func canonicalObserveRequest(req *mcp.CallToolRequest) (*mcp.CallToolRequest, string) {
	if view := publicSelector(req, "view"); view != "" {
		return req, view
	}
	args := mcpresult.Arguments(req)
	candidates := map[string]bool{}
	if stringPayload(args, "execution_task_id") != "" {
		candidates["task"] = true
	}
	if stringPayload(args, "plan_task_id") != "" {
		candidates["plan"] = true
	}
	if stringPayload(args, "edit_id") != "" {
		candidates["diff"] = true
	}
	if hasObserveArgument(args, "call_id", "event_ids", "request_ids", "operation_ids", "plan_task_ids", "execution_task_ids", "keyword", "kinds", "statuses", "created_after", "created_before") {
		candidates["history"] = true
	}
	if len(candidates) == 0 {
		if hasObserveArgument(args, "limit", "cursor", "offset", "stdout_offset", "stderr_offset") {
			return req, ""
		}
		return forwardedRequest(req, map[string]any{"view": "session"}), "session"
	}
	if len(candidates) != 1 {
		return req, ""
	}
	for view := range candidates {
		return forwardedRequest(req, map[string]any{"view": view}), view
	}
	return req, ""
}

func hasObserveArgument(args map[string]any, keys ...string) bool {
	for _, key := range keys {
		if value, ok := args[key]; ok && value != nil {
			switch typed := value.(type) {
			case string:
				if typed != "" {
					return true
				}
			case []any:
				if len(typed) > 0 {
					return true
				}
			default:
				return true
			}
		}
	}
	return false
}

func (r *Runtime) toolObservePlan(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, session, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	planTaskID := stringPayload(envReq.Payload, "plan_task_id")
	if planTaskID == "" {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLAN_TASK_ID_REQUIRED", "plan_task_id is required for observe(view=plan)")
	}
	planID, task, err := r.plans.FindTask(ctx, session.ID, planTaskID)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PLAN_TASK_NOT_FOUND", "plan_task_id does not belong to this Remote Session")
	}
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, planTaskMap(planID, task))
}

func (r *Runtime) observeTaskIDError(ctx context.Context, req *mcp.CallToolRequest, code, message string) (*mcp.CallToolResult, error) {
	envReq, _, fail := r.remoteRequest(ctx, req)
	if fail != nil {
		return fail, nil
	}
	return r.terminalError(envReq, envReq.RemoteSessionID, envReq.Workspace, code, message)
}

func (r *Runtime) toolObserveStatus(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, _, session, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	data := map[string]any{
		"remote_session_id": session.ID,
		"workspace":         session.WorkspaceName,
		"session_status":    session.Status,
	}
	activity, err := r.agentActivitySnapshot(ctx, session.ID)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "observe_status_error", err.Error())
	}
	if activity != nil {
		data["agent_activity"] = activity
	}
	if r.observation != nil && r.observation.store != nil {
		events, _, err := r.observation.store.Query(ctx, observation.HistoryQuery{
			Workspace: session.WorkspaceName, SessionID: session.ID, Limit: 1,
		})
		if err != nil {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "observe_status_error", err.Error())
		}
		if len(events) > 0 {
			data["latest_event"] = historyEventView(events[0])
		}
	}
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, data)
}
