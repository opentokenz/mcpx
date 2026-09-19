//go:build windows

package browseruse

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLiveOfficialExtension(t *testing.T) {
	if os.Getenv("MCPX_BROWSER_LIVE_TEST") != "1" {
		t.Skip("set MCPX_BROWSER_LIVE_TEST=1 to test the installed OpenAI browser extension")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	backends, err := DiscoverOfficial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(backends) == 0 {
		t.Fatal("OpenAI official browser extension was not discovered")
	}
	client := Client{Pipe: backends[0].Pipe}
	if err := client.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.UserTabs(ctx); err != nil {
		t.Fatal(err)
	}
}
