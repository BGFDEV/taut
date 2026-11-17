// Package fail defines Taut's operator-facing error type.
//
// Every error a user can see answers three questions: what happened, why, and
// the exact next step to take. The same value renders in the TUI, as plain
// text and as JSON, so the answer never depends on how Taut was invoked.
package fail

import (
	"errors"
	"fmt"
)

// Exit codes are part of the CLI contract; CI pipelines branch on them.
const (
	ExitOK         = 0
	ExitFailure    = 1
	ExitUsage      = 2
	ExitBlockers   = 3 // preflight found something that must be fixed first
	ExitPermission = 4 // a required permission or scope is missing
)

// Error is a structured, user-facing error.
type Error struct {
	// Code is a stable, machine-readable identifier such as
	// "config.not_found". Scripts may match on it.
	Code string `json:"code"`
	// What happened, in one plain sentence.
	What string `json:"what"`
	// Why it happened, if known.
	Why string `json:"why,omitempty"`
	// Next is the exact step that resolves it, ideally a command.
	Next string `json:"next,omitempty"`
	// Exit is the process exit code; zero means ExitFailure.
	Exit int `json:"-"`
	// Err is the underlying cause, kept for errors.Is/As and debug logs.
	Err error `json:"-"`
	// Quiet marks an error whose explanation has already been shown (a
	// report listing its own failures, say). It still sets the exit code
	// but is not printed again.
	Quiet bool `json:"-"`
}

// New builds an Error.
func New(code, what, why, next string) *Error {
	return &Error{Code: code, What: what, Why: why, Next: next}
}

// Wrap builds an Error around an underlying cause. If why is empty the
// cause's message is used.
func Wrap(err error, code, what, why, next string) *Error {
	if why == "" && err != nil {
		why = err.Error()
	}
	return &Error{Code: code, What: what, Why: why, Next: next, Err: err}
}

// WithExit sets the exit code and returns the same error for chaining.
func (e *Error) WithExit(code int) *Error {
	e.Exit = code
	return e
}

func (e *Error) Error() string {
	s := e.What
	if e.Why != "" {
		s += ": " + e.Why
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// From converts any error into an *Error. Errors that are not already
// structured become a generic "unexpected" error so nothing reaches the user
// without a next step.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var fe *Error
	if errors.As(err, &fe) {
		return fe
	}
	return &Error{
		Code: "unexpected",
		What: "Something unexpected went wrong",
		Why:  err.Error(),
		Next: "Run `taut doctor` to check your environment, then retry with --verbose.",
		Err:  err,
	}
}

// ExitCode returns the process exit code for err.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	fe := From(err)
	if fe.Exit != 0 {
		return fe.Exit
	}
	return ExitFailure
}

// Usagef builds a usage error (exit code 2).
func Usagef(next, format string, args ...any) *Error {
	return New("usage", fmt.Sprintf(format, args...), "", next).WithExit(ExitUsage)
}

// Exit returns a quiet error that only carries an exit code, for commands
// that have already reported what went wrong as part of their output.
func Exit(code int, what string) *Error {
	return &Error{Code: "exit", What: what, Exit: code, Quiet: true}
}
