package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Conn — JSON-RPC 2.0 connection over stdio pipes.
// Thread-safe: Call/Notify can be called concurrently.
type Conn struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser

	mu      sync.Mutex
	nextID  atomic.Int64
	pending map[int64]chan Response

	// Callbacks set by the owner (e.g. LSP session).
	onDead                func()
	onServerRequest       func(id json.RawMessage, method string, params json.RawMessage)
	onServerNotification  func(method string, params json.RawMessage)

	// Language tag for debug logging (informational, not used for logic).
	Language string

	// Debug logger: called with format + args. If nil, debug output is suppressed.
	DebugLog func(format string, args ...any)

	dead atomic.Bool
}

// NewConn creates a new JSON-RPC connection.
// stdin/stdout are the process pipes. The caller must call StartReadLoop()
// after construction to begin reading server messages.
func NewConn(
	stdin io.WriteCloser,
	stdout io.ReadCloser,
	language string,
	onDead func(),
	onServerRequest func(id json.RawMessage, method string, params json.RawMessage),
	onServerNotification func(method string, params json.RawMessage),
) *Conn {
	return &Conn{
		stdin:                stdin,
		stdout:               stdout,
		pending:              make(map[int64]chan Response),
		Language:             language,
		onDead:               onDead,
		onServerRequest:      onServerRequest,
		onServerNotification: onServerNotification,
	}
}

// Call sends a request and waits for the response.
func (c *Conn) Call(ctx context.Context, method string, params any, result any) error {
	id := c.nextID.Add(1)
	ch := make(chan Response, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(Request{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
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
			return fmt.Errorf("jsonrpc %s: %s", method, resp.Error.Message)
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	}
}

// Notify sends a notification (no ID, no response expected).
func (c *Conn) Notify(method string, params any) error {
	return c.write(Request{JSONRPC: "2.0", Method: method, Params: params})
}

// StartReadLoop launches the read loop in a background goroutine.
// It reads framed JSON-RPC messages from stdout until EOF or error.
func (c *Conn) StartReadLoop() {
	go c.readLoop()
}

// IsDead returns true if the read loop has exited (process died).
func (c *Conn) IsDead() bool {
	return c.dead.Load()
}

// Close closes the stdin pipe, which should cause the server process to exit.
func (c *Conn) Close() error {
	if c.stdin != nil {
		return c.stdin.Close()
	}
	return nil
}

// FailPending fails all pending calls with the given error.
// Exported for use by process monitoring code.
func (c *Conn) FailPending(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[int64]chan Response)
	c.mu.Unlock()
	for id, ch := range pending {
		ch <- Response{ID: id, Err: err}
	}
}

func (c *Conn) debug(format string, args ...any) {
	if c.DebugLog != nil {
		c.DebugLog(format, args...)
	}
}

func (c *Conn) write(req Request) error {
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
	if _, err := c.stdin.Write(data); err != nil {
		return err
	}
	return nil
}

func (c *Conn) readLoop() {
	defer c.FailPending(fmt.Errorf("jsonrpc %s: read loop exited", c.Language))
	defer c.dead.Store(true)
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
		var msg Incoming
		if err := json.Unmarshal(buf, &msg); err != nil {
			c.debug("jsonrpc %s: malformed JSON response: %v\n", c.Language, err)
			continue
		}
		if msg.Method != "" && msg.ID != nil {
			if c.onServerRequest != nil {
				go c.onServerRequest(*msg.ID, msg.Method, msg.Params)
			}
			continue
		}
		if msg.Method != "" {
			if c.onServerNotification != nil {
				go c.onServerNotification(msg.Method, msg.Params)
			}
			continue
		}
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
			ch <- Response{JSONRPC: msg.JSONRPC, ID: id, Result: msg.Result, Error: msg.Error}
		}
	}
}

// WriteServerResponse sends a response to a server request.
func (c *Conn) WriteServerResponse(resp ServerResponse) error {
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
