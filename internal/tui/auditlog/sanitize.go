package auditlog

import "github.com/bgfdev/taut/internal/ui"

// Sanitize makes text from the audit log safe to draw on a terminal. It is
// ui.Sanitize, kept under this name for the audit log's callers.
//
// Only rendering is sanitized. Exports carry entries exactly as recorded.
func Sanitize(s string) string { return ui.Sanitize(s) }
