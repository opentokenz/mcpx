package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"mcpx/internal/pythonruntime"
	"mcpx/internal/terminal"
)

// Execute runs skill entry via Runtime-controlled terminal bridge (not raw shell from skill code path).
func Execute(ctx context.Context, sk Skill, workDir string, arguments any) (map[string]any, error) {
	argsJSON, _ := json.Marshal(arguments)
	// Documentation / Agent skills: return markdown body, do not exec.
	if sk.Manifest.Runtime == "markdown" || sk.Manifest.Format == "skill_md" {
		path, err := ResolveEntry(sk)
		if err != nil {
			return nil, fmt.Errorf("invalid skill entry: %w", err)
		}
		body, err := os.ReadFile(path)
		if err != nil && (sk.Manifest.Entry == "" || sk.Manifest.Entry == "SKILL.md") {
			// try skill.md
			if alt, altErr := ResolveEntryName(sk, "skill.md"); altErr == nil {
				if altBody, altErr2 := os.ReadFile(alt); altErr2 == nil {
					path, body, err = alt, altBody, nil
				}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("read skill doc: %w", err)
		}
		return map[string]any{
			"name":              sk.Manifest.Name,
			"runtime":           "markdown",
			"description":       sk.Manifest.Description,
			"content":           string(body),
			"path":              path,
			"working_directory": workDir,
		}, nil
	}
	entry, err := SafeEntry(sk)
	if err != nil {
		return nil, fmt.Errorf("invalid skill entry: %w", err)
	}
	var executable string
	var args []string
	var command string
	switch strings.ToLower(strings.TrimSpace(sk.Manifest.Runtime)) {
	case "node", "js", "javascript":
		executable = "node"
		args = []string{entry}
		command = "node " + entry
	case "python", "python3", "":
		python, resolveErr := pythonruntime.Resolve()
		if resolveErr != nil {
			return nil, resolveErr
		}
		executable = python.Executable
		args = python.Args(entry)
		command = python.Command(entry)
	default:
		return nil, fmt.Errorf("unsupported runtime %q", sk.Manifest.Runtime)
	}
	// Skill executables run directly with the skill directory as cwd. Avoiding
	// `cd ... && runtime ...` keeps path quoting and shell semantics portable.
	res, err := terminal.Exec(ctx, terminal.ExecOptions{
		WorkDir:    sk.Dir,
		Command:    command,
		Executable: executable,
		Args:       args,
		Timeout:    120 * time.Second,
		ExtraEnv:   []string{"MCPX_SKILL_ARGS=" + string(argsJSON)},
	})
	out := map[string]any{
		"name":              sk.Manifest.Name,
		"command":           command,
		"working_directory": sk.Dir,
		"exit_code":         res.ExitCode,
		"stdout":            res.Stdout,
		"stderr":            res.Stderr,
		"duration_ms":       res.DurationMs,
	}
	if err != nil && res.ExitCode == -1 {
		return out, err
	}
	return out, nil
}
