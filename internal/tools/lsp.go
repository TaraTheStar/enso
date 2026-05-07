// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TaraTheStar/enso/internal/lsp"
)

// RegisterLSP adds the lsp_* tools to a registry, but only if the manager
// has at least one server configured. With no servers, the tools are not
// surfaced — the model wouldn't be able to call them anyway and listing
// them just wastes prompt tokens.
func RegisterLSP(r *Registry, mgr *lsp.Manager) {
	if mgr == nil || !mgr.HasServers() {
		return
	}
	r.Register(LSPHoverTool{mgr: mgr})
	r.Register(LSPDefinitionTool{mgr: mgr})
	r.Register(LSPReferencesTool{mgr: mgr})
	r.Register(LSPDiagnosticsTool{mgr: mgr})
}

// commonLSPParams is the JSON-Schema shape shared by hover/definition/refs.
func commonLSPParams() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"file": map[string]interface{}{
				"type":        "string",
				"description": "Path to the source file (absolute, or relative to the project cwd).",
			},
			"line": map[string]interface{}{
				"type":        "integer",
				"description": "1-based line number.",
			},
			"column": map[string]interface{}{
				"type":        "integer",
				"description": "1-based column (counted in characters; multi-byte chars use rune position).",
			},
		},
		"required": []string{"file", "line", "column"},
	}
}

// resolveFile turns the user-supplied path into an absolute one rooted
// at the agent cwd, opens the file, and ensures the LSP server has a
// didOpen for it. Returns absPath, URI, and the matched client.
//
// Goes through resolveRestricted, so when ac.RestrictedRoots is set the
// LSP tools are confined to the same roots as read/write/edit (and
// inherit the symlink-target check). Without that, the LSP tools are a
// trivial bypass: hover/diagnostics opens the file with os.ReadFile and
// sends its contents to the server.
func resolveFile(ctx context.Context, mgr *lsp.Manager, ac *AgentContext, file string) (absPath, uri string, client *lsp.Client, err error) {
	absPath, err = resolveRestricted(file, ac)
	if err != nil {
		return "", "", nil, err
	}
	client, _, err = mgr.ClientFor(ctx, absPath)
	if err != nil {
		return "", "", nil, err
	}
	if client == nil {
		return "", "", nil, fmt.Errorf("no LSP server matches %s (extension %q)", file, filepath.Ext(file))
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", "", nil, fmt.Errorf("read %s: %w", absPath, err)
	}
	uri = pathToURI(absPath)
	if err := client.DidOpen(uri, mgr.LanguageID(absPath), string(data)); err != nil {
		return "", "", nil, fmt.Errorf("didOpen: %w", err)
	}
	return absPath, uri, client, nil
}

// pathToURI mirrors lsp.pathToURI (kept private to lsp); duplicated here
// to avoid widening the lsp public surface for this single helper.
func pathToURI(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	abs = filepath.ToSlash(abs)
	if !strings.HasPrefix(abs, "/") {
		abs = "/" + abs
	}
	return "file://" + abs
}

// argInt extracts a 1-based int arg and converts to 0-based for LSP.
// LSP uses 0-based positions; user input is 1-based for parity with grep
// / editor line numbers.
func argInt(args map[string]interface{}, key string) (int, error) {
	raw, ok := args[key]
	if !ok {
		return 0, fmt.Errorf("%s is required", key)
	}
	switch v := raw.(type) {
	case float64:
		n := int(v)
		if n < 1 {
			return 0, fmt.Errorf("%s must be >= 1", key)
		}
		return n - 1, nil
	case int:
		if v < 1 {
			return 0, fmt.Errorf("%s must be >= 1", key)
		}
		return v - 1, nil
	default:
		return 0, fmt.Errorf("%s: want integer, got %T", key, raw)
	}
}

// formatLocation renders a Location as `path:line:col` using project-
// relative paths when possible.
func formatLocation(cwd string, loc lsp.Location) string {
	p := lsp.URIToPath(loc.URI)
	if rel, err := filepath.Rel(cwd, p); err == nil && !strings.HasPrefix(rel, "..") {
		p = rel
	}
	return fmt.Sprintf("%s:%d:%d", p, loc.Range.Start.Line+1, loc.Range.Start.Character+1)
}

// --- lsp_hover ---

type LSPHoverTool struct{ mgr *lsp.Manager }

func (t LSPHoverTool) Name() string { return "lsp_hover" }
func (t LSPHoverTool) Description() string {
	return "Ask the language server for hover information (type, signature, doc) at a (file, line, column) position. Useful for understanding what a symbol is or means without grepping for its declaration."
}
func (t LSPHoverTool) Parameters() map[string]interface{} { return commonLSPParams() }
func (t LSPHoverTool) Run(ctx context.Context, args map[string]interface{}, ac *AgentContext) (Result, error) {
	file, _ := args["file"].(string)
	line, err := argInt(args, "line")
	if err != nil {
		return Result{}, err
	}
	col, err := argInt(args, "column")
	if err != nil {
		return Result{}, err
	}
	_, uri, client, err := resolveFile(ctx, t.mgr, ac, file)
	if err != nil {
		return Result{}, err
	}
	text, err := client.Hover(ctx, uri, line, col)
	if err != nil {
		return Result{}, err
	}
	if text == "" {
		return Result{LLMOutput: "(no hover information at this position)"}, nil
	}
	return Result{LLMOutput: text, FullOutput: text}, nil
}

// --- lsp_definition ---

type LSPDefinitionTool struct{ mgr *lsp.Manager }

func (t LSPDefinitionTool) Name() string { return "lsp_definition" }
func (t LSPDefinitionTool) Description() string {
	return "Ask the language server where the symbol at (file, line, column) is defined. Returns one or more `path:line:col` locations the model can read with the `read` tool."
}
func (t LSPDefinitionTool) Parameters() map[string]interface{} { return commonLSPParams() }
func (t LSPDefinitionTool) Run(ctx context.Context, args map[string]interface{}, ac *AgentContext) (Result, error) {
	file, _ := args["file"].(string)
	line, err := argInt(args, "line")
	if err != nil {
		return Result{}, err
	}
	col, err := argInt(args, "column")
	if err != nil {
		return Result{}, err
	}
	_, uri, client, err := resolveFile(ctx, t.mgr, ac, file)
	if err != nil {
		return Result{}, err
	}
	locs, err := client.Definition(ctx, uri, line, col)
	if err != nil {
		return Result{}, err
	}
	if len(locs) == 0 {
		return Result{LLMOutput: "(no definition found at this position)"}, nil
	}
	var b strings.Builder
	for _, loc := range locs {
		b.WriteString(formatLocation(ac.Cwd, loc))
		b.WriteString("\n")
	}
	out := strings.TrimRight(b.String(), "\n")
	return Result{LLMOutput: out, FullOutput: out}, nil
}

// --- lsp_references ---

type LSPReferencesTool struct{ mgr *lsp.Manager }

func (t LSPReferencesTool) Name() string { return "lsp_references" }
func (t LSPReferencesTool) Description() string {
	return "Ask the language server for every reference to the symbol at (file, line, column). Pass include_declaration=true to also get the declaration site itself."
}
func (t LSPReferencesTool) Parameters() map[string]interface{} {
	p := commonLSPParams()
	props := p["properties"].(map[string]interface{})
	props["include_declaration"] = map[string]interface{}{
		"type":        "boolean",
		"description": "Include the symbol's declaration in the results. Default false.",
	}
	return p
}
func (t LSPReferencesTool) Run(ctx context.Context, args map[string]interface{}, ac *AgentContext) (Result, error) {
	file, _ := args["file"].(string)
	line, err := argInt(args, "line")
	if err != nil {
		return Result{}, err
	}
	col, err := argInt(args, "column")
	if err != nil {
		return Result{}, err
	}
	includeDecl, _ := args["include_declaration"].(bool)
	_, uri, client, err := resolveFile(ctx, t.mgr, ac, file)
	if err != nil {
		return Result{}, err
	}
	locs, err := client.References(ctx, uri, line, col, includeDecl)
	if err != nil {
		return Result{}, err
	}
	if len(locs) == 0 {
		return Result{LLMOutput: "(no references found)"}, nil
	}
	var b strings.Builder
	for _, loc := range locs {
		b.WriteString(formatLocation(ac.Cwd, loc))
		b.WriteString("\n")
	}
	out := strings.TrimRight(b.String(), "\n")
	return Result{LLMOutput: out, FullOutput: out}, nil
}

// --- lsp_diagnostics ---

type LSPDiagnosticsTool struct{ mgr *lsp.Manager }

func (t LSPDiagnosticsTool) Name() string { return "lsp_diagnostics" }
func (t LSPDiagnosticsTool) Description() string {
	return "List the diagnostics (errors, warnings, hints) the language server has published for a file. Use this after editing to confirm a file compiles cleanly without invoking the build."
}
func (t LSPDiagnosticsTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"file": map[string]interface{}{
				"type":        "string",
				"description": "Path to the source file (absolute, or relative to the project cwd).",
			},
		},
		"required": []string{"file"},
	}
}
func (t LSPDiagnosticsTool) Run(ctx context.Context, args map[string]interface{}, ac *AgentContext) (Result, error) {
	file, _ := args["file"].(string)
	_, uri, client, err := resolveFile(ctx, t.mgr, ac, file)
	if err != nil {
		return Result{}, err
	}
	diags := client.Diagnostics(uri)
	if len(diags) == 0 {
		return Result{LLMOutput: "(no diagnostics — the file is clean, or the server hasn't analysed it yet; allow a moment after didOpen and retry if you suspect this is the latter)"}, nil
	}
	var b strings.Builder
	for _, d := range diags {
		sev := severityName(d.Severity)
		fmt.Fprintf(&b, "%s:%d:%d  [%s]  %s",
			lsp.URIToPath(uri),
			d.Range.Start.Line+1,
			d.Range.Start.Character+1,
			sev,
			d.Message,
		)
		if d.Source != "" {
			fmt.Fprintf(&b, "  (%s", d.Source)
			if d.Code != "" {
				fmt.Fprintf(&b, " %s", d.Code)
			}
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	out := strings.TrimRight(b.String(), "\n")
	return Result{LLMOutput: out, FullOutput: out}, nil
}

func severityName(s int) string {
	switch s {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "info"
	case 4:
		return "hint"
	default:
		return "diagnostic"
	}
}
