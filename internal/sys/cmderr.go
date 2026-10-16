package sys

import (
	"fmt"
	"strings"

	"github.com/codeavers/cvctl/internal/output"
)

// CmdErr formats a command failure as an *output.Error.
// If runErr is non-nil (the command failed to start), Message is "<cmd>: <runErr>".
// Otherwise, Message is "<cmd> failed (exit N)", followed by ": <trimmed stderr>" if stderr is non-empty.
func CmdErr(code string, cmd Cmd, res Result, runErr error, hint string) *output.Error {
	var msg string
	if runErr != nil {
		msg = fmt.Sprintf("%s: %v", cmd.String(), runErr)
	} else {
		stderr := strings.TrimSpace(string(res.Stderr))
		msg = fmt.Sprintf("%s failed (exit %d)", cmd.String(), res.ExitCode)
		if stderr != "" {
			msg += ": " + stderr
		}
	}
	return &output.Error{
		Code:    code,
		Message: msg,
		Hint:    hint,
	}
}
