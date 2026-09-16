// Package mcputil provides shared MCP result helpers.
package mcputil

import (
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TextResult returns an MCP text content result.
func TextResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: text},
		},
	}
}

// JSONResult marshals v as compact JSON and returns it as an MCP text content
// result. Compact, not indented: the text lands in a model's context, where
// indentation is a quarter of the bytes of a large payload and carries nothing.
func JSONResult(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return Errorf("marshaling result: %v", err), nil, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(b)},
		},
	}, nil, nil
}

// Errorf returns an MCP error result (IsError set) with a formatted message.
func Errorf(format string, args ...any) *mcp.CallToolResult {
	result := TextResult(fmt.Sprintf("Error: "+format, args...))
	result.IsError = true
	return result
}
