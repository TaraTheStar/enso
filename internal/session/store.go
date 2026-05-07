// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database for sessions.
type Store struct {
	DB   *sql.DB
	Path string
}

// Open returns a Store backed by ~/.enso/enso.db (created if absent), with
// migrations applied.
func Open() (*Store, error) {
	dir, err := ensoDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create enso dir: %w", err)
	}
	// MkdirAll only sets mode on creation; clamp it on every Open so an
	// install pre-dating the 0700 tightening gets upgraded.
	_ = os.Chmod(dir, 0o700)
	path := filepath.Join(dir, "enso.db")
	return OpenAt(path)
}

// OpenAt opens a Store at the given file path.
func OpenAt(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := applyMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{DB: db, Path: path}, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	if s == nil || s.DB == nil {
		return nil
	}
	return s.DB.Close()
}

// ensoDir returns ~/.enso, creating it if needed.
func ensoDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".enso"), nil
}
