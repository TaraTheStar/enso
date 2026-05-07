// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// applyMigrations runs each embedded *.sql file in lexicographic order
// exactly once per database, using SQLite's PRAGMA user_version as the
// version cursor. Migration N runs only if user_version < N; on success
// user_version is bumped to N. Pre-existing databases (user_version=0)
// re-run the early CREATE TABLE IF NOT EXISTS migrations harmlessly and
// then proceed to anything new.
func applyMigrations(db *sql.DB) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}

	for i, name := range names {
		target := i + 1
		if current >= target {
			continue
		}
		data, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := db.Exec(string(data)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		// PRAGMA user_version = N — value must be inlined; placeholders
		// aren't supported on PRAGMA statements.
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", target)); err != nil {
			return fmt.Errorf("bump user_version after %s: %w", name, err)
		}
	}
	return nil
}
