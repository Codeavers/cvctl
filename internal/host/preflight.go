package host

import (
	"context"

	"github.com/joho/godotenv"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

// PreflightStep verifies the machine can be managed; it never changes anything.
type PreflightStep struct{}

func (PreflightStep) Name() string { return "preflight" }

// supportedUbuntu maps each supported Ubuntu LTS release to the codename of its
// download.docker.com repository.
var supportedUbuntu = map[string]string{"24.04": "noble", "26.04": "resolute"}

// ubuntuCodename returns the Docker repository codename of a supported host.
func ubuntuCodename(fs sys.FS) (string, error) {
	unsupported := &output.Error{
		Code:    "E_HOST_UNSUPPORTED",
		Message: "cvctl init supports only Ubuntu 24.04 and 26.04 LTS",
		Hint:    "run it on an Ubuntu 24.04 or 26.04 host (see /etc/os-release)",
	}
	b, err := fs.ReadFile("/etc/os-release")
	if err != nil {
		return "", unsupported
	}
	m, err := godotenv.Unmarshal(string(b))
	if err != nil || m["ID"] != "ubuntu" {
		return "", unsupported
	}
	codename, ok := supportedUbuntu[m["VERSION_ID"]]
	if !ok {
		return "", unsupported
	}
	return codename, nil
}

func (PreflightStep) Check(_ context.Context, e *Env) (Plan, error) {
	if _, err := ubuntuCodename(e.FS); err != nil {
		return Plan{}, err
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
