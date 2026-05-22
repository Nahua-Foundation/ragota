// Package lsp реализует минимальный LSP-клиент (JSON-RPC 2.0 над stdio)
// и менеджер серверов на язык. Используется MCP-сервером.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ServerSpec описывает запускаемый LSP-сервер.
type ServerSpec struct {
	Language string   // "go" | "typescript" | "python" | "java"
	Command  string   // например "gopls"
	Args     []string // аргументы
}

// DefaultServers — рекомендуемые LSP-серверы.
// Если бинаря нет в PATH, сервер для этого языка просто не стартует.
func DefaultServers() []ServerSpec {
	return []ServerSpec{
		{Language: "go", Command: "gopls"},
		{Language: "typescript", Command: "typescript-language-server", Args: []string{"--stdio"}},
		{Language: "python", Command: "pyright-langserver", Args: []string{"--stdio"}},
		{Language: "java", Command: "jdtls"},
	}
}

// Client — простой LSP-клиент над одним процессом.
type Client struct {
	Language string
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   io.ReadCloser

	mu          sync.Mutex
	nextID      atomic.Int64
	pending     map[int64]chan rpcResponse
	initialized atomic.Bool

	rootURI string
	closed      atomic.Bool
	dead        atomic.Bool
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Start запускает процесс LSP-сервера и выполняет initialize/initialized.
func Start(ctx context.Context, spec ServerSpec, root string) (*Client, error) {
	cmd := exec.CommandContext(ctx, spec.Command, spec.Args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", spec.Command, err)
	}
	c := &Client{
		Language: spec.Language,
		cmd:      cmd,
		stdin:    stdin,
		stdout:   stdout,
		stderr:   stderr,
		pending:  make(map[int64]chan rpcResponse),
		rootURI:  fileURI(root),
	}
	go c.readLoop()
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			// Печатаем ошибки LSP сервера в stderr основного процесса для отладки
			_, _ = fmt.Fprintf(os.Stderr, "LSP %s stderr: %s\n", spec.Language, scanner.Text())
		}
	}()

	if err := c.initialize(ctx, root); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("initialize %s: %w", spec.Language, err)
	}
	return c, nil
}

// IsAlive возвращает true, если процесс сервера запущен и readLoop работает.
func (c *Client) IsAlive() bool {
	return !c.closed.Load() && !c.dead.Load()
}

// Close завершает процесс.
func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	return c.cmd.Wait()
}

func (c *Client) initialize(ctx context.Context, root string) error {
	params := map[string]any{
		"processId":    nil,
		"rootUri":      fileURI(root),
		"capabilities": map[string]any{},
	}
	var res json.RawMessage
	if err := c.Call(ctx, "initialize", params, &res); err != nil {
		return err
	}
	if err := c.Notify("initialized", map[string]any{}); err != nil {
		return err
	}
	c.initialized.Store(true)
	return nil
}

// Call отправляет запрос и ждёт ответ.
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	id := c.nextID.Add(1)
	ch := make(chan rpcResponse, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return fmt.Errorf("lsp %s: %s", method, resp.Error.Message)
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	case <-time.After(30 * time.Second):
		return fmt.Errorf("lsp %s timeout", method)
	}
}

// Notify шлёт уведомление без ID.
func (c *Client) Notify(method string, params any) error {
	return c.write(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Client) write(req rpcRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := io.WriteString(c.stdin, header); err != nil {
		return err
	}
	_, err = c.stdin.Write(data)
	return err
}

func (c *Client) readLoop() {
	defer c.dead.Store(true)
	br := bufio.NewReader(c.stdout)
	for {
		length := -1
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if strings.HasPrefix(strings.ToLower(line), "content-length:") {
				v := strings.TrimSpace(line[len("Content-Length:"):])
				n, err := strconv.Atoi(v)
				if err == nil {
					length = n
				}
			}
		}
		if length < 0 {
			return
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(br, buf); err != nil {
			return
		}
		var resp rpcResponse
		if err := json.Unmarshal(buf, &resp); err != nil {
			continue // notification от сервера — игнорируем
		}
		if resp.ID == 0 {
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[resp.ID]
		delete(c.pending, resp.ID)
		c.mu.Unlock()
		if ok {
			ch <- resp
		}
	}
}

// fileURI возвращает file:// URI для абсолютного пути.
func fileURI(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	u := &url.URL{Scheme: "file", Path: abs}
	return u.String()
}

// DidOpen уведомляет LSP-сервер о содержимом файла (требуется до запросов).
func (c *Client) DidOpen(path, languageID, text string) error {
	return c.Notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        fileURI(path),
			"languageId": languageID,
			"version":    1,
			"text":       text,
		},
	})
}

// Location — упрощённое представление LSP Location.
type Location struct {
	URI       string `json:"uri"`
	StartLine int    `json:"start_line"`
	StartChar int    `json:"start_char"`
	EndLine   int    `json:"end_line"`
	EndChar   int    `json:"end_char"`
}

// lspLocation как приходит от сервера.
type lspLocation struct {
	URI   string `json:"uri"`
	Range struct {
		Start lspPosition `json:"start"`
		End   lspPosition `json:"end"`
	} `json:"range"`
}

type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

func locFromLSP(in []lspLocation) []Location {
	out := make([]Location, 0, len(in))
	for _, l := range in {
		out = append(out, Location{
			URI: l.URI, StartLine: l.Range.Start.Line, StartChar: l.Range.Start.Character,
			EndLine: l.Range.End.Line, EndChar: l.Range.End.Character,
		})
	}
	return out
}

// Definition — go-to-definition.
func (c *Client) Definition(ctx context.Context, path string, line, character int) ([]Location, error) {
	var raw json.RawMessage
	if err := c.Call(ctx, "textDocument/definition", positionParams(path, line, character), &raw); err != nil {
		return nil, err
	}
	return decodeLocations(raw), nil
}

// References — find references.
func (c *Client) References(ctx context.Context, path string, line, character int, includeDecl bool) ([]Location, error) {
	params := positionParams(path, line, character)
	params["context"] = map[string]any{"includeDeclaration": includeDecl}
	var raw json.RawMessage
	if err := c.Call(ctx, "textDocument/references", params, &raw); err != nil {
		return nil, err
	}
	return decodeLocations(raw), nil
}

// Hover возвращает текст hover-подсказки (упрощённо — markdown/plain).
func (c *Client) Hover(ctx context.Context, path string, line, character int) (string, error) {
	var raw json.RawMessage
	if err := c.Call(ctx, "textDocument/hover", positionParams(path, line, character), &raw); err != nil {
		return "", err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var h struct {
		Contents any `json:"contents"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return "", err
	}
	return hoverString(h.Contents), nil
}

func hoverString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case map[string]any:
		if val, ok := x["value"].(string); ok {
			return val
		}
	case []any:
		var parts []string
		for _, it := range x {
			parts = append(parts, hoverString(it))
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func positionParams(path string, line, character int) map[string]any {
	return map[string]any{
		"textDocument": map[string]any{"uri": fileURI(path)},
		"position":     map[string]any{"line": line, "character": character},
	}
}

func decodeLocations(raw json.RawMessage) []Location {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	// LSP может вернуть Location | Location[] | LocationLink[]
	var single lspLocation
	if err := json.Unmarshal(raw, &single); err == nil && single.URI != "" {
		return locFromLSP([]lspLocation{single})
	}
	var arr []lspLocation
	if err := json.Unmarshal(raw, &arr); err == nil {
		return locFromLSP(arr)
	}
	return nil
}
