// Package jsonrpc реализует generic JSON-RPC 2.0 транспорт поверх stdio.
// Не зависит от LSP-протокола — может использоваться для любого JSON-RPC сервера.
package jsonrpc

import "encoding/json"

// Request — клиентский запрос (или уведомление, если ID == 0).
type Request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// Response — ответ сервера на наш запрос.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
	Err     error           `json:"-"`
}

// Incoming — входящее сообщение от сервера.
type Incoming struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *Error           `json:"error,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

// ServerResponse — ответ клиента на серверный request.
type ServerResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error — стандартный объект ошибки JSON-RPC.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
