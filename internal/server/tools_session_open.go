package server

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/artifact"
	"mcpx/internal/audit"
	"mcpx/internal/instruction"
	"mcpx/internal/observation"
	"mcpx/internal/projecttask"
	"mcpx/internal/remotesession"
	"mcpx/internal/skill"
	buildversion "mcpx/internal/version"
)

// toolSessionOpen creates or reuses a Remote Session. Compact is the default;
// callers must explicitly request full to receive inventories and history.
func (r *Runtime) toolSessionOpen(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, fail := r.remoteRequest(ctx, req)
	if fail != nil {
		return fail, nil
	}

	includeInstrContent := false
	if v, ok := envReq.Payload["include_instructions_content"].(bool); ok {
		includeInstrContent = v
	}
	includeProjectTasks := false
	if v, ok := envReq.Payload["include_project_tasks"].(bool); ok {
		includeProjectTasks = v
	}
	fullResponse := strings.EqualFold(strings.TrimSpace(stringPayload(envReq.Payload, "response_profile")), "full")
	var session remotesession.Session
	remoteID, _ := envReq.Payload["remote_session_id"].(string)
	remoteID = strings.TrimSpace(remoteID)
	if remoteID == "" {
		remoteID = strings.TrimSpace(envReq.RemoteSessionID)
	}

	workspaceName := strings.TrimSpace(envReq.Workspace)
	if workspaceName == "" {
		workspaceName, _ = envReq.Payload["workspace"].(string)
	}
	if remoteID != "" {
		existing, err := r.remote.Get(ctx, principal, remoteID)
		if err != nil {
			return r.remoteError(envReq, remoteID, workspaceName, err)
		}
		session = existing
		workspaceName = session.WorkspaceName
	} else {
		created, err := r.createRemoteSession(ctx, principal, envReq, workspaceName)
		if err != nil {
			return r.remoteError(envReq, "", workspaceName, err)
		}
		session = created.Session
	}

	wsPath := session.WorkspacePath
	effective := r.effectiveConfig(wsPath)
	tools := r.runtimeToolCapabilities(effective, &session)

	var (
		servers              = []map[string]any{}
		skills               = []map[string]any{}
		docs                 []instruction.Document
		project              map[string]any
		gitHead              string
		treeDigest           string
		pendingConfirmations []map[string]any
		taskList             []map[string]any
		artifacts            []artifact.Artifact
		activeTaskIDs        []string
		artifactCount        int
		latestModelState     any
	)
	var tasks any
	var bootstrap sync.WaitGroup
	bootstrap.Add(7)
	go func() {
		defer bootstrap.Done()
		if manager, err := r.mcpManagerForWorkspace(wsPath); err == nil && effective.Discovery.MCP.Enabled {
			servers = manager.List()
		}
	}()
	go func() {
		defer bootstrap.Done()
		if effective.Discovery.Skills.Enabled {
			skills = skillItems(skill.LoadAll(effective.Discovery.Skills.Dirs, wsPath))
		}
	}()
	go func() {
		defer bootstrap.Done()
		docs = instruction.DiscoverAt(
			r.cfg.Discovery.Instructions.GlobalAgentsPath, wsPath, "",
			effective.Security.Files.MaxReadBytes,
		)
	}()
	go func() {
		defer bootstrap.Done()
		project = inspectProject(ctx, wsPath)
		if includeProjectTasks {
			tasks = projecttask.Discover(wsPath)
		}
	}()
	go func() {
		defer bootstrap.Done()
		gitHead, treeDigest = workspaceRevision(ctx, wsPath)
	}()
	go func() {
		defer bootstrap.Done()
		pendingConfirmations = pendingConfirmationItems(r.approvals.ListRemoteSession(session.ID))
		taskList, _ = r.tasks.List(session.ID, 20)
		activeTaskIDs, _ = r.tasks.ActiveIDs(session.ID)
		artifacts, _ = r.artifacts.List(ctx, session.ID, "", 20)
		artifactCount, _ = r.artifacts.Count(ctx, session.ID)
	}()
	go func() {
		defer bootstrap.Done()
		if r.observation == nil || r.observation.store == nil {
			return
		}
		page, err := r.observation.store.QueryMemory(ctx, observation.MemoryQuery{
			Workspace: session.WorkspaceName,
			SessionID: session.ID,
			Type:      "progress",
			Latest:    1,
		})
		if err == nil && len(page.Items) > 0 {
			latestModelState = page.Items[0]
		}
	}()
	bootstrap.Wait()

	var instructionPayload any
	if includeInstrContent {
		items, _ := instruction.ReadContents(docs, 256<<10)
		instructionPayload = map[string]any{"documents": items, "inline": true}
	} else {
		instructionPayload = map[string]any{"documents": docs, "inline": false}
	}
	toolManifest := r.registeredToolManifest()
	build := r.build
	if build.Version == "" {
		build.Version = buildversion.Current
	}

	guidance := agentGuidance()
	clientProtocol := clientProtocolCapabilities()
	revisions := map[string]any{
		"tool_schema_revision":         r.currentToolSchemaRevision(),
		"capability_manifest_revision": capabilityManifestRevision(toolManifest, skills, servers, docs, guidance, clientProtocol),
		"guidance_revision":            agentGuidanceRevision(),
		"instruction_revision":         instructionRevision(docs),
		"session_capability_revision":  sessionCapabilityRevision(&session),
		"client_protocol_revision":     clientProtocolRevision(),
	}

	fullData := map[string]any{
		"remote_session_id": session.ID,
		"mcpx": map[string]any{
			"version": build.Version, "commit": build.Commit, "build_time": build.Date,
		},
		"remote_session": map[string]any{
			"id": session.ID, "role": session.Role, "status": session.Status,
			"version": session.Version, "label": session.Label, "description": session.Description,
			"workspace_name": session.WorkspaceName, "workspace_path": session.WorkspacePath,
		},
		"workspace": map[string]any{
			"name": session.WorkspaceName, "path": session.WorkspacePath,
			"git_head": gitHead, "tree_digest": treeDigest,
		},
		"revisions":       revisions,
		"agent_guidance":  guidance,
		"client_protocol": clientProtocol,
		"tools":           tools,
		"extension_inventory": map[string]any{
			"skills":      compactSkillMaps(skills),
			"mcp_servers": compactMCPServerInventory(servers),
		},
		"instructions":  instructionPayload,
		"project":       project,
		"project_tasks": tasks,
		"git": map[string]any{
			"head": gitHead, "tree_digest": treeDigest,
		},
		"pending_confirmations": pendingConfirmations,
		"tasks":                 taskList,
		"artifacts":             artifacts,
		"schema_source":         "tools/list",
		"capability_version":    cleanCoreCapabilityVersion,
		"capability_groups":     capabilityGroups(),
		"recommended_workflows": map[string]any{
			"bootstrap":      []string{"workspace", "session"},
			"source_change":  []string{"read", "edit", "execute", "observe"},
			"plan_delivery":  []string{"plan", "edit", "execute", "artifact", "observe"},
			"extension_call": []string{"skill_tool", "mcp_tool"},
		},
		"opened_at": time.Now().UTC().Format(time.RFC3339),
	}
	if latestModelState != nil {
		fullData["latest_model_state"] = latestModelState
	}

	data := fullData
	if !fullResponse {
		compactGuidance := map[string]any{
			"version": guidance["version"], "priority": guidance["priority"],
			"summary": guidance["summary"], "guidance_revision": revisions["guidance_revision"],
			"tool_routing": guidance["tool_routing"],
			"full":         nextAction("runtime_read", map[string]any{"view": "capabilities", "workspace": session.WorkspaceName, "remote_session_id": session.ID}),
		}
		data = map[string]any{
			"remote_session_id": session.ID,
			"remote_session": map[string]any{
				"id": session.ID, "role": session.Role, "status": session.Status,
				"label": session.Label, "last_active_at": session.LastActiveAt,
			},
			"workspace": map[string]any{
				"name": session.WorkspaceName, "git_head": gitHead,
				"dirty": strings.TrimSpace(fmt.Sprint(project["git_status"])) != "",
			},
			"revisions":                  revisions,
			"agent_guidance":             compactGuidance,
			"pending_confirmation_count": len(pendingConfirmations),
			"active_tasks":               map[string]any{"count": len(activeTaskIDs), "execution_task_ids": activeTaskIDs},
			"artifact_count":             artifactCount,
			"response_profile":           "compact",
			"changed_sections":           changedSessionSections(revisions, envReq.Payload["known_revisions"]),
			"omitted_sections":           []string{"agent_guidance.rules", "client_protocol", "extension_inventory", "tools", "completed_tasks", "project", "recommended_workflows"},
			"recommended_next_tool":      nextAction("read", map[string]any{"remote_session_id": session.ID, "view": "list"}),
			"resources": map[string]any{
				"instructions":           nextAction("runtime_read", map[string]any{"view": "instructions", "workspace": session.WorkspaceName, "remote_session_id": session.ID}),
				"task_logs_uri_template": "mcpx://remote-sessions/{remote_session_id}/tasks/{execution_task_id}/logs",
			},
		}
		if includeInstrContent {
			data["instructions"] = instructionPayload
		}
		if includeProjectTasks {
			data["project_tasks"] = tasks
		}
		if latestModelState != nil {
			data["latest_model_state"] = compactLatestModelState(latestModelState)
		}
	} else {
		data["response_profile"] = "full"
	}

	r.logAudit(audit.Event{
		RequestID: envReq.RequestID, RemoteSessionID: session.ID, Workspace: session.WorkspaceName,
		Tool: "session", Status: "ok",
	})
	return compactToolResult(data, fmt.Sprintf("Session %s opened for workspace %s.", session.ID, session.WorkspaceName)), nil
}

func compactLatestModelState(value any) map[string]any {
	encoded, _ := json.Marshal(value)
	var source map[string]any
	_ = json.Unmarshal(encoded, &source)
	compact := map[string]any{}
	for _, key := range []string{"status", "summary", "progress_summary", "next_step", "created_at", "event_id", "sequence"} {
		if item := source[key]; item != nil && fmt.Sprint(item) != "" {
			compact[key] = item
		}
	}
	return compact
}

func changedSessionSections(current map[string]any, known any) []string {
	knownMap, _ := known.(map[string]any)
	changed := make([]string, 0, len(current))
	for key, value := range current {
		if fmt.Sprint(knownMap[key]) != fmt.Sprint(value) {
			changed = append(changed, key)
		}
	}
	slices.Sort(changed)
	return changed
}
