package main

import (
	"path/filepath"
	"runtime/debug"
	"testing"

	buildversion "mcpx/internal/version"
)

func TestResolveBuildProvenancePrefersLinkerValues(t *testing.T) {
	got := resolveBuildProvenance("1.2.3", "release-commit", "2026-08-09T00:00:00Z", []debug.BuildSetting{
		{Key: "vcs.revision", Value: "fallback-commit"},
		{Key: "vcs.modified", Value: "true"},
	})
	if got.Version != "1.2.3" || got.Commit != "release-commit" || got.Date != "2026-08-09T00:00:00Z" {
		t.Fatalf("linker provenance must win: %+v", got)
	}
}

func TestResolveBuildProvenanceFallsBackToVCSRevision(t *testing.T) {
	got := resolveBuildProvenance("", "none", "unknown", []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef"},
		{Key: "vcs.modified", Value: "true"},
	})
	if got.Version != buildversion.Current || got.Commit != "0123456789abcdef-dirty" || got.Date != "unknown" {
		t.Fatalf("unexpected VCS fallback provenance: %+v", got)
	}
}

func TestBackgroundChildSubcommandIsInternal(t *testing.T) {
	if backgroundChildSubcommand != "__background-child" {
		t.Fatalf("unexpected internal background child subcommand %q", backgroundChildSubcommand)
	}
}

func TestBackgroundChildArgsRemovesDaemonFlag(t *testing.T) {
	got := backgroundChildArgs([]string{"-addr", "127.0.0.1:9999", "-d", "-log-level", "debug"})
	want := []string{"-addr", "127.0.0.1:9999", "-log-level", "debug"}
	if len(got) != len(want) {
		t.Fatalf("unexpected args length: got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected arg at %d: got=%q want=%q", i, got[i], want[i])
		}
	}
}

func TestDaemonStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), daemonStateFilename)
	want := daemonState{PID: 4321, Executable: "/opt/mcpx/bin/mcpx"}
	if err := writeDaemonState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := readDaemonState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("daemon state mismatch: got=%+v want=%+v", got, want)
	}
}

func TestStopPreviousBackgroundRemovesInvalidState(t *testing.T) {
	path := filepath.Join(t.TempDir(), daemonStateFilename)
	if err := writeDaemonState(path, daemonState{}); err != nil {
		t.Fatal(err)
	}
	stoppedPIDs, err := stopPreviousBackground(path, "/definitely/not/a/running/mcpx")
	if err != nil {
		t.Fatal(err)
	}
	if len(stoppedPIDs) != 0 {
		t.Fatalf("invalid state must not report stopped daemon pids: %v", stoppedPIDs)
	}
	if _, err := readDaemonState(path); err == nil {
		t.Fatal("daemon state should be removed")
	}
}

// runStop 对"本来就没有后台服务"必须是幂等的：托盘的停止按钮会被重复点，
// 每次都报错会让用户以为出了问题。
func TestRunStopIsIdempotentWithoutDaemon(t *testing.T) {
	t.Setenv("MCPX_HOME", t.TempDir())
	if code := runStop(); code != 0 {
		t.Fatalf("runStop without daemon = %d, want 0", code)
	}
	if code := runStop(); code != 0 {
		t.Fatalf("repeated runStop = %d, want 0", code)
	}
}

func TestBackgroundStopMessageShowsStoppedDaemons(t *testing.T) {
	got := backgroundStopMessage([]int{35421, 35422})
	want := "mcpx stopped previous background daemon (pid=35421)\n" +
		"mcpx stopped previous background daemon (pid=35422)\n"
	if got != want {
		t.Fatalf("background stop message=%q, want %q", got, want)
	}
	if got := backgroundStopMessage(nil); got != "" {
		t.Fatalf("empty background stop message=%q", got)
	}
}

func TestBackgroundStartMessageShowsStoppedDaemonBeforeNewDaemon(t *testing.T) {
	got := backgroundStartMessage(35600, "/tmp/mcpx-daemon.log", []int{35421, 35422})
	want := "mcpx stopped previous background daemon (pid=35421)\n" +
		"mcpx stopped previous background daemon (pid=35422)\n" +
		"mcpx started in background (pid=35600, log=/tmp/mcpx-daemon.log)\n"
	if got != want {
		t.Fatalf("background start message=%q, want %q", got, want)
	}
}

func TestBackgroundStartMessageWithoutPreviousDaemonOnlyShowsStart(t *testing.T) {
	got := backgroundStartMessage(35600, "/tmp/mcpx-daemon.log", nil)
	want := "mcpx started in background (pid=35600, log=/tmp/mcpx-daemon.log)\n"
	if got != want {
		t.Fatalf("background start message=%q, want %q", got, want)
	}
}
