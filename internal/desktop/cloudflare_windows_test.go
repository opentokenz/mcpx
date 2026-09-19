//go:build windows

package desktop

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"mcpx/internal/config"
)

func TestCloudflareManageWithMCPXDefaultsAndPersists(t *testing.T) {
	t.Setenv("MCPX_HOME", t.TempDir())

	cfg, err := loadCloudflareDesktopConfig()
	if err != nil {
		t.Fatalf("load default cloudflare config: %v", err)
	}
	if cfg.ManageWithMCPX {
		t.Fatal("default config should leave Cloudflare lifecycle opt-in")
	}

	cfg.ManageWithMCPX = true
	if _, err := saveCloudflareDesktopConfig(cfg); err != nil {
		t.Fatalf("save manual Cloudflare lifecycle config: %v", err)
	}
	loaded, err := loadCloudflareDesktopConfig()
	if err != nil {
		t.Fatalf("reload manual Cloudflare lifecycle config: %v", err)
	}
	if !loaded.ManageWithMCPX {
		t.Fatal("manage_with_mcpx=true should persist")
	}

	_, configPath, _, _, _, err := cloudflarePaths()
	if err != nil {
		t.Fatalf("resolve Cloudflare config path: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(`{"mode":"quick"}`), 0o600); err != nil {
		t.Fatalf("write legacy Cloudflare config: %v", err)
	}
	legacy, err := loadCloudflareDesktopConfig()
	if err != nil {
		t.Fatalf("load legacy Cloudflare config: %v", err)
	}
	if legacy.ManageWithMCPX {
		t.Fatal("legacy config without manage_with_mcpx should remain opt-in")
	}
}

func TestNormalizePublicOrigin(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "origin", input: "https://mcp.example.com", want: "https://mcp.example.com"},
		{name: "trailing slash", input: "https://mcp.example.com/", want: "https://mcp.example.com"},
		{name: "mcp path rejected", input: "https://mcp.example.com/mcp", wantErr: true},
		{name: "http rejected", input: "http://mcp.example.com", wantErr: true},
		{name: "relative rejected", input: "mcp.example.com", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizePublicOrigin(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizePublicOrigin(%q) expected error, got %q", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizePublicOrigin(%q): %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("normalizePublicOrigin(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestStopCloudflareTunnelStopsTrackedProcess(t *testing.T) {
	t.Setenv("MCPX_HOME", t.TempDir())
	cfg := defaultCloudflareDesktopConfig()
	cfg.DesiredRunning = true
	if _, err := saveCloudflareDesktopConfig(cfg); err != nil {
		t.Fatalf("save desired-running config: %v", err)
	}

	ping, err := exec.LookPath("ping")
	if err != nil {
		t.Skipf("ping not available: %v", err)
	}
	child := exec.Command(ping, "-n", "30", "127.0.0.1")
	if err := child.Start(); err != nil {
		t.Fatalf("start test process: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})

	if err := writeCloudflareProcessState(cloudflareProcessState{
		PID:        child.Process.Pid,
		Executable: ping,
		Mode:       "named",
	}); err != nil {
		t.Fatalf("write cloudflare state: %v", err)
	}
	if err := stopCloudflareTunnel(); err != nil {
		t.Fatalf("stop cloudflare tunnel: %v", err)
	}
	alive, _ := processAlive(child.Process.Pid, ping)
	if alive {
		t.Fatal("tracked cloudflared-like process still alive after stop")
	}
	if _, err := readCloudflareProcessState(); err == nil {
		t.Fatal("cloudflare process state should be removed after stop")
	}
	cfg, err = loadCloudflareDesktopConfig()
	if err != nil {
		t.Fatalf("load Cloudflare config after stop: %v", err)
	}
	if cfg.DesiredRunning {
		t.Fatal("manual stop must clear desired_running so watchdog does not restart it")
	}
}

func TestExtractTryCloudflareURL(t *testing.T) {
	line := `INF +--------------------------------------------------------------------------------------------+`
	if got := extractTryCloudflareURL(line); got != "" {
		t.Fatalf("unexpected URL from unrelated line: %q", got)
	}

	line = `INF |  https://quiet-moon-123.trycloudflare.com                                      |`
	if got := extractTryCloudflareURL(line); got != "https://quiet-moon-123.trycloudflare.com" {
		t.Fatalf("extractTryCloudflareURL() = %q", got)
	}
}

func TestValidateCloudflareStartConfigRejectsOpenAuth(t *testing.T) {
	runtimeCfg := config.DefaultConfig()
	runtimeCfg.Auth.Mode = "open"

	err := validateCloudflareStartConfig(defaultCloudflareDesktopConfig(), runtimeCfg)
	if err == nil {
		t.Fatal("expected public tunnel start to reject auth.mode=open")
	}
}

func TestValidateCloudflareStartConfigNamedRequiresTokenAndOrigin(t *testing.T) {
	runtimeCfg := config.DefaultConfig()
	runtimeCfg.Auth.Mode = "bearer"
	runtimeCfg.Auth.Token = "test-token"

	desktopCfg := defaultCloudflareDesktopConfig()
	desktopCfg.Mode = "named"
	if err := validateCloudflareStartConfig(desktopCfg, runtimeCfg); err == nil {
		t.Fatal("expected named tunnel without token to fail")
	}

	desktopCfg.TunnelToken = "test-tunnel-token"
	if err := validateCloudflareStartConfig(desktopCfg, runtimeCfg); err == nil {
		t.Fatal("expected named tunnel without public origin to fail")
	}

	desktopCfg.PublicURL = "https://mcp.example.com"
	if err := validateCloudflareStartConfig(desktopCfg, runtimeCfg); err != nil {
		t.Fatalf("valid named tunnel config rejected: %v", err)
	}
}

func TestPublicMCPURL(t *testing.T) {
	if got := publicMCPURL("https://mcp.example.com/"); got != "https://mcp.example.com/mcp" {
		t.Fatalf("publicMCPURL() = %q", got)
	}
}

func TestCheckCloudflareHTTPAcceptsProtectedMCPReachability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	got := checkCloudflareHTTP(server.URL+"/mcp", false)
	if !got.OK || got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("protected MCP health = %+v, want reachable 401", got)
	}
}

func TestCheckCloudflareHTTPRequiresOAuthMetadataSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	if got := checkCloudflareHTTP(server.URL+"/ok", true); !got.OK {
		t.Fatalf("OAuth metadata 200 should be healthy: %+v", got)
	}
	if got := checkCloudflareHTTP(server.URL+"/missing", true); got.OK {
		t.Fatalf("OAuth metadata 404 should be unhealthy: %+v", got)
	}
}

func TestCheckLocalMCPHealthActuallyProbesHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	got := checkLocalMCPHealth(serviceState{
		Status:   statusRunning,
		Addr:     strings.TrimPrefix(server.URL, "http://"),
		Endpoint: server.URL + "/mcp",
	})
	if !got.OK || got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("local MCP health = %+v, want reachable 401", got)
	}
	if !strings.Contains(got.Detail, "HTTP 401 Unauthorized") {
		t.Fatalf("local MCP detail = %q, want HTTP status", got.Detail)
	}
}

func TestCheckLocalMCPHealthDetectsDeadHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := server.URL + "/mcp"
	addr := strings.TrimPrefix(server.URL, "http://")
	server.Close()

	got := checkLocalMCPHealth(serviceState{Status: statusRunning, Addr: addr, Endpoint: endpoint})
	if got.OK {
		t.Fatalf("dead local MCP should be unhealthy: %+v", got)
	}
	if !strings.Contains(got.Detail, "MCPX running") {
		t.Fatalf("local MCP detail = %q, want process context", got.Detail)
	}
}

func TestDefaultCloudflareDesktopConfigEnablesAutoRecovery(t *testing.T) {
	cfg := defaultCloudflareDesktopConfig()
	if !cfg.AutoRecover {
		t.Fatal("auto recovery should be enabled by default")
	}
	if cfg.DesiredRunning {
		t.Fatal("default config must not assume the Tunnel should already be running")
	}
}

func TestCloudflareHealthNeedsAutoRecovery(t *testing.T) {
	healthy := cloudflareHealth{OK: true}
	if cloudflareHealthNeedsAutoRecovery(healthy) {
		t.Fatal("healthy result must not trigger auto recovery")
	}

	public503 := cloudflareHealth{
		Software:      cloudflareHealthItem{OK: true},
		LocalMCP:      cloudflareHealthItem{OK: true},
		TunnelProcess: cloudflareHealthItem{OK: true},
		PublicMCP:     cloudflareHealthItem{StatusCode: http.StatusServiceUnavailable},
		OAuthMetadata: cloudflareHealthItem{OK: true},
	}
	if !cloudflareHealthNeedsAutoRecovery(public503) {
		t.Fatal("public MCP 503 should trigger auto recovery")
	}

	oauthMismatch := cloudflareHealth{
		Software:      cloudflareHealthItem{OK: true},
		LocalMCP:      cloudflareHealthItem{OK: true},
		TunnelProcess: cloudflareHealthItem{OK: true},
		PublicMCP:     cloudflareHealthItem{OK: true},
		OAuthMetadata: cloudflareHealthItem{OK: false, StatusCode: http.StatusOK},
	}
	if cloudflareHealthNeedsAutoRecovery(oauthMismatch) {
		t.Fatal("OAuth origin mismatch with reachable metadata is configuration, not a restart condition")
	}

	oauth503 := oauthMismatch
	oauth503.OAuthMetadata.StatusCode = http.StatusServiceUnavailable
	if !cloudflareHealthNeedsAutoRecovery(oauth503) {
		t.Fatal("OAuth metadata 503 should trigger auto recovery")
	}
}

func TestDiagnoseCloudflareHealthBadGateway(t *testing.T) {
	got := diagnoseCloudflareHealth(cloudflareHealth{
		Software:      cloudflareHealthItem{OK: true},
		LocalMCP:      cloudflareHealthItem{OK: true},
		TunnelProcess: cloudflareHealthItem{OK: true},
		PublicMCP:     cloudflareHealthItem{StatusCode: http.StatusBadGateway, Detail: "502 Bad Gateway"},
		OAuthMetadata: cloudflareHealthItem{OK: true},
	})
	if !strings.Contains(got, "502") || !strings.Contains(got, "cloudflared.log") {
		t.Fatalf("diagnosis = %q, want Cloudflare 502 guidance", got)
	}
}

func TestCloudflareStatusLabel(t *testing.T) {
	running := cloudflareState{
		Status: cloudflareStatusRunning,
		PID:    1234,
		Mode:   "named",
	}
	if got := cloudflareStatusLabel(running); got != "状态：运行中 · named · pid=1234" {
		t.Fatalf("running label = %q", got)
	}

	missing := cloudflareState{
		Status:   cloudflareStatusStopped,
		Software: cloudflareSoftwareStatus{Installed: false},
	}
	if got := cloudflareStatusLabel(missing); got != "状态：已停止 · cloudflared 未安装" {
		t.Fatalf("missing software label = %q", got)
	}
}

func TestCloudflareHealthLabel(t *testing.T) {
	desktop := &desktopApp{}
	running := cloudflareState{
		Status:   cloudflareStatusRunning,
		Software: cloudflareSoftwareStatus{Installed: true},
	}
	if got := desktop.cloudflareHealthLabel(running); got != "健康状态：未检查" {
		t.Fatalf("initial health label = %q", got)
	}

	healthy := cloudflareHealth{OK: true}
	desktop.lastCloudflareHealth = &healthy
	if got := desktop.cloudflareHealthLabel(running); got != "健康状态：正常" {
		t.Fatalf("healthy label = %q", got)
	}

	healthy.OK = false
	if got := desktop.cloudflareHealthLabel(running); got != "健康状态：异常" {
		t.Fatalf("unhealthy label = %q", got)
	}
}
