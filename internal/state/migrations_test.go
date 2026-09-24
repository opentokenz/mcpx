package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestLatestMigrationAddsTerminalTaskLimitReason(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mcpx.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	if err := applyMigrations(context.Background(), db); err != nil {
		t.Fatal(err)
	}

	var (
		name       string
		typeName   string
		notNull    int
		defaultVal sql.NullString
	)
	if err := db.QueryRow(`SELECT name, type, "notnull", dflt_value FROM pragma_table_info('terminal_tasks') WHERE name = 'limit_reason'`).Scan(&name, &typeName, &notNull, &defaultVal); err != nil {
		t.Fatal(err)
	}
	if name != "limit_reason" || typeName != "TEXT" || notNull != 1 || !defaultVal.Valid || defaultVal.String != "''" {
		t.Fatalf("limit_reason column = name=%q type=%q not_null=%d default=%v", name, typeName, notNull, defaultVal)
	}
}

func TestArtifactBlobMigrationUpgradesExistingArtifacts(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i, migration := range migrations[:len(migrations)-1] {
		if _, err := db.Exec(migration); err != nil {
			t.Fatalf("old schema %d: %v", i+1, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations VALUES (?, 0)`, i+1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON;
		INSERT INTO principals(id,kind,subject_hash,created_at,last_seen_at) VALUES('principal','test','hash',1,1);
		INSERT INTO remote_sessions(id,workspace_name,workspace_path,label,description,status,owner_principal_id,version,created_at,last_active_at)
		VALUES('session','fixture','fixture','fixture','','active','principal',1,1,1);
		INSERT INTO artifacts(id,remote_session_id,name,kind,path,mime_type,source_encoding,source_bom,size,sha256,created_by,created_at)
		VALUES('artifact','session','report','other','report.bin','application/octet-stream','binary','none',3,'sha256:test','principal',1);`); err != nil {
		t.Fatal(err)
	}

	if err := applyMigrations(context.Background(), db); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM artifacts WHERE id='artifact'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("existing artifact count=%d err=%v", count, err)
	}
	if _, err := db.Exec(`INSERT INTO artifact_blobs(artifact_id, content) VALUES('artifact', x'010203')`); err != nil {
		t.Fatalf("artifact_blobs unavailable after migration: %v", err)
	}
	var size int
	if err := db.QueryRow(`SELECT length(content) FROM artifact_blobs WHERE artifact_id='artifact'`).Scan(&size); err != nil || size != 3 {
		t.Fatalf("blob size=%d err=%v", size, err)
	}
}
