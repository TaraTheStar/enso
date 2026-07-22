// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import "embed"

// migrationsFS holds enso's schema migrations. The runner lives in
// azoth/store (Migrate), keyed on SQLite's PRAGMA user_version; enso keeps only
// the embedded files, which OpenAt passes to store.Migrate. Files are named
// NNNN_description.sql, applied once in ascending numeric-prefix order.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS
