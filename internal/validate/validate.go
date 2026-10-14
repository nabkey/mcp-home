// Package validate provides input validation for MCP tool parameters
// to prevent path injection and other input-based attacks.
package validate

import (
	"fmt"
	"regexp"
)

// safeIdentifier matches alphanumeric characters, underscores, hyphens, and dots.
// This covers Home Assistant entity IDs (e.g. "light.living_room"),
// service domains/names (e.g. "light", "turn_on"), automation IDs,
// and Frigate camera names and event IDs.
var safeIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// maxIdentifierLen bounds an identifier. Home Assistant entity ids and the
// like are far shorter; the bound stops an unbounded string being spliced
// into an upstream URL.
const maxIdentifierLen = 255

// Identifier validates that s is a safe identifier for use in URL paths.
// Returns an error if s is empty or contains characters that could enable path traversal.
func Identifier(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > maxIdentifierLen {
		return fmt.Errorf("%s is longer than %d characters", name, maxIdentifierLen)
	}
	if !safeIdentifier.MatchString(value) {
		return fmt.Errorf("%s contains invalid characters: %q", name, value)
	}
	return nil
}
