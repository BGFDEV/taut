package config

import (
	"fmt"
	"strings"

	"github.com/bgfdev/taut/internal/fail"
)

// Severity grades an Issue.
type Severity string

const (
	// SeverityError marks a problem that must be fixed before the config file
	// can be used.
	SeverityError Severity = "error"
	// SeverityWarning marks something that works but deserves attention.
	SeverityWarning Severity = "warning"
)

// Issue is one finding about a config file.
type Issue struct {
	// Path locates the field, for example "target.teams[0].owner".
	Path string `json:"path"`
	// Message says what is wrong and how to fix it. It never contains
	// secret material.
	Message  string   `json:"message"`
	Severity Severity `json:"severity"`
	// Line is the 1-based line in the config file file, or 0 if unknown.
	Line int `json:"line,omitempty"`
}

// String renders the issue as "line 12: target.teams[0].owner: message".
func (i Issue) String() string {
	var b strings.Builder
	if i.Line > 0 {
		fmt.Fprintf(&b, "line %d: ", i.Line)
	}
	if i.Path != "" {
		b.WriteString(i.Path)
		b.WriteString(": ")
	}
	b.WriteString(i.Message)
	return b.String()
}

// Issues is a list of findings, as returned by Validate.
type Issues []Issue

// HasErrors reports whether any issue has SeverityError.
func (is Issues) HasErrors() bool {
	for _, i := range is {
		if i.Severity == SeverityError {
			return true
		}
	}
	return false
}

// Errors returns the issues with SeverityError.
func (is Issues) Errors() Issues { return is.filter(SeverityError) }

// Warnings returns the issues with SeverityWarning.
func (is Issues) Warnings() Issues { return is.filter(SeverityWarning) }

func (is Issues) filter(s Severity) Issues {
	var out Issues
	for _, i := range is {
		if i.Severity == s {
			out = append(out, i)
		}
	}
	return out
}

// maxListed bounds how many issues Err spells out.
const maxListed = 12

// Err folds the error-severity issues into one *fail.Error with code
// "config.invalid" and exit code fail.ExitUsage. It returns nil when there
// are none; warnings never produce an error. The full list is available from
// the returned error through errors.As with *InvalidError.
func (is Issues) Err() error { return is.err("") }

func (is Issues) err(file string) error {
	errs := is.Errors()
	if len(errs) == 0 {
		return nil
	}

	what := "The config file is invalid"
	target := "the config file"
	if file != "" {
		what = fmt.Sprintf("The config file %s is invalid", file)
		target = file
	}

	var why strings.Builder
	if len(errs) == 1 {
		why.WriteString(errs[0].String())
	} else {
		fmt.Fprintf(&why, "%d problems:", len(errs))
		for n, i := range errs {
			if n == maxListed {
				fmt.Fprintf(&why, "\n  … and %d more", len(errs)-n)
				break
			}
			why.WriteString("\n  ")
			why.WriteString(i.String())
		}
	}

	next := fmt.Sprintf("Edit %s to fix each problem and retry, or run `taut init` to generate a fresh config file.", target)
	e := fail.Wrap(&InvalidError{Issues: errs}, "config.invalid", what, why.String(), next)
	return e.WithExit(fail.ExitUsage)
}

// InvalidError carries the structured issues behind a "config.invalid"
// *fail.Error. Retrieve it with errors.As.
type InvalidError struct {
	Issues Issues
}

func (e *InvalidError) Error() string {
	parts := make([]string, len(e.Issues))
	for n, i := range e.Issues {
		parts[n] = i.String()
	}
	return strings.Join(parts, "; ")
}

// SyntaxError carries the position behind a "config.syntax" *fail.Error.
// Retrieve it with errors.As.
type SyntaxError struct {
	// Line is the 1-based line of the error, or 0 if unknown.
	Line int
	// Message is the parser's description.
	Message string
}

func (e *SyntaxError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d: %s", e.Line, e.Message)
	}
	return e.Message
}
