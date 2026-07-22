// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"database/sql"
	"path/filepath"

	"github.com/TaraTheStar/azoth/store"

	"github.com/TaraTheStar/enso/internal/paths"
)

// Store wraps the SQLite database for sessions.
type Store struct {
	DB   *sql.DB
	Path string
}

// Open returns a Store backed by $XDG_DATA_HOME/enso/enso.db (created if
// absent), with migrations applied.
func Open() (*Store, error) {
	dir, err := paths.DataDir()
	if err != nil {
		return nil, err
	}
	return OpenAt(filepath.Join(dir, "enso.db"))
}

// OpenAt opens a Store at the given file path. The parent directory is created
// (0700) and the shared pragmas + embedded migrations are applied by
// azoth/store; enso keeps only the Store wrapper and its own migrations/ tree.
func OpenAt(path string) (*Store, error) {
	db, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	if err := store.Migrate(db, migrationsFS, "migrations"); err != nil {
		db.Close()
		return nil, err
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
