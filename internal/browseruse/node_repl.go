package browseruse

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// NodeReplHost describes the privileged Node REPL runtime installed and
// registered by the official ChatGPT/Codex browser integration. Browser Service
// must run through this host so authenticated fetch, native pipes, config and
// elicitation remain owned by OpenAI's trusted runtime rather than MCPX shims.
type NodeReplHost struct {
	NodeReplPath string
	NodePath     string
	CodexCLIPath string
}

type nodeReplCandidate struct {
	Host    NodeReplHost
	Updated time.Time
}

func FindNodeReplHost() (NodeReplHost, error) {
	explicit := NodeReplHost{
		NodeReplPath: strings.TrimSpace(os.Getenv("MCPX_NODE_REPL_PATH")),
		NodePath:     strings.TrimSpace(os.Getenv("MCPX_NODE_PATH")),
		CodexCLIPath: strings.TrimSpace(os.Getenv("MCPX_CODEX_CLI_PATH")),
	}
	explicitCount := 0
	for _, value := range []string{explicit.NodeReplPath, explicit.NodePath, explicit.CodexCLIPath} {
		if value != "" {
			explicitCount++
		}
	}
	if explicitCount > 0 {
		if explicitCount != 3 {
			return NodeReplHost{}, errors.New("MCPX_NODE_REPL_PATH, MCPX_NODE_PATH and MCPX_CODEX_CLI_PATH must be set together")
		}
		if err := validateNodeReplHost(explicit); err != nil {
			return NodeReplHost{}, err
		}
		return explicit, nil
	}

	configPath := strings.TrimSpace(os.Getenv("MCPX_CHROME_NATIVE_HOSTS_CONFIG"))
	if configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return NodeReplHost{}, fmt.Errorf("locate OpenAI browser host config: %w", err)
		}
		configPath = filepath.Join(home, ".codex", "chrome-native-hosts-v2.json")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return NodeReplHost{}, fmt.Errorf("read OpenAI browser host config %s: %w", configPath, err)
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return NodeReplHost{}, fmt.Errorf("parse OpenAI browser host config %s: %w", configPath, err)
	}

	candidates := make([]nodeReplCandidate, 0, 4)
	seen := map[string]struct{}{}
	collectNodeReplCandidates(root, &candidates, seen)
	if len(candidates) == 0 {
		return NodeReplHost{}, errors.New("OpenAI privileged node_repl runtime was not found; start or update the official ChatGPT/Codex browser integration")
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].Updated.Equal(candidates[j].Updated) {
			return candidates[i].Updated.After(candidates[j].Updated)
		}
		return candidates[i].Host.NodeReplPath < candidates[j].Host.NodeReplPath
	})
	return candidates[0].Host, nil
}

func collectNodeReplCandidates(value any, out *[]nodeReplCandidate, seen map[string]struct{}) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			collectNodeReplCandidates(item, out, seen)
		}
	case map[string]any:
		host := NodeReplHost{
			NodeReplPath: stringValue(typed["nodeReplPath"]),
			NodePath:     stringValue(typed["nodePath"]),
			CodexCLIPath: stringValue(typed["codexCliPath"]),
		}
		if host.NodeReplPath != "" && host.NodePath != "" && host.CodexCLIPath != "" {
			if updated, ok := nodeReplHostUpdated(host); ok {
				key := filepath.Clean(host.NodeReplPath) + "\x00" + filepath.Clean(host.NodePath) + "\x00" + filepath.Clean(host.CodexCLIPath)
				if _, duplicate := seen[key]; !duplicate {
					seen[key] = struct{}{}
					*out = append(*out, nodeReplCandidate{Host: host, Updated: updated})
				}
			}
		}
		for _, child := range typed {
			collectNodeReplCandidates(child, out, seen)
		}
	}
}

func nodeReplHostUpdated(host NodeReplHost) (time.Time, bool) {
	var latest time.Time
	for _, path := range []string{host.NodeReplPath, host.NodePath, host.CodexCLIPath} {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			return time.Time{}, false
		}
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest, true
}

func validateNodeReplHost(host NodeReplHost) error {
	for name, path := range map[string]string{
		"MCPX_NODE_REPL_PATH": host.NodeReplPath,
		"MCPX_NODE_PATH":      host.NodePath,
		"MCPX_CODEX_CLI_PATH": host.CodexCLIPath,
	} {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			return fmt.Errorf("%s does not point to a file: %s", name, path)
		}
	}
	return nil
}
