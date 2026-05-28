// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// langPreset holds the per-language differences that distinguish one
// generated project config from another. Everything else (alpine base,
// the surrounding comments, the [backend.egress] table shape) is
// identical across languages by design — see [[feedback-alpine-base]]:
// one base image, init overrides only.
type langPreset struct {
	// Slug is the machine-friendly name used on the CLI and in
	// detection results: "go", "node", "python", "rust", "generic".
	Slug string

	// Display is the human-readable label shown in the interactive
	// chooser ("Go", "Node.js", etc.).
	Display string

	// AlpinePackages are the `apk add --no-cache <pkgs>` arguments the
	// generated init line will install on top of plain alpine. Always
	// includes git + ca-certificates so the agent can clone over HTTPS
	// out of the box.
	AlpinePackages []string

	// EgressAllow is the default [backend.egress] allow list — the
	// hostnames the language's tooling needs to talk to. Kept tight by
	// default; users add more as needed.
	EgressAllow []string
}

// langPresets is the ordered list shown in the interactive chooser.
// "generic" is intentionally last — it's the catch-all for projects
// where detection didn't fire.
var langPresets = []langPreset{
	{
		Slug:           "go",
		Display:        "Go",
		AlpinePackages: []string{"go", "git", "ca-certificates"},
		EgressAllow:    []string{"proxy.golang.org", "sum.golang.org", "github.com"},
	},
	{
		Slug:           "node",
		Display:        "Node.js",
		AlpinePackages: []string{"nodejs", "npm", "git", "ca-certificates"},
		EgressAllow:    []string{"registry.npmjs.org", "github.com"},
	},
	{
		Slug:           "python",
		Display:        "Python",
		AlpinePackages: []string{"python3", "py3-pip", "git", "ca-certificates"},
		EgressAllow:    []string{"pypi.org", "files.pythonhosted.org", "github.com"},
	},
	{
		Slug:           "rust",
		Display:        "Rust",
		AlpinePackages: []string{"rust", "cargo", "git", "ca-certificates"},
		EgressAllow:    []string{"crates.io", "static.crates.io", "index.crates.io", "github.com"},
	},
	{
		Slug:           "generic",
		Display:        "Generic (no language toolchain)",
		AlpinePackages: []string{"git", "ca-certificates"},
		EgressAllow:    []string{"github.com"},
	},
}

// presetBySlug returns the preset matching slug, or the generic preset
// when slug is empty or unknown.
func presetBySlug(slug string) langPreset {
	for _, p := range langPresets {
		if p.Slug == slug {
			return p
		}
	}
	return langPresets[len(langPresets)-1] // generic
}

// ProjectInitOptions controls what RunProjectInit emits and whether it
// prompts. Zero-value is valid: detection runs against Workdir (or the
// current directory if empty), backend defaults to "podman", and
// non-interactive falls through to the detected language.
type ProjectInitOptions struct {
	// Lang pins the language preset. Empty triggers detection from
	// Workdir; unknown values fall through to "generic".
	Lang string

	// Backend chooses which of [backend.podman] / [backend.lima] is
	// emitted uncommented. The other block is included as a commented
	// "to switch backends, uncomment this" hint. Empty defaults to
	// "podman" — most users start there before reaching for lima.
	Backend string

	// Interactive enables prompts on top of in/out. Callers should
	// gate this on a TTY check; piped/scripted invocations should pass
	// false so the call is deterministic.
	Interactive bool

	// Workdir is the directory to detect against (and the directory
	// the resulting .enso/config.toml will sit in, though this
	// function does NOT write — that's the caller's job). Empty means
	// "skip detection."
	Workdir string
}

// ProjectInitResult is the structured outcome of RunProjectInit — what
// the rendered TOML reflects. Exposed so callers can log "wrote a Go
// project config" without re-parsing the TOML.
type ProjectInitResult struct {
	Lang    string
	Backend string
}

// RunProjectInit produces a project-scoped <repo>/.enso/config.toml
// body, optionally driven by prompts read from in. Mirrors the shape of
// RunWizard: the caller decides where the result lands on disk.
//
// The output contains ONLY project-scoped backend keys
// ([backend.podman], [backend.lima], [backend.egress]) — never
// [backend] type or workspace, which are user-scoped and would be
// stripped with a warning by Load.
func RunProjectInit(in io.Reader, out io.Writer, opts ProjectInitOptions) (ProjectInitResult, string, error) {
	lang := opts.Lang
	if lang == "" {
		lang = detectLang(opts.Workdir)
	}
	backend := opts.Backend
	if backend == "" {
		backend = "podman"
	}
	// Validate up front: callers (cmd/enso/config_cmd.go) should have
	// already vetted user-facing flag values, so an invalid Backend at
	// this layer is a programming error rather than a typo to silently
	// massage into podman.
	if backend != "podman" && backend != "lima" {
		return ProjectInitResult{}, "", fmt.Errorf("invalid backend %q (want podman or lima)", backend)
	}

	if opts.Interactive && in != nil {
		var detected string
		if opts.Lang == "" {
			detected = lang
		}
		lang, backend = projectInitPrompt(in, out, lang, backend, detected)
	}

	preset := presetBySlug(lang)
	body := buildProjectTOML(preset, backend)
	return ProjectInitResult{Lang: preset.Slug, Backend: backend}, body, nil
}

// projectInitPrompt walks the user through confirming the detected
// language and picking a backend. Empty / unparseable input keeps the
// passed-in defaults — same forgiving-of-Enter feel as RunWizard.
//
// detectedLang is non-empty only when the language was inferred (as
// opposed to passed via --lang); we use it to nudge the prompt copy
// toward "we detected X, hit Enter to keep it."
func projectInitPrompt(in io.Reader, out io.Writer, defaultLang, defaultBackend, detectedLang string) (string, string) {
	p := &prompter{in: bufio.NewReader(in), out: out}

	fmt.Fprintln(out, "Scaffolding .enso/config.toml for this project.")
	if detectedLang != "" && detectedLang != "generic" {
		fmt.Fprintf(out, "Detected language: %s\n", presetBySlug(detectedLang).Display)
	} else if detectedLang == "generic" {
		fmt.Fprintln(out, "No language toolchain detected — defaulting to generic.")
	}
	fmt.Fprintln(out)

	// Build the choice list in declared order; defaultIdx points at
	// the detected/passed lang so Enter keeps it.
	options := make([]string, 0, len(langPresets))
	defaultIdx := len(langPresets) - 1 // fall through to "generic"
	for i, ps := range langPresets {
		options = append(options, ps.Display)
		if ps.Slug == defaultLang {
			defaultIdx = i
		}
	}
	idx := p.askChoice("Language", options, defaultIdx)
	lang := langPresets[idx].Slug

	fmt.Fprintln(out)

	backendOpts := []string{"podman (rootless container)", "lima (per-project VM)"}
	backendDefault := 0
	if defaultBackend == "lima" {
		backendDefault = 1
	}
	bIdx := p.askChoice("Backend to highlight (the other will be included commented out)", backendOpts, backendDefault)
	backend := "podman"
	if bIdx == 1 {
		backend = "lima"
	}

	return lang, backend
}

// detectLang sniffs workdir for the canonical marker file of each
// supported language. First match wins (in langPresets declaration
// order), so multi-language projects get the language whose marker
// appears earliest in the table. Returns "generic" when nothing
// matches or workdir is empty / unreadable.
func detectLang(workdir string) string {
	if workdir == "" {
		return "generic"
	}
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(workdir, name))
		return err == nil
	}
	switch {
	case exists("go.mod"):
		return "go"
	case exists("package.json"):
		return "node"
	case exists("pyproject.toml"), exists("requirements.txt"):
		return "python"
	case exists("Cargo.toml"):
		return "rust"
	}
	return "generic"
}

// buildProjectTOML renders the project-scoped config file. Active
// backend gets an uncommented [backend.<name>] block; the other is
// emitted commented as a one-line-prefixed "switch by uncommenting"
// hint. The package list is identical across both backends — the only
// thing that varies between podman and lima for our defaults is the
// table key itself (image vs template) and the apk syntax stays
// shared because lima's default template is alpine.
func buildProjectTOML(preset langPreset, backend string) string {
	var b strings.Builder
	b.WriteString("# enso project configuration\n")
	b.WriteString("# This file pins the backend ENVIRONMENT (image, init, egress)\n")
	b.WriteString("# for this repo. The backend SELECTION ([backend] type /\n")
	b.WriteString("# workspace) is a personal preference and lives in your user\n")
	b.WriteString("# config (~/.config/enso/config.toml) — not here.\n")
	b.WriteString("#\n")
	b.WriteString("# Alpine is the universal base across podman + lima; per-\n")
	b.WriteString("# language differences are `apk add` overrides, not separate\n")
	b.WriteString("# base images. Override `image =` to pin a baked-in toolchain\n")
	b.WriteString("# (e.g. `golang:1.22-alpine`) if you prefer reproducibility\n")
	b.WriteString("# over cold-start init speed.\n\n")

	apkLine := "apk add --no-cache " + strings.Join(preset.AlpinePackages, " ")

	writePodman := func(prefix string) {
		fmt.Fprintf(&b, "%s[backend.podman]\n", prefix)
		fmt.Fprintf(&b, "%simage = \"alpine:latest\"\n", prefix)
		fmt.Fprintf(&b, "%sinit  = [%q]\n", prefix, apkLine)
	}
	writeLima := func(prefix string) {
		fmt.Fprintf(&b, "%s[backend.lima]\n", prefix)
		fmt.Fprintf(&b, "%stemplate = \"alpine\"\n", prefix)
		fmt.Fprintf(&b, "%sinit     = [%q]\n", prefix, apkLine)
	}

	if backend == "lima" {
		b.WriteString("# Switch to podman by uncommenting this block (and commenting\n")
		b.WriteString("# out [backend.lima] below, or leave both — only the one\n")
		b.WriteString("# matching your user config's [backend] type is used).\n")
		writePodman("# ")
		b.WriteString("\n")
		writeLima("")
	} else {
		writePodman("")
		b.WriteString("\n")
		b.WriteString("# Switch to lima by uncommenting this block (and commenting\n")
		b.WriteString("# out [backend.podman] above, or leave both — only the one\n")
		b.WriteString("# matching your user config's [backend] type is used).\n")
		writeLima("# ")
	}
	b.WriteString("\n")

	// Egress allow list — shared by podman + lima.
	b.WriteString("[backend.egress]\n")
	b.WriteString("# Outbound destinations the agent is allowed to reach. Add to\n")
	b.WriteString("# this list as your toolchain pulls from more hosts.\n")
	b.WriteString("allow = [")
	for i, h := range preset.EgressAllow {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", h)
	}
	b.WriteString("]\n")

	return b.String()
}
