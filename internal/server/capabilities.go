package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
	"mcpx/internal/remotesession"
)

const cleanCoreCapabilityVersion = "clean-core-p13"

const clientProtocolVersion = "2"

func clientProtocolCapabilities() map[string]any {
	fields := append([]string(nil), agentActivityKindNames...)
	return map[string]any{
		"version": clientProtocolVersion,
		"activity": map[string]any{
			"version":           agentActivityProtocolVersion,
			"transport":         "mcp_tool_arguments",
			"argument":          "activity",
			"fields":            fields,
			"field_order":       fields,
			"multiple_per_call": true,
			"runtime_managed":   []string{"turn_id", "sequence", "state", "related_call_id"},
			"state":             "preparing_action",
			"turn_boundary":     "non_empty_intent_starts_new_turn",
			"emission_policy":   "emit_only_semantic_changes_do_not_repeat_unchanged_fields",
			"field_semantics": map[string]any{
				"intent":     "turn_goal_or_problem_new_turn_only",
				"hypothesis": "tentative_falsifiable_judgment_not_observed_fact",
				"evidence":   "new_verifiable_observation_without_inference",
				"conclusion": "evidence_supported_current_judgment_not_action",
				"next":       "immediate_selected_action_aligned_with_current_tool_call",
				"status":     "material_phase_wait_or_block_change_only_not_heartbeat",
			},
			"summary_semantics": "public_work_summary_not_chain_of_thought",
			"current_state": map[string]any{
				"tool": "observe", "arguments": map[string]any{"view": "session"}, "field": "agent_activity",
			},
		},
	}
}

func clientProtocolRevision() string { return hashRevision(clientProtocolCapabilities()) }

func capabilityGroups() map[string][]string {
	return map[string][]string{
		"core":    {"workspace", "session", "read", "edit", "move_out", "observe", "progress", "execute", "plan", "artifact", "skill_tool", "mcp_tool"},
		"support": {"operation_batch", "operation_manage", "runtime_read", "environment_read", "environment", "browser", "screenshot_capture", "secret_provide"},
	}
}

type toolCapabilityDefinition struct {
	Name                  string
	Domain                string
	RequiresRemoteSession bool
	Roles                 []string
	Feature               string
}

// toolCapabilityDefinitions contains only capability metadata. The public name,
// description, schema, and annotations are authoritative in registerTools and
// are fingerprinted from the resulting MCP registration.
var toolCapabilityDefinitions = []toolCapabilityDefinition{
	{Name: "observe", Domain: "observation", RequiresRemoteSession: true},
	{Name: "progress", Domain: "progress", RequiresRemoteSession: true},
	{Name: "operation_batch", Domain: "operation", RequiresRemoteSession: true, Roles: []string{"owner", "editor"}},
	{Name: "operation_manage", Domain: "operation", RequiresRemoteSession: true},
	{Name: "workspace", Domain: "workspace"},
	{Name: "session", Domain: "session"},
	{Name: "read", Domain: "source", RequiresRemoteSession: true},
	{Name: "edit", Domain: "edit", RequiresRemoteSession: true, Roles: []string{"owner", "editor"}},
	{Name: "move_out", Domain: "workspace_move_out", RequiresRemoteSession: true, Roles: []string{"owner", "editor"}},
	{Name: "execute", Domain: "command", RequiresRemoteSession: true, Roles: []string{"owner", "editor"}, Feature: "terminal"},
	{Name: "plan", Domain: "plan", RequiresRemoteSession: true, Roles: []string{"owner", "editor"}},
	{Name: "artifact", Domain: "artifact", RequiresRemoteSession: true, Roles: []string{"owner", "editor"}},
	{Name: "skill_tool", Domain: "extension", RequiresRemoteSession: true, Roles: []string{"owner", "editor"}, Feature: "skills"},
	{Name: "mcp_tool", Domain: "extension", RequiresRemoteSession: true, Roles: []string{"owner", "editor"}, Feature: "mcp"},
	// Support tools intentionally remain outside the core workflow but share
	// the same remote_session_id contract; their definitions are listed above.
	{Name: "runtime_read", Domain: "runtime"},
	{Name: "environment_read", Domain: "environment"},
	{Name: "environment", Domain: "environment", RequiresRemoteSession: true, Roles: []string{"owner", "editor"}},
	{Name: "browser", Domain: "browser", RequiresRemoteSession: true, Roles: []string{"owner", "editor"}},
	{Name: "screenshot_capture", Domain: "screenshot", RequiresRemoteSession: true, Roles: []string{"owner", "editor"}},
	{Name: "secret_provide", Domain: "secrets", RequiresRemoteSession: true, Roles: []string{"owner", "editor"}},
}

func toolSupportsEmbeddedActivity(name string) bool {
	for _, definition := range toolCapabilityDefinitions {
		if definition.Name == name {
			return definition.RequiresRemoteSession
		}
	}
	return false
}

func machineToolCapabilities(effective config.Config, session *remotesession.Session) []map[string]any {
	items := make([]map[string]any, 0, len(toolCapabilityDefinitions))
	for _, definition := range toolCapabilityDefinitions {
		state, reason := "available", ""
		switch definition.Feature {
		case "terminal":
			if !effective.Terminal.Enabled {
				state, reason = "disabled", "terminal_disabled"
			}
		case "file_watch":
			if !effective.FileWatch.Enabled {
				state, reason = "disabled", "file_watch_disabled"
			}
		case "mcp":
			if !effective.Discovery.MCP.Enabled {
				state, reason = "disabled", "mcp_discovery_disabled"
			}
		case "skills":
			if !effective.Discovery.Skills.Enabled {
				state, reason = "disabled", "skill_discovery_disabled"
			}
		}
		if state == "available" && definition.RequiresRemoteSession && session == nil {
			state, reason = "requires_remote_session", "session_id_required"
		}
		if state == "available" && session != nil && len(definition.Roles) > 0 && !containsString(definition.Roles, session.Role) {
			state, reason = "forbidden", "role_not_allowed"
		}
		item := map[string]any{
			"name": definition.Name, "domain": definition.Domain, "state": state,
			"requires_remote_session": definition.RequiresRemoteSession,
		}
		if len(definition.Roles) > 0 {
			item["roles"] = definition.Roles
		}
		if reason != "" {
			item["reason"] = reason
		}
		if limit, ok := publishedLimits()[definition.Name]; ok {
			item["limits"] = limit
		}
		items = append(items, item)
	}
	return items
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func capabilityRevision(data map[string]any) string {
	encoded, _ := json.Marshal(data)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func capabilityToolNames() []string {
	names := make([]string, 0, len(toolCapabilityDefinitions))
	for _, definition := range toolCapabilityDefinitions {
		names = append(names, definition.Name)
	}
	sort.Strings(names)
	return names
}

// revision helpers — hash any stable JSON-serializable value.
func hashRevision(value any) string {
	encoded, _ := json.Marshal(value)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func skillRevision(skills any) string     { return hashRevision(skills) }
func mcpRevision(servers any) string      { return hashRevision(servers) }
func instructionRevision(docs any) string { return hashRevision(docs) }

func sessionCapabilityRevision(session *remotesession.Session) string {
	if session == nil {
		return "sha256:none"
	}
	return hashRevision(map[string]any{"id": session.ID, "role": session.Role, "status": session.Status})
}

// capabilityManifestRevision is independent of a single session role snapshot.
func capabilityManifestRevision(tools, skills, servers, instructions, guidance, clientProtocol any) string {
	return hashRevision(map[string]any{
		"tools": tools, "skills": skills, "servers": servers, "instructions": instructions,
		"guidance": guidance, "client_protocol": clientProtocol,
	})
}

// registeredToolManifest is derived from the MCP server snapshot. Capability
// and documentation consumers therefore cannot silently drift from tools/list.
func (r *Runtime) registeredToolManifest() []map[string]any {
	tools := r.registeredTools()
	manifest := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		encoded, err := json.Marshal(tool)
		if err != nil {
			continue
		}
		var item map[string]any
		if json.Unmarshal(encoded, &item) == nil {
			manifest = append(manifest, item)
		}
	}
	return manifest
}

func toolSafetyMetadata(tool map[string]any) map[string]any {
	switch meta := tool["_meta"].(type) {
	case map[string]any:
		safety, _ := meta["mcpx/safety"].(map[string]any)
		return safety
	case mcp.Meta:
		safety, _ := meta["mcpx/safety"].(map[string]any)
		return safety
	default:
		return nil
	}
}

func (r *Runtime) runtimeToolCapabilities(effective config.Config, session *remotesession.Session) []map[string]any {
	items := machineToolCapabilities(effective, session)
	manifest := r.registeredToolManifest()
	byName := make(map[string]map[string]any, len(manifest))
	for _, tool := range manifest {
		if name, ok := tool["name"].(string); ok {
			byName[name] = tool
		}
	}
	for _, item := range items {
		name, _ := item["name"].(string)
		if tool := byName[name]; tool != nil {
			if safety := toolSafetyMetadata(tool); safety != nil {
				item["safety"] = safety
			}
		}
	}
	return items
}
