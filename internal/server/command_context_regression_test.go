package server

import (
	"mcpx/internal/mcpresult"

	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"mcpx/internal/remotesession"
)

func TestCommandExecuteBindsPurposeAndWorkspaceScope(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	rt.cfg.Security.Commands.Allow = append(rt.cfg.Security.Commands.Allow, `^printf\b`)
	rt.cfg.Security.Commands.Confirm = append(rt.cfg.Security.Commands.Confirm, `^echo\b`)
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registered, ok := rt.reg.Get("demo")
	if !ok {
		t.Fatal("demo workspace was not registered")
	}
	created, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{
		WorkspaceName: "demo", WorkspacePath: registered.Path,
	})
	if err != nil {
		t.Fatal(err)
	}

	missingPurpose := mcpresult.Request(map[string]any{
		"remote_session_id": created.Session.ID, "command": testPrintCommand("context"),
	})

	missingResult, err := rt.toolCommandExecute(context.Background(), missingPurpose)
	if err != nil {
		t.Fatal(err)
	}
	missing := decodeToolResult(t, missingResult)
	if missing["status"] != "failed" {
		t.Fatalf("missing purpose was accepted: %+v", missing)
	}

	invalidScope := mcpresult.Request(map[string]any{
		"intent":            "validate command scope",
		"remote_session_id": created.Session.ID, "command": testPrintCommand("context"),
		"purpose": "verify context binding", "scope": "host",
	})

	invalidResult, err := rt.toolCommandExecute(context.Background(), invalidScope)
	if err != nil {
		t.Fatal(err)
	}
	invalid := decodeToolResult(t, invalidResult)
	if invalid["status"] != "failed" {
		t.Fatalf("invalid scope was accepted: %+v", invalid)
	}

	// Confirm rules create a pending action carrying the exact command context.
	confirmationRequest := mcpresult.Request(map[string]any{
		"intent":            "request semantic confirmation for a command",
		"remote_session_id": created.Session.ID, "command": "echo confirmation",
		"purpose": "verify confirmation context", "scope": "workspace",
	})

	confirmationResult, err := rt.toolCommandExecute(context.Background(), confirmationRequest)
	if err != nil {
		t.Fatal(err)
	}
	confirmationResponse := decodeToolResult(t, confirmationResult)
	confirmationData, _ := confirmationResponse["data"].(map[string]any)
	if confirmationResponse["status"] != "waiting_confirmation" || confirmationData["purpose"] != "verify confirmation context" || confirmationData["scope"] != "workspace" {
		t.Fatalf("confirm rule must create semantic confirmation: %+v", confirmationResponse)
	}
	if text, ok := confirmationResult.Content[0].(*mcp.TextContent); ok {
		marker := "confirmation_token: "
		tokenIndex := strings.Index(text.Text, marker)
		if tokenIndex < 0 || tokenIndex > 300 {
			t.Fatalf("confirmation_token must be near the start of the response so preview truncation cannot hide it: %s", text.Text)
		}
		rest := text.Text[tokenIndex+len(marker):]
		if len(rest) < 35 {
			t.Fatalf("confirmation_token in the message looks truncated: %s", text.Text)
		}
	}
	if digest, _ := confirmationData["command_digest"].(string); !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("missing command digest: %+v", confirmationData)
	}
	invalidRetry := mcpresult.Request(map[string]any{
		"intent":            "retry with a stale confirmation token",
		"remote_session_id": created.Session.ID, "command": "echo confirmation",
		"purpose": "verify confirmation context", "scope": "workspace", "confirmation_token": "ct_stale-token",
	})

	invalidResult, retryErr := rt.toolCommandExecute(context.Background(), invalidRetry)
	if retryErr != nil {
		t.Fatal(retryErr)
	}
	invalidResponse := decodeToolResult(t, invalidResult)
	invalidData, _ := invalidResponse["data"].(map[string]any)
	if invalidResponse["status"] != "waiting_confirmation" || invalidData["confirmation_token"] != confirmationData["confirmation_token"] {
		t.Fatalf("stale token must reuse the pending confirmation: %+v", invalidResponse)
	}
	invalidMessage, _ := invalidData["confirmation_message"].(string)
	if !strings.Contains(invalidMessage, "未匹配") {
		t.Fatalf("stale token retry must explain the mismatch: %s", invalidMessage)
	}
	confirmRequest := mcpresult.Request(map[string]any{
		"intent":            "用户已确认执行该命令",
		"remote_session_id": created.Session.ID, "command": "echo confirmation",
		"purpose": "verify confirmation context", "scope": "workspace", "confirmation_token": confirmationData["confirmation_token"],
	})

	confirmed, err := rt.toolCommandExecute(context.Background(), confirmRequest)
	if err != nil {
		t.Fatal(err)
	}
	confirmedResponse := decodeToolResult(t, confirmed)
	if confirmedResponse["status"] != "ok" {
		t.Fatalf("confirmed command failed: %+v", confirmedResponse)
	}

	// A retry may rephrase the purpose without invalidating the confirmation
	// token: the confirmed action is the command itself.
	rephraseConfirm := mcpresult.Request(map[string]any{
		"intent":            "request confirmation for a rephrased retry",
		"remote_session_id": created.Session.ID, "command": "echo rephrase",
		"purpose": "original intent", "scope": "workspace",
	})

	rephrasePending, err := rt.toolCommandExecute(context.Background(), rephraseConfirm)
	if err != nil {
		t.Fatal(err)
	}
	rephrasePendingResponse := decodeToolResult(t, rephrasePending)
	rephraseData, _ := rephrasePendingResponse["data"].(map[string]any)
	rephraseRetry := mcpresult.Request(map[string]any{
		"intent":            "用户已确认，重新表述用途",
		"remote_session_id": created.Session.ID, "command": "echo rephrase",
		"purpose": "rephrased intent after user confirmation", "scope": "workspace",
		"confirmation_token": rephraseData["confirmation_token"],
	})

	rephraseExecuted, err := rt.toolCommandExecute(context.Background(), rephraseRetry)
	if err != nil {
		t.Fatal(err)
	}
	if response := decodeToolResult(t, rephraseExecuted); response["status"] != "ok" {
		t.Fatalf("rephrased confirmation must reuse the pending token: %+v", response)
	}

	valid := mcpresult.Request(map[string]any{
		"intent":            "execute the scoped command",
		"remote_session_id": created.Session.ID, "command": testPrintCommand("context"),
		"purpose": "verify context binding", "scope": "workspace",
	})

	validResult, err := rt.toolCommandExecute(context.Background(), valid)
	if err != nil {
		t.Fatal(err)
	}
	response := decodeToolResult(t, validResult)
	data, _ := response["data"].(map[string]any)
	if response["status"] != "ok" || data["purpose"] != "verify context binding" || data["scope"] != "workspace" || data["workspace_scoped"] != true {
		t.Fatalf("execution context was not returned: %+v", response)
	}
	if digest, _ := data["command_digest"].(string); !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("missing command digest: %+v", data)
	}
}

func TestCommandDeniedExplainsUnsafeShellFeatures(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registered, ok := rt.reg.Get("demo")
	if !ok {
		t.Fatal("demo workspace was not registered")
	}
	created, err := rt.remote.Create(context.Background(), principal, remotesession.CreateInput{
		WorkspaceName: "demo", WorkspacePath: registered.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := mcpresult.Request(map[string]any{
		"intent":            "deny unsafe compound verification",
		"remote_session_id": created.Session.ID,
		"command":           `printf '%s' "$(echo hi)"`,
		"purpose":           "verify remote branch pointer",
		"scope":             "workspace",
	})

	result, err := rt.toolCommandExecute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	response := decodeToolResult(t, result)
	errorBody, _ := response["error"].(map[string]any)
	if errorBody["code"] != "DENIED" {
		t.Fatalf("unsafe command must be denied: %+v", response)
	}
	message, _ := errorBody["message"].(string)
	for _, phrase := range []string{"shell 特性", "简单命令", "git fetch"} {
		if !strings.Contains(message, phrase) {
			t.Fatalf("denied message must explain %q: %s", phrase, message)
		}
	}
}
