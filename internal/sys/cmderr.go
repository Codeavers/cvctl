package sys

import (
	"fmt"
	"strings"

	"github.com/codeavers/cvctl/internal/output"
)

// CmdErr formats a command failure as an *output.Error.
// If runErr is non-nil (the command failed to start), Message is "<cmd>: <runErr>".
// Otherwise, Message is "<cmd> failed (exit N)", followed by ": <trimmed stderr>" if stderr is non-empty,
// or ": <trimmed stdout>" if stderr is empty and stdout is non-empty.
func CmdErr(code string, cmd Cmd, res Result, runErr error, hint string) *output.Error {
	var msg string
	if runErr != nil {
		msg = fmt.Sprintf("%s: %v", cmd.String(), runErr)
	} else {
		out := strings.TrimSpace(string(res.Stderr))
		if out == "" {
			out = strings.TrimSpace(string(res.Stdout))
		}
		msg = fmt.Sprintf("%s failed (exit %d)", cmd.String(), res.ExitCode)
		if out != "" {
			msg += ": " + out
		}
	}
	return &output.Error{
		Code:    code,
		Message: msg,
		Hint:    hint,
	}
}
