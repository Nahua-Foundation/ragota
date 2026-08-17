// Package lsp implements a minimal Language Server Protocol client over TCP
// and an indexing refinement pass ("lsp" indexer) that uses language servers
// to add precise symbols and reference edges on top of the tree-sitter graph.
//
// The client speaks JSON-RPC 2.0 with Content-Length framing (the LSP wire
// format) and supports exactly the requests the refiner needs: initialize,
// initialized, textDocument/didOpen, textDocument/documentSymbol,
// textDocument/references and shutdown/exit. Server-to-client requests
// (workspace/configuration, client/registerCapability, ...) are answered
// with empty results so servers that require a round-trip do not stall.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout bounds a single LSP request when the config does not set one.
const DefaultTimeout = 30 * time.Second

// rpcError is a JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("lsp: server error %d: %s", e.Code, e.Message)
}

// message is a JSON-RPC 2.0 message (request, response or notification).
// ID is kept raw so server-initiated requests can be echoed back verbatim.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Client is a minimal LSP client over a TCP connection.
type Client struct {
	conn    net.Conn
	timeout time.Duration

	mu      sync.Mutex // guards writes, nextID and pending
	nextID  int64
	pending map[int64]chan *message

	readDone chan struct{}
	readErr  error
}

// Dial connects to a TCP-exposed language server. timeout bounds the dial and
// every subsequent request; values <= 0 fall back to DefaultTimeout.
func Dial(addr string, timeout time.Duration) (*Client, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("lsp: dial %s: %w", addr, err)
	}
	c := &Client{
		conn:     conn,
		timeout:  timeout,
		pending:  make(map[int64]chan *message),
		readDone: make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// readLoop reads framed messages and routes responses to pending calls.
func (c *Client) readLoop() {
	r := bufio.NewReader(c.conn)
	for {
		msg, err := readMessage(r)
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			for id, ch := range c.pending {
				close(ch)
				delete(c.pending, id)
			}
			c.mu.Unlock()
			close(c.readDone)
			return
		}
		switch {
		case msg.Method != "" && len(msg.ID) > 0:
			c.answerServerRequest(msg) // server -> client request
		case msg.Method != "":
			// Notification (diagnostics, logs, progress) — ignored.
		default:
			c.dispatch(msg)
		}
	}
}

// dispatch delivers a response to the call waiting for it.
func (c *Client) dispatch(msg *message) {
	id, err := strconv.ParseInt(strings.Trim(string(msg.ID), `"`), 10, 64)
	if err != nil {
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ok {
		ch <- msg
	}
}

// answerServerRequest replies to server-initiated requests with an empty
// result, which satisfies gopls/jdtls/ts-server round-trips during startup.
func (c *Client) answerServerRequest(msg *message) {
	var result any
	if msg.Method == "workspace/configuration" {
		// The response must be an array with one entry per requested item.
		var p struct {
			Items []json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		result = make([]any, len(p.Items))
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result}
	body, err := json.Marshal(resp)
	if err != nil {
		return
	}
	c.mu.Lock()
	_ = c.writeFrame(body)
	c.mu.Unlock()
}

// readMessage reads one Content-Length framed JSON-RPC message.
func readMessage(r *bufio.Reader) (*message, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of headers
		}
		if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("lsp: bad Content-Length %q", v)
			}
			length = n
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("lsp: missing Content-Length header")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	msg := &message{}
	if err := json.Unmarshal(body, msg); err != nil {
		return nil, fmt.Errorf("lsp: decode message: %w", err)
	}
	return msg, nil
}

// writeFrame writes one framed message. Callers must hold c.mu.
func (c *Client) writeFrame(body []byte) error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(c.timeout))
	if _, err := fmt.Fprintf(c.conn, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err := c.conn.Write(body)
	return err
}

// call performs a request and unmarshals the result into out (may be nil).
func (c *Client) call(ctx context.Context, method string, params, out any) error {
	body, id, ch, err := c.encodeRequest(method, params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.pending[id] = ch
	err = c.writeFrame(body)
	if err != nil {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("lsp: write %s: %w", method, err)
	}

	timer := time.NewTimer(c.timeout)
	defer timer.Stop()
	select {
	case msg, ok := <-ch:
		if !ok {
			return fmt.Errorf("lsp: connection closed during %s: %w", method, c.readErr)
		}
		if msg.Error != nil {
			return msg.Error
		}
		if out != nil && len(msg.Result) > 0 {
			if err := json.Unmarshal(msg.Result, out); err != nil {
				return fmt.Errorf("lsp: decode %s result: %w", method, err)
			}
		}
		return nil
	case <-timer.C:
		c.forget(id)
		return fmt.Errorf("lsp: %s timed out after %s", method, c.timeout)
	case <-ctx.Done():
		c.forget(id)
		return ctx.Err()
	}
}

func (c *Client) encodeRequest(method string, params any) ([]byte, int64, chan *message, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("lsp: encode %s: %w", method, err)
	}
	return body, id, make(chan *message, 1), nil
}

func (c *Client) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// notify sends a notification (no response expected).
func (c *Client) notify(method string, params any) error {
	req := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		req["params"] = params
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("lsp: encode %s: %w", method, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.writeFrame(body); err != nil {
		return fmt.Errorf("lsp: write %s: %w", method, err)
	}
	return nil
}

// Initialize performs the LSP handshake with rootURI as the workspace root.
// initOpts is passed as LSP initializationOptions (nil means empty); some
// servers require server-specific options here (see LSPServerConfig).
func (c *Client) Initialize(ctx context.Context, rootURI string, initOpts map[string]any) error {
	if initOpts == nil {
		initOpts = map[string]any{}
	}
	rootPath := strings.TrimPrefix(rootURI, "file://")
	params := map[string]any{
		"processId": nil,
		"rootUri":   rootURI,
		"rootPath":  rootPath,
		"workspaceFolders": []map[string]any{
			{"uri": rootURI, "name": filepath.Base(rootPath)},
		},
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"documentSymbol": map[string]any{
					"hierarchicalDocumentSymbolSupport": true,
				},
				"publishDiagnostics": map[string]any{},
			},
			"workspace": map[string]any{
				"workspaceFolders": true,
				"configuration":    true,
			},
		},
		"initializationOptions": initOpts,
	}
	var res json.RawMessage
	if err := c.call(ctx, "initialize", params, &res); err != nil {
		return err
	}
	return c.notify("initialized", map[string]any{})
}

// DidOpen announces an open document with its full content.
func (c *Client) DidOpen(uri, languageID, text string) error {
	return c.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": languageID,
			"version":    1,
			"text":       text,
		},
	})
}

// Symbol is a flattened document symbol. Positions are 0-based (LSP style);
// SelLine/SelChar point at the symbol name (selection range) when the server
// returned hierarchical symbols, and at the range start otherwise.
type Symbol struct {
	Name      string
	Kind      int // LSP SymbolKind
	StartLine int
	EndLine   int
	SelLine   int
	SelChar   int
	Container string
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// rawSymbol covers both DocumentSymbol[] and SymbolInformation[] responses.
type rawSymbol struct {
	Name           string    `json:"name"`
	Kind           int       `json:"kind"`
	Range          *lspRange `json:"range"`
	SelectionRange *lspRange `json:"selectionRange"`
	ContainerName  string    `json:"containerName"`
	Location       *struct {
		URI   string   `json:"uri"`
		Range lspRange `json:"range"`
	} `json:"location"`
	Children []rawSymbol `json:"children"`
}

// DocumentSymbols returns the flattened symbols of an open document.
func (c *Client) DocumentSymbols(ctx context.Context, uri string) ([]Symbol, error) {
	var raw []rawSymbol
	err := c.call(ctx, "textDocument/documentSymbol",
		map[string]any{"textDocument": map[string]any{"uri": uri}}, &raw)
	if err != nil {
		return nil, err
	}
	var out []Symbol
	flattenSymbols(raw, "", &out)
	return out, nil
}

func flattenSymbols(raw []rawSymbol, container string, out *[]Symbol) {
	for _, r := range raw {
		// Some servers (jdtls in flat SymbolInformation mode) append the
		// parameter list to the name: "onOrderCreated(String)". Strip it so
		// symbol names are comparable across servers and with AST units.
		name := r.Name
		if i := strings.IndexByte(name, '('); i > 0 {
			name = name[:i]
		}
		s := Symbol{Name: name, Kind: r.Kind, Container: container}
		if r.ContainerName != "" {
			s.Container = r.ContainerName
		}
		switch {
		case r.Range != nil: // hierarchical DocumentSymbol
			s.StartLine, s.EndLine = r.Range.Start.Line, r.Range.End.Line
			s.SelLine, s.SelChar = r.Range.Start.Line, r.Range.Start.Character
			if r.SelectionRange != nil {
				s.SelLine, s.SelChar = r.SelectionRange.Start.Line, r.SelectionRange.Start.Character
			}
		case r.Location != nil: // flat SymbolInformation
			s.StartLine, s.EndLine = r.Location.Range.Start.Line, r.Location.Range.End.Line
			s.SelLine, s.SelChar = r.Location.Range.Start.Line, r.Location.Range.Start.Character
		}
		*out = append(*out, s)
		flattenSymbols(r.Children, r.Name, out)
	}
}

// Location is a reference site returned by textDocument/references.
type Location struct {
	URI       string
	Line      int // 0-based
	Character int
}

// References returns all reference sites of the symbol at the given 0-based
// position, excluding the declaration itself.
func (c *Client) References(ctx context.Context, uri string, line, character int) ([]Location, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": character},
		"context":      map[string]any{"includeDeclaration": false},
	}
	var raw []struct {
		URI   string   `json:"uri"`
		Range lspRange `json:"range"`
	}
	if err := c.call(ctx, "textDocument/references", params, &raw); err != nil {
		return nil, err
	}
	locs := make([]Location, 0, len(raw))
	for _, r := range raw {
		locs = append(locs, Location{URI: r.URI, Line: r.Range.Start.Line, Character: r.Range.Start.Character})
	}
	return locs, nil
}

// Close performs a best-effort shutdown/exit and closes the connection.
func (c *Client) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var res json.RawMessage
	_ = c.call(ctx, "shutdown", nil, &res)
	_ = c.notify("exit", nil)
	return c.conn.Close()
}

// Mapper translates between host file paths and container file URIs.
// hostPath -> "file://" + MountRoot + strings.TrimPrefix(hostPath, HostRoot).
type Mapper struct {
	hostRoot  string
	mountRoot string
}

// NewMapper creates a path mapper. Empty roots make it an identity mapping.
func NewMapper(hostRoot, mountRoot string) Mapper {
	return Mapper{hostRoot: strings.TrimSuffix(hostRoot, "/"), mountRoot: strings.TrimSuffix(mountRoot, "/")}
}

// ToURI converts a host path to a file URI in container coordinates.
func (m Mapper) ToURI(hostPath string) string {
	p := filepath.ToSlash(hostPath)
	if m.hostRoot != "" && strings.HasPrefix(p, m.hostRoot) {
		p = m.mountRoot + strings.TrimPrefix(p, m.hostRoot)
	}
	return "file://" + p
}

// FromURI converts a container file URI back to a host path.
func (m Mapper) FromURI(uri string) string {
	p := strings.TrimPrefix(uri, "file://")
	if unescaped, err := url.PathUnescape(p); err == nil {
		p = unescaped
	}
	if m.mountRoot != "" && strings.HasPrefix(p, m.mountRoot) {
		p = m.hostRoot + strings.TrimPrefix(p, m.mountRoot)
	}
	return filepath.FromSlash(p)
}
