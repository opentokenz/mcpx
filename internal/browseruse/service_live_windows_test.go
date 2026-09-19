//go:build windows

package browseruse

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLiveOfficialBrowserServicePersistsAcrossCommands(t *testing.T) {
	if os.Getenv("MCPX_BROWSER_LIVE_TEST") != "1" {
		t.Skip("set MCPX_BROWSER_LIVE_TEST=1 to test the installed OpenAI Browser Service")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	backends, err := DiscoverOfficial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(backends) == 0 {
		t.Fatal("OpenAI official browser extension was not discovered")
	}
	instanceID := backends[0].Info.Metadata.ExtensionInstanceID
	service := NewService()
	t.Cleanup(func() { _ = service.Close() })

	created, err := service.Execute(ctx, ServiceRequest{
		SessionID: "mcpx_live_test", TurnID: "mcpx_live_test_create", BrowserInstanceID: instanceID,
		Command: map[string]any{"type": "create_tab"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Error != nil {
		t.Fatalf("create tab: %+v", created.Error)
	}
	createdResult, _ := created.Result.(map[string]any)
	tabID, _ := createdResult["id"].(string)
	if tabID == "" {
		t.Fatalf("create tab result = %+v", created.Result)
	}
	defer func() {
		if tabID == "" {
			return
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_, _ = service.Execute(closeCtx, ServiceRequest{
			SessionID: "mcpx_live_test", TurnID: "mcpx_live_test_cleanup", BrowserInstanceID: instanceID,
			Command: map[string]any{"type": "close_tab", "tab_id": tabID},
		})
	}()

	listed, err := service.Execute(ctx, ServiceRequest{
		SessionID: "mcpx_live_test", TurnID: "mcpx_live_test_list", BrowserInstanceID: instanceID,
		Command: map[string]any{"type": "list_tabs"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Error != nil {
		t.Fatalf("list tabs: %+v", listed.Error)
	}
	listedResult, _ := listed.Result.(map[string]any)
	tabs, _ := listedResult["tabs"].([]any)
	found := false
	for _, value := range tabs {
		tab, _ := value.(map[string]any)
		if tab["id"] == tabID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("created tab %s not present in list_tabs result: %+v", tabID, listed.Result)
	}

	closed, err := service.Execute(ctx, ServiceRequest{
		SessionID: "mcpx_live_test", TurnID: "mcpx_live_test_close", BrowserInstanceID: instanceID,
		Command: map[string]any{"type": "close_tab", "tab_id": tabID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if closed.Error != nil {
		t.Fatalf("close tab: %+v", closed.Error)
	}
	tabID = ""
}
