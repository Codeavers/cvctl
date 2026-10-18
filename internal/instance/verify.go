package instance

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/envspec"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/kinds"
)

// placeholderDigest stands in for digests that only a deploy resolves, so
// paths that never deploy can still run the other decision checks.
const placeholderDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// Verify runs contract.Validate (including the raw allowlist) and Check on a
// loaded instance and reports every issue as one E_DEPLOY_INVALID error. The
// env spec in infraDir is used when it exists; any other spec error is
// returned. With needDigests false a missing app digest is not an issue.
func Verify(infraDir string, p *contract.Project, d *Decisions, cat kinds.Catalog, needDigests bool) error {
	var spec *envspec.Spec
	specPath := filepath.Join(infraDir, "env.spec.yml")
	if _, err := os.Stat(specPath); err == nil {
		if spec, err = envspec.Load(specPath); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if !needDigests {
		cp := *d
		cp.Digests = make(map[string]string, len(p.Apps))
		for k, v := range d.Digests {
			cp.Digests[k] = v
		}
		for _, a := range p.Apps {
			if cp.Digests[a.Name] == "" {
				cp.Digests[a.Name] = placeholderDigest
			}
		}
		d = &cp
	}
	issues := append(contract.Validate(p, cat, spec), Check(d, p, cat)...)
	if len(issues) == 0 {
		return nil
	}
	return issuesErr(issues)
}

func issuesErr(issues []output.Issue) error {
	output.SortIssues(issues)
	msgs := make([]string, 0, len(issues))
	for _, iss := range issues {
		msgs = append(msgs, fmt.Sprintf("%s: %s", iss.Code, iss.Message))
	}
	return &output.Error{
		Code:    "E_DEPLOY_INVALID",
		Message: strings.Join(msgs, "; "),
		Hint:    "fix the listed issues in the infra repo or cvctl.env",
	}
}

// WrapLoadErr turns the lint findings of contract.Load into an E_DEPLOY_INVALID
// error that carries the issue text, which an IssuesError does not.
func WrapLoadErr(err error) error {
	var ie *output.IssuesError
	if !errors.As(err, &ie) {
		return err
	}
	return issuesErr(ie.Issues)
}
