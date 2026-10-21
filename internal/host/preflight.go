package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"

	"github.com/joho/godotenv"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

// PreflightStep verifies the machine can be managed; it never changes anything.
type PreflightStep struct{}

func (PreflightStep) Name() string { return "preflight" }

// osInfo is the subset of /etc/os-release that decides how the host is prepared.
type osInfo struct {
	ID             string
	IDLike         []string
	Codename       string
	UbuntuCodename string
}

// hostOS reads /etc/os-release. A missing file yields an empty osInfo: capabilities
// are checked directly, so an unknown distribution is not an error.
func hostOS(fsys sys.FS) (osInfo, error) {
	b, err := fsys.ReadFile("/etc/os-release")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return osInfo{}, nil
		}
		return osInfo{}, osReleaseErr(err)
	}
	m, err := godotenv.Unmarshal(string(b))
	if err != nil {
		return osInfo{}, osReleaseErr(err)
	}
	return osInfo{
		ID:             strings.ToLower(m["ID"]),
		IDLike:         strings.Fields(strings.ToLower(m["ID_LIKE"])),
		Codename:       m["VERSION_CODENAME"],
		UbuntuCodename: m["UBUNTU_CODENAME"],
	}, nil
}

func osReleaseErr(err error) error {
	return &output.Error{
		Code:    "E_HOST_OS_RELEASE",
		Message: fmt.Sprintf("cannot use /etc/os-release: %v", err),
		Hint:    "check /etc/os-release syntax",
	}
}

func (o osInfo) is(distro string) bool {
	if o.ID == distro {
		return true
	}
	for _, l := range o.IDLike {
		if l == distro {
			return true
		}
	}
	return false
}

// apt reports whether the host uses apt packages (Debian, Ubuntu and derivatives).
func (o osInfo) apt() bool { return o.is("debian") || o.is("ubuntu") }

// dockerRepo returns the download.docker.com distribution and suite for the host.
func (o osInfo) dockerRepo() (distro, codename string, ok bool) {
	var name string
	switch {
	case o.is("ubuntu"):
		distro, name = "ubuntu", o.UbuntuCodename
		if name == "" {
			name = o.Codename
		}
	case o.is("debian"):
		distro, name = "debian", o.Codename
	default:
		return "", "", false
	}
	if !codenameRe.MatchString(name) {
		return "", "", false
	}
	return distro, name, true
}

var codenameRe = regexp.MustCompile(`^[a-z]+$`)

func (PreflightStep) Check(_ context.Context, e *Env) (Plan, error) {
	if fi, err := e.FS.Stat("/run/systemd/system"); err != nil || !fi.IsDir {
		return Plan{}, &output.Error{
			Code:    "E_HOST_UNSUPPORTED",
			Message: "cvctl init needs a Linux host with systemd",
			Hint:    "run it on a systemd-based Linux host (/run/systemd/system must exist)",
		}
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

// hasCommand reports whether name is on PATH. name must be a constant, never user input.
func hasCommand(ctx context.Context, e *Env, name string) bool {
	res, err := e.Run.Run(ctx, sys.Cmd{Name: "sh", Args: []string{"-c", "command -v " + name}})
	return err == nil && res.ExitCode == 0
}
