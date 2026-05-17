// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/TaraTheStar/enso/internal/llm"
)

// TestConcurrentStoreWriters mirrors the Backend-seam reality under
// LocalBackend: the worker process and the host process each hold an
// independent connection to the SAME sqlite file — the worker appends
// messages, the host appends audit events — and they can race. WAL +
// busy_timeout(5000) (see store.go DSN) is supposed to absorb that.
// This is the regression guard for "two writers, one db file": if a
// future DSN/schema change drops WAL or the timeout, interleaved writes
// start returning "database is locked" and this fails.
func TestConcurrentStoreWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")

	// Two independent opens == two processes (host + worker).
	hostStore, err := OpenAt(path)
	if err != nil {
		t.Fatalf("open host store: %v", err)
	}
	defer hostStore.Close()
	workerStore, err := OpenAt(path)
	if err != nil {
		t.Fatalf("open worker store: %v", err)
	}
	defer workerStore.Close()

	// Host owns row creation (as in the real seam wiring).
	const sid = "race-1"
	if _, err := NewSessionWithID(hostStore, sid, "m", "p", t.TempDir()); err != nil {
		t.Fatalf("create session: %v", err)
	}

	msgW, err := AttachWriter(workerStore, sid) // worker: messages
	if err != nil {
		t.Fatalf("worker attach: %v", err)
	}
	evW, err := AttachWriter(hostStore, sid) // host: audit events
	if err != nil {
		t.Fatalf("host attach: %v", err)
	}

	const n = 60
	var wg sync.WaitGroup
	errCh := make(chan error, 2*n)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			if err := msgW.AppendMessage(llm.Message{Role: "user", Content: "ping"}, ""); err != nil {
				errCh <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			if err := evW.AppendEvent("ToolCallEnd", map[string]any{"i": i}); err != nil {
				errCh <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent write failed (WAL/busy_timeout regression?): %v", err)
	}
}
