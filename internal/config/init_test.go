package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestEnsureGlobalLayoutCreatesFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPX_HOME", dir)

	res, err := EnsureGlobalLayout()
	if err != nil {
		t.Fatal(err)
	}
	if !res.CreatedConfig || !res.CreatedMCP {
		t.Fatalf("expected create config+mcp: %+v", res)
	}
	configInfo, err := os.Stat(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := configInfo.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("config mode %o, want 600", got)
	}
	b, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	mcpInfo, err := os.Stat(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := mcpInfo.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("mcp file mode %o, want 600", got)
	}
	var f MCPFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if f.MCPServers == nil {
		t.Fatal("mcpServers should be object")
	}
	if _, err := os.Stat(filepath.Join(dir, "skills")); err != nil {
		t.Fatal("skills dir", err)
	}

	// second call does not overwrite
	res2, err := EnsureGlobalLayout()
	if err != nil {
		t.Fatal(err)
	}
	if res2.CreatedConfig || res2.CreatedMCP {
		t.Fatalf("should not recreate: %+v", res2)
	}
}
