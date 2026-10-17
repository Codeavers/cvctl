package host

import (
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

// CmdErr forwards to sys.CmdErr.
func CmdErr(code string, cmd sys.Cmd, res sys.Result, runErr error, hint string) *output.Error {
	return sys.CmdErr(code, cmd, res, runErr, hint)
}
