// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openRawDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

func TestMigrationVersion(t *testing.T) {
	cases := map[string]int{
		"0001_init.sql":              1,
		"0003_messages_agent_id.sql": 3,
		"0042_x.sql":                 42,
	}
	for name, want := range cases {
		got, err := migrationVersion(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("%s: got %d, want %d", name, got, want)
		}
	}
	if _, err := migrationVersion("init.sql"); err == nil {
		t.Error("non-numeric prefix should error")
	}
}

// TestApplyMigrations_SetsVersionToHighest applies the embedded set to a
// fresh DB and confirms the cursor lands on the highest filename number
// (not the count) and is idempotent on a second run.
func TestApplyMigrations_SetsVersionToHighest(t *testing.T) {
	db := openRawDB(t)
	if err := applyMigrations(db); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	v1 := userVersion(t, db)
	if v1 == 0 {
		t.Fatal("user_version should be non-zero after migrating")
	}
	// The version must equal the highest migration filename number.
	entries, _ := migrationsFS.ReadDir("migrations")
	highest := 0
	for _, e := range entries {
		if n, err := migrationVersion(e.Name()); err == nil && n > highest {
			highest = n
		}
	}
	if v1 != highest {
		t.Errorf("user_version = %d, want highest filename number %d", v1, highest)
	}

	// Idempotent: a second apply is a no-op and leaves the version put.
	if err := applyMigrations(db); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if v2 := userVersion(t, db); v2 != v1 {
		t.Errorf("re-apply changed version: %d -> %d", v1, v2)
	}
}

// TestApplyOneMigration_RollsBackOnFailure is the transactionality
// regression: a migration whose body fails partway must leave NEITHER a
// half-applied schema NOR a bumped user_version.
func TestApplyOneMigration_RollsBackOnFailure(t *testing.T) {
	db := openRawDB(t)

	// First statement succeeds, second is invalid SQL — the whole tx
	// must roll back.
	body := `CREATE TABLE good (x INTEGER);
THIS IS NOT VALID SQL;`
	err := applyOneMigration(db, body, 1)
	if err == nil {
		t.Fatal("expected error from invalid migration body")
	}

	// user_version must NOT have advanced.
	if v := userVersion(t, db); v != 0 {
		t.Errorf("user_version advanced to %d despite failure", v)
	}
	// The table from the first statement must NOT exist (rolled back).
	var n int
	row := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='good'`)
	if err := row.Scan(&n); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != 0 {
		t.Errorf("table 'good' survived a failed migration (count=%d) — not transactional", n)
	}
}
