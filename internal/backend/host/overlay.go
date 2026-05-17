// SPDX-License-Identifier: AGPL-3.0-or-later

package host

import (
	"context"
	"io"

	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/lima"
	"github.com/TaraTheStar/enso/internal/backend/podman"
	"github.com/TaraTheStar/enso/internal/backend/workspace"
	"github.com/TaraTheStar/enso/internal/config"
)

// SetupWorkspaceOverlay wires the throwaway workspace overlay for the
// chosen backend when `[bash.sandbox_options] workspace = "overlay"`.
// It is the single place this safety-critical wiring lives so the run
// and TUI call sites cannot drift (they previously each carried a
// duplicate podman + lima block).
//
// Returns the overlay (nil when not applicable: local backend, or
// overlay not enabled) for the caller to Resolve at task end —
// interactively for the TUI, non-interactively for `enso run`/daemon.
// Podman gets a fresh per-task temp copy; Lima a stable per-project
// stage dir (so the persistent VM's mount config never changes).
func SetupWorkspaceOverlay(ctx context.Context, b backend.Backend, cfg *config.Config, cwd string, out io.Writer) (*workspace.Overlay, error) {
	if cfg.Bash.Sb.Workspace != "overlay" {
		return nil, nil
	}
	switch be := b.(type) {
	case *podman.Backend:
		ov, err := workspace.New(ctx, cwd)
		if err != nil {
			return nil, err
		}
		be.MountSource = ov.Copy
		return ov, nil
	case *lima.Backend:
		stage, err := lima.StageDir(cwd)
		if err != nil {
			return nil, err
		}
		ov, err := workspace.NewAt(ctx, cwd, stage, out)
		if err != nil {
			return nil, err
		}
		be.MountSource = ov.Copy
		return ov, nil
	default:
		return nil, nil // local backend: nothing to overlay
	}
}
