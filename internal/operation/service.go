package operation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultWorkers              = 4
	MaxWorkers                  = 32
	MaxSteps                    = 32
	MaxBatchQueries             = 32
	DefaultResultBytes          = 64 << 10
	MaxResultBytes              = 256 << 10
	OperationRetention          = 24 * time.Hour
	operationEventStarted       = "operation.started"
	operationEventStepStarted   = "operation.step.started"
	operationEventStepCompleted = "operation.step.completed"
	operationEventCompleted     = "operation.completed"
)

var (
	ErrNotFound         = errors.New("operation not found")
	ErrInvalidSpec      = errors.New("invalid operation specification")
	ErrAlreadyCompleted = errors.New("operation already completed")
	ErrNotActive        = errors.New("operation is not active")
	ErrConfirmation     = errors.New("confirmation token does not match")
	ErrResultExpired    = errors.New("operation result expired; submission must not be replayed")
)

type Service struct {
	db             *sql.DB
	now            func() time.Time
	mu             sync.Mutex
	reconcileMu    sync.Mutex
	active         map[string]*activeOperation
	jobs           chan stepJob
	pending        []stepJob
	wake           chan struct{}
	workspaceLocks map[string]*sync.RWMutex
	sink           EventSink
	stop           chan struct{}
	closed         chan struct{}
	closeOnce      sync.Once
	closeErr       error
	wg             sync.WaitGroup
}

type activeOperation struct {
	ctx             context.Context
	cancel          context.CancelFunc
	executor        Executor
	specs           map[string]StepSpec
	enqueued        map[string]bool
	stepCancel      map[string]context.CancelFunc
	cancelRequested bool
	done            chan struct{}
}

type stepJob struct {
	operationID string
	stepID      string
}

// New creates an operation service and marks work left by an earlier process
// as interrupted. Waiting-for-confirmation operations remain resumable.
func New(db *sql.DB, workers int, sink EventSink) (*Service, error) {
	if db == nil {
		return nil, errors.New("operation database is required")
	}
	if workers <= 0 {
		workers = DefaultWorkers
	}
	if workers > MaxWorkers {
		workers = MaxWorkers
	}
	now := time.Now().UTC()
	if err := recoverInterrupted(db, now.UnixMilli()); err != nil {
		return nil, err
	}
	s := &Service{
		db:             db,
		now:            time.Now,
		active:         make(map[string]*activeOperation),
		jobs:           make(chan stepJob, workers*2),
		wake:           make(chan struct{}, 1),
		workspaceLocks: make(map[string]*sync.RWMutex),
		sink:           sink,
		stop:           make(chan struct{}),
		closed:         make(chan struct{}),
	}
	s.wg.Add(1)
	go s.dispatch()
	for i := 0; i < workers; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	return s, nil
}

// Submit persists a complete operation before any step is scheduled.
func (s *Service) Submit(ctx context.Context, spec SubmitSpec, executor Executor) (Record, error) {
	if s == nil || s.db == nil {
		return Record{}, errors.New("operation service is unavailable")
	}
	if executor == nil {
		return Record{}, errors.New("operation executor is required")
	}
	if err := validateSpec(spec); err != nil {
		return Record{}, err
	}
	if spec.ID == "" {
		spec.ID = newID("op_")
	}
	if spec.RunID == "" {
		spec.RunID = spec.ID
	}
	if strings.TrimSpace(spec.RunID) != spec.RunID || strings.TrimSpace(spec.ID) != spec.ID || len(spec.RunID) > 200 || len(spec.ID) > 200 {
		return Record{}, fmt.Errorf("%w: invalid run or operation ID", ErrInvalidSpec)
	}
	canonical := spec
	canonical.RequestID = "" // 新传输请求可恢复同一逻辑操作。
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return Record{}, fmt.Errorf("%w: cannot encode operation", ErrInvalidSpec)
	}
	digest := sha256.Sum256(encoded)
	submissionHash := hex.EncodeToString(digest[:])
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, fmt.Errorf("begin operation: %w", err)
	}
	defer tx.Rollback()
	var existingHash string
	err = tx.QueryRowContext(ctx, `SELECT submission_sha256 FROM operation_submissions WHERE operation_id=?`, spec.ID).Scan(&existingHash)
	if err == nil {
		if existingHash != submissionHash {
			return Record{}, fmt.Errorf("%w: operation ID already belongs to another submission", ErrInvalidSpec)
		}
		if err := tx.Rollback(); err != nil {
			return Record{}, err
		}
		record, err := s.Get(ctx, spec.ID)
		if errors.Is(err, ErrNotFound) {
			return Record{}, ErrResultExpired
		}
		return record, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Record{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO operation_submissions(operation_id,remote_session_id,submission_sha256) VALUES(?,?,?)`,
		spec.ID, spec.RemoteSessionID, submissionHash); err != nil {
		return Record{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO operations
		(id, remote_session_id, workspace_name, request_id, purpose, state, result_json, error_json, created_at, expires_at, run_id, submission_sha256)
		VALUES (?, ?, ?, ?, ?, 'queued', '{}', '{}', ?, ?, ?, ?)`,
		spec.ID, spec.RemoteSessionID, spec.WorkspaceName, spec.RequestID, spec.Purpose,
		now.UnixMilli(), now.Add(OperationRetention).UnixMilli(), spec.RunID, submissionHash); err != nil {
		return Record{}, fmt.Errorf("persist operation: %w", err)
	}
	for _, step := range spec.Steps {
		arguments, marshalErr := json.Marshal(argumentsOrEmpty(step.Arguments))
		if marshalErr != nil {
			return Record{}, fmt.Errorf("encode step %s arguments: %w", step.ID, marshalErr)
		}
		dependsOn, marshalErr := json.Marshal(step.DependsOn)
		if marshalErr != nil {
			return Record{}, fmt.Errorf("encode step %s dependencies: %w", step.ID, marshalErr)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO operation_steps
			(operation_id, step_id, tool_name, arguments_json, depends_on_json, exclusive, state, request_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?, 'queued', ?, ?)`,
			spec.ID, step.ID, step.Tool, string(arguments), string(dependsOn), boolInt(step.Exclusive), newID("req_"), now.UnixMilli()); err != nil {
			return Record{}, fmt.Errorf("persist operation step %s: %w", step.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Record{}, fmt.Errorf("commit operation: %w", err)
	}

	activeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	active := &activeOperation{
		ctx: activeCtx, cancel: cancel, executor: executor,
		specs:      make(map[string]StepSpec, len(spec.Steps)),
		enqueued:   make(map[string]bool, len(spec.Steps)),
		stepCancel: make(map[string]context.CancelFunc), done: make(chan struct{}),
	}
	for _, step := range spec.Steps {
		active.specs[step.ID] = step
	}
	s.mu.Lock()
	s.active[spec.ID] = active
	s.mu.Unlock()
	s.emit(Event{OperationID: spec.ID, RemoteSessionID: spec.RemoteSessionID, WorkspaceName: spec.WorkspaceName, RequestID: spec.RequestID, Type: operationEventStarted, State: StateQueued, Summary: "operation queued", CreatedAt: now})
	s.enqueueReady(spec.ID)
	return s.Get(ctx, spec.ID)
}

// Get reads the operation and all steps from SQLite.
func (s *Service) Get(ctx context.Context, operationID string) (Record, error) {
	if strings.TrimSpace(operationID) == "" {
		return Record{}, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Record{}, err
	}
	defer tx.Rollback()
	var record Record
	var createdAt, startedAt, completedAt sql.NullInt64
	var result, operationError string
	err = tx.QueryRowContext(ctx, `SELECT id, remote_session_id, workspace_name, request_id, purpose, state,
		result_json, error_json, created_at, started_at, completed_at, state_sequence, state_event_id, run_id FROM operations WHERE id = ?`, operationID).Scan(
		&record.ID, &record.RemoteSessionID, &record.WorkspaceName, &record.RequestID, &record.Purpose, &record.State,
		&result, &operationError, &createdAt, &startedAt, &completedAt, &record.StateSequence, &record.StateEventID, &record.RunID)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	record.Result = json.RawMessage(result)
	record.Error = json.RawMessage(operationError)
	record.CreatedAt = time.UnixMilli(createdAt.Int64).UTC()
	if startedAt.Valid {
		value := time.UnixMilli(startedAt.Int64).UTC()
		record.StartedAt = &value
	}
	if completedAt.Valid {
		value := time.UnixMilli(completedAt.Int64).UTC()
		record.CompletedAt = &value
	}
	rows, err := tx.QueryContext(ctx, `SELECT step_id, tool_name, arguments_json, depends_on_json, exclusive, state, request_id,
		result_json, error_json, confirmation_token, created_at, started_at, completed_at
		FROM operation_steps WHERE operation_id = ? ORDER BY step_id`, operationID)
	if err != nil {
		return Record{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var step StepRecord
		var arguments, dependsOn, stepResult, stepError string
		var stepCreated, stepStarted, stepCompleted sql.NullInt64
		var exclusive int
		if err := rows.Scan(&step.ID, &step.Tool, &arguments, &dependsOn, &exclusive, &step.State, &step.RequestID, &stepResult, &stepError, &step.ConfirmationToken, &stepCreated, &stepStarted, &stepCompleted); err != nil {
			return Record{}, err
		}
		_ = json.Unmarshal([]byte(arguments), &step.Arguments)
		_ = json.Unmarshal([]byte(dependsOn), &step.DependsOn)
		step.Exclusive = exclusive != 0
		step.Result = json.RawMessage(stepResult)
		step.Error = json.RawMessage(stepError)
		step.CreatedAt = time.UnixMilli(stepCreated.Int64).UTC()
		if stepStarted.Valid {
			value := time.UnixMilli(stepStarted.Int64).UTC()
			step.StartedAt = &value
		}
		if stepCompleted.Valid {
			value := time.UnixMilli(stepCompleted.Int64).UTC()
			step.CompletedAt = &value
		}
		record.Steps = append(record.Steps, step)
	}
	if err := rows.Err(); err != nil {
		return Record{}, err
	}
	if err := rows.Close(); err != nil {
		return Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, err
	}
	return record, nil
}

// Wait waits for a terminal operation state. The bool reports timeout.
func (s *Service) Wait(ctx context.Context, operationID string, timeout time.Duration) (Record, bool, error) {
	record, err := s.Get(ctx, operationID)
	if err != nil {
		return Record{}, false, err
	}
	if record.State.terminal() || record.State == StateWaitingConfirmation {
		return record, false, nil
	}
	s.mu.Lock()
	active := s.active[operationID]
	s.mu.Unlock()
	if active == nil {
		// 首次读取与检查活动句柄之间可能已经完成并移除了句柄。
		// 句柄缺失本身不是终态证据，必须重新读取持久状态。
		record, err = s.Get(ctx, operationID)
		if err != nil {
			return Record{}, false, err
		}
		return record, record.State != StateWaitingConfirmation && !record.State.terminal(), nil
	}
	if timeout <= 0 {
		return record, true, nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-active.done:
		record, err := s.Get(ctx, operationID)
		return record, false, err
	case <-timer.C:
		record, err := s.Get(ctx, operationID)
		if err != nil {
			return Record{}, false, err
		}
		return record, record.State != StateWaitingConfirmation && !record.State.terminal(), nil
	case <-ctx.Done():
		return record, false, ctx.Err()
	}
}

// Result returns a bounded result page without executing the operation again.
func (s *Service) Result(ctx context.Context, operationID, stepID, cursor string, limit int) (ResultPage, error) {
	record, err := s.Get(ctx, operationID)
	if err != nil {
		return ResultPage{}, err
	}
	if limit <= 0 {
		limit = DefaultResultBytes
	}
	if limit > MaxResultBytes {
		limit = MaxResultBytes
	}
	raw := record.Result
	if stepID != "" {
		found := false
		for _, step := range record.Steps {
			if step.ID == stepID {
				raw, found = step.Result, true
				break
			}
		}
		if !found {
			return ResultPage{}, fmt.Errorf("step %q: %w", stepID, ErrNotFound)
		}
	}
	if len(raw) <= limit {
		return ResultPage{Operation: record, StepID: stepID, Result: raw}, nil
	}
	offset, err := parseCursor(cursor)
	if err != nil {
		return ResultPage{}, err
	}
	if offset > len(raw) {
		offset = len(raw)
	}
	end := offset + limit
	if end > len(raw) {
		end = len(raw)
	}
	page, _ := json.Marshal(map[string]any{
		"chunk": string(raw[offset:end]), "offset": offset, "next_offset": end, "truncated": end < len(raw),
	})
	var next string
	if end < len(raw) {
		next = strconv.Itoa(end)
	}
	return ResultPage{Operation: record, StepID: stepID, Result: page, NextCursor: next}, nil
}

// Cancel prevents new steps and cancels all currently running steps.
func (s *Service) Cancel(ctx context.Context, operationID string) (Record, error) {
	s.reconcileMu.Lock()
	defer func() { s.reconcileMu.Unlock(); s.reconcile(operationID) }()
	record, err := s.Get(ctx, operationID)
	if err != nil {
		return Record{}, err
	}
	if record.State.terminal() {
		return Record{}, ErrAlreadyCompleted
	}
	s.mu.Lock()
	active := s.active[operationID]
	if active != nil {
		active.cancelRequested = true
		active.cancel()
		for _, cancel := range active.stepCancel {
			cancel()
		}
	}
	s.mu.Unlock()
	now := s.now().UTC().UnixMilli()
	if _, err := s.db.ExecContext(ctx, `UPDATE operation_steps SET state = CASE WHEN state IN ('queued','waiting_confirmation') THEN 'cancelled' ELSE state END, completed_at = CASE WHEN state IN ('queued','waiting_confirmation') THEN ? ELSE completed_at END WHERE operation_id = ?`, now, operationID); err != nil {
		return Record{}, err
	}
	if active == nil {
		// 没有本进程运行步骤的持久 waiting 状态可直接取消。
		current, err := s.Get(ctx, operationID)
		if err != nil {
			return Record{}, err
		}
		if err := s.persistState(current, StateCancelled); err != nil {
			return Record{}, err
		}
		return s.Get(ctx, operationID)
	}
	return s.Get(ctx, operationID)
}

// Resume requeues one waiting-confirmation step after checking its token.
func (s *Service) Resume(ctx context.Context, operationID, stepID, confirmationToken string, executor Executor) (Record, error) {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	if strings.TrimSpace(stepID) == "" || strings.TrimSpace(confirmationToken) == "" {
		return Record{}, ErrConfirmation
	}
	record, err := s.Get(ctx, operationID)
	if err != nil {
		return Record{}, err
	}
	if record.State.terminal() {
		return Record{}, ErrAlreadyCompleted
	}
	var target StepRecord
	found := false
	for _, step := range record.Steps {
		if step.ID == stepID {
			target, found = step, true
			break
		}
	}
	if !found || target.State != StateWaitingConfirmation || target.ConfirmationToken != confirmationToken {
		return Record{}, ErrConfirmation
	}
	s.mu.Lock()
	active := s.active[operationID]
	if active != nil && active.cancelRequested {
		s.mu.Unlock()
		return Record{}, ErrAlreadyCompleted
	}
	if active == nil && executor == nil {
		s.mu.Unlock()
		return Record{}, ErrNotActive
	}
	s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, err
	}
	defer tx.Rollback()
	updated, err := tx.ExecContext(ctx, `UPDATE operation_steps SET state='queued', confirmation_token='', error_json='{}', completed_at=NULL
		WHERE operation_id=? AND step_id=? AND state='waiting_confirmation' AND confirmation_token=?
		AND EXISTS (SELECT 1 FROM operations WHERE id=? AND state NOT IN ('succeeded','failed','interrupted','cancelled'))`, operationID, stepID, confirmationToken, operationID)
	if err != nil {
		return Record{}, err
	}
	if count, err := updated.RowsAffected(); err != nil || count != 1 {
		return Record{}, ErrConfirmation
	}
	if _, err := tx.ExecContext(ctx, `UPDATE operations SET state='queued', completed_at=NULL WHERE id=? AND state NOT IN ('succeeded','failed','interrupted','cancelled')`, operationID); err != nil {
		return Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, err
	}
	s.mu.Lock()
	if active == nil {
		if executor == nil {
			s.mu.Unlock()
			return Record{}, ErrNotActive
		}
		active = s.restoreActiveLocked(ctx, record, executor)
		s.active[operationID] = active
	}
	if executor != nil {
		active.executor = executor
	}
	active.cancelRequested = false
	stepSpec := active.specs[stepID]
	stepSpec.Arguments = argumentsOrEmpty(cloneStepArguments(stepSpec.Arguments))
	stepSpec.Arguments["confirmation_token"] = confirmationToken
	stepSpec.Arguments["user_confirmed"] = true
	active.specs[stepID] = stepSpec
	active.enqueued[stepID] = false
	s.mu.Unlock()
	s.enqueueReady(operationID)
	return s.Get(ctx, operationID)
}

func cloneStepArguments(input map[string]any) map[string]any {
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

// Close stops workers and marks unfinished running operations interrupted.
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		close(s.stop)
		s.mu.Lock()
		for _, active := range s.active {
			active.cancel()
			for _, cancel := range active.stepCancel {
				cancel()
			}
		}
		s.mu.Unlock()
		s.wg.Wait()
		now := s.now().UTC().UnixMilli()
		s.closeErr = recoverInterrupted(s.db, now)
		close(s.closed)
	})
	return s.closeErr
}

func recoverInterrupted(db *sql.DB, now int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// 只有纯等待 DAG 可恢复。包含失联运行步骤的整图必须一致中断。
	if _, err := tx.Exec(`UPDATE operations SET state='interrupted', completed_at=? WHERE state IN ('queued','running') OR
		(state='waiting_confirmation' AND EXISTS (SELECT 1 FROM operation_steps WHERE operation_id=operations.id AND state='running'))`, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE operation_steps SET state='interrupted', completed_at=? WHERE state IN ('queued','running','waiting_confirmation')
		AND operation_id IN (SELECT id FROM operations WHERE state='interrupted')`, now); err != nil {
		return err
	}
	return tx.Commit()
}

// 独立调度者承担有界 worker channel 的背压，worker 不在排入后继时阻塞。
func (s *Service) dispatch() {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		var output chan stepJob
		var job stepJob
		if len(s.pending) > 0 {
			output, job = s.jobs, s.pending[0]
		}
		s.mu.Unlock()
		select {
		case output <- job:
			s.mu.Lock()
			s.pending = s.pending[1:]
			s.mu.Unlock()
		case <-s.wake:
		case <-s.stop:
			return
		}
	}
}

func (s *Service) worker() {
	defer s.wg.Done()
	for {
		select {
		case job := <-s.jobs:
			s.runStep(job)
		case <-s.stop:
			return
		}
	}
}

func (s *Service) runStep(job stepJob) {
	s.reconcileMu.Lock()
	s.mu.Lock()
	active := s.active[job.operationID]
	if active == nil || active.cancelRequested {
		s.mu.Unlock()
		s.reconcileMu.Unlock()
		return
	}
	step, ok := active.specs[job.stepID]
	s.mu.Unlock()
	if !ok {
		s.reconcileMu.Unlock()
		return
	}
	if !s.markStepRunning(job.operationID, job.stepID) {
		s.reconcileMu.Unlock()
		return
	}
	stepCtx, cancel := context.WithCancel(active.ctx)
	s.mu.Lock()
	active.stepCancel[job.stepID] = cancel
	executor := active.executor
	s.mu.Unlock()
	s.reconcileMu.Unlock()
	lock := s.workspaceLock(activeSpecWorkspace(s, job.operationID))
	if step.Exclusive {
		lock.Lock()
	} else {
		lock.RLock()
	}
	s.emit(Event{OperationID: job.operationID, StepID: job.stepID, RemoteSessionID: activeSpecSession(s, job.operationID), WorkspaceName: activeSpecWorkspace(s, job.operationID), Tool: step.Tool, Type: operationEventStepStarted, State: StateRunning, Summary: "operation step started", CreatedAt: s.now().UTC()})
	result := ExecuteResult{Err: stepCtx.Err()}
	if result.Err == nil {
		result = executeStepSafely(executor, stepCtx, ExecuteInput{
			OperationID: job.operationID, StepID: job.stepID,
			RequestID:       stepRequestID(s, job.operationID, job.stepID),
			RemoteSessionID: activeSpecSession(s, job.operationID), WorkspaceName: activeSpecWorkspace(s, job.operationID),
			Purpose: activeSpecPurpose(s, job.operationID), Tool: step.Tool, Arguments: argumentsOrEmpty(step.Arguments),
		})
	}
	cancel()
	if step.Exclusive {
		lock.Unlock()
	} else {
		lock.RUnlock()
	}
	s.reconcileMu.Lock()
	s.mu.Lock()
	cancelled := active.cancelRequested
	s.mu.Unlock()
	state := StateSucceeded
	if errors.Is(result.Err, ErrEffectsUnconfirmed) {
		state = StateInterrupted
	} else if cancelled || errors.Is(result.Err, context.Canceled) {
		state = StateCancelled
	} else if result.WaitingConfirmation {
		state = StateWaitingConfirmation
	} else if result.Err != nil {
		state = StateFailed
	}
	errorJSON := json.RawMessage(`{}`)
	if result.Err != nil {
		errorJSON = errorValue(result.Err)
	}
	if state == StateWaitingConfirmation && result.ConfirmationToken == "" {
		result.ConfirmationToken = newID("confirm_")
	}
	finished := s.finishStep(job.operationID, job.stepID, state, result.Result, errorJSON, result.ConfirmationToken)
	s.mu.Lock()
	if finished {
		delete(active.stepCancel, job.stepID)
	}
	s.mu.Unlock()
	s.reconcileMu.Unlock()
	if !finished {
		return
	}
	s.emit(Event{OperationID: job.operationID, StepID: job.stepID, RemoteSessionID: activeSpecSession(s, job.operationID), WorkspaceName: activeSpecWorkspace(s, job.operationID), Tool: step.Tool, Type: operationEventStepCompleted, State: state, Summary: "operation step " + string(state), CreatedAt: s.now().UTC()})
	s.reconcile(job.operationID)
}

func executeStepSafely(executor Executor, ctx context.Context, input ExecuteInput) (result ExecuteResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = ExecuteResult{Err: fmt.Errorf("operation executor panic recovered: %v", recovered)}
		}
	}()
	return executor(ctx, input)
}

func (s *Service) enqueueReady(operationID string) {
	record, err := s.Get(context.Background(), operationID)
	if err != nil {
		return
	}
	s.mu.Lock()
	active := s.active[operationID]
	if active == nil || active.cancelRequested {
		s.mu.Unlock()
		return
	}
	jobs := make([]stepJob, 0)
	byID := make(map[string]StepRecord, len(record.Steps))
	for _, step := range record.Steps {
		byID[step.ID] = step
	}
	for _, step := range record.Steps {
		if step.State != StateQueued || active.enqueued[step.ID] {
			continue
		}
		ready := true
		for _, dependency := range step.DependsOn {
			if byID[dependency].State != StateSucceeded {
				ready = false
				break
			}
		}
		if ready {
			active.enqueued[step.ID] = true
			jobs = append(jobs, stepJob{operationID: operationID, stepID: step.ID})
		}
	}
	s.pending = append(s.pending, jobs...)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) reconcile(operationID string) {
	s.reconcileMu.Lock()
	enqueue := false
	defer func() {
		s.reconcileMu.Unlock()
		if enqueue {
			s.enqueueReady(operationID)
		}
	}()
	record, err := s.Get(context.Background(), operationID)
	if err != nil {
		return
	}
	s.mu.Lock()
	active := s.active[operationID]
	cancelled := active != nil && active.cancelRequested
	canFinishCancelled := cancelled && active != nil && len(active.stepCancel) == 0
	s.mu.Unlock()
	if cancelled {
		if canFinishCancelled {
			terminalState := StateCancelled
			for _, step := range record.Steps {
				if step.State == StateInterrupted {
					terminalState = StateInterrupted
				}
			}
			if err := s.persistState(record, terminalState); err != nil {
				return
			}
			if s.finishActive(operationID) {
				s.emit(Event{OperationID: operationID, RemoteSessionID: record.RemoteSessionID, WorkspaceName: record.WorkspaceName, RequestID: record.RequestID, Type: operationEventCompleted, State: terminalState, Summary: "operation " + string(terminalState), CreatedAt: s.now().UTC()})
			}
		}
		return
	}
	byID := make(map[string]StepRecord, len(record.Steps))
	for _, step := range record.Steps {
		byID[step.ID] = step
	}
	for _, step := range record.Steps {
		if step.State != StateQueued {
			continue
		}
		for _, dependency := range step.DependsOn {
			dependencyState := byID[dependency].State
			if dependencyState == StateFailed || dependencyState == StateCancelled || dependencyState == StateInterrupted || dependencyState == StateSkipped {
				_, _ = s.db.Exec(`UPDATE operation_steps SET state = 'skipped', completed_at = ? WHERE operation_id = ? AND step_id = ? AND state = 'queued'`, s.now().UTC().UnixMilli(), operationID, step.ID)
				break
			}
		}
	}
	record, err = s.Get(context.Background(), operationID)
	if err != nil {
		return
	}
	state := aggregateState(record.Steps)
	if err := s.persistState(record, state); err != nil {
		return
	}
	if state == StateSucceeded || state == StateFailed || state == StateInterrupted || state == StateCancelled {
		if s.finishActive(operationID) {
			s.emit(Event{OperationID: operationID, RemoteSessionID: record.RemoteSessionID, WorkspaceName: record.WorkspaceName, RequestID: record.RequestID, Type: operationEventCompleted, State: state, Summary: "operation " + string(state), CreatedAt: s.now().UTC()})
		}
		return
	}
	enqueue = true
}

func (s *Service) finishActive(operationID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := s.active[operationID]
	if active == nil {
		return false
	}
	close(active.done)
	delete(s.active, operationID)
	return true
}

func (s *Service) persistState(record Record, state State) error {
	result, operationError := aggregateResult(record)
	_, err := s.db.Exec(`UPDATE operations SET state = ?, result_json = ?, error_json = ?,
		started_at = CASE WHEN started_at IS NULL AND ? IN ('running','succeeded','failed','waiting_confirmation') THEN ? ELSE started_at END,
		completed_at = CASE WHEN ? IN ('succeeded','failed','interrupted','cancelled') THEN ? ELSE completed_at END
		WHERE id = ? AND state NOT IN ('succeeded','failed','interrupted','cancelled')`, state, string(result), string(operationError),
		state, s.now().UTC().UnixMilli(), state, s.now().UTC().UnixMilli(), record.ID)
	return err
}

func aggregateResult(record Record) (json.RawMessage, json.RawMessage) {
	if len(record.Steps) == 1 {
		result := ensureJSON(record.Steps[0].Result)
		operationError := ensureJSON(record.Steps[0].Error)
		return result, operationError
	}
	results := make(map[string]any, len(record.Steps))
	errorsByStep := make(map[string]any)
	for _, step := range record.Steps {
		results[step.ID] = decodeJSON(step.Result)
		if step.State != StateSucceeded && step.State != StateSkipped {
			errorsByStep[step.ID] = map[string]any{
				"state": step.State,
				"error": decodeJSON(step.Error),
			}
		}
	}
	result, _ := json.Marshal(results)
	operationError, _ := json.Marshal(errorsByStep)
	return result, operationError
}

func ensureJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return json.RawMessage(`{}`)
	}
	return raw
}

func decodeJSON(raw json.RawMessage) any {
	raw = ensureJSON(raw)
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return map[string]any{}
	}
	return value
}

func aggregateState(steps []StepRecord) State {
	if len(steps) == 0 {
		return StateFailed
	}
	allTerminal := true
	anyRunning := false
	anyWaiting := false
	anyFailed := false
	anyInterrupted := false
	for _, step := range steps {
		switch step.State {
		case StateQueued:
			allTerminal = false
		case StateRunning:
			allTerminal = false
			anyRunning = true
		case StateWaitingConfirmation:
			allTerminal = false
			anyWaiting = true
		case StateFailed:
			anyFailed = true
		case StateCancelled:
			anyFailed = true
		case StateInterrupted:
			anyInterrupted = true
		case StateSucceeded, StateSkipped:
		default:
			allTerminal = false
		}
	}
	if anyInterrupted && allTerminal {
		return StateInterrupted
	}
	if anyFailed && !anyRunning && !anyWaiting && allTerminal {
		return StateFailed
	}
	if anyRunning || !allTerminal && !anyWaiting {
		return StateRunning
	}
	if anyWaiting {
		return StateWaitingConfirmation
	}
	if allTerminal {
		return StateSucceeded
	}
	return StateQueued
}

func (s *Service) markStepRunning(operationID, stepID string) bool {
	now := s.now().UTC().UnixMilli()
	result, err := s.db.Exec(`UPDATE operation_steps SET state = 'running', started_at = ? WHERE operation_id = ? AND step_id = ? AND state = 'queued'`, now, operationID, stepID)
	if err != nil {
		return false
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return false
	}
	_, _ = s.db.Exec(`UPDATE operations SET state = 'running', started_at = COALESCE(started_at, ?) WHERE id = ? AND state IN ('queued','waiting_confirmation')`, now, operationID)
	return true
}

func (s *Service) finishStep(operationID, stepID string, state State, result, operationError json.RawMessage, token string) bool {
	if len(result) == 0 {
		result = json.RawMessage(`{}`)
	}
	if len(operationError) == 0 {
		operationError = json.RawMessage(`{}`)
	}
	res, err := s.db.Exec(`UPDATE operation_steps SET state = ?, result_json = ?, error_json = ?, confirmation_token = ?, completed_at = ? WHERE operation_id = ? AND step_id = ?`, state, string(result), string(operationError), token, s.now().UTC().UnixMilli(), operationID, stepID)
	if err != nil {
		return false
	}
	rows, _ := res.RowsAffected()
	return rows == 1
}

func (s *Service) workspaceLock(workspace string) *sync.RWMutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock := s.workspaceLocks[workspace]
	if lock == nil {
		lock = &sync.RWMutex{}
		s.workspaceLocks[workspace] = lock
	}
	return lock
}

func (s *Service) emit(event Event) {
	if event.Type == operationEventStarted || event.Type == operationEventCompleted {
		record, err := s.Get(context.Background(), event.OperationID)
		if err != nil || record.State != event.State {
			// 未持久化或已过期的生命周期通知不能冒充当前权威状态。
			return
		}
		event.StateSequence, event.StateEventID = record.StateSequence, record.StateEventID
		event.RunID = record.RunID
	}
	if s.sink != nil {
		s.sink(event)
	}
}

func validateSpec(spec SubmitSpec) error {
	if strings.TrimSpace(spec.RemoteSessionID) == "" || strings.TrimSpace(spec.WorkspaceName) == "" {
		return fmt.Errorf("%w: session and workspace are required", ErrInvalidSpec)
	}
	if len(spec.Steps) == 0 || len(spec.Steps) > MaxSteps {
		return fmt.Errorf("%w: step count must be between 1 and %d", ErrInvalidSpec, MaxSteps)
	}
	byID := make(map[string]StepSpec, len(spec.Steps))
	for _, step := range spec.Steps {
		id := strings.TrimSpace(step.ID)
		if id == "" || strings.TrimSpace(step.Tool) == "" {
			return fmt.Errorf("%w: step id and tool are required", ErrInvalidSpec)
		}
		if _, exists := byID[id]; exists {
			return fmt.Errorf("%w: duplicate step id %q", ErrInvalidSpec, id)
		}
		byID[id] = step
	}
	visiting := make(map[string]bool, len(byID))
	visited := make(map[string]bool, len(byID))
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("%w: dependency cycle at %q", ErrInvalidSpec, id)
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, dependency := range byID[id].DependsOn {
			if _, exists := byID[dependency]; !exists {
				return fmt.Errorf("%w: step %q depends on unknown step %q", ErrInvalidSpec, id, dependency)
			}
			if err := visit(dependency); err != nil {
				return err
			}
		}
		visiting[id] = false
		visited[id] = true
		return nil
	}
	for id := range byID {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) restoreActiveLocked(ctx context.Context, record Record, executor Executor) *activeOperation {
	activeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	active := &activeOperation{
		ctx: activeCtx, cancel: cancel, executor: executor,
		specs: make(map[string]StepSpec, len(record.Steps)), enqueued: make(map[string]bool, len(record.Steps)),
		stepCancel: make(map[string]context.CancelFunc), done: make(chan struct{}),
	}
	for _, step := range record.Steps {
		active.specs[step.ID] = StepSpec{ID: step.ID, Tool: step.Tool, Arguments: step.Arguments, DependsOn: append([]string(nil), step.DependsOn...), Exclusive: step.Exclusive}
	}
	return active
}

func activeSpecWorkspace(s *Service, operationID string) string {
	if record, err := s.Get(context.Background(), operationID); err == nil {
		return record.WorkspaceName
	}
	return ""
}

func activeSpecSession(s *Service, operationID string) string {
	if record, err := s.Get(context.Background(), operationID); err == nil {
		return record.RemoteSessionID
	}
	return ""
}

func activeSpecPurpose(s *Service, operationID string) string {
	if record, err := s.Get(context.Background(), operationID); err == nil {
		return record.Purpose
	}
	return ""
}

func stepRequestID(s *Service, operationID, stepID string) string {
	if record, err := s.Get(context.Background(), operationID); err == nil {
		for _, step := range record.Steps {
			if step.ID == stepID {
				return step.RequestID
			}
		}
	}
	return newID("req_")
}

func argumentsOrEmpty(arguments map[string]any) map[string]any {
	if arguments == nil {
		return map[string]any{}
	}
	return arguments
}

func errorValue(err error) json.RawMessage {
	encoded, marshalErr := json.Marshal(map[string]any{"message": err.Error()})
	if marshalErr != nil {
		return json.RawMessage(`{"message":"operation failed"}`)
	}
	return encoded
}

func parseCursor(cursor string) (int, error) {
	if strings.TrimSpace(cursor) == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(cursor)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid result cursor")
	}
	return value, nil
}

func newID(prefix string) string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
