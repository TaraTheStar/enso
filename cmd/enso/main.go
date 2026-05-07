// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/daemon"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/session"
	"github.com/TaraTheStar/enso/internal/tui"
	"github.com/spf13/cobra"
)

// initLogging routes slog away from stderr (which would corrupt the TUI) to
// ~/.enso/enso.log. Level is INFO by default, DEBUG when --debug or
// ENSO_DEBUG is set. When debug is on, also opens ~/.enso/debug.log for
// raw SSE chunk dumps via llm.SetDebug.
func initLogging() {
	envDebug := os.Getenv("ENSO_DEBUG")
	envOn := envDebug != "" && envDebug != "0" && !strings.EqualFold(envDebug, "false")
	debugOn := flagDebug || envOn

	level := slog.LevelInfo
	if debugOn {
		level = slog.LevelDebug
	}

	var w io.Writer = io.Discard
	if home, err := os.UserHomeDir(); err == nil {
		dir := filepath.Join(home, ".enso")
		// 0700: ~/.enso/ holds the daemon socket, session DB, trust
		// store, and debug log. Restricting to owner is the primary
		// defence against a same-host attacker connecting to the socket
		// or reading session contents.
		if err := os.MkdirAll(dir, 0o700); err == nil {
			// MkdirAll won't tighten an existing dir; clamp on startup
			// so installs pre-dating the 0700 tightening get upgraded.
			_ = os.Chmod(dir, 0o700)
			if f, err := os.OpenFile(filepath.Join(dir, "enso.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				w = f
			}
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})))

	if debugOn {
		path := envDebug
		if !envOn || envDebug == "1" || strings.EqualFold(envDebug, "true") {
			home, _ := os.UserHomeDir()
			path = filepath.Join(home, ".enso", "debug.log")
		}
		if err := llm.SetDebug(path); err != nil {
			slog.Warn("debug log", "err", err)
		}
	}
}

var (
	flagYolo         bool
	flagSession      string
	flagResume       string
	flagContinue     bool
	flagEphemeral    bool
	flagMaxTurns     int
	flagWorkflow     string
	flagProvider     string
	flagDetach       bool
	flagConfig       string
	flagDebug        bool
	flagFormat       string
	flagAgent        string
	flagWorktree     bool
	flagTrustProject bool
)

func tuiOptions() tui.Options {
	return tui.Options{
		Yolo:      flagYolo,
		Session:   flagSession,
		Ephemeral: flagEphemeral,
		MaxTurns:  flagMaxTurns,
		Config:    flagConfig,
		Agent:     flagAgent,
	}
}

var rootCmd = &cobra.Command{
	Use:   "enso",
	Short: "enso — TUI agentic coding agent",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		initLogging()
		if err := resolveSessionFlags(); err != nil {
			return err
		}
		if err := ensureProjectTrust(cmd); err != nil {
			return err
		}
		// Only commands that actually run an agent in this process
		// honour --worktree. Daemon / attach run elsewhere; fork /
		// export / stats / config don't run an agent at all.
		if flagWorktree {
			switch cmd.Name() {
			case "enso", "tui", "run":
				if err := setupWorktree(); err != nil {
					return err
				}
			}
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// Default to tui subcommand
		return tui.Run(tuiOptions())
	},
}

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Launch the interactive TUI",
	RunE: func(cmd *cobra.Command, args []string) error {
		// First-run gate: if no config exists, print the welcome to
		// stderr and exit cleanly instead of dropping into a TUI that
		// can't reach any model.
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get cwd: %w", err)
		}
		if _, err := loadOrWelcome(cwd); err != nil {
			return err
		}
		return tui.Run(tuiOptions())
	},
}

var runCmd = &cobra.Command{
	Use:   "run [prompt]",
	Short: "Run a non-interactive prompt (streams to stdout, exits when settled)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if flagDetach {
			return runDetached(args)
		}
		if flagWorkflow != "" {
			return runWorkflow(flagWorkflow, args)
		}
		return runOnce(args)
	},
}

var flagDaemonDetach bool

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Run the enso daemon (long-lived agent server on a unix socket)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if flagDaemonDetach {
			return spawnDetachedDaemon()
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			<-sigCh
			cancel()
		}()
		return daemon.Run(ctx, flagConfig)
	},
}

var forkCmd = &cobra.Command{
	Use:   "fork <session-id>",
	Short: "Create a new session with a copy of the source's messages; prints the new id",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := session.Open()
		if err != nil {
			return fmt.Errorf("open session store: %w", err)
		}
		defer store.Close()
		newID, err := session.Fork(store, args[0])
		if err != nil {
			return err
		}
		fmt.Println(newID)
		return nil
	},
}

var flagStatsDays int

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Print a summary of session / message / tool activity",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStats(flagStatsDays)
	},
}

var flagExportOut string

var exportCmd = &cobra.Command{
	Use:   "export <session-id>",
	Short: "Export a session transcript as markdown",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runExport(args[0], flagExportOut)
	},
}

var attachCmd = &cobra.Command{
	Use:   "attach [session-id]",
	Short: "Attach to a running session in the daemon (omit id to pick from a list)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return pickAndAttach()
		}
		return tui.RunAttached(args[0])
	},
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&flagYolo, "yolo", false, "auto-allow all tool calls (no permission prompts)")
	rootCmd.PersistentFlags().StringVar(&flagSession, "session", "", "resume the session with this id (default: new session)")
	rootCmd.PersistentFlags().StringVar(&flagResume, "resume", "", "alias for --session: resume the session with this id")
	rootCmd.PersistentFlags().BoolVar(&flagContinue, "continue", false, "resume the most recently updated session")
	rootCmd.PersistentFlags().BoolVar(&flagEphemeral, "ephemeral", false, "do not persist this session to ~/.enso/enso.db")
	rootCmd.PersistentFlags().IntVar(&flagMaxTurns, "max-turns", 0, "stop after this many tool/chat turns per user message (0 = default 50)")
	rootCmd.PersistentFlags().StringVarP(&flagConfig, "config", "c", "", "additional config file to layer on top of /etc, user, and project defaults")
	rootCmd.PersistentFlags().BoolVar(&flagDebug, "debug", false, "log raw SSE chunks and request bodies to ~/.enso/debug.log; bumps slog level to DEBUG")
	rootCmd.PersistentFlags().StringVar(&flagAgent, "agent", "", "select a declarative agent profile by name (built-in: \"plan\"; user/project: ~/.enso/agents/<name>.md or ./.enso/agents/<name>.md)")
	rootCmd.PersistentFlags().BoolVar(&flagWorktree, "worktree", false, "create a fresh git worktree at ~/.enso/worktrees/<repo>-<rand> on a new branch and run from there (requires git)")
	rootCmd.PersistentFlags().BoolVar(&flagTrustProject, "trust-project", false, "skip the project-config trust prompt this run (also: ENSO_TRUST_PROJECT=1)")
	runCmd.Flags().StringVar(&flagWorkflow, "workflow", "", "run a declarative workflow by name (looks in ./.enso/workflows/<name>.md and ~/.enso/workflows/<name>.md)")
	runCmd.Flags().StringVar(&flagProvider, "provider", "", "use this provider for this run (overrides default_provider; takes effect on resumes too)")
	runCmd.Flags().BoolVar(&flagDetach, "detach", false, "submit the prompt to a running daemon and return the session id (requires `enso daemon` to be running)")
	runCmd.Flags().StringVar(&flagFormat, "format", "text", "output format: text (default, human-readable) or json (newline-delimited bus events)")
	daemonCmd.Flags().BoolVar(&flagDaemonDetach, "detach", false, "fork into the background and exit immediately; child writes to ~/.enso/enso.log")
	exportCmd.Flags().StringVarP(&flagExportOut, "out", "o", "", "write markdown to this path instead of stdout")
	statsCmd.Flags().IntVar(&flagStatsDays, "days", 0, "only count sessions updated within the last N days (0 = all)")
	sandboxCmd.AddCommand(sandboxListCmd, sandboxStopCmd, sandboxRmCmd, sandboxPruneCmd)
	trustCmd.Flags().BoolVar(&flagTrustList, "list", false, "list every trusted project config and exit")
	trustCmd.Flags().BoolVar(&flagTrustRevoke, "revoke", false, "remove the trust entry for [path] (default cwd) and exit")
	rootCmd.AddCommand(tuiCmd, runCmd, daemonCmd, attachCmd, exportCmd, statsCmd, forkCmd, sandboxCmd, trustCmd, versionCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		// First-run welcome already printed itself to stderr; treat as a
		// clean exit so users aren't greeted by a redundant cobra error
		// trailer on top of the welcome.
		if errors.Is(err, errFirstRunWelcome) {
			os.Exit(0)
		}
		os.Exit(1)
	}
}

// resolveSessionFlags collapses --session / --resume / --continue into a
// single flagSession value. The three are mutually exclusive — combining
// them is a usage error. --continue looks up the most-recently-updated
// session in the SQLite store; absence of any prior session is also a
// usage error.
func resolveSessionFlags() error {
	set := 0
	if flagSession != "" {
		set++
	}
	if flagResume != "" {
		set++
	}
	if flagContinue {
		set++
	}
	if set > 1 {
		return errors.New("--session, --resume, and --continue are mutually exclusive")
	}
	if flagResume != "" {
		flagSession = flagResume
	}
	if flagContinue {
		store, err := session.Open()
		if err != nil {
			return fmt.Errorf("--continue: open session store: %w", err)
		}
		defer store.Close()
		recent, err := session.ListRecent(store, 1)
		if err != nil {
			return fmt.Errorf("--continue: list sessions: %w", err)
		}
		if len(recent) == 0 {
			return errors.New("--continue: no sessions to resume")
		}
		flagSession = recent[0].ID
	}
	return nil
}

// ensureProjectTrust runs the project-config trust gate for commands that
// will load <cwd>/.enso/config.toml. Bypasses gate for commands that don't
// touch project config (attach/export/stats/fork/config/trust/sandbox*),
// for `run --detach` (work runs inside the daemon, which gated itself),
// and when the user explicitly opts out via --trust-project /
// ENSO_TRUST_PROJECT.
func ensureProjectTrust(cmd *cobra.Command) error {
	if !commandLoadsProjectConfig(cmd) {
		return nil
	}
	if flagTrustProject {
		return nil
	}
	if v := os.Getenv("ENSO_TRUST_PROJECT"); v != "" && v != "0" && !strings.EqualFold(v, "false") {
		return nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	untrusted, err := config.CheckTrust(cwd)
	if err != nil {
		return fmt.Errorf("trust check: %w", err)
	}
	if len(untrusted) == 0 {
		return nil
	}
	return promptOrRefuseUntrusted(untrusted)
}

func commandLoadsProjectConfig(cmd *cobra.Command) bool {
	switch cmd.Name() {
	case "enso", "tui":
		return true
	case "run":
		// --detach hands the prompt to the daemon, which gated itself
		// when it started.
		return !flagDetach
	case "daemon":
		return true
	default:
		return false
	}
}

func promptOrRefuseUntrusted(untrusted []config.UntrustedConfig) error {
	fmt.Fprintln(os.Stderr, "enso refuses to load an untrusted project config:")
	for _, u := range untrusted {
		fmt.Fprintf(os.Stderr, "  %s\n", u.Path)
		if u.PriorSHA256 != "" {
			fmt.Fprintln(os.Stderr, "    (contents changed since last 'enso trust')")
		}
	}
	fmt.Fprintln(os.Stderr, "Project configs can run arbitrary commands via [hooks], [lsp.*], [mcp.*], etc.")
	fmt.Fprintln(os.Stderr, "Inspect the file(s) above before trusting.")

	if !stdinIsTTY() {
		return errors.New("untrusted project config (run 'enso trust .' or pass --trust-project)")
	}

	fmt.Fprint(os.Stderr, "Trust and continue? [y/N] ")
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("read response: %w", err)
	}
	resp := strings.ToLower(strings.TrimSpace(line))
	if resp != "y" && resp != "yes" {
		return errors.New("aborted: project config not trusted")
	}
	for _, u := range untrusted {
		if err := config.TrustFile(u.Path); err != nil {
			return fmt.Errorf("record trust for %s: %w", u.Path, err)
		}
	}
	return nil
}

func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
