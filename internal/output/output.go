// Package output renders command results and errors as human text or as a
// single JSON document, and maps errors to process exit codes.
package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Error is a stable, coded error. Every error carries a hint.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

type Issue struct {
	Code    string `json:"code"`
	Service string `json:"service"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

// IssuesError reports lint findings; Fail emits them under "issues".
type IssuesError struct {
	Issues []Issue
}

func (e *IssuesError) Error() string {
	return fmt.Sprintf("E_LINT_FAILED: %d issue(s) found", len(e.Issues))
}

// UsageError marks bad invocations (unknown command/flag, wrong args); exit 2.
type UsageError struct {
	Err error
}

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }

type Printer struct {
	JSON bool
	Out  io.Writer
	Err  io.Writer
}

type successEnvelope struct {
	OK   bool `json:"ok"`
	Data any  `json:"data"`
}

type failureEnvelope struct {
	OK     bool    `json:"ok"`
	Error  *Error  `json:"error"`
	Issues []Issue `json:"issues,omitempty"`
}

// Result prints v as JSON in JSON mode; otherwise calls human (if non-nil).
func (p *Printer) Result(v any, human func(io.Writer)) {
	if p.JSON {
		p.encode(successEnvelope{OK: true, Data: v})
		return
	}
	if human != nil {
		human(p.Out)
	}
}

// Fail prints err and returns the process exit code.
func (p *Printer) Fail(err error) int {
	code := ExitError
	var usage *UsageError
	if errors.As(err, &usage) {
		code = ExitUsage
	}

	var oe *Error
	var ie *IssuesError
	var issues []Issue
	switch {
	case errors.As(err, &oe):
	case errors.As(err, &ie):
		issues = ie.Issues
		oe = &Error{Code: "E_LINT_FAILED", Message: fmt.Sprintf("%d issue(s) found", len(ie.Issues)), Hint: "fix the listed issues and re-run lint"}
	case code == ExitUsage:
		oe = &Error{Code: "E_CLI_USAGE", Message: err.Error(), Hint: "run with --help to see valid usage"}
	default:
		oe = &Error{Code: "E_INTERNAL", Message: err.Error(), Hint: "re-run with --json for details or report a bug"}
	}

	if p.JSON {
		p.encode(failureEnvelope{OK: false, Error: oe, Issues: issues})
		return code
	}
	fmt.Fprintf(p.Err, "error %s: %s\n", oe.Code, oe.Message)
	for _, i := range issues {
		fmt.Fprintf(p.Err, "  %s [%s] %s (hint: %s)\n", i.Code, i.Service, i.Message, i.Hint)
	}
	fmt.Fprintf(p.Err, "hint: %s\n", oe.Hint)
	return code
}

func (p *Printer) encode(v any) {
	// json.Encoder appends the trailing newline.
	_ = json.NewEncoder(p.Out).Encode(v)
}
