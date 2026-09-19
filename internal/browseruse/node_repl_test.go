package browseruse

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFindNodeReplHostExplicitOverrides(t *testing.T) {
	dir := t.TempDir()
	host := NodeReplHost{
		NodeReplPath: filepath.Join(dir, "node_repl.exe"),
		NodePath:     filepath.Join(dir, "node.exe"),
		CodexCLIPath: filepath.Join(dir, "codex.exe"),
	}
	for _, path := range []string{host.NodeReplPath, host.NodePath, host.CodexCLIPath} {
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MCPX_NODE_REPL_PATH", host.NodeReplPath)
	t.Setenv("MCPX_NODE_PATH", host.NodePath)
	t.Setenv("MCPX_CODEX_CLI_PATH", host.CodexCLIPath)

	got, err := FindNodeReplHost()
	if err != nil {
		t.Fatal(err)
	}
	if got != host {
		t.Fatalf("host = %+v, want %+v", got, host)
	}
}

func TestFindNodeReplHostUsesNewestValidRegistration(t *testing.T) {
	t.Setenv("MCPX_NODE_REPL_PATH", "")
	t.Setenv("MCPX_NODE_PATH", "")
	t.Setenv("MCPX_CODEX_CLI_PATH", "")
	dir := t.TempDir()
	makeHost := func(prefix string, stamp time.Time) NodeReplHost {
		host := NodeReplHost{
			NodeReplPath: filepath.Join(dir, prefix+"-node_repl.exe"),
			NodePath:     filepath.Join(dir, prefix+"-node.exe"),
			CodexCLIPath: filepath.Join(dir, prefix+"-codex.exe"),
		}
		for _, path := range []string{host.NodeReplPath, host.NodePath, host.CodexCLIPath} {
			if err := os.WriteFile(path, []byte(prefix), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, stamp, stamp); err != nil {
				t.Fatal(err)
			}
		}
		return host
	}
	oldHost := makeHost("old", time.Now().Add(-time.Hour))
	newHost := makeHost("new", time.Now())
	config := map[string]any{
		"registrations": []any{
			map[string]any{"runtime": map[string]any{"nodeReplPath": oldHost.NodeReplPath, "nodePath": oldHost.NodePath, "codexCliPath": oldHost.CodexCLIPath, "proxyToken": "must-not-matter"}},
			map[string]any{"runtime": map[string]any{"nodeReplPath": newHost.NodeReplPath, "nodePath": newHost.NodePath, "codexCliPath": newHost.CodexCLIPath}},
		},
	}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "chrome-native-hosts-v2.json")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCPX_CHROME_NATIVE_HOSTS_CONFIG", configPath)

	got, err := FindNodeReplHost()
	if err != nil {
		t.Fatal(err)
	}
	if got != newHost {
		t.Fatalf("host = %+v, want newest %+v", got, newHost)
	}
}
