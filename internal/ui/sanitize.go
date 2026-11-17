package ui

import "github.com/bgfdev/taut/internal/ui/sanitize"

// Sanitize makes text Taut did not write safe to draw on one line of a
// terminal: escape sequences, control characters and the invisible Unicode
// characters that reorder or hide text are removed, and whitespace is
// collapsed. See package sanitize, which code that must not link the
// terminal UI uses directly.
func Sanitize(s string) string { return sanitize.Text(s) }

// SanitizeLines is Sanitize for prose that keeps its line breaks.
func SanitizeLines(s string) string { return sanitize.Lines(s) }
