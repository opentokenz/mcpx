package state

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOpenAppliesMigrationsAndSecuresDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "mcpx.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("database mode %o, want 600", got)
	}
	var version int
	if err := store.DB().QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Fatalf("migration version %d, want %d", version, len(migrations))
	}
	var foreignKeys int
	if err := store.DB().QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys=%d", foreignKeys)
	}
}

func TestBusinessTablesOnlyUseRemoteSessionOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcpx.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for _, table := range []string{"approvals", "secret_requests"} {
		rows, err := store.DB().Query("PRAGMA table_info(" + table + ")")
		if err != nil {
			t.Fatal(err)
		}
		columns := map[string]bool{}
		required := map[string]bool{}
		for rows.Next() {
			var cid, notNull, primaryKey int
			var name, columnType string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			columns[name] = true
			required[name] = notNull == 1
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if columns["transport_session_id"] {
			t.Fatalf("%s must not contain transport_session_id", table)
		}
		if !required["remote_session_id"] || !required["principal_id"] {
			t.Fatalf("%s must require Remote Session and Principal ownership", table)
		}
	}
}
