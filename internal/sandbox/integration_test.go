// SPDX-License-Identifier: AGPL-3.0-or-later

package sandbox

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSandboxLifecycleIntegration drives a real podman/docker container
// end-to-end: ensure → exec a benign command → exec a "modify the
// project" command → confirm the host file changed → tear down.
//
// Skipped when neither runtime is on PATH so CI without containers
// stays green; same pattern as the gopls integration test.
func TestSandboxLifecycleIntegration(t *testing.T) {
	if _, err := exec.LookPath("podman"); err != nil {
		if _, err := exec.LookPath("docker"); err != nil {
			t.Skip("neither podman nor docker on PATH; skipping container integration test")
		}
	}

	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "marker.txt"), []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(cwd, Config{
		Runtime: RuntimeAuto,
		Image:   "alpine:latest",
		Init:    []string{"echo init-ran > /tmp/init.flag"},
		Name:    "enso-test-" + filepath.Base(cwd),
		// Leave UID empty: rootless podman maps the host user to the
		// in-container root via user namespaces, which is what we want
		// for bind-mount writes. Setting `--user` on rootless podman
		// breaks bind-mount permissions; on docker (root daemon) it
		// would matter, but the wiring layer decides.
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = mgr.Remove(context.Background())
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var log bytes.Buffer
	if err := mgr.Ensure(ctx, &log); err != nil {
		t.Fatalf("Ensure: %v\nlog:\n%s", err, log.String())
	}

	// Sanity exec: the marker file is visible inside.
	var out bytes.Buffer
	if err := mgr.Exec(ctx, &out, "cat /work/marker.txt"); err != nil {
		t.Fatalf("Exec cat: %v\nout:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "before") {
		t.Errorf("file content = %q, want to contain 'before'", out.String())
	}

	// Init must have run — flag file should exist.
	out.Reset()
	if err := mgr.Exec(ctx, &out, "cat /tmp/init.flag"); err != nil {
		t.Fatalf("init flag missing: %v\nout:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "init-ran") {
		t.Errorf("init flag = %q", out.String())
	}

	// Mutating exec: change the host file via the bind mount.
	out.Reset()
	if err := mgr.Exec(ctx, &out, "echo after > /work/marker.txt"); err != nil {
		t.Fatalf("Exec write: %v\nout:\n%s", err, out.String())
	}
	got, err := os.ReadFile(filepath.Join(cwd, "marker.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "after") {
		t.Errorf("host file = %q, want to contain 'after'", got)
	}

	// Cwd confinement: paths outside /work shouldn't expose the host's
	// filesystem. We write a file outside cwd with a distinctive
	// content marker, then assert the container can't read it via its
	// host path.
	const escapeMarker = "ESCAPE_MARKER_42"
	outsideName := "secret-" + filepath.Base(cwd) + ".txt"
	outsidePath := filepath.Join(filepath.Dir(cwd), outsideName)
	if err := os.WriteFile(outsidePath, []byte(escapeMarker), 0o644); err == nil {
		t.Cleanup(func() { _ = os.Remove(outsidePath) })
		out.Reset()
		err := mgr.Exec(ctx, &out, "cat "+outsidePath+" 2>&1; true")
		if err != nil {
			t.Fatalf("benign cat returned err: %v", err)
		}
		if strings.Contains(out.String(), escapeMarker) {
			t.Errorf("sandbox escape: container could read host-only file %q (output: %s)", outsidePath, out.String())
		}
	}

	// Idempotent re-ensure: same hash → reuse, no recreate.
	log.Reset()
	if err := mgr.Ensure(ctx, &log); err != nil {
		t.Fatalf("re-Ensure: %v", err)
	}
	if strings.Contains(log.String(), "creating") || strings.Contains(log.String(), "recreating") {
		t.Errorf("re-Ensure should be a no-op, log:\n%s", log.String())
	}
}
