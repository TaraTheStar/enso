// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// version is overridable at build time via:
//
//	-ldflags '-X main.version=v1.2.3'
//
// When unset (the default for `go install` and `go build`), it falls
// back to module-version + VCS info read from runtime/debug.BuildInfo,
// which is automatically populated for any binary built from a Go
// module checkout.
var version = ""

func resolveVersion() (ver, commit, modified string) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown", "", ""
	}
	ver = version
	if ver == "" {
		ver = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			commit = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				modified = "+dirty"
			}
		}
	}
	if ver == "" || ver == "(devel)" {
		if commit != "" {
			ver = "devel-" + shortCommit(commit) + modified
		} else {
			ver = "devel"
		}
	}
	return ver, commit, modified
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print enso version and build info",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ver, commit, modified := resolveVersion()
		var sb strings.Builder
		fmt.Fprintf(&sb, "enso %s\n", ver)
		if commit != "" {
			fmt.Fprintf(&sb, "commit: %s%s\n", commit, modified)
		}
		fmt.Fprintf(&sb, "go: %s\n", runtime.Version())
		fmt.Fprintf(&sb, "platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
		fmt.Print(sb.String())
		return nil
	},
}
