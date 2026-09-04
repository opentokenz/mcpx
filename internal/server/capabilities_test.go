package server

import (
	"mcpx/internal/mcpresult"

	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/auth"
	"mcpx/internal/config"
	"mcpx/internal/edit"
	"mcpx/internal/file"
	"mcpx/internal/operation"
	"mcpx/internal/remotesession"
	"mcpx/internal/source"
)

func TestCapabilityCatalogMatchesRegisteredTools(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	runtime, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	runtime.registerTools(protocol)
	registered := make([]string, 0, len(runtime.listedToolMap()))
	for name := range runtime.listedToolMap() {
		registered = append(registered, name)
	}
	sort.Strings(registered)
	declared := capabilityToolNames()
	if len(registered) != len(declared) {
		t.Fatalf("tool count mismatch: registered=%d declared=%d\nregistered=%v\ndeclared=%v", len(registered), len(declared), registered, declared)
	}
	for index := range registered {
		if registered[index] != declared[index] {
			t.Fatalf("tool catalog mismatch at %d: registered=%q declared=%q", index, registered[index], declared[index])
		}
	}
}

func TestMachineToolCapabilitiesApplyRoleAndFeatureState(t *testing.T) {
	effective := config.DefaultConfig()
	effective.Terminal.Enabled = false
	viewer := remotesession.Session{Role: "viewer"}
	items := machineToolCapabilities(effective, &viewer)
	states := map[string]string{}
	for _, item := range items {
		states[item["name"].(string)] = item["state"].(string)
	}
	if states["read"] != "available" || states["edit"] != "forbidden" || states["execute"] != "disabled" {
		t.Fatalf("unexpected capability states: %+v", states)
	}
}

func TestMachineCapabilitiesPublishHardLimits(t *testing.T) {
	limits := publishedLimits()
	read := limits["read"].(map[string]any)
	if read["max_source_bytes"] != file.MaxSourceBytes || read["max_items"] != MaxReadItems || read["max_direct_entries"] != source.MaxDirectListEntries {
		t.Fatalf("read limits=%+v", read)
	}
	if limits["operation_batch"].(map[string]any)["max_steps"] != operation.MaxSteps {
		t.Fatalf("operation limits=%+v", limits["operation_batch"])
	}
	if limits["edit"].(map[string]any)["max_changed_lines"] != edit.MaxChangedLines {
		t.Fatalf("edit limits=%+v", limits["edit"])
	}
	moveOut := limits["move_out"].(map[string]any)
	if moveOut["max_targets"] != MaxMoveOutTargets || moveOut["max_response_preview_targets"] != MaxMoveOutResponsePreviewTargets || moveOut["max_manifest_entries"] != nil {
		t.Fatalf("move-out limits=%+v", moveOut)
	}
	items := machineToolCapabilities(config.DefaultConfig(), nil)
	for _, item := range items {
		if item["name"] == "read" && item["limits"] == nil {
			t.Fatalf("read tool limits missing: %+v", item)
		}
	}
}

func TestCapabilityListIncludesInstructionsSkillsAndRoleState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	workspace := filepath.Join(home, "project")
	if err := os.MkdirAll(filepath.Join(workspace, ".skills", "review"), 0o755); err != nil {
		t.Fatal(err)
	}
	globalAgents := filepath.Join(home, "GLOBAL_AGENTS.md")
	if err := os.WriteFile(globalAgents, []byte("# Global\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("# Project\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".skills", "review", "SKILL.md"), []byte("---\nname: review\ndescription: Review code\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Auth.Mode = "bearer"
	cfg.Auth.Token = "developer-token"
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "project", Path: workspace}}
	cfg.Discovery.Instructions.GlobalAgentsPath = globalAgents
	cfg.Discovery.Skills.Dirs = []string{".skills"}
	cfg.Logging.Enabled = false
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	ctx := authContextForCapabilities()
	created := callEnvelope(t, runtime.toolSessionOpen, ctx, map[string]any{"workspace": "project", "response_profile": "full"})
	createdData := created["data"].(map[string]any)
	openedInventory := createdData["extension_inventory"].(map[string]any)
	openedSkills := openedInventory["skills"].([]any)
	if len(openedSkills) != 1 || openedSkills[0].(map[string]any)["name"] != "review" {
		t.Fatalf("session_open skills should follow server config: %+v", openedInventory)
	}
	if skill := openedSkills[0].(map[string]any); len(skill) > 2 || skill["description"] == nil || skill["revision"] != nil || skill["arguments_schema"] != nil || skill["instructions"] != nil {
		t.Fatalf("session_open skill inventory must stay compact: %+v", skill)
	}
	remoteID, _ := created["remote_session_id"].(string)
	response := callEnvelope(t, runtime.toolCapabilityList, ctx, map[string]any{"remote_session_id": remoteID})
	data := response["data"].(map[string]any)
	if data["revision"] == "" || data["schema_source"] != "tools/list" {
		t.Fatalf("capability metadata: %+v", data)
	}
	clientProtocol, _ := data["client_protocol"].(map[string]any)
	activityProtocol, _ := clientProtocol["activity"].(map[string]any)
	if clientProtocol["version"] != clientProtocolVersion || activityProtocol["version"] != agentActivityProtocolVersion || activityProtocol["transport"] != "mcp_tool_arguments" || activityProtocol["argument"] != "activity" {
		t.Fatalf("client protocol capability missing: %+v", clientProtocol)
	}
	fields, _ := activityProtocol["fields"].([]any)
	if len(fields) != len(agentActivityKindNames) || activityProtocol["multiple_per_call"] != true || activityProtocol["turn_boundary"] != "non_empty_intent_starts_new_turn" {
		t.Fatalf("activity fields missing from capability: %+v", activityProtocol)
	}
	revisions := data["revisions"].(map[string]any)
	if revisions["client_protocol_revision"] == "" {
		t.Fatalf("client protocol revision missing: %+v", revisions)
	}
	instructions := data["instructions"].(map[string]any)["documents"].([]any)
	if len(instructions) != 2 {
		t.Fatalf("instructions: %+v", instructions)
	}
	extensionInventory := data["extension_inventory"].(map[string]any)
	skills := extensionInventory["skills"].([]any)
	if len(skills) != 1 || skills[0].(map[string]any)["name"] != "review" {
		t.Fatalf("skills: %+v", skills)
	}
	if data["skills"] != nil || data["upstream_mcp"] != nil {
		t.Fatalf("runtime capability must use compact extension_inventory: %+v", data)
	}
	for _, removed := range []string{"skill_revision", "mcp_revision"} {
		if revisions[removed] != nil {
			t.Fatalf("runtime capability must not expose %s: %+v", removed, revisions)
		}
	}
	tools := data["tools"].([]any)
	foundEdit := false
	for _, raw := range tools {
		item := raw.(map[string]any)
		if item["name"] == "edit" {
			foundEdit = item["state"] == "available"
		}
	}
	if !foundEdit {
		t.Fatal("owner capability did not expose edit as available")
	}

	readRequest := mcpresult.Request(map[string]any{"intent": "read project instructions", "remote_session_id": remoteID, "id": "project"})

	readResult, err := runtime.toolAgentInstructionRead(ctx, readRequest)
	if err != nil {
		t.Fatal(err)
	}
	readResponse := decodeToolResult(t, readResult)
	if readResponse["data"].(map[string]any)["content"] != "# Project\n" {
		t.Fatalf("instruction read: %+v", readResponse)
	}
}

func TestRuntimeCapabilitiesPublishBuildProvenanceAndNoLegacyRevisionAliases(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	workspace := filepath.Join(home, "project")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Logging.Enabled = false
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "project", Path: workspace}}
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := New(Options{Version: "9.8.7-test", Commit: "abc123def", Date: "2026-08-09T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	response := callEnvelope(t, runtime.toolRuntimeRead, context.Background(), map[string]any{
		"view": "capabilities", "workspace": "project",
	})
	if response["status"] != "succeeded" && response["status"] != "ok" {
		t.Fatalf("runtime capabilities=%+v", response)
	}
	data := response["data"].(map[string]any)
	metadata, ok := data["runtime"].(map[string]any)
	if !ok {
		t.Fatalf("runtime provenance missing: %+v", data)
	}
	revisions := data["revisions"].(map[string]any)
	if revisions["client_protocol_revision"] == "" || metadata["client_protocol_revision"] != revisions["client_protocol_revision"] {
		t.Fatalf("runtime client protocol revision=%+v revisions=%+v", metadata, revisions)
	}
	if metadata["version"] != "9.8.7-test" || metadata["build_commit"] != "abc123def" || metadata["build_time"] != "2026-08-09T00:00:00Z" {
		t.Fatalf("build provenance=%+v", metadata)
	}
	if metadata["tool_schema_revision"] == "" || metadata["tool_schema_revision"] != revisions["tool_schema_revision"] {
		t.Fatalf("runtime tool schema revision=%+v revisions=%+v", metadata, revisions)
	}
	if metadata["capability_version"] != cleanCoreCapabilityVersion || metadata["capability_groups"] == nil {
		t.Fatalf("runtime capabilities metadata=%+v", metadata)
	}
	for _, legacy := range []string{"tool_schema_revision", "skill_revision", "mcp_revision"} {
		if data[legacy] != nil {
			t.Fatalf("runtime capabilities must not duplicate deprecated top-level %s: %+v", legacy, data)
		}
	}
}

func authContextForCapabilities() context.Context {
	return auth.ContextWithAuthorization(context.Background(), "Bearer developer-token")
}
