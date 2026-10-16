package host

import (
	"context"
	"fmt"
	"path"

	"github.com/codeavers/cvctl/internal/output"
)

const (
	autoUpgradesPath       = "/etc/apt/apt.conf.d/20auto-upgrades"
	autoUpgradesContent    = "APT::Periodic::Update-Package-Lists \"1\";\nAPT::Periodic::Unattended-Upgrade \"1\";\n"
	cvctlUnattendedPath    = "/etc/apt/apt.conf.d/52cvctl-unattended"
	cvctlUnattendedContent = "Unattended-Upgrade::Automatic-Reboot \"false\";\n"
)

type unattendedFileSpec struct {
	path    string
	content string
}

var unattendedFiles = []unattendedFileSpec{
	{path: autoUpgradesPath, content: autoUpgradesContent},
	{path: cvctlUnattendedPath, content: cvctlUnattendedContent},
}

type UnattendedStep struct{}

func (UnattendedStep) Name() string { return "unattended-upgrades" }

func (UnattendedStep) Check(_ context.Context, e *Env) (Plan, error) {
	var changes []string
	for _, f := range unattendedFiles {
		needsWrite, err := fileNeedsWrite(e.FS, f.path, []byte(f.content), 0o644, rootOwner)
		if err != nil {
			return Plan{}, unattendedErr(f.path, err)
		}
		if needsWrite {
			changes = append(changes, "configure "+f.path)
		}
	}
	return Plan{Changes: changes}, nil
}

func (UnattendedStep) Apply(_ context.Context, e *Env, p Plan) (string, error) {
	if len(p.Changes) == 0 {
		return "", nil
	}

	changeSet := make(map[string]bool, len(p.Changes))
	for _, c := range p.Changes {
		changeSet[c] = true
	}

	for _, f := range unattendedFiles {
		if !changeSet["configure "+f.path] {
			continue
		}
		if err := e.FS.MkdirAll(path.Dir(f.path), 0o755, rootOwner); err != nil {
			return "", unattendedErr(f.path, err)
		}
		if err := e.FS.WriteFile(f.path, []byte(f.content), 0o644, rootOwner); err != nil {
			return "", unattendedErr(f.path, err)
		}
	}

	return "", nil
}

func unattendedErr(p string, err error) error {
	return &output.Error{
		Code:    "E_UNATTENDED_FAILED",
		Message: fmt.Sprintf("write %s failed: %v", p, err),
		Hint:    fmt.Sprintf("check `write %s` permissions and disk space", p),
	}
}
