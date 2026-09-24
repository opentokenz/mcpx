package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOperationIdentityMigrationPreservesExistingTerminal(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	// 从本候选之前的完整 schema 构造升级对象，不能用新库冒充升级。
	for i, migration := range migrations[:len(migrations)-2] {
		if _, err := db.Exec(migration); err != nil {
			t.Fatalf("旧 schema %d: %v", i+1, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations VALUES (?, 0)`, i+1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON;
		INSERT INTO principals(id,kind,subject_hash,created_at,last_seen_at) VALUES('principal','test','hash',1,1);
		INSERT INTO remote_sessions(id,workspace_name,workspace_path,label,description,status,owner_principal_id,version,created_at,last_active_at)
		VALUES('session','fixture','fixture','fixture','','active','principal',1,1,1);
		INSERT INTO operations(id,remote_session_id,workspace_name,request_id,purpose,state,result_json,error_json,created_at,expires_at)
		VALUES('legacy','session','fixture','req','迁移测试','succeeded','{"preserved":true}','{}',1,9999999999999);`); err != nil {
		t.Fatal(err)
	}
	if err := applyMigrations(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var sequence int64
	var eventID, result, state string
	if err := db.QueryRow(`SELECT state_sequence,state_event_id,result_json,state FROM operations WHERE id='legacy'`).Scan(&sequence, &eventID, &result, &state); err != nil {
		t.Fatal(err)
	}
	if sequence != 1 || eventID == "" || result != `{"preserved":true}` || state != "succeeded" {
		t.Fatalf("迁移改变既有结果: %d %q %q %q", sequence, eventID, result, state)
	}
	if _, err := db.Exec(`UPDATE operations SET state='running' WHERE id='legacy'`); err == nil {
		t.Fatal("迁移后既有终态仍能回退")
	}
	if err := applyMigrations(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var again string
	if err := db.QueryRow(`SELECT state_event_id FROM operations WHERE id='legacy'`).Scan(&again); err != nil || again != eventID {
		t.Fatalf("重开改变终态 ID: %q %v", again, err)
	}
}
