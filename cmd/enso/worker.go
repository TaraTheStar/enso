// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/worker"
	"github.com/spf13/cobra"
)

// workerCmd is the hidden worker-side entrypoint. It is never typed by
// users: a Backend launches `enso __worker` as the agent-core child
// process and drives it entirely over the Channel. Reusing the enso
// binary (rather than shipping a second artifact) is a locked decision;
// the unused TUI/cobra surface is inert here because only this command
// runs in worker mode.
//
// Transport for LocalBackend is the process's own stdio: the host
// writes envelopes to the child's stdin and reads them from its stdout.
// Nothing else may write to stdout in worker mode — slog is already
// routed to a file by initLogging, and the run/tui paths are never
// entered.
var workerCmd = &cobra.Command{
	Use:    "__worker",
	Short:  "internal: agent-core worker process (do not invoke directly)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			<-sigCh
			cancel()
		}()

		ch := backend.NewStreamChannelRW(os.Stdin, os.Stdout, stdioCloser{})
		return worker.Serve(ctx, ch, worker.RunAgent)
	},
}

// stdioCloser is the Channel closer for the stdio transport. Closing
// the process's stdin/stdout on teardown is undesirable (the runtime
// owns them and the process is exiting anyway), so Close is a no-op;
// the channel dies with the process.
type stdioCloser struct{}

func (stdioCloser) Close() error { return nil }

func init() {
	rootCmd.AddCommand(workerCmd)
}
