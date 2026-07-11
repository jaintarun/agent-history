package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestSQLiteDriverSupportsFTS5AndWAL(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "compat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var journalMode string
	if err := db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&journalMode); err != nil {
		t.Fatalf("enable WAL: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal mode = %q, want wal", journalMode)
	}

	if _, err := db.Exec(`CREATE VIRTUAL TABLE compatibility_fts USING fts5(body)`); err != nil {
		t.Fatalf("create FTS5 table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO compatibility_fts(body) VALUES ('searchable text')`); err != nil {
		t.Fatalf("insert FTS5 row: %v", err)
	}
	var body string
	if err := db.QueryRow(`SELECT body FROM compatibility_fts WHERE compatibility_fts MATCH 'searchable'`).Scan(&body); err != nil {
		t.Fatalf("query FTS5 row: %v", err)
	}
	if body != "searchable text" {
		t.Fatalf("FTS5 body = %q, want searchable text", body)
	}
}
