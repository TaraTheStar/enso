// SPDX-License-Identifier: AGPL-3.0-or-later

package lsp

import "encoding/json"

// Position is a 0-based (line, character) point. The character offset is
// in UTF-16 code units per the LSP spec; for ASCII this is the same as
// rune index. Multi-byte content (CJK, emoji) is approximated for v1.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a half-open interval over Positions.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location names a range inside a document.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// TextDocumentIdentifier names a doc by URI.
type TextDocumentIdentifier struct {
	URI string `json:"uri"`
}

// VersionedTextDocumentIdentifier adds a monotonic version number.
type VersionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

// TextDocumentItem is the full doc payload sent on didOpen.
type TextDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

// TextDocumentPositionParams is the cursor-coordinates parameter shared
// by hover / definition / etc.
type TextDocumentPositionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// InitializeParams is the LSP initialize request payload.
type InitializeParams struct {
	ProcessID             int                `json:"processId"`
	RootURI               string             `json:"rootUri,omitempty"`
	Capabilities          ClientCapabilities `json:"capabilities"`
	InitializationOptions json.RawMessage    `json:"initializationOptions,omitempty"`
}

// ClientCapabilities is the slice of client capabilities we admit. We
// keep this minimal — LSP servers will fill in defaults.
type ClientCapabilities struct {
	TextDocument *TextDocumentClientCapabilities `json:"textDocument,omitempty"`
	Workspace    *WorkspaceClientCapabilities    `json:"workspace,omitempty"`
}

type TextDocumentClientCapabilities struct {
	Hover              *HoverClientCapabilities              `json:"hover,omitempty"`
	Definition         *EmptyCapability                      `json:"definition,omitempty"`
	References         *EmptyCapability                      `json:"references,omitempty"`
	PublishDiagnostics *PublishDiagnosticsClientCapabilities `json:"publishDiagnostics,omitempty"`
}

type WorkspaceClientCapabilities struct {
	Configuration bool `json:"configuration,omitempty"`
}

type HoverClientCapabilities struct {
	ContentFormat []string `json:"contentFormat,omitempty"`
}

type PublishDiagnosticsClientCapabilities struct {
	RelatedInformation bool `json:"relatedInformation,omitempty"`
}

// EmptyCapability is the shape of "we support this; no further options".
type EmptyCapability struct{}

// InitializeResult is what the server sends back from initialize.
type InitializeResult struct {
	Capabilities json.RawMessage `json:"capabilities"`
}

// DidOpenTextDocumentParams is the payload for textDocument/didOpen.
type DidOpenTextDocumentParams struct {
	TextDocument TextDocumentItem `json:"textDocument"`
}

// DidCloseTextDocumentParams is the payload for textDocument/didClose.
type DidCloseTextDocumentParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

// HoverResult is the response for textDocument/hover. Contents can be a
// string, MarkupContent, or list — we accept all three via RawMessage and
// flatten to plain text in client code.
type HoverResult struct {
	Contents json.RawMessage `json:"contents"`
	Range    *Range          `json:"range,omitempty"`
}

// MarkupContent is the modern hover payload shape.
type MarkupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// MarkedString is the legacy hover-content shape (string or
// {language, value}). We try unmarshalling to the union.
type MarkedString struct {
	Language string `json:"language,omitempty"`
	Value    string `json:"value"`
}

// ReferenceParams extends position params with a context block.
type ReferenceParams struct {
	TextDocumentPositionParams
	Context ReferenceContext `json:"context"`
}

// ReferenceContext controls whether the declaration is included in
// references results.
type ReferenceContext struct {
	IncludeDeclaration bool `json:"includeDeclaration"`
}

// PublishDiagnosticsParams is the server→client diagnostics push.
type PublishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Version     *int         `json:"version,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// Diagnostic describes a single problem (warning, error, etc.).
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity,omitempty"` // 1=error,2=warning,3=info,4=hint
	Code     string `json:"code,omitempty"`
	Source   string `json:"source,omitempty"`
	Message  string `json:"message"`
}
