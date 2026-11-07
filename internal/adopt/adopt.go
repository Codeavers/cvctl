// Package adopt keeps the registry of services cvctl exposes through Traefik
// but does not deploy: no contract, no notifications, no backups.
package adopt

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

const (
	Dir       = "/etc/cvctl/adopted"
	RouteFile = "adopted.yml"
)

var (
	nameRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	targetRE = regexp.MustCompile(`^(http|https)://host:([0-9]{1,5})$`)
)

type Service struct {
	Name        string `yaml:"name" json:"name"`
	Domain      string `yaml:"domain" json:"domain"`
	Target      string `yaml:"target" json:"target"`
	InsecureTLS bool   `yaml:"insecure_tls,omitempty" json:"insecure_tls,omitempty"`
}

func usage(code, msg, hint string) error {
	return &output.UsageError{Err: &output.Error{Code: code, Message: msg, Hint: hint}}
}

func (s Service) Validate() error {
	if !nameRE.MatchString(s.Name) || strings.Contains(s.Name, "--") {
		return usage("E_ADOPT_NAME", fmt.Sprintf("invalid name %q", s.Name),
			"use lowercase letters, digits and single dashes")
	}
	if !instance.ValidHostname(s.Domain) {
		return usage("E_USAGE_DOMAIN", fmt.Sprintf("domain %q is not a valid hostname", s.Domain),
			"use a lowercase hostname such as app.example.com")
	}
	const hint = "use http://host:<port> or https://host:<port>"
	m := targetRE.FindStringSubmatch(s.Target)
	if m == nil {
		return usage("E_ADOPT_TARGET", fmt.Sprintf("invalid target %q", s.Target), hint)
	}
	if p, _ := strconv.Atoi(m[2]); p < 1 || p > 65535 {
		return usage("E_ADOPT_TARGET", fmt.Sprintf("invalid port in target %q", s.Target), hint)
	}
	if s.InsecureTLS && m[1] != "https" {
		return usage("E_ADOPT_TARGET", "--insecure-tls needs an https target", hint)
	}
	return nil
}

func (s Service) Port() int {
	m := targetRE.FindStringSubmatch(s.Target)
	if m == nil {
		return 0
	}
	p, _ := strconv.Atoi(m[2])
	return p
}

func path(dir, name string) string { return filepath.Join(dir, name+".yml") }

// Load returns the registry sorted by name. One unreadable file fails the whole
// load: rendering without it would silently drop that route.
func Load(fsys sys.FS, dir string) ([]Service, error) {
	entries, err := fsys.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var svcs []Service
	for _, ent := range entries {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".yml" {
			continue
		}
		p := filepath.Join(dir, ent.Name())
		b, err := fsys.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var s Service
		if err := yaml.Unmarshal(b, &s); err != nil {
			return nil, corrupt(p, err)
		}
		if err := s.Validate(); err != nil {
			return nil, corrupt(p, err)
		}
		svcs = append(svcs, s)
	}
	slices.SortFunc(svcs, func(a, b Service) int { return strings.Compare(a.Name, b.Name) })
	return svcs, nil
}

func corrupt(p string, err error) error {
	return &output.Error{Code: "E_ADOPT_REGISTRY", Message: fmt.Sprintf("cannot read %s: %v", p, err),
		Hint: "fix or delete the file"}
}

func Save(fsys sys.FS, dir string, s Service) error {
	if err := fsys.MkdirAll(dir, 0o755, sys.Owner{}); err != nil {
		return err
	}
	b, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	return fsys.WriteFile(path(dir, s.Name), b, 0o644, sys.Owner{})
}

func Remove(fsys sys.FS, dir, name string) error {
	return fsys.Remove(path(dir, name))
}

// Sync rewrites the Traefik file from the registry, or removes it when the
// registry is empty: Traefik rejects an empty dynamic file.
func Sync(fsys sys.FS, dir, dynamicDir, edgeMode, gateway string) error {
	svcs, err := Load(fsys, dir)
	if err != nil {
		return err
	}
	return WriteRoutes(fsys, dynamicDir, svcs, edgeMode, gateway)
}

// WriteRoutes takes the list instead of the registry so a removal can publish
// the new routes first and drop the registry entry only once everything worked.
func WriteRoutes(fsys sys.FS, dynamicDir string, svcs []Service, edgeMode, gateway string) error {
	b, err := Render(svcs, edgeMode, gateway)
	if err != nil {
		return err
	}
	dst := filepath.Join(dynamicDir, RouteFile)
	if b == nil {
		return fsys.Remove(dst)
	}
	return fsys.WriteFile(dst, b, 0o644, sys.Owner{})
}
