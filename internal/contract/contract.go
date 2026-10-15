// Package contract loads and validates a project's compose.yml contract
// (cvctl.* labels plus the x-cvctl extension).
package contract

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/kinds"
)

const (
	LabelRole     = "cvctl.role"
	LabelKind     = "cvctl.kind"
	LabelVersion  = "cvctl.version"
	LabelEnv      = "cvctl.env"
	LabelBackup   = "cvctl.backup"
	LabelExternal = "cvctl.external"
)

type HealthSpec struct {
	Service string
	HTTP    string
	Timeout time.Duration
}

type VerifySpec struct {
	Service string
	SQL     string
}

type Hook struct {
	Service string
	Run     string
}

type Hooks struct {
	PreDeploy  []Hook
	PostDeploy []Hook
}

type Ext struct {
	Health HealthSpec
	Verify *VerifySpec
	Hooks  Hooks
}

type AppService struct {
	Name       string
	Role       string
	Image      string
	ExposePort int
}

type DataService struct {
	Name            string
	Kind            string
	Version         string
	EnvPrefix       string
	ExternalAllowed bool
	Backup          string
}

type Project struct {
	Dir        string
	Compose    *types.Project
	X          Ext
	Apps       []AppService
	Data       []DataService
	RawCompose []byte
}

type rawExt struct {
	Health struct {
		Service string `yaml:"service"`
		HTTP    string `yaml:"http"`
		Timeout string `yaml:"timeout"`
	} `yaml:"health"`
	Verify *struct {
		Service string `yaml:"service"`
		SQL     string `yaml:"sql"`
	} `yaml:"verify"`
	Hooks struct {
		PreDeploy  []rawHook `yaml:"pre_deploy"`
		PostDeploy []rawHook `yaml:"post_deploy"`
	} `yaml:"hooks"`
}

type rawHook struct {
	Service string `yaml:"service"`
	Run     string `yaml:"run"`
}

func composeErr(msg, hint string) error {
	return &output.Error{Code: "E_CONTRACT_COMPOSE_INVALID", Message: msg, Hint: hint}
}

func Load(dir string, cat kinds.Catalog) (*Project, error) {
	path := filepath.Join(dir, "compose.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &output.Error{
			Code:    "E_CONTRACT_NO_COMPOSE",
			Message: fmt.Sprintf("cannot read %s: %v", path, err),
			Hint:    "run from the infra repo root or pass the directory containing compose.yml",
		}
	}

	var doc struct {
		X rawExt `yaml:"x-cvctl"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, composeErr(fmt.Sprintf("cannot parse x-cvctl in %s: %v", path, err), "fix the x-cvctl block")
	}

	cp, err := loader.LoadWithContext(context.Background(), types.ConfigDetails{
		WorkingDir:  dir,
		ConfigFiles: []types.ConfigFile{{Filename: path, Content: raw}},
		Environment: types.Mapping{},
	}, func(o *loader.Options) {
		o.SkipInterpolation = true
		o.SkipConsistencyCheck = true
		o.SkipResolveEnvironment = true
		o.SetProjectName("cvctl-lint", true)
	})
	if err != nil {
		return nil, composeErr(fmt.Sprintf("invalid %s: %v", path, err), "fix the compose file syntax")
	}

	x := Ext{Health: HealthSpec{Service: doc.X.Health.Service, HTTP: doc.X.Health.HTTP}}
	if t := doc.X.Health.Timeout; t != "" {
		d, err := time.ParseDuration(t)
		if err != nil {
			return nil, composeErr(fmt.Sprintf("x-cvctl.health.timeout %q: %v", t, err), "use a duration such as 30s")
		}
		x.Health.Timeout = d
	}
	if v := doc.X.Verify; v != nil {
		x.Verify = &VerifySpec{Service: v.Service, SQL: v.SQL}
	}
	for _, h := range doc.X.Hooks.PreDeploy {
		x.Hooks.PreDeploy = append(x.Hooks.PreDeploy, Hook(h))
	}
	for _, h := range doc.X.Hooks.PostDeploy {
		x.Hooks.PostDeploy = append(x.Hooks.PostDeploy, Hook(h))
	}

	p := &Project{Dir: dir, Compose: cp, X: x, RawCompose: raw}
	names := make([]string, 0, len(cp.Services))
	for n := range cp.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		svc := cp.Services[n]
		if kind := svc.Labels[LabelKind]; kind != "" {
			d := DataService{
				Name:            n,
				Kind:            kind,
				Version:         svc.Labels[LabelVersion],
				EnvPrefix:       svc.Labels[LabelEnv],
				ExternalAllowed: svc.Labels[LabelExternal] == "true",
			}
			if k, ok := cat[kind]; ok {
				if d.Version == "" {
					d.Version = k.DefaultVersion
				}
				d.Backup = k.Backup
			}
			if b := svc.Labels[LabelBackup]; b != "" {
				d.Backup = b
			}
			p.Data = append(p.Data, d)
			continue
		}
		if role := svc.Labels[LabelRole]; role != "" {
			a := AppService{Name: n, Role: role, Image: svc.Image}
			if len(svc.Expose) > 0 {
				a.ExposePort, _ = strconv.Atoi(strings.SplitN(svc.Expose[0], "/", 2)[0])
			}
			p.Apps = append(p.Apps, a)
		}
	}
	return p, nil
}
