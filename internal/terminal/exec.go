package terminal

import (
	"bytes"
	"context"
	"os/exec"
	"time"
)

// ExecOptions configures a short command run. When Executable is non-empty,
// Exec bypasses the shell and launches Executable with Args directly.
type ExecOptions struct {
	WorkDir    string
	Command    string
	Executable string
	Args       []string
	Timeout    time.Duration
	ExtraEnv   []string // KEY=VAL
}

// Result is the outcome of Exec.
type Result struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMs int64  `json:"duration_ms"`
}

// Exec runs a command in WorkDir with optional timeout. Shell execution is
// retained for legacy callers; structured callers should set Executable/Args.
func Exec(ctx context.Context, opts ExecOptions) (Result, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	var cmd *exec.Cmd
	if opts.Executable != "" {
		cmd = exec.CommandContext(ctx, opts.Executable, opts.Args...)
	} else {
		cmd = commandShell(ctx, opts.Command)
	}
	configureProcess(cmd)
	cmd.Dir = opts.WorkDir
	if len(opts.ExtraEnv) > 0 {
		cmd.Env = append(cmd.Environ(), opts.ExtraEnv...)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	dur := time.Since(start).Milliseconds()

	res := Result{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		DurationMs: dur,
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		if ctx.Err() != nil {
			res.ExitCode = -1
			res.Stderr = res.Stderr + "\n" + ctx.Err().Error()
			return res, ctx.Err()
		}
		return res, err
	}
	res.ExitCode = 0
	return res, nil
}
