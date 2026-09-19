package server

import (
	"strings"
	"sync"

	"mcpx/internal/envelope"
	"mcpx/internal/server/guidance"
)

const agentGuidanceVersion = "2.0"

// agentGuidanceConfig mirrors guidance.Config for existing call sites.
type agentGuidanceConfig = guidance.Config

var (
	defaultAgentGuidance     agentGuidanceConfig
	defaultAgentGuidanceOnce sync.Once
)

func loadDefaultAgentGuidance() agentGuidanceConfig {
	defaultAgentGuidanceOnce.Do(func() {
		defaultAgentGuidance = guidance.MustLoadAgent()
		if defaultAgentGuidance.Version != agentGuidanceVersion {
			// Keep code constant and YAML in lockstep during local edits.
			defaultAgentGuidance.Version = agentGuidanceVersion
		}
	})
	return defaultAgentGuidance
}

// agentGuidance is the compact engineering contract shown at bootstrap.
// Tool schemas, Runtime policy and structured recovery remain authoritative for
// protocol details; guidance only carries stable decision principles.
func agentGuidance() map[string]any {
	config := loadDefaultAgentGuidance()
	return map[string]any{
		"version":      config.Version,
		"priority":     config.Priority,
		"summary":      config.Summary,
		"rules":        config.Rules,
		"tool_routing": config.ToolRouting,
	}
}

func agentGuidanceRevision() string { return hashRevision(agentGuidance()) }

func agentGuidanceInstructions() string {
	config := loadDefaultAgentGuidance()
	lines := []string{"MCPX Agent 指引（高优先级）："}
	for _, rule := range config.Rules {
		lines = append(lines, "- "+rule)
	}
	return strings.Join(lines, "\n")
}

func nextActionWithReason(tool, reason string, arguments map[string]any) map[string]any {
	tool, arguments = normalizePublicAction(tool, arguments)
	return map[string]any{
		"tool":      tool,
		"reason":    reason,
		"arguments": argumentsOrEmpty(arguments),
	}
}

func addRecoveryAction(response *envelope.Response, tool, reason string, arguments map[string]any) {
	if response == nil || response.Error == nil || strings.TrimSpace(tool) == "" {
		return
	}
	if response.Error.Details == nil {
		response.Error.Details = map[string]any{}
	}
	tool, arguments = normalizePublicAction(tool, arguments)
	response.Error.Recovery = &envelope.Recovery{Action: tool, Tool: tool, Arguments: argumentsOrEmpty(arguments)}
	response.Error.Details["next_action"] = map[string]any{
		"tool":      tool,
		"reason":    reason,
		"arguments": argumentsOrEmpty(arguments),
	}
}

func normalizePublicAction(tool string, arguments map[string]any) (string, map[string]any) {
	result := argumentsOrEmpty(arguments)
	action, _ := result["action"].(string)
	action = strings.ToLower(strings.TrimSpace(action))
	setView := func(view string) {
		delete(result, "action")
		result["view"] = view
	}
	switch tool {
	case "workspace_read":
		tool = "read"
		setView("list")
	case "source_read", "file_read", "context_query":
		legacyTool := tool
		tool = "read"
		if legacyTool == "file_read" || action == "" {
			setView("file")
		} else if action == "list" {
			setView("list")
		} else if action == "query" {
			if mode, ok := result["mode"]; ok {
				result["search_mode"] = mode
				delete(result, "mode")
			}
			setView("context")
		} else {
			setView("search")
		}
	case "command_execute":
		tool = "execute"
		if action == "" {
			result["action"] = "run"
		}
	case "task_manage":
		if action == "attach" || action == "stop" || action == "stdin" {
			tool = "execute"
			result["action"] = action
			delete(result, "operation")
		} else {
			tool = "observe"
			if action == "status" {
				setView("task")
			} else {
				setView(action)
			}
		}
	case "plan_manage":
		switch action {
		case "create":
			tool = "plan"
			result["action"] = "create"
		case "get":
			tool = "plan"
			result["action"] = "read"
		default:
			tool = "plan"
			result["action"] = action
			delete(result, "transition")
		}
	case "runtime_inspect":
		tool = "runtime_read"
		setView(action)
	case "environment_inspect":
		if value, ok := result["save_snapshot"].(bool); ok && value {
			tool = "environment"
			delete(result, "action")
			delete(result, "save_snapshot")
		} else {
			tool = "environment_read"
			if _, ok := result["compare_to"]; ok {
				result["snapshot_id"] = result["compare_to"]
				delete(result, "compare_to")
			}
			if action == "" {
				setView("current")
			} else {
				setView(action)
			}
		}
	case "workspace_state":
		tool = "observe"
		setView(action)
	case "artifact_manage":
		tool = "artifact"
		result["action"] = action
	case "secrets_provide":
		tool = "secret_provide"
	}
	if isCleanPublicTool(tool) {
		if sessionID, ok := result["session_id"]; ok {
			if _, exists := result["remote_session_id"]; !exists {
				result["remote_session_id"] = sessionID
			}
			delete(result, "session_id")
		}
	} else if sessionID, ok := result["remote_session_id"]; ok {
		result["session_id"] = sessionID
		delete(result, "remote_session_id")
	}
	return tool, result
}

func isCleanPublicTool(tool string) bool {
	switch tool {
	case "session", "read", "edit", "move_out", "observe", "progress", "execute", "plan", "artifact", "skill_tool", "mcp_tool", "browser",
		"operation_batch", "operation_manage", "runtime_read", "environment_read", "environment", "screenshot_capture", "secret_provide":
		return true
	default:
		return false
	}
}

func addRecoveryActions(response *envelope.Response, actions ...map[string]any) {
	if response == nil || response.Error == nil || len(actions) == 0 {
		return
	}
	if response.Error.Details == nil {
		response.Error.Details = map[string]any{}
	}
	response.Error.Details["next_actions"] = actions
	if len(actions) > 0 {
		if tool, _ := actions[0]["tool"].(string); tool != "" {
			arguments, _ := actions[0]["arguments"].(map[string]any)
			response.Error.Recovery = &envelope.Recovery{Action: tool, Tool: tool, Arguments: argumentsOrEmpty(arguments)}
		}
	}
}

func argumentsOrEmpty(arguments map[string]any) map[string]any {
	if arguments == nil {
		return map[string]any{}
	}
	return arguments
}
