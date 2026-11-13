package terminal

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"mcpx/internal/auth"
	"mcpx/internal/remotesession"
	"mcpx/internal/state"
)

// A live task's completion persists its result while holding task.mu. Listing
// must not hold the sole database connection while waiting for the same mutex.
func TestPersistentTaskListDoesNotBlockCompletionWrite(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	created, err := remotesession.NewService(store.DB()).Create(context.Background(),
		auth.Principal{ID: "list-principal", Kind: "test", SubjectHash: "list-subject"},
		remotesession.CreateInput{WorkspaceName: "project", WorkspacePath: t.TempDir(), Label: "list test"})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewPersistentTaskManager(store.DB(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Add(-time.Second)
	_, err = store.DB().Exec(`INSERT INTO terminal_tasks
		(id, remote_session_id, workspace_name, workspace_path, command, status, log_path, started_at, updated_at)
		VALUES (?, ?, 'project', '', 'synthetic task', 'running', '', ?, ?)`,
		"list-task", created.Session.ID, started.UnixMilli(), started.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	live := &Task{ID: "list-task", RemoteSessionID: created.Session.ID,
		WorkspaceName: "project", Command: "synthetic task", Status: TaskRunning, StartedAt: started}
	manager.tasks[live.ID] = live

	live.mu.Lock()
	locked := true
	type listResult struct {
		items []map[string]any
		err   error
	}
	done := make(chan listResult, 1)
	go func() {
		items, err := manager.List(created.Session.ID, 10)
		done <- listResult{items: items, err: err}
	}()
	defer func() {
		if locked {
			live.mu.Unlock()
		}
		// Drain the listing even on a failed write, before closing its database.
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("listing did not resume after releasing the task mutex")
		}
	}()

	// Synchronize on the actual wait rather than depending on a fixed sleep.
	// The stack is used only to arrange the interleaving; the assertion below
	// exercises a real write through the production single-connection store.
	deadline := time.Now().Add(5 * time.Second)
	for {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		waiting := false
		for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(stack, "terminal.(*Task).StatusView(") &&
				strings.Contains(stack, "terminal.(*TaskManager).List(") {
				waiting = true
				break
			}
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("listing did not reach the live task mutex")
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = store.DB().ExecContext(ctx,
		`UPDATE terminal_tasks SET status = 'exited', exit_code = 0 WHERE id = ?`, live.ID)
	if err != nil {
		t.Fatalf("completion write blocked while listing waited for the task mutex: %v", err)
	}
	finished := time.Now().UTC()
	code := 0
	live.Status, live.ExitCode, live.FinishedAt = TaskExited, &code, &finished
	live.mu.Unlock()
	locked = false

	select {
	case result := <-done:
		// Leave a value for the deferred cleanup after inspecting the result.
		done <- result
		if result.err != nil || len(result.items) != 1 {
			t.Fatalf("list = %+v, error = %v", result.items, result.err)
		}
		item := result.items[0]
		if item["execution_task_id"] != "list-task" || item["status"] != TaskExited || item["exit_code"] != 0 {
			t.Fatalf("list did not use completed live state: %+v", item)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listing did not complete")
	}
}
