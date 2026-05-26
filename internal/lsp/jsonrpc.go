package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Файл содержит реализацию транспорта JSON-RPC 2.0 поверх stdio для LSP-клиента:
// типы сообщений, чтение/запись framed-пакетов и обработку входящих
// запросов/уведомлений от сервера.

// rpcRequest — клиентский запрос (или уведомление, если ID == 0) к серверу.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcResponse — ответ сервера на наш запрос. Err используется внутренне для
// доставки ошибок транспорта/процесса в ожидающую горутину Call.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	Err     error           `json:"-"`
}

// rpcIncoming используется для разбора входящих сообщений от сервера,
// которые могут быть либо ответами (есть result/error), либо запросами
// от сервера к клиенту (есть method и id), либо уведомлениями (method без id).
type rpcIncoming struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

// rpcServerResponse — ответ клиента на серверный request.
type rpcServerResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError — стандартный объект ошибки JSON-RPC.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
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
		if resp.Err != nil {
			return resp.Err
		}
		if resp.Error != nil {
			return fmt.Errorf("lsp %s: %s", method, resp.Error.Message)
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	}
}

// Notify шлёт уведомление без ID.
func (c *Client) Notify(method string, params any) error {
	return c.write(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

// write отправляет один framed JSON-RPC пакет (Content-Length + JSON).
func (c *Client) write(req rpcRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := io.WriteString(c.stdin, header); err != nil {
		return c.withStderrLocked(err)
	}
	if _, err := c.stdin.Write(data); err != nil {
		return c.withStderrLocked(err)
	}
	return nil
}

// readLoop читает framed JSON-RPC пакеты от LSP-сервера и диспатчит их:
//   - серверные requests → handleServerRequest в отдельной горутине;
//   - серверные notifications → handleServerNotification;
//   - ответы → доставляются в pending-канал ожидающего Call.
func (c *Client) readLoop() {
	defer c.failPending(fmt.Errorf("lsp %s process stopped before responding", c.Language))
	defer c.dead.Store(true)
	// notify Manager о смерти клиента для cleanup
	defer func() {
		if c.onDead != nil {
			c.onDead()
		}
	}()
	br := bufio.NewReader(c.stdout)
	for {
		length := -1
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
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
		var msg rpcIncoming
		if err := json.Unmarshal(buf, &msg); err != nil {
			// логируем malformed JSON вместо silent discard
			lspDebug("LSP %s: malformed JSON response: %v\n", c.Language, err)
			continue
		}
		// Случай 1: серверный request (method != "" && id != nil) — обязаны ответить.
		// ВАЖНО: обработка идёт в отдельной горутине, иначе readLoop блокируется
		// на writeServerResponse → берёт c.mu (которая может быть удержана Call'ом),
		// и pyright/tsserver встают по таймауту, не успев ответить на initialize.
		if msg.Method != "" && msg.ID != nil {
			id := *msg.ID
			method := msg.Method
			params := msg.Params
			go c.handleServerRequest(id, method, params)
			continue
		}
		// Случай 2: серверное уведомление (method != "" && id == nil).
		// Pyright присылает publishDiagnostics после первичного анализа didOpen;
		// используем это как сигнал, что definition уже можно спрашивать без гонки.
		// ВАЖНО: обрабатываем в горутине, чтобы не блокировать readLoop.
		if msg.Method != "" {
			method := msg.Method
			params := msg.Params
			go c.handleServerNotification(method, params)
			continue
		}
		// Случай 3: ответ на наш запрос.
		if msg.ID == nil {
			continue
		}
		var id int64
		if err := json.Unmarshal(*msg.ID, &id); err != nil || id == 0 {
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ok {
			ch <- rpcResponse{JSONRPC: msg.JSONRPC, ID: id, Result: msg.Result, Error: msg.Error}
		}
	}
}

// failPending завершает все ожидающие Call ошибкой (используется при падении
// процесса LSP или закрытии stdout).
func (c *Client) failPending(err error) {
	if processDetails := c.processSummary(); processDetails != "" {
		err = fmt.Errorf("%w; process: %s", err, processDetails)
	}
	if details := c.stderrSummary(); details != "" {
		err = fmt.Errorf("%w; recent stderr: %s", err, details)
	}
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[int64]chan rpcResponse)
	c.mu.Unlock()
	for id, ch := range pending {
		ch <- rpcResponse{ID: id, Err: err}
	}
}

// handleServerNotification обрабатывает серверные уведомления, которые нам
// интересны (publishDiagnostics — сигнал готовности анализа; language/status —
// jdtls-специфичный сигнал готовности).
func (c *Client) handleServerNotification(method string, params json.RawMessage) {
	switch method {
	case "textDocument/publishDiagnostics":
		var p struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.URI == "" {
			return
		}
		localPath := c.toLocalPath(p.URI)
		lspDebug("LSP %s: publishDiagnostics: uri=%q local=%q\n",
			c.Language, p.URI, localPath)
		select {
		case c.diagnosticsReady <- localPath:
		default:
		}
	case "language/status":
		// jdtls-специфичное уведомление. Структура: {type, message}.
		// Сигналы готовности к ответам: "ServiceReady" (проект импортирован,
		// classpath построен) и "Started" (legacy/Standard режим).
		// "Starting", "Started" одинаково означают что OSGi контейнер
		// поднят; для invisible-project режима ServiceReady может не приходить,
		// поэтому "Started" тоже принимаем.
		var p struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return
		}
		lspDebug("LSP %s: language/status: type=%q message=%q\n",
			c.Language, p.Type, p.Message)
		switch p.Type {
		case "ServiceReady", "Started", "Ready":
			if c.javaReadyClosed.CompareAndSwap(false, true) {
				close(c.javaReady)
			}
		}
	case "$/progress":
		// LSP прогресс. Для gopls ищем токен "gopls.indexing" с value.kind == "end".
		var p struct {
			Token any `json:"token"` // может быть string или number
			Value struct {
				Kind string `json:"kind"`
			} `json:"value"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return
		}
		tokenStr := fmt.Sprintf("%v", p.Token)
		lspDebug("LSP %s: $/progress: token=%q kind=%q\n",
			c.Language, tokenStr, p.Value.Kind)
		if (tokenStr == "gopls.indexing" || tokenStr == "Initial workspace scan") && p.Value.Kind == "end" {
			if c.goplsReadyClosed.CompareAndSwap(false, true) {
				close(c.goplsReady)
			}
		}
	}
}

// handleServerRequest отвечает на серверный request минимально валидным
// результатом, чтобы LSP-сервер не блокировался в ожидании ответа клиента.
func (c *Client) handleServerRequest(rawID json.RawMessage, method string, params json.RawMessage) {
	var result any
	switch method {
	case "workspace/configuration":
		// Сервер (pyright/tsserver) запрашивает настройки для N items.
		// Если вернуть пустой массив — pyright не индексирует проект и не
		// находит definition. Возвращаем массив той же длины с разумными
		// дефолтами для python/typescript.
		n := 1
		var p struct {
			Items []struct {
				ScopeURI string `json:"scopeUri"`
				Section  string `json:"section"`
			} `json:"items"`
		}
		if err := json.Unmarshal(params, &p); err == nil && len(p.Items) > 0 {
			n = len(p.Items)
		}
		arr := make([]any, 0, n)
		for i := 0; i < n; i++ {
			section := ""
			if i < len(p.Items) {
				section = p.Items[i].Section
			}
			arr = append(arr, c.configFor(section))
		}
		result = arr
	case "workspace/workspaceFolders":
		result = []map[string]any{{"uri": c.rootURI, "name": "root"}}
	case "client/registerCapability", "client/unregisterCapability":
		result = nil
	case "window/workDoneProgress/create":
		result = nil
	case "workspace/applyEdit":
		result = map[string]any{"applied": false}
	default:
		// Неизвестный метод — отвечаем ошибкой MethodNotFound (-32601),
		// чтобы сервер не висел.
		resp := rpcServerResponse{
			JSONRPC: "2.0",
			ID:      rawID,
			Error:   &rpcError{Code: -32601, Message: "method not found: " + method},
		}
		_ = c.writeServerResponse(resp)
		return
	}
	resp := rpcServerResponse{JSONRPC: "2.0", ID: rawID, Result: result}
	_ = c.writeServerResponse(resp)
}

// writeServerResponse отправляет ответ клиента на серверный request.
func (c *Client) writeServerResponse(resp rpcServerResponse) error {
	msg := map[string]any{
		"jsonrpc": resp.JSONRPC,
		"id":      resp.ID,
	}
	if resp.Error != nil {
		msg["error"] = resp.Error
	} else {
		msg["result"] = resp.Result
	}
	data, err := json.Marshal(msg)
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
