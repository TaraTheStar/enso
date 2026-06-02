// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// applyMigrations runs each embedded *.sql file exactly once per
// database, using SQLite's PRAGMA user_version as the version cursor.
// The version is the migration filename's numeric prefix (`0003_*` → 3),
// NOT its position in the sorted list — so inserting, removing, or
// gapping files can't silently shift every subsequent version and
// re-run or skip a migration against the wrong schema. Migration N runs
// only if user_version < N; each migration's statements AND the
// user_version bump run in ONE transaction, so a failure mid-file rolls
// back cleanly instead of leaving a half-applied schema with a stale
// cursor. Pre-existing databases re-run the early
// CREATE TABLE IF NOT EXISTS migrations harmlessly (the filename numbers
// match the old index+1 scheme, so their user_version carries over).
func applyMigrations(db *sql.DB) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}

	type migration struct {
		version int
		name    string
	}
	migs := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, err := migrationVersion(e.Name())
		if err != nil {
			return err
		}
		migs = append(migs, migration{version: v, name: e.Name()})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	for i := 1; i < len(migs); i++ {
		if migs[i].version == migs[i-1].version {
			return fmt.Errorf("duplicate migration version %d (%s and %s)", migs[i].version, migs[i-1].name, migs[i].name)
		}
	}

	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}

	for _, m := range migs {
		if current >= m.version {
			continue
		}
		data, err := migrationsFS.ReadFile("migrations/" + m.name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", m.name, err)
		}
		if err := applyOneMigration(db, string(data), m.version); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		current = m.version
	}
	return nil
}

// applyOneMigration runs a single migration's body and the matching
// user_version bump inside one transaction, so the pair is atomic: a
// failure anywhere rolls back the whole migration (and the cursor stays
// put), never leaving a half-applied schema. user_version is part of the
// SQLite database header and is itself transactional, so the rollback
// reverts the bump too.
func applyOneMigration(db *sql.DB, body string, version int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit
	if _, err := tx.Exec(body); err != nil {
		return err
	}
	// PRAGMA user_version = N — value must be inlined; placeholders
	// aren't supported on PRAGMA statements. version is an int derived
	// from a trusted embedded filename, so no injection surface.
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return err
	}
	return tx.Commit()
}

// migrationVersion parses the leading numeric prefix of a migration
// filename (`0003_messages_agent_id.sql` → 3). A file without a numeric
// prefix is a programming error in the embedded set, so it's rejected
// loudly rather than silently skipped.
func migrationVersion(name string) (int, error) {
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("migration %q has no numeric version prefix", name)
	}
	return strconv.Atoi(name[:i])
}
