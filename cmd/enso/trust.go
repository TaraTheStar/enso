// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/TaraTheStar/enso/internal/config"
	"github.com/spf13/cobra"
)

var (
	flagTrustList   bool
	flagTrustRevoke bool
)

var trustCmd = &cobra.Command{
	Use:   "trust [path]",
	Short: "Trust a project's .enso/config.toml so enso will load it",
	Long: `Mark <path>/.enso/config.toml as trusted (default <path>: cwd).

A project config can introduce arbitrary command execution via [hooks],
[lsp.*].command, [mcp.*].command, override providers/api_key, disable the
bash sandbox, mount the host root, etc. enso refuses to load an
unfamiliar project config until the user explicitly trusts it.

Trust is recorded as a SHA-256 of the file's contents in
$XDG_STATE_HOME/enso/trust.json. If the file is later modified, enso will re-prompt.

  enso trust            # trust ./.enso/config.toml
  enso trust ../foo     # trust ../foo/.enso/config.toml
  enso trust --revoke   # forget the trust entry for ./.enso/config.toml
  enso trust --list     # show all trusted entries`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if flagTrustList {
			return runTrustList()
		}
		path := "."
		if len(args) == 1 {
			path = args[0]
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", path, err)
		}
		if flagTrustRevoke {
			return runTrustRevoke(abs)
		}
		return runTrustAdd(abs)
	},
}

func runTrustAdd(cwd string) error {
	trusted, err := config.TrustProjectTier(cwd)
	if err != nil {
		return err
	}
	if len(trusted) == 0 {
		fmt.Fprintf(os.Stderr, "no project config found under %s/.enso/\n", cwd)
		return nil
	}
	for _, p := range trusted {
		fmt.Println("trusted", p)
	}
	return nil
}

func runTrustRevoke(cwd string) error {
	any := false
	for _, p := range []string{filepath.Join(cwd, ".enso", "config.toml")} {
		ok, err := config.RevokeTrust(p)
		if err != nil {
			return err
		}
		if ok {
			fmt.Println("revoked", p)
			any = true
		}
	}
	if !any {
		fmt.Fprintf(os.Stderr, "no trust entries to revoke under %s/.enso/\n", cwd)
	}
	return nil
}

func runTrustList() error {
	entries, err := config.ListTrusted()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "no trusted project configs")
		return nil
	}
	for _, e := range entries {
		fmt.Printf("%s  %s  %s\n", e.TrustedAt.Format("2006-01-02"), e.SHA256[:12], e.Path)
	}
	return nil
}
