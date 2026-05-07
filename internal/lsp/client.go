// SPDX-License-Identifier: AGPL-3.0-or-later

package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// Client is a single LSP server connection. It owns the JSON-RPC Conn,
// tracks didOpen documents to avoid double-opens, and caches the most
// recent diagnostics per URI.
type Client struct {
	Conn *Conn

	openMu      sync.Mutex
	opened      map[string]bool // URIs we've sent didOpen for
	docVersions map[string]int

	diagMu     sync.RWMutex
	diagsByURI map[string][]Diagnostic
}

// NewClient wraps a Conn with bookkeeping. The caller must call Run on
// Conn in a separate goroutine before issuing requests.
func NewClient(conn *Conn) *Client {
	cl := &Client{
		Conn:        conn,
		opened:      map[string]bool{},
		docVersions: map[string]int{},
		diagsByURI:  map[string][]Diagnostic{},
	}
	conn.SetNotificationHandler(cl.handleNotification)
	return cl
}

func (c *Client) handleNotification(method string, params json.RawMessage) {
	switch method {
	case "textDocument/publishDiagnostics":
		var p PublishDiagnosticsParams
		if err := json.Unmarshal(params, &p); err != nil {
			return
		}
		c.diagMu.Lock()
		c.diagsByURI[p.URI] = p.Diagnostics
		c.diagMu.Unlock()
	}
}

// Initialize performs the LSP initialize/initialized handshake. `rootURI`
// is the project root the server should work against; `initOpts` is the
// opaque server-specific blob (passed through verbatim).
func (c *Client) Initialize(ctx context.Context, rootURI string, initOpts json.RawMessage) error {
	params := InitializeParams{
		ProcessID:             -1, // we don't expose our pid
		RootURI:               rootURI,
		InitializationOptions: initOpts,
		Capabilities: ClientCapabilities{
			TextDocument: &TextDocumentClientCapabilities{
				Hover:              &HoverClientCapabilities{ContentFormat: []string{"markdown", "plaintext"}},
				Definition:         &EmptyCapability{},
				References:         &EmptyCapability{},
				PublishDiagnostics: &PublishDiagnosticsClientCapabilities{RelatedInformation: true},
			},
			Workspace: &WorkspaceClientCapabilities{Configuration: false},
		},
	}
	var result InitializeResult
	if err := c.Conn.Call(ctx, "initialize", params, &result); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if err := c.Conn.Notify("initialized", struct{}{}); err != nil {
		return fmt.Errorf("initialized: %w", err)
	}
	return nil
}

// Shutdown asks the server to exit cleanly. Errors are tolerated — the
// caller will kill the process if shutdown stalls.
func (c *Client) Shutdown(ctx context.Context) error {
	_ = c.Conn.Call(ctx, "shutdown", nil, nil)
	_ = c.Conn.Notify("exit", nil)
	return nil
}

// DidOpen sends textDocument/didOpen for a URI if we haven't already. The
// server then starts producing diagnostics for that document.
func (c *Client) DidOpen(uri, languageID, text string) error {
	c.openMu.Lock()
	defer c.openMu.Unlock()
	if c.opened[uri] {
		return nil
	}
	c.docVersions[uri] = 1
	if err := c.Conn.Notify("textDocument/didOpen", DidOpenTextDocumentParams{
		TextDocument: TextDocumentItem{
			URI:        uri,
			LanguageID: languageID,
			Version:    1,
			Text:       text,
		},
	}); err != nil {
		return err
	}
	c.opened[uri] = true
	return nil
}

// DidClose tells the server to release a document. Idempotent.
func (c *Client) DidClose(uri string) error {
	c.openMu.Lock()
	defer c.openMu.Unlock()
	if !c.opened[uri] {
		return nil
	}
	delete(c.opened, uri)
	delete(c.docVersions, uri)
	return c.Conn.Notify("textDocument/didClose", DidCloseTextDocumentParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
	})
}

// Hover returns a flattened plain-text rendering of the hover response, or
// the empty string if the position has no hover content.
func (c *Client) Hover(ctx context.Context, uri string, line, char int) (string, error) {
	params := TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
		Position:     Position{Line: line, Character: char},
	}
	var raw json.RawMessage
	if err := c.Conn.Call(ctx, "textDocument/hover", params, &raw); err != nil {
		return "", err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var result HoverResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("decode hover: %w", err)
	}
	return flattenHoverContents(result.Contents), nil
}

// Definition returns a list of locations the symbol at (uri, line, char)
// is defined in. Servers may answer with a single Location, a list, or a
// list of LocationLink — we accept the first two; LocationLink falls back
// to its `targetUri` + `targetSelectionRange`.
func (c *Client) Definition(ctx context.Context, uri string, line, char int) ([]Location, error) {
	params := TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
		Position:     Position{Line: line, Character: char},
	}
	var raw json.RawMessage
	if err := c.Conn.Call(ctx, "textDocument/definition", params, &raw); err != nil {
		return nil, err
	}
	return decodeLocations(raw)
}

// References returns every location that references the symbol at the
// given position. `includeDeclaration` decides whether the declaration
// itself is included.
func (c *Client) References(ctx context.Context, uri string, line, char int, includeDecl bool) ([]Location, error) {
	params := ReferenceParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: uri},
			Position:     Position{Line: line, Character: char},
		},
		Context: ReferenceContext{IncludeDeclaration: includeDecl},
	}
	var raw json.RawMessage
	if err := c.Conn.Call(ctx, "textDocument/references", params, &raw); err != nil {
		return nil, err
	}
	return decodeLocations(raw)
}

// Diagnostics returns the most recent published diagnostics for `uri`,
// or nil if the server hasn't published any yet.
func (c *Client) Diagnostics(uri string) []Diagnostic {
	c.diagMu.RLock()
	defer c.diagMu.RUnlock()
	out := make([]Diagnostic, len(c.diagsByURI[uri]))
	copy(out, c.diagsByURI[uri])
	return out
}

// flattenHoverContents collapses the polymorphic hover-contents shape
// into plain text.
func flattenHoverContents(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Single MarkupContent: {"kind":"markdown","value":"..."}
	var mc MarkupContent
	if err := json.Unmarshal(raw, &mc); err == nil && mc.Value != "" {
		return mc.Value
	}
	// Single string: "..."
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		return s
	}
	// Single MarkedString: {"language":"go","value":"..."}
	var ms MarkedString
	if err := json.Unmarshal(raw, &ms); err == nil && ms.Value != "" {
		return ms.Value
	}
	// List of MarkedString-or-string.
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err == nil {
		var b strings.Builder
		for _, item := range list {
			var s string
			if err := json.Unmarshal(item, &s); err == nil {
				b.WriteString(s)
				b.WriteString("\n")
				continue
			}
			var ms MarkedString
			if err := json.Unmarshal(item, &ms); err == nil {
				b.WriteString(ms.Value)
				b.WriteString("\n")
			}
		}
		return strings.TrimRight(b.String(), "\n")
	}
	return ""
}

// locationLink is the LocationLink shape — we map it to plain Location
// using TargetURI and TargetSelectionRange.
type locationLink struct {
	TargetURI            string `json:"targetUri"`
	TargetRange          Range  `json:"targetRange"`
	TargetSelectionRange Range  `json:"targetSelectionRange"`
}

// decodeLocations accepts the three shapes a definition/references
// response can take: null, single Location, or array (of Location or
// LocationLink) and returns a flat []Location.
func decodeLocations(raw json.RawMessage) ([]Location, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	// Single Location.
	var single Location
	if err := json.Unmarshal(raw, &single); err == nil && single.URI != "" {
		return []Location{single}, nil
	}
	// Array.
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode locations: %w", err)
	}
	out := make([]Location, 0, len(list))
	for _, item := range list {
		var loc Location
		if err := json.Unmarshal(item, &loc); err == nil && loc.URI != "" {
			out = append(out, loc)
			continue
		}
		var link locationLink
		if err := json.Unmarshal(item, &link); err == nil && link.TargetURI != "" {
			out = append(out, Location{
				URI:   link.TargetURI,
				Range: link.TargetSelectionRange,
			})
		}
	}
	return out, nil
}
