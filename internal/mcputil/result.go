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

// JSONResult marshals v as indented JSON and returns it as an MCP text content result.
func JSONResult(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return TextResult(fmt.Sprintf("Error marshaling result: %v", err)), nil, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(b)},
		},
	}, nil, nil
}

// Errorf returns an MCP text result with a formatted error message.
func Errorf(format string, args ...any) *mcp.CallToolResult {
	return TextResult(fmt.Sprintf("Error: "+format, args...))
}
