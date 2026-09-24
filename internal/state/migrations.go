package state

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS schema_migrations (
        version INTEGER PRIMARY KEY,
        applied_at INTEGER NOT NULL
    );
    CREATE TABLE IF NOT EXISTS runtime_instances (
        id TEXT PRIMARY KEY,
        started_at INTEGER NOT NULL,
        last_seen_at INTEGER NOT NULL
    );
    CREATE TABLE IF NOT EXISTS principals (
        id TEXT PRIMARY KEY,
        kind TEXT NOT NULL,
        subject_hash TEXT NOT NULL,
        created_at INTEGER NOT NULL,
        last_seen_at INTEGER NOT NULL
    );
    CREATE TABLE IF NOT EXISTS remote_sessions (
        id TEXT PRIMARY KEY,
        workspace_name TEXT NOT NULL,
        workspace_path TEXT NOT NULL,
        label TEXT NOT NULL,
        description TEXT NOT NULL DEFAULT '',
        status TEXT NOT NULL CHECK (status IN ('active','idle','blocked','closed','archived')),
        owner_principal_id TEXT NOT NULL,
        base_git_head TEXT,
        base_tree_digest TEXT,
        environment_snapshot_id TEXT,
        version INTEGER NOT NULL DEFAULT 1,
        created_at INTEGER NOT NULL,
        last_active_at INTEGER NOT NULL,
        closed_at INTEGER,
        FOREIGN KEY (owner_principal_id) REFERENCES principals(id)
    );
    CREATE TABLE IF NOT EXISTS remote_session_members (
        remote_session_id TEXT NOT NULL,
        principal_id TEXT NOT NULL,
        role TEXT NOT NULL CHECK (role IN ('viewer','editor','approver','owner')),
        joined_at INTEGER NOT NULL,
        last_active_at INTEGER NOT NULL,
        PRIMARY KEY (remote_session_id, principal_id),
        FOREIGN KEY (remote_session_id) REFERENCES remote_sessions(id) ON DELETE CASCADE,
        FOREIGN KEY (principal_id) REFERENCES principals(id) ON DELETE CASCADE
    );
    CREATE TABLE IF NOT EXISTS remote_session_clients (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        remote_session_id TEXT NOT NULL,
        principal_id TEXT NOT NULL,
        client_name TEXT NOT NULL,
        client_version TEXT NOT NULL,
        first_seen_at INTEGER NOT NULL,
        last_seen_at INTEGER NOT NULL,
        UNIQUE (remote_session_id, principal_id, client_name, client_version),
        FOREIGN KEY (remote_session_id) REFERENCES remote_sessions(id) ON DELETE CASCADE
    );
    CREATE TABLE IF NOT EXISTS remote_session_events (
        sequence INTEGER PRIMARY KEY AUTOINCREMENT,
        remote_session_id TEXT NOT NULL,
        principal_id TEXT,
        client_name TEXT,
        event_type TEXT NOT NULL,
        operation_id TEXT,
        summary TEXT NOT NULL,
        resource_uri TEXT,
        metadata_json TEXT NOT NULL DEFAULT '{}',
        created_at INTEGER NOT NULL,
        FOREIGN KEY (remote_session_id) REFERENCES remote_sessions(id) ON DELETE CASCADE
    );
    CREATE TABLE IF NOT EXISTS environment_snapshots (
        id TEXT PRIMARY KEY,
        remote_session_id TEXT,
        runtime_instance_id TEXT NOT NULL,
        static_digest TEXT NOT NULL,
        snapshot_json TEXT NOT NULL,
        created_at INTEGER NOT NULL,
        FOREIGN KEY (remote_session_id) REFERENCES remote_sessions(id) ON DELETE CASCADE
    );
    CREATE TABLE IF NOT EXISTS idempotency_records (
        remote_session_id TEXT NOT NULL,
        principal_id TEXT NOT NULL,
        client_request_id TEXT NOT NULL,
        operation TEXT NOT NULL,
        response_json TEXT NOT NULL,
        created_at INTEGER NOT NULL,
        expires_at INTEGER NOT NULL,
        PRIMARY KEY (remote_session_id, principal_id, client_request_id, operation)
    );
    CREATE INDEX IF NOT EXISTS idx_remote_sessions_workspace_activity
        ON remote_sessions(workspace_name, last_active_at DESC, id DESC);
    CREATE INDEX IF NOT EXISTS idx_remote_members_principal
        ON remote_session_members(principal_id, last_active_at DESC);
    CREATE INDEX IF NOT EXISTS idx_remote_events_session_sequence
        ON remote_session_events(remote_session_id, sequence DESC);`,
	`SELECT 1;`,
	`SELECT 1;`,
	`CREATE TABLE IF NOT EXISTS workspace_baselines (
        remote_session_id TEXT PRIMARY KEY,
        git_head TEXT NOT NULL DEFAULT '',
        captured_at INTEGER NOT NULL,
        FOREIGN KEY (remote_session_id) REFERENCES remote_sessions(id) ON DELETE CASCADE
    );
    CREATE TABLE IF NOT EXISTS workspace_baseline_files (
        remote_session_id TEXT NOT NULL,
        path TEXT NOT NULL,
        status TEXT NOT NULL,
        PRIMARY KEY (remote_session_id, path),
        FOREIGN KEY (remote_session_id) REFERENCES remote_sessions(id) ON DELETE CASCADE
    );
    CREATE INDEX IF NOT EXISTS idx_workspace_baseline_files_session
        ON workspace_baseline_files(remote_session_id, path);`,
	`CREATE TABLE IF NOT EXISTS terminal_tasks (
        id TEXT PRIMARY KEY,
        remote_session_id TEXT NOT NULL,
        workspace_name TEXT NOT NULL,
        workspace_path TEXT NOT NULL,
        command TEXT NOT NULL,
        status TEXT NOT NULL CHECK (status IN ('running','exited','killed','interrupted','failed')),
        pid INTEGER NOT NULL DEFAULT 0,
        exit_code INTEGER,
        log_path TEXT NOT NULL,
        log_size INTEGER NOT NULL DEFAULT 0,
        log_truncated INTEGER NOT NULL DEFAULT 0,
        started_at INTEGER NOT NULL,
        finished_at INTEGER,
        updated_at INTEGER NOT NULL,
        FOREIGN KEY (remote_session_id) REFERENCES remote_sessions(id) ON DELETE CASCADE
    );
    CREATE INDEX IF NOT EXISTS idx_terminal_tasks_session_started
        ON terminal_tasks(remote_session_id, started_at DESC, id DESC);`,
	`CREATE TABLE IF NOT EXISTS approvals (
        id TEXT PRIMARY KEY,
        remote_session_id TEXT NOT NULL,
        principal_id TEXT NOT NULL,
        tool TEXT NOT NULL,
        summary TEXT NOT NULL,
        payload_json TEXT NOT NULL,
        status TEXT NOT NULL CHECK (status IN ('pending','consumed','expired')),
        created_at INTEGER NOT NULL,
        expires_at INTEGER NOT NULL,
        consumed_at INTEGER,
        FOREIGN KEY (remote_session_id) REFERENCES remote_sessions(id) ON DELETE CASCADE
    );
    CREATE INDEX IF NOT EXISTS idx_approvals_remote_pending
        ON approvals(remote_session_id, created_at DESC) WHERE status = 'pending';`,
	`CREATE TABLE IF NOT EXISTS file_snapshots (
        id TEXT PRIMARY KEY,
        remote_session_id TEXT NOT NULL,
        snapshot_json TEXT NOT NULL,
        created_at INTEGER NOT NULL,
        FOREIGN KEY (remote_session_id) REFERENCES remote_sessions(id) ON DELETE CASCADE
    );
    CREATE INDEX IF NOT EXISTS idx_file_snapshots_session_created
        ON file_snapshots(remote_session_id, created_at DESC);`,
	`CREATE TABLE IF NOT EXISTS secret_requests (
        id TEXT PRIMARY KEY,
        remote_session_id TEXT NOT NULL,
        principal_id TEXT NOT NULL,
        payload_json TEXT NOT NULL,
        status TEXT NOT NULL CHECK (status IN ('pending','consumed','expired')),
        created_at INTEGER NOT NULL,
        expires_at INTEGER NOT NULL,
        consumed_at INTEGER,
        FOREIGN KEY (remote_session_id) REFERENCES remote_sessions(id) ON DELETE CASCADE
    );
    CREATE INDEX IF NOT EXISTS idx_secret_requests_remote_pending
        ON secret_requests(remote_session_id, created_at DESC) WHERE status = 'pending';`,
	`CREATE TABLE IF NOT EXISTS artifacts (
        id TEXT PRIMARY KEY,
        remote_session_id TEXT NOT NULL,
        name TEXT NOT NULL,
        kind TEXT NOT NULL,
        path TEXT NOT NULL,
        mime_type TEXT NOT NULL,
        source_encoding TEXT NOT NULL CHECK (source_encoding IN ('utf-8','utf-16le','utf-16be','binary','unknown')),
        source_bom TEXT NOT NULL,
        size INTEGER NOT NULL,
        sha256 TEXT NOT NULL,
        created_by TEXT NOT NULL,
        created_at INTEGER NOT NULL,
        FOREIGN KEY (remote_session_id) REFERENCES remote_sessions(id) ON DELETE CASCADE
    );
    CREATE INDEX IF NOT EXISTS idx_artifacts_session_created
        ON artifacts(remote_session_id, created_at DESC, id DESC);`,
	`CREATE TABLE IF NOT EXISTS plans (
        id TEXT PRIMARY KEY,
        remote_session_id TEXT NOT NULL,
        goal TEXT NOT NULL,
        summary TEXT NOT NULL DEFAULT '',
        status TEXT NOT NULL CHECK (status IN ('ready','in_progress','blocked','completed','cancelled')),
        version INTEGER NOT NULL DEFAULT 1,
        created_by TEXT NOT NULL,
        created_at INTEGER NOT NULL,
        updated_at INTEGER NOT NULL,
        completed_at INTEGER,
        FOREIGN KEY (remote_session_id) REFERENCES remote_sessions(id) ON DELETE CASCADE
    );
    CREATE TABLE IF NOT EXISTS plan_tasks (
        id TEXT PRIMARY KEY,
        plan_id TEXT NOT NULL,
        ordinal INTEGER NOT NULL,
        title TEXT NOT NULL,
        description TEXT NOT NULL DEFAULT '',
        status TEXT NOT NULL CHECK (status IN ('todo','in_progress','blocked','completed','skipped')),
        depends_on_json TEXT NOT NULL DEFAULT '[]',
        created_at INTEGER NOT NULL,
        updated_at INTEGER NOT NULL,
        completed_at INTEGER,
        FOREIGN KEY (plan_id) REFERENCES plans(id) ON DELETE CASCADE,
        UNIQUE (plan_id, ordinal)
    );
    CREATE TABLE IF NOT EXISTS plan_task_evidence (
        id TEXT PRIMARY KEY,
        plan_id TEXT NOT NULL,
        task_id TEXT NOT NULL,
        kind TEXT NOT NULL CHECK (kind IN ('read','edit','execute','artifact','source','verification','observe')),
        reference_id TEXT NOT NULL,
        validated INTEGER NOT NULL CHECK (validated IN (0,1)),
        source_event_id TEXT NOT NULL DEFAULT '',
        metadata_json TEXT NOT NULL DEFAULT '{}',
        created_by TEXT NOT NULL,
        created_at INTEGER NOT NULL,
        FOREIGN KEY (plan_id) REFERENCES plans(id) ON DELETE CASCADE,
        FOREIGN KEY (task_id) REFERENCES plan_tasks(id) ON DELETE CASCADE,
        UNIQUE (task_id, kind, reference_id)
    );
    CREATE TABLE IF NOT EXISTS plan_events (
        id TEXT PRIMARY KEY,
        plan_id TEXT NOT NULL,
        task_id TEXT,
        event_type TEXT NOT NULL,
        reason TEXT NOT NULL DEFAULT '',
        payload_json TEXT NOT NULL DEFAULT '{}',
        created_by TEXT NOT NULL,
        created_at INTEGER NOT NULL,
        FOREIGN KEY (plan_id) REFERENCES plans(id) ON DELETE CASCADE,
        FOREIGN KEY (task_id) REFERENCES plan_tasks(id) ON DELETE SET NULL
    );
    CREATE INDEX IF NOT EXISTS idx_plans_session_updated
        ON plans(remote_session_id, updated_at DESC, id DESC);
    CREATE INDEX IF NOT EXISTS idx_plan_tasks_plan_ordinal
        ON plan_tasks(plan_id, ordinal, id);
    CREATE INDEX IF NOT EXISTS idx_plan_evidence_plan_task
        ON plan_task_evidence(plan_id, task_id, created_at, id);
    CREATE INDEX IF NOT EXISTS idx_plan_events_plan_created
        ON plan_events(plan_id, created_at, id);`,
	`ALTER TABLE approvals ADD COLUMN content_key TEXT NOT NULL DEFAULT '';
	CREATE UNIQUE INDEX IF NOT EXISTS idx_approvals_content_key
		ON approvals(remote_session_id, content_key) WHERE status = 'pending' AND content_key <> '';`,
	`CREATE TABLE IF NOT EXISTS observation_events (
		sequence INTEGER PRIMARY KEY AUTOINCREMENT,
		workspace_name TEXT NOT NULL,
		remote_session_id TEXT NOT NULL DEFAULT '',
		request_id TEXT NOT NULL DEFAULT '',
		operation_id TEXT NOT NULL DEFAULT '',
		tool_name TEXT NOT NULL DEFAULT '',
		event_type TEXT NOT NULL,
		intent TEXT NOT NULL DEFAULT '',
		input_json TEXT NOT NULL DEFAULT '{}',
		output_json TEXT NOT NULL DEFAULT '{}',
		summary TEXT NOT NULL DEFAULT '',
		resource_uri TEXT NOT NULL DEFAULT '',
		stream TEXT NOT NULL DEFAULT '',
		stream_offset INTEGER NOT NULL DEFAULT 0,
		truncated INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_observation_events_workspace_sequence
		ON observation_events(workspace_name, sequence);`,
	`ALTER TABLE observation_events ADD COLUMN progress_summary TEXT NOT NULL DEFAULT '';`,
	`CREATE INDEX IF NOT EXISTS idx_observation_events_retention
		ON observation_events(workspace_name, event_type, created_at, sequence);
	CREATE INDEX IF NOT EXISTS idx_terminal_tasks_retention
		ON terminal_tasks(status, finished_at, updated_at, remote_session_id);
	CREATE INDEX IF NOT EXISTS idx_file_snapshots_retention
		ON file_snapshots(created_at, remote_session_id);
	CREATE INDEX IF NOT EXISTS idx_environment_snapshots_retention
		ON environment_snapshots(created_at, remote_session_id);`,
	`SELECT 1;`,
	`ALTER TABLE observation_events ADD COLUMN status TEXT NOT NULL DEFAULT '';
	ALTER TABLE observation_events ADD COLUMN purpose TEXT NOT NULL DEFAULT '';
	ALTER TABLE observation_events ADD COLUMN parent_operation_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE observation_events ADD COLUMN command TEXT NOT NULL DEFAULT '';
	ALTER TABLE observation_events ADD COLUMN working_directory TEXT NOT NULL DEFAULT '';
	ALTER TABLE observation_events ADD COLUMN exit_code INTEGER;
	ALTER TABLE observation_events ADD COLUMN duration_ms INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE observation_events ADD COLUMN skill_name TEXT NOT NULL DEFAULT '';
	ALTER TABLE observation_events ADD COLUMN mcp_server TEXT NOT NULL DEFAULT '';
	ALTER TABLE observation_events ADD COLUMN mcp_tool TEXT NOT NULL DEFAULT '';
	ALTER TABLE observation_events ADD COLUMN path TEXT NOT NULL DEFAULT '';
	CREATE INDEX IF NOT EXISTS idx_observation_events_history_filters
		ON observation_events(workspace_name, remote_session_id, created_at, sequence);`,
	`CREATE TABLE IF NOT EXISTS operations (
		id TEXT PRIMARY KEY,
		remote_session_id TEXT NOT NULL,
		workspace_name TEXT NOT NULL,
		request_id TEXT NOT NULL,
		purpose TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL CHECK (state IN ('queued','running','succeeded','failed','waiting_confirmation','interrupted','cancelled')),
		result_json TEXT NOT NULL DEFAULT '{}',
		error_json TEXT NOT NULL DEFAULT '{}',
		created_at INTEGER NOT NULL,
		started_at INTEGER,
		completed_at INTEGER,
		expires_at INTEGER NOT NULL,
		FOREIGN KEY (remote_session_id) REFERENCES remote_sessions(id) ON DELETE CASCADE
	);
	CREATE TABLE IF NOT EXISTS operation_steps (
		operation_id TEXT NOT NULL,
		step_id TEXT NOT NULL,
		tool_name TEXT NOT NULL,
		arguments_json TEXT NOT NULL DEFAULT '{}',
		depends_on_json TEXT NOT NULL DEFAULT '[]',
		exclusive INTEGER NOT NULL DEFAULT 1,
		state TEXT NOT NULL CHECK (state IN ('queued','running','succeeded','failed','waiting_confirmation','interrupted','cancelled','skipped')),
		request_id TEXT NOT NULL,
		result_json TEXT NOT NULL DEFAULT '{}',
		error_json TEXT NOT NULL DEFAULT '{}',
		confirmation_token TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		started_at INTEGER,
		completed_at INTEGER,
		PRIMARY KEY (operation_id, step_id),
		FOREIGN KEY (operation_id) REFERENCES operations(id) ON DELETE CASCADE
	);
	CREATE INDEX IF NOT EXISTS idx_operations_session_created
		ON operations(remote_session_id, created_at DESC, id DESC);
	CREATE INDEX IF NOT EXISTS idx_operations_state
		ON operations(state, created_at);
	CREATE INDEX IF NOT EXISTS idx_operation_steps_state
		ON operation_steps(operation_id, state, step_id);
	CREATE INDEX IF NOT EXISTS idx_operations_retention
		ON operations(state, expires_at, remote_session_id);`,
	`ALTER TABLE observation_events ADD COLUMN step_id TEXT NOT NULL DEFAULT '';`,
	`SELECT 1;`,
	`CREATE TABLE IF NOT EXISTS clean_idempotency_records (
		remote_session_id TEXT NOT NULL,
		principal_id TEXT NOT NULL,
		operation TEXT NOT NULL,
		idempotency_key TEXT NOT NULL,
		fingerprint TEXT NOT NULL,
		state TEXT NOT NULL CHECK (state IN ('pending','succeeded','failed','in_doubt')),
		response_json TEXT NOT NULL DEFAULT '{}',
		metadata_json TEXT NOT NULL DEFAULT '{}',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		PRIMARY KEY (remote_session_id, principal_id, operation, idempotency_key)
	);
	CREATE INDEX IF NOT EXISTS idx_clean_idempotency_expiry
		ON clean_idempotency_records(expires_at, state);
	CREATE INDEX IF NOT EXISTS idx_clean_idempotency_session
		ON clean_idempotency_records(remote_session_id, principal_id, operation, updated_at);`,
	`CREATE TABLE IF NOT EXISTS clean_edit_records (
		id TEXT PRIMARY KEY,
		remote_session_id TEXT NOT NULL,
		principal_id TEXT NOT NULL,
		state TEXT NOT NULL CHECK (state IN ('pending','succeeded','in_doubt')),
		result_json TEXT NOT NULL DEFAULT '{}',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_clean_edit_records_session
		ON clean_edit_records(remote_session_id, created_at DESC, id);
	CREATE INDEX IF NOT EXISTS idx_clean_edit_records_expiry
		ON clean_edit_records(expires_at, state);`,
	`ALTER TABLE observation_events ADD COLUMN call_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE observation_events ADD COLUMN phase TEXT NOT NULL DEFAULT '';
	CREATE INDEX IF NOT EXISTS idx_observation_events_correlation
		ON observation_events(workspace_name, call_id, sequence);`,
	`ALTER TABLE observation_events ADD COLUMN goal TEXT NOT NULL DEFAULT '';
	ALTER TABLE observation_events ADD COLUMN reasoning_summary TEXT NOT NULL DEFAULT '';
	ALTER TABLE observation_events ADD COLUMN next_step TEXT NOT NULL DEFAULT '';
	ALTER TABLE observation_events ADD COLUMN plan_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE observation_events ADD COLUMN plan_task_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE observation_events ADD COLUMN execution_task_id TEXT NOT NULL DEFAULT '';`,
	`CREATE TABLE IF NOT EXISTS delete_requests (
		id TEXT PRIMARY KEY,
		remote_session_id TEXT NOT NULL,
		principal_id TEXT NOT NULL,
		workspace_name TEXT NOT NULL,
		workspace_path TEXT NOT NULL,
		purpose TEXT NOT NULL,
		idempotency_key TEXT NOT NULL,
		manifest_json TEXT NOT NULL,
		manifest_sha256 TEXT NOT NULL,
		status TEXT NOT NULL CHECK (status IN ('prepared','committing','committed','partial','failed','expired')),
		approval_receipt_hash TEXT NOT NULL DEFAULT '',
		result_json TEXT NOT NULL DEFAULT '{}',
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		committed_at INTEGER,
		FOREIGN KEY (remote_session_id) REFERENCES remote_sessions(id) ON DELETE CASCADE
	);
	CREATE INDEX IF NOT EXISTS idx_delete_requests_idempotency
		ON delete_requests(remote_session_id, principal_id, idempotency_key, created_at DESC);
	CREATE UNIQUE INDEX IF NOT EXISTS uq_delete_requests_idempotency
		ON delete_requests(remote_session_id, principal_id, idempotency_key);
	CREATE INDEX IF NOT EXISTS idx_delete_requests_expiry
		ON delete_requests(status, expires_at);`,
	`DROP TABLE IF EXISTS change_journals;
	DROP TABLE IF EXISTS changeset_files;
	DROP TABLE IF EXISTS changesets;`,
	`ALTER TABLE observation_events ADD COLUMN turn_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE observation_events ADD COLUMN activity_sequence INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE observation_events ADD COLUMN activity_kind TEXT NOT NULL DEFAULT '';
	ALTER TABLE observation_events ADD COLUMN related_call_id TEXT NOT NULL DEFAULT '';
	CREATE INDEX IF NOT EXISTS idx_observation_events_activity
		ON observation_events(remote_session_id, turn_id, activity_sequence) WHERE event_type = 'agent.activity';
	CREATE TABLE IF NOT EXISTS agent_activity_turns (
		remote_session_id TEXT NOT NULL,
		turn_id TEXT NOT NULL,
		sequence INTEGER NOT NULL,
		state TEXT NOT NULL,
		kind TEXT NOT NULL,
		summary TEXT NOT NULL DEFAULT '',
		related_call_id TEXT NOT NULL DEFAULT '',
		persisted_at INTEGER NOT NULL DEFAULT 0,
		state_since INTEGER NOT NULL,
		seen_at INTEGER NOT NULL,
		PRIMARY KEY (remote_session_id, turn_id),
		FOREIGN KEY (remote_session_id) REFERENCES remote_sessions(id) ON DELETE CASCADE
	);
	CREATE INDEX IF NOT EXISTS idx_agent_activity_turns_session_seen
		ON agent_activity_turns(remote_session_id, seen_at DESC);`,
	`ALTER TABLE terminal_tasks ADD COLUMN limit_reason TEXT NOT NULL DEFAULT '';`,
	`ALTER TABLE operations ADD COLUMN state_sequence INTEGER NOT NULL DEFAULT 1;
	ALTER TABLE operations ADD COLUMN run_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE operations ADD COLUMN submission_sha256 TEXT NOT NULL DEFAULT '';
	UPDATE operations SET run_id=id;
	CREATE TABLE operation_submissions (
		operation_id TEXT PRIMARY KEY,
		remote_session_id TEXT NOT NULL,
		submission_sha256 TEXT NOT NULL,
		FOREIGN KEY(remote_session_id) REFERENCES remote_sessions(id) ON DELETE CASCADE
	);
	INSERT INTO operation_submissions SELECT id, remote_session_id, submission_sha256 FROM operations;
	CREATE TABLE workspace_run_identities (
		run_id TEXT PRIMARY KEY,
		remote_session_id TEXT NOT NULL,
		identity_json TEXT NOT NULL,
		version INTEGER NOT NULL DEFAULT 1,
		FOREIGN KEY(remote_session_id) REFERENCES remote_sessions(id) ON DELETE CASCADE
	);
	CREATE TABLE workspace_identity_transitions (
		run_id TEXT NOT NULL,
		operation_id TEXT NOT NULL,
		remote_session_id TEXT NOT NULL,
		before_json TEXT NOT NULL,
		after_json TEXT NOT NULL,
		command TEXT NOT NULL,
		task_id TEXT NOT NULL DEFAULT '',
		phase TEXT NOT NULL DEFAULT 'pending',
		PRIMARY KEY(run_id,operation_id),
		FOREIGN KEY(run_id) REFERENCES workspace_run_identities(run_id) ON DELETE CASCADE
	);
	CREATE UNIQUE INDEX workspace_one_pending_transition ON workspace_identity_transitions(run_id) WHERE phase='pending';
	ALTER TABLE operations ADD COLUMN state_event_id TEXT NOT NULL DEFAULT '';
	UPDATE operations SET state_event_id = 'operation:' || hex(id) || ':1';
	CREATE TRIGGER operations_initial_state_event AFTER INSERT ON operations
	BEGIN
		UPDATE operations SET state_event_id = 'operation:' || hex(NEW.id) || ':1', state_sequence = 1 WHERE id = NEW.id;
	END;
	CREATE TRIGGER operations_state_event AFTER UPDATE OF state ON operations
	WHEN NEW.state != OLD.state
	BEGIN
		UPDATE operations SET state_sequence = OLD.state_sequence + 1,
			state_event_id = 'operation:' || hex(NEW.id) || ':' || (OLD.state_sequence + 1)
			WHERE id = NEW.id;
	END;
	CREATE TRIGGER operations_terminal_immutable BEFORE UPDATE OF state ON operations
	WHEN OLD.state IN ('succeeded','failed','interrupted','cancelled') AND NEW.state != OLD.state
	BEGIN
		SELECT RAISE(ABORT, 'operation terminal state is immutable');
	END;
	CREATE TRIGGER operation_steps_initial_version AFTER INSERT ON operation_steps
	BEGIN
		UPDATE operations SET state_sequence = state_sequence + 1,
			state_event_id = 'operation:' || hex(id) || ':' || (state_sequence + 1)
			WHERE id = NEW.operation_id AND state NOT IN ('succeeded','failed','interrupted','cancelled');
	END;
	CREATE TRIGGER operation_steps_changed_version AFTER UPDATE ON operation_steps
	WHEN NEW.state != OLD.state OR NEW.result_json != OLD.result_json OR NEW.error_json != OLD.error_json
		OR NEW.confirmation_token != OLD.confirmation_token
	BEGIN
		UPDATE operations SET state_sequence = state_sequence + 1,
			state_event_id = 'operation:' || hex(id) || ':' || (state_sequence + 1)
			WHERE id = NEW.operation_id AND state NOT IN ('succeeded','failed','interrupted','cancelled');
	END;`,
	`CREATE TABLE artifact_blobs (
		artifact_id TEXT PRIMARY KEY,
		content BLOB NOT NULL,
		FOREIGN KEY (artifact_id) REFERENCES artifacts(id) ON DELETE CASCADE
	);`,
}

func applyMigrations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
        version INTEGER PRIMARY KEY,
        applied_at INTEGER NOT NULL
    )`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	for i, migration := range migrations {
		version := i + 1
		var exists int
		err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = ?", version).Scan(&exists)
		if err != nil {
			return fmt.Errorf("read migration %d: %w", version, err)
		}
		if exists != 0 {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, migration); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)", version, time.Now().UTC().UnixMilli()); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", version, err)
		}
	}
	return nil
}
