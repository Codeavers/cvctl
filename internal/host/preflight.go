package host

import (
	"context"
	"fmt"
	"regexp"
	"strconv"

	"github.com/joho/godotenv"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

// PreflightStep verifies the machine can be managed; it never changes anything.
type PreflightStep struct{}

func (PreflightStep) Name() string { return "preflight" }

// minUbuntu is the oldest release cvctl is tested on. Newer releases, LTS or not, are
// accepted: the only release-specific input is the Docker apt repository, which uses
// VERSION_CODENAME and is checked when Docker is installed.
const minUbuntu = 24.04

var codenameRe = regexp.MustCompile(`^[a-z]+$`)

// ubuntuCodename returns the codename of a supported Ubuntu host.
func ubuntuCodename(fs sys.FS) (string, error) {
	unsupported := &output.Error{
		Code:    "E_HOST_UNSUPPORTED",
		Message: fmt.Sprintf("cvctl init supports Ubuntu %.2f or later", minUbuntu),
		Hint:    "run it on an Ubuntu host (see ID, VERSION_ID and VERSION_CODENAME in /etc/os-release)",
	}
	b, err := fs.ReadFile("/etc/os-release")
	if err != nil {
		return "", unsupported
	}
	m, err := godotenv.Unmarshal(string(b))
	if err != nil || m["ID"] != "ubuntu" || !codenameRe.MatchString(m["VERSION_CODENAME"]) {
		return "", unsupported
	}
	v, err := strconv.ParseFloat(m["VERSION_ID"], 64)
	if err != nil || v < minUbuntu {
		return "", unsupported
	}
	return m["VERSION_CODENAME"], nil
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
