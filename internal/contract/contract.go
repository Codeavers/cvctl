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
	LabelExpose   = "cvctl.expose"
	LabelEnv      = "cvctl.env"
	LabelBackup   = "cvctl.backup"
	LabelExternal = "cvctl.external"
)

type HealthSpec struct {
	Service string
	HTTP    string
	Port    int
	Timeout time.Duration
}

func (h HealthSpec) EffectivePort(exposePort int) int {
	if h.Port > 0 {
		return h.Port
	}
	return exposePort
}

type VerifySpec struct {
	Service string
	SQL     string
}

type Hook struct {
	Service string
	Run     string
	// Timeout is a Go duration; empty means the host default. Validate checks it.
	Timeout string
}

type Hooks struct {
	PreDeploy  []Hook
	PostDeploy []Hook
}

type Bucket struct {
	Name   string
	Public bool
}

type Ext struct {
	Health HealthSpec
	Verify *VerifySpec
	Hooks  Hooks
	S3     map[string][]Bucket
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
	ExposePort      int
}

type Project struct {
	Dir        string
	Compose    *types.Project
	X          Ext
	Apps       []AppService
	Data       []DataService
	RawCompose []byte
}

func (p *Project) HealthPort() int {
	if p.X.Health.Port > 0 {
		return p.X.Health.Port
	}
	for _, a := range p.Apps {
		if a.Name == p.X.Health.Service {
			return a.ExposePort
		}
	}
	return 0
}

func (p *Project) Buckets(service string) []Bucket {
	if p.X.S3 == nil {
		return nil
	}
	return p.X.S3[service]
}

func (p *Project) PublicBuckets(service string) []Bucket {
	var out []Bucket
	for _, b := range p.Buckets(service) {
		if b.Public {
			out = append(out, b)
		}
	}
	return out
}

type rawExt struct {
	Health struct {
		Service string `yaml:"service"`
		HTTP    string `yaml:"http"`
		Port    int    `yaml:"port"`
		Timeout string `yaml:"timeout"`
	} `yaml:"health"`
	Verify *struct {
		Service string `yaml:"service"`
		SQL     string `yaml:"sql"`
	} `yaml:"verify"`
	Hooks struct {
		PreDeploy  []rawHook `yaml:"pre-deploy"`
		PostDeploy []rawHook `yaml:"post-deploy"`
	} `yaml:"hooks"`
	S3 map[string]struct {
		Buckets map[string]struct {
			Public bool `yaml:"public"`
		} `yaml:"buckets"`
	} `yaml:"s3"`
}

type rawHook struct {
	Service string `yaml:"service"`
	Run     string `yaml:"run"`
	Timeout string `yaml:"timeout"`
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

	if issues := CheckFields(raw); len(issues) > 0 {
		return nil, &output.IssuesError{Issues: issues}
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
		o.SkipInclude = true
		o.SkipExtends = true
		o.SetProjectName("cvctl-lint", true)
	})
	if err != nil {
		return nil, composeErr(fmt.Sprintf("invalid %s: %v", path, err), "fix the compose file syntax")
	}

	x := Ext{Health: HealthSpec{
		Service: doc.X.Health.Service,
		HTTP:    doc.X.Health.HTTP,
		Port:    doc.X.Health.Port,
	}}
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
	if len(doc.X.S3) > 0 {
		x.S3 = make(map[string][]Bucket, len(doc.X.S3))
		for svcName, s3Data := range doc.X.S3 {
			var bks []Bucket
			for bName, bData := range s3Data.Buckets {
				bks = append(bks, Bucket{
					Name:   bName,
					Public: bData.Public,
				})
			}
			sort.Slice(bks, func(i, j int) bool {
				return bks[i].Name < bks[j].Name
			})
			x.S3[svcName] = bks
		}
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
				ExternalAllowed: svc.Labels[LabelExternal] == "allowed",
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
			d.ExposePort, _ = strconv.Atoi(svc.Labels[LabelExpose])
			p.Data = append(p.Data, d)
			continue
		}
		if role := svc.Labels[LabelRole]; role != "" {
			a := AppService{Name: n, Role: role, Image: svc.Image}
			// A non-integer label leaves ExposePort 0; Validate reports it.
			a.ExposePort, _ = strconv.Atoi(svc.Labels[LabelExpose])
			p.Apps = append(p.Apps, a)
		}
	}
	return p, nil
}
