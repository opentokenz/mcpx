package terminal

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// TaskStatus is long-running process state.
type TaskStatus string

const (
	TaskRunning     TaskStatus = "running"
	TaskExited      TaskStatus = "exited"
	TaskKilled      TaskStatus = "killed"
	TaskInterrupted TaskStatus = "interrupted"
	TaskFailed      TaskStatus = "failed"
)

// ProcessSpec describes a direct executable invocation. Executable and Args
// never pass through a shell. Stdin is a finite payload and reaches EOF when
// the reader is drained, which is suitable for ephemeral runtimes.
type ProcessSpec struct {
	Executable   string
	Args         []string
	Stdin        string
	WallLimit    time.Duration
	CPUTimeLimit time.Duration
}

// OutputChunk is a single stdout/stderr increment emitted after the task log
// writer has accepted the bytes. Offset is the absolute byte offset before
// this chunk within its stream.
type OutputChunk struct {
	TaskID          string
	RequestID       string
	CallID          string
	Tool            string
	RemoteSessionID string
	WorkspaceName   string
	Command         string
	WorkDir         string
	Stream          string
	Offset          int64
	Final           bool
	Data            []byte
}

// OutputSink observes task output. Implementations must be prepared for
// concurrent calls and should return quickly; the callback runs outside the
// task mutex but on the command's output-copy path.
type OutputSink func(OutputChunk)

// Task is a background command.
type Task struct {
	ID              string
	RequestID       string
	CallID          string
	Tool            string
	RemoteSessionID string
	WorkspaceName   string
	Command         string
	WorkDir         string
	Status          TaskStatus
	PID             int
	ExitCode        *int
	StartedAt       time.Time
	FinishedAt      *time.Time
	LimitReason     string

	mu            sync.Mutex
	logBuf        bytes.Buffer
	logBase       int
	logPath       string
	logFile       *os.File // combined log retained for the Resource endpoint
	stdoutLogPath string
	stdoutLogFile *os.File
	stderrLogPath string
	stderrLogFile *os.File
	logSize       int64
	stdoutLogSize int64
	stderrLogSize int64
	logTruncated  bool
	stdoutBuf     bytes.Buffer
	stderrBuf     bytes.Buffer
	stdoutBase    int
	stderrBase    int
	stdoutOffset  int64
	stderrOffset  int64
	stdin         io.WriteCloser
	done          chan struct{}
	db            *sql.DB
	cmd           *exec.Cmd
	cancel        context.CancelFunc
	outputSink    func(OutputChunk)
}

const maxTaskLogBytes = 1 << 20

// MaxPersistedTaskLogBytes bounds the durable task log and is also the
// default cumulative budget for task output written to observation_events.
const MaxPersistedTaskLogBytes = 10 << 20

// TaskManager tracks long tasks per process.
type TaskManager struct {
	mu         sync.Mutex
	tasks      map[string]*Task
	seq        uint64
	db         *sql.DB
	logDir     string
	sinkMu     sync.RWMutex
	outputSink OutputSink
}

// NewTaskManager creates an empty manager.
func NewTaskManager() *TaskManager {
	return &TaskManager{tasks: map[string]*Task{}}
}

// SetOutputSink replaces the non-blocking task output observer. Passing nil
// disables observation without changing task execution or log persistence.
func (m *TaskManager) SetOutputSink(sink OutputSink) {
	if m == nil {
		return
	}
	m.sinkMu.Lock()
	m.outputSink = sink
	m.sinkMu.Unlock()
}

func (m *TaskManager) emitOutput(chunk OutputChunk) {
	// Output observation is best-effort. A renderer/store regression must not
	// panic the os/exec copy goroutine and take down the MCP process.
	defer func() { _ = recover() }()
	m.sinkMu.RLock()
	sink := m.outputSink
	m.sinkMu.RUnlock()
	if sink != nil {
		sink(chunk)
	}
}

// NewPersistentTaskManager restores queryable task metadata and marks tasks
// left running by a previous MCPX process as interrupted.
func NewPersistentTaskManager(db *sql.DB, logDir string) (*TaskManager, error) {
	if db == nil || logDir == "" {
		return nil, fmt.Errorf("task database and log directory are required")
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, fmt.Errorf("create task log directory: %w", err)
	}
	if err := os.Chmod(logDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure task log directory: %w", err)
	}
	now := time.Now().UTC().UnixMilli()
	if _, err := db.Exec(`UPDATE terminal_tasks SET status = 'interrupted', finished_at = ?, updated_at = ? WHERE status = 'running'`, now, now); err != nil {
		return nil, fmt.Errorf("recover terminal tasks: %w", err)
	}
	return &TaskManager{tasks: map[string]*Task{}, db: db, logDir: logDir}, nil
}

// StartRemote launches a durable task owned by a Remote Session.
func (m *TaskManager) StartRemote(ctx context.Context, remoteSessionID, workspaceName, workDir, command string) (*Task, error) {
	return m.start(ctx, "", "", "", remoteSessionID, workspaceName, workDir, command)
}

// StartRemoteWithObservation launches a task and carries its originating MCP
// request through output callbacks without changing ordinary task callers.
func (m *TaskManager) StartRemoteWithObservation(ctx context.Context, requestID, tool, remoteSessionID, workspaceName, workDir, command string) (*Task, error) {
	return m.StartRemoteWithObservationContext(ctx, requestID, requestID, tool, remoteSessionID, workspaceName, workDir, command)
}

// StartRemoteWithObservationContext carries call correlation through task
// output events. Empty callID falls back to requestID at the observation boundary.
func (m *TaskManager) StartRemoteWithObservationContext(ctx context.Context, requestID, callID, tool, remoteSessionID, workspaceName, workDir, command string) (*Task, error) {
	return m.start(ctx, requestID, callID, tool, remoteSessionID, workspaceName, workDir, command)
}

func (m *TaskManager) start(_ context.Context, requestID, callID, tool, remoteSessionID, workspaceName, workDir, command string) (*Task, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := commandShell(ctx, command)
	cmd.Dir = workDir
	configureProcess(cmd)
	return m.startPrepared(requestID, callID, tool, remoteSessionID, workspaceName, workDir, command, cmd, cancel, true, "", 0, 0)
}

// StartRemoteProcessWithObservationContext launches an explicit executable and
// a finite stdin payload without shell parsing.
func (m *TaskManager) StartRemoteProcessWithObservationContext(requestID, callID, tool, remoteSessionID, workspaceName, workDir, displayCommand string, spec ProcessSpec) (*Task, error) {
	if strings.TrimSpace(spec.Executable) == "" {
		return nil, fmt.Errorf("process executable is required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, spec.Executable, spec.Args...)
	cmd.Dir = workDir
	configureProcess(cmd)
	return m.startPrepared(requestID, callID, tool, remoteSessionID, workspaceName, workDir, displayCommand, cmd, cancel, false, spec.Stdin, spec.WallLimit, spec.CPUTimeLimit)
}

func (m *TaskManager) startPrepared(requestID, callID, tool, remoteSessionID, workspaceName, workDir, command string, cmd *exec.Cmd, cancel context.CancelFunc, interactive bool, stdinContent string, wallLimit, cpuLimit time.Duration) (*Task, error) {
	m.mu.Lock()
	if len(m.tasks) >= 256 {
		m.pruneFinishedLocked(128)
		if len(m.tasks) >= 256 {
			m.mu.Unlock()
			cancel()
			return nil, fmt.Errorf("too many active tasks")
		}
	}
	m.seq++
	id := taskID(m.seq)
	m.mu.Unlock()

	t := &Task{
		ID: id, RequestID: requestID, CallID: callID, Tool: tool,
		RemoteSessionID: remoteSessionID, WorkspaceName: workspaceName, Command: command,
		WorkDir: workDir, Status: TaskRunning, StartedAt: time.Now().UTC(), db: m.db,
		cmd: cmd, cancel: cancel, done: make(chan struct{}), outputSink: m.emitOutput,
	}
	if m.db != nil {
		t.logPath = filepath.Join(m.logDir, id+".log")
		t.stdoutLogPath = filepath.Join(m.logDir, id+".stdout.log")
		t.stderrLogPath = filepath.Join(m.logDir, id+".stderr.log")
		var err error
		if t.logFile, err = os.OpenFile(t.logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err != nil {
			cancel()
			return nil, fmt.Errorf("create task log: %w", err)
		}
		if t.stdoutLogFile, err = os.OpenFile(t.stdoutLogPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err != nil {
			_ = t.logFile.Close()
			cancel()
			return nil, fmt.Errorf("create stdout log: %w", err)
		}
		if t.stderrLogFile, err = os.OpenFile(t.stderrLogPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err != nil {
			_ = t.stdoutLogFile.Close()
			_ = t.logFile.Close()
			cancel()
			return nil, fmt.Errorf("create stderr log: %w", err)
		}
	}
	var stdin io.WriteCloser
	if interactive {
		var err error
		stdin, err = cmd.StdinPipe()
		if err != nil {
			t.closeFiles()
			cancel()
			return nil, fmt.Errorf("create task stdin: %w", err)
		}
		t.stdin = stdin
	} else {
		cmd.Stdin = strings.NewReader(stdinContent)
	}
	cmd.Stdout = &lockedWriter{t: t, stream: "stdout"}
	cmd.Stderr = &lockedWriter{t: t, stream: "stderr"}

	if err := cmd.Start(); err != nil {
		t.closeFiles()
		if stdin != nil {
			_ = stdin.Close()
		}
		cancel()
		return nil, err
	}
	if cmd.Process != nil {
		t.PID = cmd.Process.Pid
	}
	if m.db != nil {
		_, err := m.db.Exec(`INSERT INTO terminal_tasks
            (id, remote_session_id, workspace_name, workspace_path, command, status, pid, log_path, started_at, updated_at)
            VALUES (?, ?, ?, ?, ?, 'running', ?, ?, ?, ?)`, t.ID, t.RemoteSessionID, t.WorkspaceName,
			t.WorkDir, t.Command, t.PID, t.logPath, t.StartedAt.UnixMilli(), t.StartedAt.UnixMilli())
		if err != nil {
			killProcessTree(cmd)
			_, _ = cmd.Process.Wait()
			if t.stdin != nil {
				_ = t.stdin.Close()
			}
			t.closeFiles()
			cancel()
			return nil, fmt.Errorf("persist task: %w", err)
		}
	}
	m.mu.Lock()
	m.tasks[id] = t
	m.mu.Unlock()

	if wallLimit > 0 {
		go t.enforceWallLimit(wallLimit)
	}
	if cpuLimit > 0 {
		go t.enforceCPUTimeLimit(cpuLimit)
	}
	go func() {
		err := cmd.Wait()
		t.mu.Lock()
		if t.Status == TaskKilled {
			t.finishLocked(TaskKilled, -1)
			t.mu.Unlock()
			t.emitOutputFinal()
			close(t.done)
			return
		}
		t.Status = TaskExited
		code := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		t.ExitCode = &code
		t.finishLocked(TaskExited, code)
		t.mu.Unlock()
		t.emitOutputFinal()
		close(t.done)
		cancel()
	}()
	return t, nil
}

type lockedWriter struct {
	t      *Task
	stream string
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.t.mu.Lock()
	n := len(p)
	if err := writeBounded(w.t.logFile, &w.t.logSize, p, &w.t.logTruncated); err != nil {
		w.t.mu.Unlock()
		return n, err
	}
	var (
		streamFile *os.File
		streamSize *int64
		streamBuf  *bytes.Buffer
		streamBase *int
	)
	if w.stream == "stderr" {
		streamFile, streamSize, streamBuf, streamBase = w.t.stderrLogFile, &w.t.stderrLogSize, &w.t.stderrBuf, &w.t.stderrBase
	} else {
		streamFile, streamSize, streamBuf, streamBase = w.t.stdoutLogFile, &w.t.stdoutLogSize, &w.t.stdoutBuf, &w.t.stdoutBase
	}
	offset := w.t.stdoutOffset
	if w.stream == "stderr" {
		offset = w.t.stderrOffset
	}
	if err := writeBounded(streamFile, streamSize, p, &w.t.logTruncated); err != nil {
		w.t.mu.Unlock()
		return n, err
	}
	if w.stream == "stderr" {
		w.t.stderrOffset += int64(n)
	} else {
		w.t.stdoutOffset += int64(n)
	}
	_, _ = streamBuf.Write(p)
	if overflow := streamBuf.Len() - maxTaskLogBytes; overflow > 0 {
		streamBuf.Next(overflow)
		*streamBase += overflow
	}
	_, _ = w.t.logBuf.Write(p)
	if overflow := w.t.logBuf.Len() - maxTaskLogBytes; overflow > 0 {
		w.t.logBuf.Next(overflow)
		w.t.logBase += overflow
	}
	sink := w.t.outputSink
	chunk := OutputChunk{
		TaskID: w.t.ID, RequestID: w.t.RequestID, CallID: w.t.CallID, Tool: w.t.Tool,
		RemoteSessionID: w.t.RemoteSessionID, WorkspaceName: w.t.WorkspaceName,
		Command: w.t.Command, WorkDir: w.t.WorkDir, Stream: w.stream, Offset: offset, Data: append([]byte(nil), p...),
	}
	w.t.mu.Unlock()
	if sink != nil {
		sink(chunk)
	}
	return n, nil
}

func (t *Task) emitOutputFinal() {
	t.mu.Lock()
	sink := t.outputSink
	chunks := []OutputChunk{
		{TaskID: t.ID, RequestID: t.RequestID, CallID: t.CallID, Tool: t.Tool, RemoteSessionID: t.RemoteSessionID, WorkspaceName: t.WorkspaceName, Command: t.Command, WorkDir: t.WorkDir, Stream: "stdout", Offset: t.stdoutOffset, Final: true},
		{TaskID: t.ID, RequestID: t.RequestID, CallID: t.CallID, Tool: t.Tool, RemoteSessionID: t.RemoteSessionID, WorkspaceName: t.WorkspaceName, Command: t.Command, WorkDir: t.WorkDir, Stream: "stderr", Offset: t.stderrOffset, Final: true},
	}
	t.mu.Unlock()
	if sink == nil {
		return
	}
	for _, chunk := range chunks {
		sink(chunk)
	}
}

func writeBounded(file *os.File, size *int64, content []byte, truncated *bool) error {
	if file == nil || *size >= MaxPersistedTaskLogBytes {
		if file != nil && len(content) > 0 {
			*truncated = true
		}
		return nil
	}
	remaining := MaxPersistedTaskLogBytes - *size
	write := content
	if int64(len(write)) > remaining {
		write = write[:remaining]
		*truncated = true
	}
	written, err := file.Write(write)
	*size += int64(written)
	if len(write) < len(content) {
		*truncated = true
	}
	return err
}

// Get returns task if the Remote Session owns it.
func (m *TaskManager) Get(remoteSessionID, taskID string) (*Task, error) {
	m.mu.Lock()
	t, ok := m.tasks[taskID]
	m.mu.Unlock()
	if !ok && m.db != nil {
		var t Task
		var exitCode, finishedAt sql.NullInt64
		var startedAt int64
		var truncated int
		err := m.db.QueryRow(`SELECT id, remote_session_id, workspace_name, workspace_path, command,
            status, pid, exit_code, log_path, log_size, log_truncated, started_at, finished_at, limit_reason
            FROM terminal_tasks WHERE id = ? AND remote_session_id = ?`, taskID, remoteSessionID).Scan(
			&t.ID, &t.RemoteSessionID, &t.WorkspaceName, &t.WorkDir, &t.Command, &t.Status, &t.PID,
			&exitCode, &t.logPath, &t.logSize, &truncated, &startedAt, &finishedAt, &t.LimitReason)
		if err != nil {
			return nil, fmt.Errorf("task not found")
		}
		t.StartedAt = time.UnixMilli(startedAt).UTC()
		t.logTruncated = truncated != 0
		if t.logPath != "" {
			t.stdoutLogPath = strings.TrimSuffix(t.logPath, ".log") + ".stdout.log"
			t.stderrLogPath = strings.TrimSuffix(t.logPath, ".log") + ".stderr.log"
		}
		t.db = m.db
		if exitCode.Valid {
			code := int(exitCode.Int64)
			t.ExitCode = &code
		}
		if finishedAt.Valid {
			finished := time.UnixMilli(finishedAt.Int64).UTC()
			t.FinishedAt = &finished
		}
		return &t, nil
	}
	if !ok {
		return nil, fmt.Errorf("task not found")
	}
	if t.RemoteSessionID != remoteSessionID {
		return nil, fmt.Errorf("task belongs to another Remote Session")
	}
	return t, nil
}

// StatusView is JSON-serializable status.
func (t *Task) StatusView() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]any{
		"execution_task_id": t.ID,
		"status":            t.Status,
		"pid":               t.PID,
		"runtime_ms":        time.Since(t.StartedAt).Milliseconds(),
		"workspace":         t.WorkspaceName,
		"command":           t.Command,
		"log_truncated":     t.logTruncated,
	}
	if t.FinishedAt != nil {
		out["finished_at"] = *t.FinishedAt
		out["runtime_ms"] = t.FinishedAt.Sub(t.StartedAt).Milliseconds()
	}
	if t.ExitCode != nil {
		out["exit_code"] = *t.ExitCode
	}
	if t.LimitReason != "" {
		out["limit_reason"] = t.LimitReason
	}
	return out
}

// Logs returns the combined incremental log retained for backward-compatible
// Resource reads. New task_manage callers should use LogsFor for independent
// stdout and stderr offsets.
func (t *Task) Logs(offset int) (chunk string, next int) {
	return t.logsFor("combined", offset, 256<<10)
}

func (t *Task) LogsFor(stream string, offset int) (chunk string, next int) {
	return t.LogsForLimit(stream, offset, 256<<10)
}

// LogsForLimit reads one byte-offset window without changing the durable log.
func (t *Task) LogsForLimit(stream string, offset, limit int) (chunk string, next int) {
	if limit <= 0 || limit > 256<<10 {
		limit = 256 << 10
	}
	switch stream {
	case "stdout", "stderr":
		return t.logsFor(stream, offset, limit)
	default:
		return t.logsFor("combined", offset, limit)
	}
}

func (t *Task) logsFor(stream string, offset, limit int) (chunk string, next int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	path := t.logPath
	buffer := &t.logBuf
	base := t.logBase
	if stream == "stdout" {
		path, buffer, base = t.stdoutLogPath, &t.stdoutBuf, t.stdoutBase
	} else if stream == "stderr" {
		path, buffer, base = t.stderrLogPath, &t.stderrBuf, t.stderrBase
	}
	if path != "" {
		file, err := os.Open(path)
		if err != nil {
			return "", offset
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return "", offset
		}
		size := info.Size()
		if offset < 0 {
			offset = 0
		}
		if int64(offset) > size {
			offset = int(size)
		}
		remaining := size - int64(offset)
		if remaining > int64(limit) {
			remaining = int64(limit)
		}
		data := make([]byte, int(remaining))
		read, _ := file.ReadAt(data, int64(offset))
		if read == 0 && remaining != 0 {
			return "", offset
		}
		return string(data[:read]), offset + read
	}
	data := buffer.Bytes()
	if offset < base {
		offset = base
	}
	if offset > base+len(data) {
		offset = base + len(data)
	}
	end := offset - base + limit
	if end > len(data) {
		end = len(data)
	}
	return string(data[offset-base : end]), base + end
}

func (t *Task) LogSize() int64 {
	return t.LogStreamSize("combined")
}

// LogStreamSize returns the absolute byte size for a requested output stream.
func (t *Task) LogStreamSize(stream string) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	path := t.logPath
	buffer := &t.logBuf
	base := t.logBase
	if stream == "stdout" {
		path, buffer, base = t.stdoutLogPath, &t.stdoutBuf, t.stdoutBase
	} else if stream == "stderr" {
		path, buffer, base = t.stderrLogPath, &t.stderrBuf, t.stderrBase
	}
	if path != "" {
		if info, err := os.Stat(path); err == nil {
			return info.Size()
		}
	}
	return int64(base + buffer.Len())
}

func (t *Task) ReadAllLogs(maxBytes int64) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if maxBytes <= 0 {
		maxBytes = 8 << 20
	}
	if t.logPath != "" {
		info, err := os.Stat(t.logPath)
		if err != nil {
			return nil, err
		}
		if info.Size() > maxBytes {
			return nil, fmt.Errorf("task log exceeds resource limit; use terminal_logs pagination")
		}
		return os.ReadFile(t.logPath)
	}
	if int64(t.logBuf.Len()) > maxBytes {
		return nil, fmt.Errorf("task log exceeds resource limit; use terminal_logs pagination")
	}
	return append([]byte(nil), t.logBuf.Bytes()...), nil
}

func (m *TaskManager) pruneFinishedLocked(target int) {
	for id, task := range m.tasks {
		if len(m.tasks) <= target {
			return
		}
		task.mu.Lock()
		finished := task.Status != TaskRunning
		task.mu.Unlock()
		if finished {
			delete(m.tasks, id)
		}
	}
}

// Kill stops the task.
func (t *Task) Kill() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Status != TaskRunning {
		return nil
	}
	t.Status = TaskKilled
	if t.cancel != nil {
		t.cancel()
	}
	killProcessTree(t.cmd)
	code := -1
	t.ExitCode = &code
	t.finishLocked(TaskKilled, code)
	return nil
}

func (t *Task) enforceWallLimit(limit time.Duration) {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-t.done:
		return
	case <-timer.C:
		t.terminateForLimit("wall_time_limit")
	}
}

func (t *Task) enforceCPUTimeLimit(limit time.Duration) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
			t.mu.Lock()
			pid, running := t.PID, t.Status == TaskRunning
			t.mu.Unlock()
			if !running {
				return
			}
			if used, ok := processCPUTime(pid); ok && used >= limit {
				t.terminateForLimit("cpu_time_limit")
				return
			}
		}
	}
}

func (t *Task) terminateForLimit(reason string) {
	t.mu.Lock()
	if t.Status != TaskRunning {
		t.mu.Unlock()
		return
	}
	t.Status = TaskKilled
	t.LimitReason = reason
	cmd, cancel := t.cmd, t.cancel
	t.mu.Unlock()
	killProcessTree(cmd)
	if cancel != nil {
		cancel()
	}
}

// List returns newest durable tasks for a Remote Session.
func (m *TaskManager) List(remoteSessionID string, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if m.db == nil {
		m.mu.Lock()
		items := make([]*Task, 0, len(m.tasks))
		for _, task := range m.tasks {
			if task.RemoteSessionID == remoteSessionID {
				items = append(items, task)
			}
		}
		m.mu.Unlock()
		sort.Slice(items, func(i, j int) bool { return items[i].StartedAt.After(items[j].StartedAt) })
		if len(items) > limit {
			items = items[:limit]
		}
		result := make([]map[string]any, 0, len(items))
		for _, task := range items {
			result = append(result, task.StatusView())
		}
		return result, nil
	}

	rows, err := m.db.Query(`SELECT id, workspace_name, command, status, pid,
        exit_code, log_truncated, started_at, finished_at
        FROM terminal_tasks WHERE remote_session_id = ? ORDER BY started_at DESC, id DESC LIMIT ?`, remoteSessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]map[string]any, 0, limit)
	for rows.Next() {
		var id, workspaceName, command, status string
		var pid, logTruncated int
		var exitCode, startedAt, finishedAt sql.NullInt64
		if err := rows.Scan(&id, &workspaceName, &command, &status, &pid,
			&exitCode, &logTruncated, &startedAt, &finishedAt); err != nil {
			return nil, err
		}
		m.mu.Lock()
		active := m.tasks[id]
		m.mu.Unlock()
		if active != nil {
			items = append(items, active.StatusView())
			continue
		}
		started := time.UnixMilli(startedAt.Int64).UTC()
		view := map[string]any{
			"execution_task_id": id, "status": TaskStatus(status), "pid": pid,
			"workspace": workspaceName, "command": command, "log_truncated": logTruncated != 0,
			"runtime_ms": time.Since(started).Milliseconds(),
		}
		if finishedAt.Valid {
			finished := time.UnixMilli(finishedAt.Int64).UTC()
			view["finished_at"] = finished
			view["runtime_ms"] = finished.Sub(started).Milliseconds()
		}
		if exitCode.Valid {
			view["exit_code"] = int(exitCode.Int64)
		}
		items = append(items, view)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// ActiveIDs returns every currently running Task without completed history.
func (m *TaskManager) ActiveIDs(remoteSessionID string) ([]string, error) {
	if m.db == nil {
		m.mu.Lock()
		tasks := make([]*Task, 0, len(m.tasks))
		for _, task := range m.tasks {
			if task.RemoteSessionID == remoteSessionID {
				tasks = append(tasks, task)
			}
		}
		m.mu.Unlock()
		ids := make([]string, 0, len(tasks))
		for _, task := range tasks {
			task.mu.Lock()
			if task.Status == TaskRunning {
				ids = append(ids, task.ID)
			}
			task.mu.Unlock()
		}
		sort.Strings(ids)
		return ids, nil
	}
	rows, err := m.db.Query(`SELECT id FROM terminal_tasks WHERE remote_session_id = ? AND status = 'running' ORDER BY id`, remoteSessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (t *Task) finishLocked(status TaskStatus, code int) {
	now := time.Now().UTC()
	t.Status = status
	t.ExitCode = &code
	t.FinishedAt = &now
	if t.stdin != nil {
		_ = t.stdin.Close()
		t.stdin = nil
	}
	t.closeFiles()
	if t.db != nil {
		_, _ = t.db.Exec(`UPDATE terminal_tasks SET status = ?, exit_code = ?, log_size = ?, log_truncated = ?, limit_reason = ?, finished_at = ?, updated_at = ? WHERE id = ?`,
			status, code, t.logSize, boolInt(t.logTruncated), t.LimitReason, now.UnixMilli(), now.UnixMilli(), t.ID)
	}
}

func (t *Task) closeFiles() {
	for _, file := range []*os.File{t.logFile, t.stdoutLogFile, t.stderrLogFile} {
		if file != nil {
			_ = file.Sync()
			_ = file.Close()
		}
	}
	t.logFile = nil
	t.stdoutLogFile = nil
	t.stderrLogFile = nil
}

// Wait waits for a live task to exit. Restored tasks have no process and are
// already terminal or marked interrupted, so they never report a false live wait.
func (t *Task) Wait(ctx context.Context) bool {
	if t.done == nil {
		return false
	}
	select {
	case <-t.done:
		return true
	case <-ctx.Done():
		return false
	}
}

// WriteStdin safely forwards interactive input to a live Task. stdin is not
// persisted and is unavailable after a server restart.
func (t *Task) WriteStdin(content string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Status != TaskRunning || t.stdin == nil {
		return fmt.Errorf("task stdin is unavailable")
	}
	_, err := io.WriteString(t.stdin, content)
	return err
}

func taskID(sequence uint64) string {
	var random [8]byte
	if _, err := rand.Read(random[:]); err == nil {
		return "task_" + hex.EncodeToString(random[:])
	}
	return fmt.Sprintf("task_%d_%d", sequence, time.Now().UnixNano())
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// Close stops live tasks before the database is closed.
func (m *TaskManager) Close() {
	m.mu.Lock()
	tasks := make([]*Task, 0, len(m.tasks))
	for _, task := range m.tasks {
		tasks = append(tasks, task)
	}
	m.mu.Unlock()
	for _, task := range tasks {
		_ = task.Kill()
	}
}

var _ io.Writer = (*lockedWriter)(nil)
