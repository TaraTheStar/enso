// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// atomicWriteFile writes data to path via a sibling tempfile + rename
// so a kill mid-write can't leave the destination half-written. The
// tempfile is created in the same directory as path so the rename
// stays on one filesystem (cross-fs renames fail with EXDEV).
//
// We do not use O_NOFOLLOW: H3's confinement check already rejects
// symlinks whose targets escape the workspace, and inside the
// workspace symlinked files (a README symlinked into a project, etc.)
// are legitimate edit targets that O_NOFOLLOW would surprise-block.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	suffix, err := randomSuffix()
	if err != nil {
		return fmt.Errorf("atomic suffix: %w", err)
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp."+suffix)

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("atomic create %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("atomic write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("atomic fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("atomic close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("atomic rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

func randomSuffix() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
