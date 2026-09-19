package plan

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"mcpx/internal/file"
)

type Service struct {
	db  *sql.DB
	now func() time.Time
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func NewService(db *sql.DB) *Service { return &Service{db: db, now: time.Now} }

func (s *Service) Create(ctx context.Context, remoteSessionID, principalID string, input CreateInput) (Plan, error) {
	if strings.TrimSpace(remoteSessionID) == "" || strings.TrimSpace(principalID) == "" {
		return Plan{}, fmt.Errorf("%w: remote session and principal are required", ErrInvalidInput)
	}
	goal := strings.TrimSpace(input.Goal)
	if goal == "" {
		return Plan{}, fmt.Errorf("%w: goal is required", ErrInvalidInput)
	}
	tasks, err := normalizeTaskInputs(input.Tasks)
	if err != nil {
		return Plan{}, err
	}
	now := s.now().UTC()
	planID := newID("pl_")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Plan{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO plans
        (id, remote_session_id, goal, summary, status, version, created_by, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?)`, planID, remoteSessionID, goal, strings.TrimSpace(input.Summary), PlanReady,
		principalID, now.UnixMilli(), now.UnixMilli()); err != nil {
		return Plan{}, err
	}
	if err := insertTasksTx(ctx, tx, planID, tasks, now); err != nil {
		return Plan{}, err
	}
	if err := insertEventTx(ctx, tx, planID, "", "plan.created", "", principalID, map[string]any{"task_count": len(tasks)}, now); err != nil {
		return Plan{}, err
	}
	if err := tx.Commit(); err != nil {
		return Plan{}, err
	}
	return s.Get(ctx, remoteSessionID, planID)
}

func (s *Service) Get(ctx context.Context, remoteSessionID, planID string) (Plan, error) {
	if strings.TrimSpace(remoteSessionID) == "" || strings.TrimSpace(planID) == "" {
		return Plan{}, fmt.Errorf("%w: remote session and plan are required", ErrInvalidInput)
	}
	return loadPlan(ctx, s.db, remoteSessionID, planID)
}

func (s *Service) FindTask(ctx context.Context, remoteSessionID, planTaskID string) (string, Task, error) {
	remoteSessionID = strings.TrimSpace(remoteSessionID)
	planTaskID = strings.TrimSpace(planTaskID)
	if remoteSessionID == "" || planTaskID == "" {
		return "", Task{}, fmt.Errorf("%w: remote session and plan_task_id are required", ErrInvalidInput)
	}
	var planID string
	err := s.db.QueryRowContext(ctx, `SELECT pt.plan_id
		FROM plan_tasks pt
		JOIN plans p ON p.id = pt.plan_id
		WHERE pt.id = ? AND p.remote_session_id = ?`, planTaskID, remoteSessionID).Scan(&planID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", Task{}, ErrNotFound
	}
	if err != nil {
		return "", Task{}, err
	}
	item, err := s.Get(ctx, remoteSessionID, planID)
	if err != nil {
		return "", Task{}, err
	}
	for _, task := range item.Tasks {
		if task.ID == planTaskID {
			return planID, task, nil
		}
	}
	return "", Task{}, ErrNotFound
}

func (s *Service) StartTask(ctx context.Context, remoteSessionID, planID, taskID, principalID string) (Task, error) {
	return s.transitionTask(ctx, remoteSessionID, planID, taskID, principalID, TaskInProgress, "plan_task.started", "", nil)
}

func (s *Service) CompleteTask(ctx context.Context, remoteSessionID, planID, taskID, principalID string, inputs []EvidenceInput) (Task, error) {
	return s.transitionTask(ctx, remoteSessionID, planID, taskID, principalID, TaskCompleted, "plan_task.completed", "", inputs)
}

func (s *Service) BlockTask(ctx context.Context, remoteSessionID, planID, taskID, principalID, reason string, inputs []EvidenceInput) (Task, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Task{}, fmt.Errorf("%w: blocking reason is required", ErrInvalidInput)
	}
	return s.transitionTask(ctx, remoteSessionID, planID, taskID, principalID, TaskBlocked, "plan_task.blocked", reason, inputs)
}

func (s *Service) transitionTask(ctx context.Context, remoteSessionID, planID, taskID, principalID, target, eventType, reason string, evidence []EvidenceInput) (Task, error) {
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(principalID) == "" {
		return Task{}, fmt.Errorf("%w: task and principal are required", ErrInvalidInput)
	}
	if target == TaskCompleted && len(evidence) == 0 {
		return Task{}, ErrEvidenceRequired
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
	item, err := loadPlan(ctx, tx, remoteSessionID, planID)
	if err != nil {
		return Task{}, err
	}
	task, _, err := findTask(item.Tasks, taskID)
	if err != nil {
		return Task{}, err
	}
	if item.Status == PlanCompleted || item.Status == PlanCancelled {
		return Task{}, fmt.Errorf("%w: plan is %s", ErrInvalidState, item.Status)
	}
	switch target {
	case TaskInProgress:
		if task.Status != TaskTodo && task.Status != TaskBlocked {
			return Task{}, fmt.Errorf("%w: task %s is %s", ErrInvalidState, task.ID, task.Status)
		}
		if err := dependenciesComplete(item.Tasks, task); err != nil {
			return Task{}, err
		}
	case TaskCompleted:
		if task.Status != TaskInProgress {
			return Task{}, fmt.Errorf("%w: task %s is %s", ErrInvalidState, task.ID, task.Status)
		}
	case TaskBlocked:
		if task.Status != TaskInProgress {
			return Task{}, fmt.Errorf("%w: task %s is %s", ErrInvalidState, task.ID, task.Status)
		}
	default:
		return Task{}, fmt.Errorf("%w: unsupported task target %s", ErrInvalidState, target)
	}
	now := s.now().UTC()
	completedAt := any(nil)
	if target == TaskCompleted {
		completedAt = now.UnixMilli()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plan_tasks SET status = ?, updated_at = ?, completed_at = ? WHERE id = ? AND plan_id = ?`,
		target, now.UnixMilli(), completedAt, task.ID, planID); err != nil {
		return Task{}, err
	}
	if err := insertEvidenceBatchTx(ctx, tx, remoteSessionID, planID, task.ID, principalID, evidence, now); err != nil {
		return Task{}, err
	}
	planStatus := item.Status
	if target == TaskInProgress && (planStatus == PlanReady || planStatus == PlanBlocked) {
		planStatus = PlanInProgress
	}
	if target == TaskBlocked {
		planStatus = PlanBlocked
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plans SET status = ?, version = version + 1, updated_at = ? WHERE id = ? AND remote_session_id = ?`,
		planStatus, now.UnixMilli(), planID, remoteSessionID); err != nil {
		return Task{}, err
	}
	payload := map[string]any{"status": target}
	if len(evidence) > 0 {
		payload["evidence_count"] = len(evidence)
	}
	if err := insertEventTx(ctx, tx, planID, task.ID, eventType, reason, principalID, payload, now); err != nil {
		return Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	updated, err := s.Get(ctx, remoteSessionID, planID)
	if err != nil {
		return Task{}, err
	}
	return updated.Tasks[indexByID(updated.Tasks, task.ID)], nil
}

func (s *Service) Replan(ctx context.Context, remoteSessionID, planID, principalID string, input ReplanInput) (Plan, error) {
	if strings.TrimSpace(principalID) == "" || strings.TrimSpace(input.Reason) == "" {
		return Plan{}, fmt.Errorf("%w: principal and reason are required", ErrInvalidInput)
	}
	if len(input.Operations) == 0 && strings.TrimSpace(input.Goal) == "" && strings.TrimSpace(input.Summary) == "" {
		return Plan{}, fmt.Errorf("%w: replan changes are required", ErrInvalidInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Plan{}, err
	}
	defer tx.Rollback()
	item, err := loadPlan(ctx, tx, remoteSessionID, planID)
	if err != nil {
		return Plan{}, err
	}
	originalIDs := make(map[string]bool, len(item.Tasks))
	for _, task := range item.Tasks {
		originalIDs[task.ID] = true
	}
	if item.Status == PlanCompleted || item.Status == PlanCancelled {
		return Plan{}, fmt.Errorf("%w: plan is %s", ErrInvalidState, item.Status)
	}
	if err := applyOperations(&item, input.Operations, originalIDs); err != nil {
		return Plan{}, err
	}
	if len(item.Tasks) == 0 || len(item.Tasks) > 100 {
		return Plan{}, fmt.Errorf("%w: replanned tasks must contain between 1 and 100 items", ErrInvalidInput)
	}
	if err := validateTaskGraph(item.Tasks); err != nil {
		return Plan{}, err
	}
	if goal := strings.TrimSpace(input.Goal); goal != "" {
		item.Goal = goal
	}
	if summary := strings.TrimSpace(input.Summary); summary != "" {
		item.Summary = summary
	}
	status := item.Status
	if status == PlanReady || status == PlanBlocked {
		status = PlanInProgress
	}
	now := s.now().UTC()
	if err := persistReplanTasksTx(ctx, tx, planID, originalIDs, item.Tasks, now); err != nil {
		return Plan{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plans SET goal = ?, summary = ?, status = ?, version = version + 1, updated_at = ? WHERE id = ? AND remote_session_id = ?`,
		item.Goal, item.Summary, status, now.UnixMilli(), planID, remoteSessionID); err != nil {
		return Plan{}, err
	}
	payload := map[string]any{"operations": input.Operations}
	if err := insertEventTx(ctx, tx, planID, "", "plan.replanned", strings.TrimSpace(input.Reason), principalID, payload, now); err != nil {
		return Plan{}, err
	}
	if err := tx.Commit(); err != nil {
		return Plan{}, err
	}
	return s.Get(ctx, remoteSessionID, planID)
}

func (s *Service) Deliver(ctx context.Context, remoteSessionID, planID, principalID string) (Delivery, error) {
	if strings.TrimSpace(principalID) == "" {
		return Delivery{}, fmt.Errorf("%w: principal is required", ErrInvalidInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Delivery{}, err
	}
	defer tx.Rollback()
	item, err := loadPlan(ctx, tx, remoteSessionID, planID)
	if err != nil {
		return Delivery{}, err
	}
	if item.Status == PlanCancelled {
		return Delivery{}, fmt.Errorf("%w: plan is cancelled", ErrInvalidState)
	}
	checks, blockers, err := deliveryChecks(ctx, tx, remoteSessionID, item, s.now().UTC())
	if err != nil {
		return Delivery{}, err
	}
	now := s.now().UTC()
	status := "blocked"
	var deliveredAt *time.Time
	planStatus := PlanBlocked
	if len(blockers) == 0 {
		status = "completed"
		planStatus = PlanCompleted
		deliveredAt = &now
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plans SET status = ?, version = version + 1, updated_at = ?, completed_at = ? WHERE id = ? AND remote_session_id = ?`,
		planStatus, now.UnixMilli(), nullableTimeMillis(deliveredAt), planID, remoteSessionID); err != nil {
		return Delivery{}, err
	}
	eventType := "plan.delivery_blocked"
	if len(blockers) == 0 {
		eventType = "plan.delivered"
	}
	if err := insertEventTx(ctx, tx, planID, "", eventType, strings.Join(blockers, "; "), principalID, map[string]any{"blockers": blockers}, now); err != nil {
		return Delivery{}, err
	}
	if err := tx.Commit(); err != nil {
		return Delivery{}, err
	}
	updated, err := s.Get(ctx, remoteSessionID, planID)
	if err != nil {
		return Delivery{}, err
	}
	return Delivery{PlanID: planID, RemoteSessionID: remoteSessionID, Status: status, Ready: len(blockers) == 0,
		Checks: checks, Blockers: blockers, Plan: updated, DeliveredAt: deliveredAt}, nil
}

func normalizeTaskInputs(inputs []TaskInput) ([]Task, error) {
	if len(inputs) == 0 || len(inputs) > 100 {
		return nil, fmt.Errorf("%w: tasks must contain between 1 and 100 items", ErrInvalidInput)
	}
	// task_id from the model is only a local dependency reference inside this
	// plan. The service always issues a globally unique final task id so the
	// same label can be reused across plans without hitting the primary key.
	localIDs := make([]string, len(inputs))
	finalByLocal := make(map[string]string, len(inputs))
	for i, input := range inputs {
		local := strings.TrimSpace(input.ID)
		if local == "" {
			local = fmt.Sprintf("task-%d", i+1)
		}
		if _, exists := finalByLocal[local]; exists {
			return nil, fmt.Errorf("%w: duplicate task id %s", ErrInvalidInput, local)
		}
		finalByLocal[local] = newID("pt_")
		localIDs[i] = local
	}
	result := make([]Task, len(inputs))
	now := time.Time{}
	for i, input := range inputs {
		title := strings.TrimSpace(input.Title)
		if title == "" {
			return nil, fmt.Errorf("%w: task %d title is required", ErrInvalidInput, i)
		}
		result[i] = Task{
			ID: finalByLocal[localIDs[i]], Ordinal: i, Title: title,
			Description: strings.TrimSpace(input.Description), Status: TaskTodo,
			CreatedAt: now, UpdatedAt: now,
		}
	}
	for i, input := range inputs {
		deps := make([]string, 0, len(input.DependsOn))
		for _, ref := range input.DependsOn {
			ref = strings.TrimSpace(ref)
			final, ok := finalByLocal[ref]
			if !ok {
				return nil, fmt.Errorf("%w: dependency %s does not exist", ErrDependency, ref)
			}
			deps = append(deps, final)
		}
		result[i].DependsOn = uniqueStrings(deps)
	}
	if err := validateTaskGraph(result); err != nil {
		return nil, err
	}
	return result, nil
}

func validateTaskGraph(tasks []Task) error {
	byID := make(map[string]Task, len(tasks))
	for _, task := range tasks {
		byID[task.ID] = task
	}
	state := make(map[string]uint8, len(tasks))
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("%w: %s", ErrCycle, id)
		}
		if state[id] == 2 {
			return nil
		}
		task, ok := byID[id]
		if !ok {
			return fmt.Errorf("%w: dependency %s does not exist", ErrDependency, id)
		}
		state[id] = 1
		for _, dependency := range task.DependsOn {
			if dependency == id {
				return fmt.Errorf("%w: task %s depends on itself", ErrCycle, id)
			}
			if err := visit(dependency); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for _, task := range tasks {
		if err := visit(task.ID); err != nil {
			return err
		}
	}
	return nil
}

func dependenciesComplete(tasks []Task, task Task) error {
	byID := make(map[string]Task, len(tasks))
	for _, item := range tasks {
		byID[item.ID] = item
	}
	for _, dependencyID := range task.DependsOn {
		dependency, ok := byID[dependencyID]
		if !ok || (dependency.Status != TaskCompleted && dependency.Status != TaskSkipped) {
			return fmt.Errorf("%w: task %s depends on %s", ErrDependency, task.ID, dependencyID)
		}
	}
	return nil
}

func persistReplanTasksTx(ctx context.Context, tx *sql.Tx, planID string, originalIDs map[string]bool, tasks []Task, now time.Time) error {
	currentIDs := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		currentIDs[task.ID] = true
	}
	removed := make([]string, 0)
	for id := range originalIDs {
		if !currentIDs[id] {
			removed = append(removed, id)
		}
	}
	if len(removed) > 0 {
		args := make([]any, 0, len(removed)+1)
		args = append(args, planID)
		for _, id := range removed {
			args = append(args, id)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM plan_tasks WHERE plan_id = ? AND id IN (`+placeholders(len(removed))+`)`, args...); err != nil {
			return err
		}
	}
	existing := make([]Task, 0, len(tasks))
	added := make([]Task, 0)
	for _, task := range tasks {
		if originalIDs[task.ID] {
			existing = append(existing, task)
		} else {
			added = append(added, task)
		}
	}
	if len(existing) > 0 {
		query := `UPDATE plan_tasks SET ordinal = CASE id `
		args := make([]any, 0, len(existing)*8+len(existing)+1)
		for _, task := range existing {
			query += "WHEN ? THEN ? "
			args = append(args, task.ID, task.Ordinal)
		}
		query += `ELSE ordinal END, title = CASE id `
		for _, task := range existing {
			query += "WHEN ? THEN ? "
			args = append(args, task.ID, task.Title)
		}
		query += `ELSE title END, description = CASE id `
		for _, task := range existing {
			query += "WHEN ? THEN ? "
			args = append(args, task.ID, task.Description)
		}
		query += `ELSE description END, depends_on_json = CASE id `
		for _, task := range existing {
			depends, err := json.Marshal(task.DependsOn)
			if err != nil {
				return err
			}
			query += "WHEN ? THEN ? "
			args = append(args, task.ID, string(depends))
		}
		query += `ELSE depends_on_json END, updated_at = ? WHERE plan_id = ? AND id IN (` + placeholders(len(existing)) + `)`
		args = append(args, now.UnixMilli(), planID)
		for _, task := range existing {
			args = append(args, task.ID)
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	return insertTasksTx(ctx, tx, planID, added, now)
}

func applyOperations(item *Plan, operations []TaskOperation, originalIDs map[string]bool) error {
	maxOrdinal := -1
	for _, task := range item.Tasks {
		if task.Ordinal > maxOrdinal {
			maxOrdinal = task.Ordinal
		}
	}
	for _, operation := range operations {
		action := strings.ToLower(strings.TrimSpace(operation.Action))
		switch action {
		case "add":
			title := strings.TrimSpace(operation.Title)
			if title == "" {
				return fmt.Errorf("%w: added task title is required", ErrInvalidInput)
			}
			id := strings.TrimSpace(operation.TaskID)
			if id == "" {
				id = newID("pt_")
			}
			if taskIndex(item.Tasks, id) >= 0 {
				return fmt.Errorf("%w: task %s already exists", ErrInvalidInput, id)
			}
			if originalIDs[id] {
				return fmt.Errorf("%w: task %s cannot be reused after removal", ErrInvalidInput, id)
			}
			maxOrdinal++
			item.Tasks = append(item.Tasks, Task{ID: id, PlanID: item.ID, Ordinal: maxOrdinal, Title: title,
				Description: strings.TrimSpace(operation.Description), Status: TaskTodo, DependsOn: uniqueStrings(operation.DependsOn)})
		case "update":
			index := taskIndex(item.Tasks, strings.TrimSpace(operation.TaskID))
			if index < 0 {
				return fmt.Errorf("%w: task %s", ErrNotFound, operation.TaskID)
			}
			task := &item.Tasks[index]
			if task.Status == TaskCompleted || task.Status == TaskSkipped {
				return fmt.Errorf("%w: completed task %s cannot be changed", ErrInvalidState, task.ID)
			}
			if title := strings.TrimSpace(operation.Title); title != "" {
				task.Title = title
			}
			task.Description = strings.TrimSpace(operation.Description)
			task.DependsOn = uniqueStrings(operation.DependsOn)
		case "remove":
			id := strings.TrimSpace(operation.TaskID)
			index := taskIndex(item.Tasks, id)
			if index < 0 {
				return fmt.Errorf("%w: task %s", ErrNotFound, id)
			}
			if item.Tasks[index].Status == TaskCompleted || item.Tasks[index].Status == TaskSkipped {
				return fmt.Errorf("%w: completed task %s cannot be removed", ErrInvalidState, id)
			}
			for _, other := range item.Tasks {
				if containsString(other.DependsOn, id) {
					return fmt.Errorf("%w: task %s is still required by %s", ErrDependency, id, other.ID)
				}
			}
			item.Tasks = append(item.Tasks[:index], item.Tasks[index+1:]...)
		default:
			return fmt.Errorf("%w: unsupported replan action %s", ErrInvalidInput, action)
		}
	}
	return nil
}

func findTask(tasks []Task, taskID string) (Task, int, error) {
	for index, task := range tasks {
		if task.ID == taskID {
			return task, index, nil
		}
	}
	return Task{}, -1, fmt.Errorf("%w: task %s", ErrNotFound, taskID)
}

func indexByID(tasks []Task, taskID string) int {
	index := taskIndex(tasks, taskID)
	if index < 0 {
		return 0
	}
	return index
}

func taskIndex(tasks []Task, taskID string) int {
	for index, task := range tasks {
		if task.ID == taskID {
			return index
		}
	}
	return -1
}

func insertTasksTx(ctx context.Context, tx *sql.Tx, planID string, tasks []Task, now time.Time) error {
	if len(tasks) == 0 {
		return nil
	}
	groups := make([]string, 0, len(tasks))
	args := make([]any, 0, len(tasks)*9)
	for _, task := range tasks {
		depends, err := json.Marshal(task.DependsOn)
		if err != nil {
			return err
		}
		groups = append(groups, "(?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args, task.ID, planID, task.Ordinal, task.Title, task.Description, TaskTodo,
			string(depends), now.UnixMilli(), now.UnixMilli())
	}
	query := `INSERT INTO plan_tasks
        (id, plan_id, ordinal, title, description, status, depends_on_json, created_at, updated_at)
        VALUES ` + strings.Join(groups, ", ")
	_, err := tx.ExecContext(ctx, query, args...)
	return err
}

type normalizedEvidence struct {
	kind          string
	referenceID   string
	metadata      string
	validated     bool
	sourceEventID string
}

func insertEvidenceBatchTx(ctx context.Context, tx *sql.Tx, remoteSessionID, planID, taskID, principalID string, inputs []EvidenceInput, now time.Time) error {
	if len(inputs) == 0 {
		return nil
	}
	normalized := make([]normalizedEvidence, 0, len(inputs))
	for _, input := range inputs {
		kind := strings.ToLower(strings.TrimSpace(input.Kind))
		referenceID := strings.TrimSpace(input.ReferenceID)
		if kind == "" || referenceID == "" {
			return fmt.Errorf("%w: evidence kind and reference_id are required", ErrEvidence)
		}
		if !IsEvidenceKind(kind) {
			return fmt.Errorf("%w: unsupported evidence kind %s; expected one of %s", ErrEvidence, kind, strings.Join(EvidenceKinds(), ", "))
		}
		metadata := input.Metadata
		if metadata == nil {
			metadata = map[string]any{}
		}
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("%w: metadata: %v", ErrEvidence, err)
		}
		normalized = append(normalized, normalizedEvidence{kind: kind, referenceID: referenceID, metadata: string(encoded)})
	}
	if err := validateEvidenceRefs(ctx, tx, remoteSessionID, normalized); err != nil {
		return err
	}
	groups := make([]string, 0, len(normalized))
	args := make([]any, 0, len(normalized)*10)
	for _, evidence := range normalized {
		groups = append(groups, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args, newID("ev_"), planID, taskID, evidence.kind, evidence.referenceID, boolInt(evidence.validated), evidence.sourceEventID, evidence.metadata, principalID, now.UnixMilli())
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO plan_task_evidence
        (id, plan_id, task_id, kind, reference_id, validated, source_event_id, metadata_json, created_by, created_at)
        VALUES `+strings.Join(groups, ", "), args...)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrEvidence, err)
	}
	return nil
}

func validateEvidenceRefs(ctx context.Context, tx *sql.Tx, remoteSessionID string, evidence []normalizedEvidence) error {
	for i := range evidence {
		sourceEventID, err := validateEvidenceRef(ctx, tx, remoteSessionID, evidence[i].kind, evidence[i].referenceID)
		if err != nil {
			return err
		}
		evidence[i].validated = true
		evidence[i].sourceEventID = sourceEventID
	}
	return nil
}

func validateEvidenceRef(ctx context.Context, tx *sql.Tx, remoteSessionID, kind, referenceID string) (string, error) {
	switch kind {
	case EvidenceRead:
		return validateObservationOrOperation(ctx, tx, remoteSessionID, referenceID, "read")
	case EvidenceObserve:
		return validateObservationOrOperation(ctx, tx, remoteSessionID, referenceID, "observe")
	case EvidenceVerification:
		return validateObservationOrOperation(ctx, tx, remoteSessionID, referenceID, "")
	case EvidenceEdit:
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM clean_edit_records WHERE remote_session_id = ? AND id = ?`, remoteSessionID, referenceID).Scan(&state); err != nil {
			return "", evidenceLookupError(kind, referenceID, err)
		}
		if state != "succeeded" {
			return "", fmt.Errorf("%w: %s %s is not completed successfully (state=%s)", ErrEvidence, kind, referenceID, state)
		}
		return "", nil
	case EvidenceExecute:
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM terminal_tasks WHERE remote_session_id = ? AND id = ?`, remoteSessionID, referenceID).Scan(&status); err != nil {
			return "", evidenceLookupError(kind, referenceID, err)
		}
		if status != "exited" {
			return "", fmt.Errorf("%w: %s %s is not completed (status=%s)", ErrEvidence, kind, referenceID, status)
		}
		return "", nil
	case EvidenceArtifact:
		var id string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM artifacts WHERE remote_session_id = ? AND id = ?`, remoteSessionID, referenceID).Scan(&id); err != nil {
			return "", evidenceLookupError(kind, referenceID, err)
		}
		return "", nil
	case EvidenceSource:
		var workspaceRoot string
		if err := tx.QueryRowContext(ctx, `SELECT workspace_path FROM remote_sessions WHERE id = ?`, remoteSessionID).Scan(&workspaceRoot); err != nil {
			return "", fmt.Errorf("%w: source session: %v", ErrEvidence, err)
		}
		resolved, err := file.Resolve(workspaceRoot, referenceID)
		if err != nil {
			return "", fmt.Errorf("%w: source path: %v", ErrEvidence, err)
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("%w: source %s does not exist as a regular file in remote session", ErrEvidence, referenceID)
		}
		return "", nil
	default:
		return "", fmt.Errorf("%w: unsupported evidence kind %s", ErrEvidence, kind)
	}
}

func validateObservationOrOperation(ctx context.Context, tx *sql.Tx, remoteSessionID, referenceID, expectedTool string) (string, error) {
	if sequence, err := strconv.ParseInt(referenceID, 10, 64); err == nil && sequence > 0 {
		var tool, eventType, status string
		err := tx.QueryRowContext(ctx, `SELECT tool_name, event_type, status FROM observation_events WHERE sequence = ? AND remote_session_id = ?`, sequence, remoteSessionID).Scan(&tool, &eventType, &status)
		if err != nil {
			return "", evidenceLookupError("observation", referenceID, err)
		}
		if expectedTool != "" && tool != expectedTool {
			return "", fmt.Errorf("%w: observation %s belongs to tool %s, expected %s", ErrEvidence, referenceID, tool, expectedTool)
		}
		if !completedObservationType(eventType) || !successfulEvidenceStatus(status) {
			return "", fmt.Errorf("%w: observation %s is not a completed successful event (type=%s status=%s)", ErrEvidence, referenceID, eventType, status)
		}
		return referenceID, nil
	}

	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM operations WHERE id = ? AND remote_session_id = ?`, referenceID, remoteSessionID).Scan(&state); err != nil {
		return "", evidenceLookupError("operation", referenceID, err)
	}
	if state != "succeeded" {
		return "", fmt.Errorf("%w: operation %s is not completed successfully (state=%s)", ErrEvidence, referenceID, state)
	}
	if expectedTool != "" {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_steps WHERE operation_id = ? AND tool_name = ? AND state = 'succeeded'`, referenceID, expectedTool).Scan(&count); err != nil {
			return "", fmt.Errorf("%w: operation %s step validation: %v", ErrEvidence, referenceID, err)
		}
		if count == 0 {
			return "", fmt.Errorf("%w: operation %s has no succeeded %s step", ErrEvidence, referenceID, expectedTool)
		}
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT sequence FROM observation_events WHERE remote_session_id = ? AND operation_id = ? AND event_type IN ('operation.completed','operation.step.completed') ORDER BY sequence DESC LIMIT 1`, remoteSessionID, referenceID).Scan(&sequence); err == nil {
		return strconv.FormatInt(sequence, 10), nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: operation %s observation lookup: %v", ErrEvidence, referenceID, err)
	}
	return "", nil
}

func completedObservationType(eventType string) bool {
	switch eventType {
	case "tool.completed", "operation.completed", "operation.step.completed":
		return true
	default:
		return false
	}
}

func successfulEvidenceStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ok", "succeeded", "completed", "exited", "success":
		return true
	default:
		return false
	}
}

func evidenceLookupError(kind, referenceID string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s %s does not belong to remote session or does not exist", ErrEvidence, kind, referenceID)
	}
	return fmt.Errorf("%w: %s %s lookup: %v", ErrEvidence, kind, referenceID, err)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func insertEventTx(ctx context.Context, tx *sql.Tx, planID, taskID, eventType, reason, principalID string, payload map[string]any, now time.Time) error {
	if payload == nil {
		payload = map[string]any{}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO plan_events
        (id, plan_id, task_id, event_type, reason, payload_json, created_by, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, newID("pe_"), planID, nullableString(taskID), eventType, reason, string(encoded), principalID, now.UnixMilli())
	return err
}

func loadPlan(ctx context.Context, q queryer, remoteSessionID, planID string) (Plan, error) {
	var plan Plan
	var createdAt, updatedAt int64
	var completedAt sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT id, remote_session_id, goal, summary, status, version, created_by, created_at, updated_at, completed_at
        FROM plans WHERE id = ? AND remote_session_id = ?`, planID, remoteSessionID).Scan(
		&plan.ID, &plan.RemoteSessionID, &plan.Goal, &plan.Summary, &plan.Status, &plan.Version, &plan.CreatedBy,
		&createdAt, &updatedAt, &completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, ErrNotFound
	}
	if err != nil {
		return Plan{}, err
	}
	plan.CreatedAt = time.UnixMilli(createdAt).UTC()
	plan.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	plan.CompletedAt = timeFromNullable(completedAt)
	plan.Tasks, err = loadTasks(ctx, q, plan.ID)
	if err != nil {
		return Plan{}, err
	}
	plan.Events, err = loadEvents(ctx, q, plan.ID)
	if err != nil {
		return Plan{}, err
	}
	plan.Progress = calculateProgress(plan.Tasks)
	return plan, nil
}

func loadTasks(ctx context.Context, q queryer, planID string) ([]Task, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, plan_id, ordinal, title, description, status, depends_on_json, created_at, updated_at, completed_at
        FROM plan_tasks WHERE plan_id = ? ORDER BY ordinal, id`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Task, 0)
	byID := make(map[string]int)
	for rows.Next() {
		var task Task
		var dependsJSON string
		var createdAt, updatedAt int64
		var completedAt sql.NullInt64
		if err := rows.Scan(&task.ID, &task.PlanID, &task.Ordinal, &task.Title, &task.Description, &task.Status, &dependsJSON, &createdAt, &updatedAt, &completedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(dependsJSON), &task.DependsOn); err != nil {
			return nil, err
		}
		task.CreatedAt = time.UnixMilli(createdAt).UTC()
		task.UpdatedAt = time.UnixMilli(updatedAt).UTC()
		task.CompletedAt = timeFromNullable(completedAt)
		byID[task.ID] = len(result)
		result = append(result, task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := loadEvidence(ctx, q, planID, result, byID); err != nil {
		return nil, err
	}
	return result, nil
}

func loadEvidence(ctx context.Context, q queryer, planID string, tasks []Task, byID map[string]int) error {
	rows, err := q.QueryContext(ctx, `SELECT id, plan_id, task_id, kind, reference_id, validated, source_event_id, metadata_json, created_by, created_at
        FROM plan_task_evidence WHERE plan_id = ? ORDER BY created_at, id`, planID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var evidence Evidence
		var validated int
		var metadataJSON string
		var createdAt int64
		if err := rows.Scan(&evidence.ID, &evidence.PlanID, &evidence.TaskID, &evidence.Kind, &evidence.ReferenceID, &validated, &evidence.SourceEventID, &metadataJSON, &evidence.CreatedBy, &createdAt); err != nil {
			return err
		}
		evidence.Validated = validated != 0
		if err := json.Unmarshal([]byte(metadataJSON), &evidence.Metadata); err != nil {
			return err
		}
		evidence.CreatedAt = time.UnixMilli(createdAt).UTC()
		if index, ok := byID[evidence.TaskID]; ok {
			tasks[index].Evidence = append(tasks[index].Evidence, evidence)
		}
	}
	return rows.Err()
}

func loadEvents(ctx context.Context, q queryer, planID string) ([]Event, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, plan_id, COALESCE(task_id, ''), event_type, reason, payload_json, created_by, created_at
        FROM plan_events WHERE plan_id = ? ORDER BY created_at, id`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Event, 0)
	for rows.Next() {
		var event Event
		var payloadJSON string
		var createdAt int64
		if err := rows.Scan(&event.ID, &event.PlanID, &event.TaskID, &event.Type, &event.Reason, &payloadJSON, &event.CreatedBy, &createdAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payloadJSON), &event.Payload); err != nil {
			return nil, err
		}
		event.CreatedAt = time.UnixMilli(createdAt).UTC()
		result = append(result, event)
	}
	return result, rows.Err()
}

func calculateProgress(tasks []Task) Progress {
	progress := Progress{Total: len(tasks)}
	for _, task := range tasks {
		switch task.Status {
		case TaskTodo:
			progress.Todo++
		case TaskInProgress:
			progress.InProgress++
		case TaskBlocked:
			progress.Blocked++
		case TaskCompleted:
			progress.Completed++
		case TaskSkipped:
			progress.Skipped++
		}
	}
	return progress
}

func newID(prefix string) string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return prefix + fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return prefix + base64.RawURLEncoding.EncodeToString(value[:])
}

func uniqueStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableTimeMillis(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UnixMilli()
}

func timeFromNullable(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	result := time.UnixMilli(value.Int64).UTC()
	return &result
}
