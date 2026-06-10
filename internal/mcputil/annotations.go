package mcputil

import "github.com/modelcontextprotocol/go-sdk/mcp"

// Annotation helpers for tool registration. Fresh values are returned each
// call so registrations never share mutable state.

// ReadOnly marks a tool as not modifying its environment.
func ReadOnly() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true}
}

// Destructive marks a tool as able to perform destructive updates
// (delete/overwrite state).
func Destructive() *mcp.ToolAnnotations {
	t := true
	return &mcp.ToolAnnotations{DestructiveHint: &t}
}

// Additive marks a tool as writing but only adding state (e.g. adding a movie
// to a download queue), never destroying it.
func Additive() *mcp.ToolAnnotations {
	f := false
	return &mcp.ToolAnnotations{DestructiveHint: &f}
}
