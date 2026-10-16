package host

import (
	"context"

	"github.com/joho/godotenv"

	"github.com/codeavers/cvctl/internal/output"
)

// PreflightStep verifies the machine can be managed; it never changes anything.
type PreflightStep struct{}

func (PreflightStep) Name() string { return "preflight" }

func (PreflightStep) Check(_ context.Context, e *Env) (Plan, error) {
	unsupported := &output.Error{
		Code:    "E_HOST_UNSUPPORTED",
		Message: "cvctl init supports only Ubuntu 24.04",
		Hint:    "run it on an Ubuntu 24.04 host (see /etc/os-release)",
	}
	b, err := e.FS.ReadFile("/etc/os-release")
	if err != nil {
		return Plan{}, unsupported
	}
	m, err := godotenv.Unmarshal(string(b))
	if err != nil || m["ID"] != "ubuntu" || m["VERSION_ID"] != "24.04" {
		return Plan{}, unsupported
	}
	if e.EUID != 0 {
		return Plan{}, &output.Error{
			Code:    "E_HOST_NOT_ROOT",
			Message: "cvctl init must run as root",
			Hint:    "sudo cvctl init",
		}
	}
	return Plan{}, nil
}

func (PreflightStep) Apply(context.Context, *Env, Plan) (string, error) { return "", nil }
