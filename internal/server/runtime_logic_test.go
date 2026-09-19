package server

import (
	"mcpx/internal/mcpresult"

	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcpx/internal/auth"
	"mcpx/internal/config"
	"mcpx/internal/remotesession"
	"mcpx/internal/security"
	"mcpx/internal/terminal"
)

func TestStartupPrunesMissingWorkspaces(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	valid := filepath.Join(home, "valid")
	if err := os.MkdirAll(valid, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(home, "gone")
	cfg := config.DefaultConfig()
	cfg.Auth.Mode = "open"
	cfg.Logging.Enabled = false
	cfg.Logging.Dir = filepath.Join(home, "logs")
	cfg.Workspaces = []config.WorkspaceEntry{
		{Name: "valid", Path: valid},
		{Name: "gone", Path: missing},
	}
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}

	rt, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if got := rt.reg.List(); len(got) != 1 || got[0].Name != "valid" {
		t.Fatalf("registry after startup=%+v", got)
	}
	persisted, err := config.LoadGlobal(filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.Workspaces) != 1 || persisted.Workspaces[0].Name != "valid" {
		t.Fatalf("persisted after startup=%+v", persisted.Workspaces)
	}
}

func TestStartupWaitsForUnavailableWorkspaceParent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	mountRoot := filepath.Join(home, "volume")
	workspacePath := filepath.Join(mountRoot, "code", "project")
	cfg := config.DefaultConfig()
	cfg.Auth.Mode = "open"
	cfg.Logging.Enabled = false
	cfg.Logging.Dir = filepath.Join(home, "logs")
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "project", Path: workspacePath}}
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}

	type result struct {
		runtime *Runtime
		err     error
	}
	done := make(chan result, 1)
	go func() {
		rt, err := New(Options{})
		done <- result{runtime: rt, err: err}
	}()

	select {
	case got := <-done:
		if got.runtime != nil {
			_ = got.runtime.Close()
		}
		t.Fatalf("startup did not wait for unavailable workspace parent: %v", got.err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		t.Cleanup(func() { _ = got.runtime.Close() })
		if names := got.runtime.reg.List(); len(names) != 1 || names[0].Name != "project" {
			t.Fatalf("registry after mount=%+v", names)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startup did not resume after workspace parent appeared")
	}
}

func TestExecPipelineAllowConfirmDeny(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	ws := filepath.Join(home, "proj")
	_ = os.MkdirAll(ws, 0o755)
	cfg := config.DefaultConfig()
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "proj", Path: ws}}
	cfg.Security.Commands = config.CommandRules{
		Allow:   []string{`^echo\b`},
		Confirm: []string{`^npm install`},
		Deny:    []string{`^rm -rf /`},
	}
	cfg.Logging.Dir = filepath.Join(home, "logs")
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	eff := rt.effectiveConfig(ws)
	if security.MatchCommand(eff.Security.Commands, "echo hi") != security.Allow {
		t.Fatal("allow")
	}
	if security.MatchCommand(eff.Security.Commands, "npm install") != security.Confirm {
		t.Fatal("confirm rules must require semantic confirmation")
	}
	if security.MatchCommand(eff.Security.Commands, "rm -rf /") != security.Deny {
		t.Fatal("deny")
	}
	res, err := terminal.Exec(context.Background(), terminal.ExecOptions{WorkDir: ws, Command: "echo pipeline-ok"})
	if err != nil || res.ExitCode != 0 || !strings.Contains(res.Stdout, "pipeline-ok") {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestTerminalExecRunsAfterSemanticConfirmation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	ws := filepath.Join(home, "proj")
	_ = os.MkdirAll(ws, 0o755)
	cfg := config.DefaultConfig()
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "proj", Path: ws}}
	// Confirm rules require semantic confirmation before starting the task.
	cfg.Security.Commands = config.CommandRules{
		Confirm: []string{`^echo\b`},
	}
	cfg.Logging.Dir = filepath.Join(home, "logs")
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	created, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{
		WorkspaceName: "proj", WorkspacePath: ws, Label: "no confirmation test",
	})
	if err != nil {
		t.Fatal(err)
	}

	req := mcpresult.Request(map[string]any{
		"intent":            "request an approved command",
		"remote_session_id": created.Session.ID,
		"command":           "echo approved-run",
		"purpose":           "run the command test",
		"scope":             "workspace",
	})

	out, err := rt.toolCommandExecute(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	response := decodeToolResult(t, out)
	if response["status"] != "waiting_confirmation" {
		t.Fatalf("command must require semantic confirmation: %+v", response)
	}
	data, _ := response["data"].(map[string]any)
	if data["confirmation_required"] != true || data["command"] != "echo approved-run" {
		t.Fatalf("confirmation response missing semantic prompt: %+v", response)
	}
	confirmReq := mcpresult.Request(map[string]any{
		"intent":            "用户已确认执行该命令",
		"remote_session_id": created.Session.ID, "command": "echo approved-run",
		"purpose": "run the command test", "scope": "workspace", "confirmation_token": data["confirmation_token"],
	})

	approved, err := rt.toolCommandExecute(context.Background(), confirmReq)
	if err != nil {
		t.Fatal(err)
	}
	response = decodeToolResult(t, approved)
	if response["status"] != "ok" {
		t.Fatalf("approved command did not run: %+v", response)
	}
	data, _ = response["data"].(map[string]any)
	stdout, _ := data["stdout"].(string)
	if !strings.Contains(stdout, "approved-run") {
		t.Fatalf("stdout %v", data)
	}
	if pending := rt.approvals.ListRemoteSession(created.Session.ID); len(pending) != 0 {
		t.Fatalf("confirmation must be consumed after execution: %+v", pending)
	}
}

func TestTerminalStartUsesCommandPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	ws := filepath.Join(home, "proj")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "proj", Path: ws}}
	cfg.Security.Commands = config.CommandRules{Default: "deny"}
	cfg.Logging.Dir = filepath.Join(home, "logs")
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	created, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{
		WorkspaceName: "proj", WorkspacePath: ws, Label: "terminal policy test",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := mcpresult.Request(map[string]any{
		"intent":            "verify command policy",
		"remote_session_id": created.Session.ID,
		"command":           "echo must-not-start",
		"purpose":           "verify denied command policy",
		"scope":             "workspace",
	})

	out, err := rt.toolCommandExecute(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	response := decodeToolResult(t, out)
	if response["status"] != "failed" {
		t.Fatalf("got %+v", response)
	}
	errBody, _ := response["error"].(map[string]any)
	// Denied commands must not start; status failed with a policy/denied style error.
	if code, _ := errBody["code"].(string); code == "" && response["data"] == nil {
		t.Fatalf("denied response missing error body: %+v", response)
	}
}

func TestRequireAuthBearerFromContext(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCPX_HOME", home)
	t.Setenv("MCPX_TEST_BEARER", "") // ensure HTTP path, not env fallback
	ws := filepath.Join(home, "proj")
	_ = os.MkdirAll(ws, 0o755)
	cfg := config.DefaultConfig()
	cfg.Auth.Token = "secret-token"
	cfg.Workspaces = []config.WorkspaceEntry{{Name: "proj", Path: ws}}
	cfg.Logging.Dir = filepath.Join(home, "logs")
	if err := config.WriteGlobal(filepath.Join(home, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	// missing header
	if _, err := rt.principalFromContext(context.Background()); err == nil {
		t.Fatal("expected missing credentials to be rejected")
	}

	// wrong token
	wrong := auth.ContextWithAuthorization(context.Background(), "Bearer wrong")
	if _, err := rt.principalFromContext(wrong); err == nil {
		t.Fatal("expected wrong credentials to be rejected")
	}

	// ok via context injection path used by HTTP
	ctx := auth.ContextWithAuthorization(context.Background(), "Bearer secret-token")
	principal, err := rt.principalFromContext(ctx)
	if err != nil || principal.Kind != "bearer" {
		t.Fatalf("expected bearer principal, got %+v err=%v", principal, err)
	}
}
