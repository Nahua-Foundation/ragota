package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

// jsonResult — упаковывает payload в текстовый MCP-ответ (JSON).
// Никогда не возвращает null для nil слайсов, так как они должны быть
// инициализированы в store. Но для надежности проверяет.
func jsonResult(v any) (*mcp.CallToolResult, error) {
	if v == nil {
		return mcp.NewToolResultText("[]"), nil
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(data)), nil
}

// errorToResult превращает ошибку в успешный ToolResult с пометкой Error.
// Это позволяет LLM увидеть текст ошибки и попробовать другой путь.
func errorToResult(name string, err error) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError(fmt.Sprintf("Tool %s failed: %v", name, err)), nil
}
