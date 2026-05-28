// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/TaraTheStar/enso/internal/config"
)

var (
	flagInitPrint   bool
	flagInitForce   bool
	flagInitPath    string
	flagInitWizard  bool
	flagInitProject bool
	flagInitLang    string
	flagInitBackend string
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Inspect or initialize enso configuration",
}

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Write the default config to the user config path (or --path)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if flagInitProject {
			return runProjectInit()
		}
		// --print is a pure stdout dump; never touches disk.
		if flagInitPrint {
			fmt.Print(config.DefaultTOML())
			return nil
		}
		path := flagInitPath
		if path == "" {
			p, err := config.UserConfigPath()
			if err != nil {
				return err
			}
			path = p
		}
		if _, err := os.Stat(path); err == nil && !flagInitForce {
			return fmt.Errorf("%s already exists (pass --force to overwrite)", path)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create dir: %w", err)
		}
		// Config file can hold api_key — clamp parent dir mode in case
		// it predates the 0700 tightening.
		_ = os.Chmod(filepath.Dir(path), 0o700)

		// --wizard runs the interactive prompt flow, building the
		// config from the user's preset choice instead of writing the
		// embedded default verbatim. Same path-resolution + file-mode
		// guarantees as the default-write branch above.
		body := config.DefaultTOML()
		if flagInitWizard {
			_, w, err := config.RunWizard(os.Stdin, os.Stdout)
			if err != nil {
				return fmt.Errorf("wizard: %w", err)
			}
			body = w
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		fmt.Printf("wrote config to %s\n", path)
		return nil
	},
}

// runProjectInit scaffolds <cwd>/.enso/config.toml with backend
// environment defaults tuned to the detected language. Prompts the
// user under a TTY; emits a fixed result under pipes/CI. Honors the
// existing --print / --force / --path / --lang / --backend flags.
func runProjectInit() error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	path := flagInitPath
	if path == "" {
		path = filepath.Join(cwd, ".enso", "config.toml")
	}
	// Refuse to clobber BEFORE prompting, so an interactive user
	// who can't proceed doesn't waste time answering questions.
	// --print short-circuits below and never writes.
	if !flagInitPrint {
		if _, err := os.Stat(path); err == nil && !flagInitForce {
			return fmt.Errorf("%s already exists (pass --force to overwrite)", path)
		}
	}
	// Suppress prompts when the caller has already pinned everything
	// via flags — at that point there's nothing left to ask.
	interactive := !flagInitPrint && stdinIsTTY() && (flagInitLang == "" || flagInitBackend == "")
	opts := config.ProjectInitOptions{
		Lang:        flagInitLang,
		Backend:     flagInitBackend,
		Interactive: interactive,
		Workdir:     cwd,
	}
	_, body, err := config.RunProjectInit(os.Stdin, os.Stdout, opts)
	if err != nil {
		return fmt.Errorf("project init: %w", err)
	}
	if flagInitPrint {
		fmt.Print(body)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Printf("wrote project config to %s\n", path)
	return nil
}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print the layered config search paths and which ones exist",
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		paths := config.SearchPaths(cwd, flagConfig)
		fmt.Println("Search paths (low → high priority):")
		for _, p := range paths {
			marker := "  "
			if _, err := os.Stat(p); err == nil {
				marker = "✓ "
			}
			fmt.Printf("%s%s\n", marker, p)
		}
		return nil
	},
}

func init() {
	configInitCmd.Flags().BoolVarP(&flagInitPrint, "print", "p", false, "print the default config to stdout instead of writing a file")
	configInitCmd.Flags().BoolVarP(&flagInitForce, "force", "f", false, "overwrite if the destination already exists")
	configInitCmd.Flags().StringVar(&flagInitPath, "path", "", "destination path (defaults to $XDG_CONFIG_HOME/enso/config.toml, or <cwd>/.enso/config.toml with --project)")
	configInitCmd.Flags().BoolVarP(&flagInitWizard, "wizard", "w", false, "interactive prompt: pick a provider preset, model, and (optional) API key")
	configInitCmd.Flags().BoolVar(&flagInitProject, "project", false, "scaffold <cwd>/.enso/config.toml (backend env for this repo) instead of the user config")
	configInitCmd.Flags().StringVar(&flagInitLang, "lang", "", "language preset for --project (go|node|python|rust|generic); empty = auto-detect")
	configInitCmd.Flags().StringVar(&flagInitBackend, "backend", "", "backend to highlight in --project output (podman|lima); default podman")
	configCmd.AddCommand(configInitCmd, configShowCmd)
	rootCmd.AddCommand(configCmd)
}
