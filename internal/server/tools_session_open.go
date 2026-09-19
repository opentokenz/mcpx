package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/audit"
	workspacefile "mcpx/internal/file"
	"mcpx/internal/instruction"
	"mcpx/internal/observation"
	"mcpx/internal/projecttask"
	"mcpx/internal/remotesession"
	"mcpx/internal/skill"
	buildversion "mcpx/internal/version"
	workspaceidentity "mcpx/internal/workspace"
)

// toolSessionOpen creates or reuses a Remote Session. New sessions return the full
// bootstrap bundle; resumed sessions return dynamic state plus revisions so static
// capability metadata is not repeatedly injected into model context.
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
	var session remotesession.Session
	remoteID, _ := envReq.Payload["remote_session_id"].(string)
	remoteID = strings.TrimSpace(remoteID)
	if remoteID == "" {
		remoteID = strings.TrimSpace(envReq.RemoteSessionID)
	}
	resuming := remoteID != ""

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
	var gitIdentity *workspaceidentity.GitIdentity
	if runID := stringPayload(envReq.Payload, "run_id"); runID != "" || stringPayload(envReq.Payload, "git_identity_path") != "" {
		if err := r.reconcileWorkspaceTransition(ctx, session.ID, runID); err != nil {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "WORKSPACE_TRANSITION_UNVERIFIED", err.Error())
		}
		identityPath := stringPayload(envReq.Payload, "git_identity_path")
		if identityPath == "" {
			identityPath = "."
		}
		resolved, err := workspacefile.Resolve(wsPath, identityPath)
		if err != nil {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "WORKSPACE_IDENTITY_UNAVAILABLE", "identity target must be inside registered workspace")
		}
		remoteName := stringPayload(envReq.Payload, "remote_name")
		if remoteName == "" {
			remoteName = "origin"
		}
		identity, err := workspaceidentity.CaptureGitIdentity(ctx, resolved, remoteName, runID)
		if err != nil {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "WORKSPACE_IDENTITY_UNAVAILABLE", err.Error())
		}
		if err := workspaceidentity.FreezeGitIdentity(ctx, r.state.DB(), session.ID, identity); err != nil {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "WORKSPACE_IDENTITY_MISMATCH", err.Error())
		}
		gitIdentity = &identity
	}
	effective := r.effectiveConfig(wsPath)
	var tools []map[string]any
	if !resuming {
		tools = r.runtimeToolCapabilities(effective, &session)
	}

	var (
		servers              = []map[string]any{}
		skills               = []map[string]any{}
		docs                 []instruction.Document
		project              map[string]any
		gitHead              string
		treeDigest           string
		pendingConfirmations []map[string]any
		taskList             any
		artifacts            any
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
		if !resuming || includeProjectTasks {
			project = inspectProject(ctx, wsPath)
		}
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
		taskLimit := 20
		if resuming {
			taskLimit = 100
		}
		recentTasks, _ := r.tasks.List(session.ID, taskLimit)
		if resuming {
			activeTasks := make([]map[string]any, 0, len(recentTasks))
			for _, task := range recentTasks {
				if fmt.Sprint(task["status"]) == "running" {
					activeTasks = append(activeTasks, task)
				}
			}
			taskList = activeTasks
		} else {
			taskList = recentTasks
			artifacts, _ = r.artifacts.List(ctx, session.ID, "", 20)
		}
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

	data := map[string]any{
		"remote_session_id": session.ID,
		"remote_session": map[string]any{
			"id": session.ID, "role": session.Role, "status": session.Status,
			"version": session.Version, "label": session.Label, "description": session.Description,
			"workspace_name": session.WorkspaceName, "workspace_path": session.WorkspacePath,
		},
		"workspace": map[string]any{
			"name": session.WorkspaceName, "path": session.WorkspacePath,
			"git_head": gitHead, "tree_digest": treeDigest,
		},
		"revisions":             revisions,
		"pending_confirmations": pendingConfirmations,
		"tasks":                 taskList,
	}
	if resuming {
		data["tasks_scope"] = "running"
	}
	if !resuming {
		data["artifacts"] = artifacts
		data["mcpx"] = map[string]any{
			"version": build.Version, "commit": build.Commit, "build_time": build.Date,
		}
		data["extension_inventory"] = map[string]any{
			"skills":      compactSkillMaps(skills),
			"mcp_servers": compactMCPServerInventory(servers),
		}
		data["project"] = project
		data["project_tasks"] = tasks
		data["git"] = map[string]any{"head": gitHead, "tree_digest": treeDigest}
		data["opened_at"] = time.Now().UTC().Format(time.RFC3339)
		data["agent_guidance"] = guidance
		data["client_protocol"] = clientProtocol
		data["tools"] = tools
		data["schema_source"] = "tools/list"
		data["capability_version"] = cleanCoreCapabilityVersion
		data["capability_groups"] = capabilityGroups()
		data["recommended_workflows"] = map[string]any{
			"bootstrap":      []string{"workspace", "session"},
			"source_change":  []string{"read", "edit", "execute", "observe"},
			"plan_delivery":  []string{"plan", "edit", "execute", "artifact", "observe"},
			"extension_call": []string{"skill_tool", "mcp_tool"},
		}
	}
	if !resuming || includeInstrContent {
		data["instructions"] = instructionPayload
	}
	if resuming && includeProjectTasks {
		data["project"] = project
		data["project_tasks"] = tasks
	}
	if latestModelState != nil {
		data["latest_model_state"] = latestModelState
	}
	if gitIdentity != nil {
		data["git_identity"] = gitIdentity
	}

	r.bindRemoteSession(ctx, principal, session.ID)
	r.logAudit(audit.Event{
		RequestID: envReq.RequestID, RemoteSessionID: session.ID, Workspace: session.WorkspaceName,
		Tool: "session", Status: "ok",
	})
	return compactToolResult(data, fmt.Sprintf("Session %s opened for workspace %s.", session.ID, session.WorkspaceName)), nil
}
