// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"strings"
)

// Signature-only ("outline") read mode (R4). Instead of every line of a
// large source file, the model gets just its structure: package + import
// summary + top-level declaration signatures (function headers, type specs,
// const/var names). For Go this is AST-exact; for other languages it falls
// back to a cheap indentation/keyword heuristic. The point is to let the
// model grasp a file's shape for a fraction of the tokens, then `read` the
// specific range it actually needs.

// outlineFile produces a signature-only view of file content. lang is the
// lower-cased file extension (".go", ".py", …). Returns (outline, true)
// when an outline was produced, or ("", false) to signal the caller should
// fall back to a normal read (e.g. a Go file that failed to parse).
func outlineFile(path, content string) (string, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		if out, ok := outlineGo(content); ok {
			return out, true
		}
		// Parse failure (partial/invalid file) → heuristic fallback so
		// outline mode still does something useful.
		return outlineHeuristic(content), true
	default:
		return outlineHeuristic(content), true
	}
}

// outlineGo renders Go top-level declarations without function bodies using
// the AST. Returns false if the source doesn't parse.
func outlineGo(src string) (string, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return "", false
	}

	var b strings.Builder
	b.WriteString("package " + f.Name.Name + "\n")

	if len(f.Imports) > 0 {
		b.WriteString("\n")
		if len(f.Imports) <= 12 {
			b.WriteString("imports:\n")
			for _, imp := range f.Imports {
				b.WriteString("\t" + imp.Path.Value + "\n")
			}
		} else {
			b.WriteString("imports: " + itoa(len(f.Imports)) + " packages\n")
		}
	}

	var decls []string
	for _, d := range f.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			// Drop the body; print the signature only.
			sig := *decl
			sig.Body = nil
			sig.Doc = nil
			decls = append(decls, printNode(fset, &sig))
		case *ast.GenDecl:
			// import/const/var/type. Imports already summarised above.
			if decl.Tok == token.IMPORT {
				continue
			}
			gd := *decl
			gd.Doc = nil
			decls = append(decls, printNode(fset, &gd))
		}
	}
	if len(decls) > 0 {
		b.WriteString("\n")
		b.WriteString(strings.Join(decls, "\n"))
		b.WriteString("\n")
	}
	return b.String(), true
}

func printNode(fset *token.FileSet, node ast.Node) string {
	var buf bytes.Buffer
	cfg := printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 4}
	if err := cfg.Fprint(&buf, fset, node); err != nil {
		return ""
	}
	return buf.String()
}

// outlineHeuristic is the language-agnostic fallback: keep top-level
// (unindented) non-blank lines and any line introducing a definition in a
// common language, drop the indented bodies. Conservative — when in doubt
// it keeps the line.
func outlineHeuristic(content string) string {
	lines := strings.Split(content, "\n")
	var out []string
	skipped := 0
	flush := func() {
		if skipped > 0 {
			out = append(out, "    … "+itoa(skipped)+" body lines …")
			skipped = 0
		}
	}
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		indent := len(ln) - len(strings.TrimLeft(ln, " \t"))
		switch {
		case trimmed == "":
			// swallow blank lines inside bodies; keep them at top level
			if indent == 0 {
				flush()
				out = append(out, ln)
			} else {
				skipped++
			}
		case indent == 0 || isDefinitionLine(trimmed):
			flush()
			out = append(out, ln)
		default:
			skipped++
		}
	}
	flush()
	return strings.Join(out, "\n")
}

// definitionKeywords introduce a declaration in one of the common languages
// enso users work in. A line beginning with one (after its access modifier)
// is kept by the heuristic outline even when indented.
var definitionKeywords = []string{
	"func ", "function ", "def ", "class ", "interface ", "struct ",
	"type ", "impl ", "trait ", "enum ", "fn ", "module ", "package ",
	"public ", "private ", "protected ", "static ", "export ", "const ",
	"async ", "abstract ",
}

func isDefinitionLine(trimmed string) bool {
	for _, kw := range definitionKeywords {
		if strings.HasPrefix(trimmed, kw) {
			return true
		}
	}
	return false
}

// itoa is a tiny strconv.Itoa stand-in kept local so this file's import set
// stays minimal.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
