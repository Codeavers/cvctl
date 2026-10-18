package project

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/envspec"
	"github.com/codeavers/cvctl/internal/gitrepo"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/registry"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/kinds"
)

var deployOwner = sys.Owner{User: "deployer", Group: "deploy"}
var identRe = regexp.MustCompile(`^[a-z0-9_]+$`)
var instanceNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

const alphaNum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randomPassword32() (string, error) {
	b := make([]byte, 32)
	var buf [1]byte
	for i := 0; i < 32; {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", err
		}
		if int(buf[0]) >= 256/len(alphaNum)*len(alphaNum) {
			continue
		}
		b[i] = alphaNum[int(buf[0])%len(alphaNum)]
		i++
	}
	return string(b), nil
}

type RepoLike interface {
	Clone(ctx context.Context, url, branch string) error
	LsRemote(ctx context.Context, url string) error
	Fetch(ctx context.Context, branch string) (string, error)
	Show(ctx context.Context, commit, path string) ([]byte, error)
	Checkout(ctx context.Context, commit string) error
}

type Env struct {
	Run          sys.Runner
	FS           sys.FS
	Prompt       hostcfg.Prompter
	Interactive  bool
	Host         *hostcfg.Config
	Secrets      *hostcfg.Secrets
	Catalog      kinds.Catalog
	Deployer     *deploy.Deployer
	Data         DataRestorer
	DeployRoot   string
	StateRoot    string
	RepoFactory  func(dir, keyPath string) RepoLike
	Resolver     registry.Resolver
	PollInterval time.Duration
}

func (e *Env) deployRoot() string {
	if e.DeployRoot != "" {
		return e.DeployRoot
	}
	return "/srv/deploy"
}

func (e *Env) stateRoot() string {
	if e.StateRoot != "" {
		return e.StateRoot
	}
	return "/var/lib/cvctl"
}

func (e *Env) fs() sys.FS {
	if e.FS != nil {
		return e.FS
	}
	return sys.NewOSFS()
}

func (e *Env) runner() sys.Runner {
	if e.Run != nil {
		return e.Run
	}
	return sys.NewExecRunner()
}

func (e *Env) repo(dir, keyPath string) RepoLike {
	if e.RepoFactory != nil {
		return e.RepoFactory(dir, keyPath)
	}
	return &gitrepo.Repo{
		Run:     e.runner(),
		Dir:     dir,
		KeyPath: keyPath,
	}
}

func (e *Env) catalog() (kinds.Catalog, error) {
	if e.Catalog != nil {
		return e.Catalog, nil
	}
	return kinds.Load()
}

type AddOptions struct {
	Project  string
	Env      string
	Branch   string
	TrackTag string
	Strategy string
	Set      map[string]string
	Domains  map[string]string
	Ext      map[string]map[string]string
}

type UpdateOptions struct {
	Ext map[string]map[string]string
}

type AddResult struct {
	Instance     string
	Deploy       deploy.Result
	DeployKeyPub string
	Decisions    *instance.Decisions
	Warnings     []string
}

func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteByte('-')
		}
	}
	res := b.String()
	for strings.Contains(res, "--") {
		res = strings.ReplaceAll(res, "--", "-")
	}
	return strings.Trim(res, "-")
}

func ValidateInstance(deployRoot, stateRoot, inst, env string) error {
	if len(inst) == 0 || len(inst) > 63 || !instanceNameRe.MatchString(inst) || !strings.HasSuffix(inst, "-"+env) {
		return &output.Error{
			Code:    "E_INSTANCE_NAME",
			Message: fmt.Sprintf("invalid instance name %q", inst),
			Hint:    "use --name with letters, digits and '-'",
		}
	}
	expectedDeployDir := strings.TrimRight(deployRoot, "/") + "/" + inst
	if filepath.Join(deployRoot, inst) != expectedDeployDir {
		return &output.Error{
			Code:    "E_INSTANCE_NAME",
			Message: fmt.Sprintf("path traversal detected in instance %q", inst),
			Hint:    "use --name with letters, digits and '-'",
		}
	}
	expectedStateDir := strings.TrimRight(stateRoot, "/") + "/" + inst
	if filepath.Join(stateRoot, inst) != expectedStateDir {
		return &output.Error{
			Code:    "E_INSTANCE_NAME",
			Message: fmt.Sprintf("path traversal detected in instance %q", inst),
			Hint:    "use --name with letters, digits and '-'",
		}
	}
	return nil
}

func ValidateInstanceName(deployRoot, stateRoot, inst string) error {
	envName := ""
	if strings.HasSuffix(inst, "-dev") {
		envName = "dev"
	} else if strings.HasSuffix(inst, "-prod") {
		envName = "prod"
	}
	if envName == "" {
		return &output.Error{
			Code:    "E_INSTANCE_NAME",
			Message: fmt.Sprintf("invalid instance name %q: must end with -dev or -prod", inst),
			Hint:    "use --name with letters, digits and '-'",
		}
	}
	return ValidateInstance(deployRoot, stateRoot, inst, envName)
}

func repoURLErr(url string) error {
	return &output.Error{
		Code:    "E_PROJECT_REPO_URL",
		Message: fmt.Sprintf("invalid repository URL %q", gitrepo.RedactURL(url)),
		Hint:    "accepted formats: ssh://..., git@host:path, https://... (or file://... with CVCTL_ALLOW_FILE_REPOS=1)",
	}
}

func validateRepoURL(repoURL string) error {
	if strings.HasPrefix(repoURL, "-") {
		return repoURLErr(repoURL)
	}
	if strings.HasPrefix(repoURL, "file://") {
		if os.Getenv("CVCTL_ALLOW_FILE_REPOS") == "1" {
			return nil
		}
		return repoURLErr(repoURL)
	}
	if strings.HasPrefix(repoURL, "https://") {
		rest := strings.TrimPrefix(repoURL, "https://")
		if idx := strings.Index(rest, "@"); idx >= 0 {
			rest = rest[idx+1:]
		}
		if rest == "" || strings.HasPrefix(rest, "-") {
			return repoURLErr(repoURL)
		}
		return nil
	}
	if strings.HasPrefix(repoURL, "ssh://") {
		rest := strings.TrimPrefix(repoURL, "ssh://")
		if idx := strings.Index(rest, "@"); idx >= 0 {
			rest = rest[idx+1:]
		}
		if rest == "" || strings.HasPrefix(rest, "-") {
			return repoURLErr(repoURL)
		}
		return nil
	}
	if strings.HasPrefix(repoURL, "git@") {
		rest := strings.TrimPrefix(repoURL, "git@")
		host, path, found := strings.Cut(rest, ":")
		if found && host != "" && !strings.HasPrefix(host, "-") && path != "" {
			return nil
		}
	}
	return repoURLErr(repoURL)
}

func removeAll(fsys sys.FS, p string) error {
	fi, err := fsys.Lstat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fsys.Remove(p)
	}
	if fi.IsSymlink || !fi.IsDir {
		return fsys.Remove(p)
	}
	entries, err := fsys.ReadDir(p)
	if err == nil {
		for _, entry := range entries {
			child := filepath.Join(p, entry.Name())
			if entry.Type()&os.ModeSymlink != 0 {
				if err := fsys.Remove(child); err != nil {
					return err
				}
				continue
			}
			if err := removeAll(fsys, child); err != nil {
				return err
			}
		}
	}
	return fsys.Remove(p)
}

func DeriveInstanceName(repoURL, project, env string) string {
	if env == "" {
		env = "dev"
	}
	name := project
	if name == "" {
		trimmed := strings.TrimRight(repoURL, "/")
		trimmed = strings.TrimSuffix(trimmed, ".git")
		idx := strings.LastIndexAny(trimmed, "/:")
		if idx >= 0 {
			name = trimmed[idx+1:]
		} else {
			name = trimmed
		}
		name = strings.TrimSuffix(name, "-infra")
	}
	name = sanitizeName(name)
	return fmt.Sprintf("%s-%s", name, strings.ToLower(env))
}

func promptDataServiceMode(ctx context.Context, e *Env, dsName string, defaultMode string) (string, error) {
	if defaultMode == "" {
		defaultMode = "local"
	}
	for {
		ans := defaultMode
		if e.Prompt != nil {
			var err error
			ans, err = e.Prompt.Ask(hostcfg.Question{
				Key:     "data_" + dsName + "_mode",
				Prompt:  fmt.Sprintf("Mode for data service %s (local|external)", dsName),
				Options: []string{"local", "external"},
			}, defaultMode)
			if err != nil {
				return "", err
			}
		}
		ans = strings.ToLower(strings.TrimSpace(ans))
		if ans == "" {
			ans = defaultMode
		}
		if ans == "local" || ans == "external" {
			return ans, nil
		}
		if !e.Interactive {
			return "", &output.Error{
				Code:    "E_PROJECT_ANSWER_INVALID",
				Message: fmt.Sprintf("invalid data mode %q for service %s: must be 'local' or 'external'", ans, dsName),
				Hint:    "answer 'local' or 'external'",
			}
		}
	}
}

func loadContractAndSpec(fsys sys.FS, infraDir string, cat kinds.Catalog) (*contract.Project, *envspec.Spec, error) {
	p, err := contract.Load(infraDir, cat)
	if err != nil {
		return nil, nil, err
	}

	specPath := filepath.Join(infraDir, "env.spec.yml")
	var spec *envspec.Spec
	var extra []output.Issue
	if _, statErr := fsys.Stat(specPath); statErr != nil {
		extra = append(extra, output.Issue{
			Code:    "E_CONTRACT_NO_ENVSPEC",
			Message: "env.spec.yml not found",
			Hint:    "create env.spec.yml next to compose.yml declaring every variable the compose file references",
		})
	} else if spec, err = envspec.Load(specPath); err != nil {
		var oe *output.Error
		if !errors.As(err, &oe) {
			return nil, nil, err
		}
		extra = append(extra, output.Issue{Code: oe.Code, Message: oe.Message, Hint: oe.Hint})
	}

	issues := append(contract.Validate(p, cat, spec), extra...)
	output.SortIssues(issues)
	if len(issues) > 0 {
		return nil, nil, &output.IssuesError{Issues: issues}
	}
	return p, spec, nil
}

func isSecretField(field string) bool {
	f := strings.ToLower(field)
	return strings.Contains(f, "password") ||
		strings.Contains(f, "secret") ||
		strings.Contains(f, "key") ||
		strings.Contains(f, "token") ||
		strings.Contains(f, "url")
}

func askExternalField(e *Env, dsName, field, defaultVal string, secret bool) (string, error) {
	val := ""
	if e.Prompt != nil {
		ans, err := e.Prompt.Ask(hostcfg.Question{
			Key:    "data_" + dsName + "_ext_" + field,
			Prompt: fmt.Sprintf("%s external %s", dsName, field),
			Secret: secret,
		}, defaultVal)
		if err != nil {
			return "", err
		}
		val = ans
	}
	if val == "" && defaultVal != "" {
		val = defaultVal
	}
	return val, nil
}

func promptDataServices(ctx context.Context, e *Env, cat kinds.Catalog, p *contract.Project, d *instance.Decisions) error {
	for _, ds := range p.Data {
		k := cat[ds.Kind]
		sd := d.Services[ds.Name]

		mode := sd.Mode
		if mode == "" {
			mode = "local"
			defaultMode := "local"
			if d.Env == "prod" && len(p.PublicBuckets(ds.Name)) > 0 {
				defaultMode = "external"
			}
			if ds.ExternalAllowed {
				var err error
				mode, err = promptDataServiceMode(ctx, e, ds.Name, defaultMode)
				if err != nil {
					return err
				}
			}
			sd.Mode = mode
		}

		if mode == "local" {
			// Values left from an earlier external mode do not apply to a local service.
			sd.External = nil

			user := sd.Creds.User
			db := sd.Creds.DB
			pass := sd.Creds.Password

			if user == "" {
				user = strings.ToLower(contract.Normalize(ds.Name))
				user = strings.ReplaceAll(user, "-", "_")
				user = strings.ReplaceAll(user, ".", "_")
				if !identRe.MatchString(user) {
					return &output.Error{
						Code:    "E_DECISION_INVALID",
						Message: fmt.Sprintf("service %s normalizes to invalid identifier %q", ds.Name, user),
						Hint:    "use characters a-z, 0-9 and _",
					}
				}
			}
			if db == "" {
				db = user
			}
			if pass == "" {
				var err error
				pass, err = randomPassword32()
				if err != nil {
					return err
				}
			}

			appUser := sd.Creds.AppUser
			appPass := sd.Creds.AppPassword
			if k.UsesApp() {
				if appUser == "" {
					appUser = ds.Name + "-app"
				}
				if appPass == "" {
					var err error
					appPass, err = randomPassword32()
					if err != nil {
						return err
					}
				}
			}

			sd.Mode = "local"
			if sd.Version == "" {
				sd.Version = ds.Version
			}
			sd.Creds = kinds.Credentials{
				User:        user,
				DB:          db,
				Password:    pass,
				AppUser:     appUser,
				AppPassword: appPass,
			}
			d.Services[ds.Name] = sd
		} else {
			if sd.External == nil {
				sd.External = make(map[string]string)
			}
			buckets := p.Buckets(ds.Name)
			hasBuckets := len(buckets) > 0
			for _, field := range k.External {
				if hasBuckets && field == "bucket" {
					continue
				}
				if sd.External[field] != "" {
					continue
				}
				def := ""
				if field == "region" {
					def = "auto"
				}
				val, err := askExternalField(e, ds.Name, field, def, isSecretField(field))
				if err != nil {
					return err
				}
				sd.External[field] = val
			}

			for _, b := range buckets {
				bLower := strings.ToLower(b.Name)
				bKey := "bucket_" + bLower
				if sd.External[bKey] == "" {
					val, err := askExternalField(e, ds.Name, bKey, b.Name, false)
					if err != nil {
						return err
					}
					sd.External[bKey] = val
				}

				if b.Public {
					pKey := "public_url_" + bLower
					if sd.External[pKey] == "" {
						val, err := askExternalField(e, ds.Name, pKey, "", false)
						if err != nil {
							return err
						}
						sd.External[pKey] = val
					}
				}
			}
			sd.Mode = "external"
			if sd.Version == "" {
				sd.Version = ds.Version
			}
			d.Services[ds.Name] = sd
		}
	}
	return nil
}

func promptDomains(ctx context.Context, e *Env, p *contract.Project, d *instance.Decisions) error {
	for _, a := range p.Apps {
		if a.ExposePort > 0 && d.Domains[a.Name] == "" {
			domain := ""
			if e.Prompt != nil {
				ans, err := e.Prompt.Ask(hostcfg.Question{
					Key:    "domain_" + a.Name,
					Prompt: fmt.Sprintf("Domain for %s", a.Name),
				}, "")
				if err != nil {
					return err
				}
				domain = ans
			}
			d.Domains[a.Name] = domain
		}
	}
	for _, ds := range p.Data {
		if ds.ExposePort > 0 && d.Services[ds.Name].Mode == "local" && d.Domains[ds.Name] == "" {
			domain := ""
			if e.Prompt != nil {
				ans, err := e.Prompt.Ask(hostcfg.Question{
					Key:    "domain_" + ds.Name,
					Prompt: fmt.Sprintf("Domain for %s", ds.Name),
				}, "")
				if err != nil {
					return err
				}
				domain = ans
			}
			d.Domains[ds.Name] = domain
		}
	}
	return nil
}

func applyExt(d *instance.Decisions, p *contract.Project, cat kinds.Catalog, ext map[string]map[string]string) error {
	if len(ext) == 0 {
		return nil
	}
	knownData := make(map[string]contract.DataService, len(p.Data))
	for _, ds := range p.Data {
		knownData[ds.Name] = ds
	}

	svcNames := make([]string, 0, len(ext))
	for s := range ext {
		svcNames = append(svcNames, s)
	}
	sort.Strings(svcNames)

	for _, svc := range svcNames {
		ds, ok := knownData[svc]
		if !ok {
			return &output.UsageError{
				Err: &output.Error{
					Code:    "E_PROJECT_UNKNOWN_SERVICE",
					Message: fmt.Sprintf("unknown service %q for --ext", svc),
					Hint:    "specify a data service defined in compose.yml",
				},
			}
		}

		sd := d.Services[svc]
		// --ext is the explicit way to move a local service to external storage.
		if sd.Mode == "local" && ds.ExternalAllowed {
			sd.Mode = "external"
			sd.External = nil
			d.Services[svc] = sd
		}
		if sd.Mode == "local" {
			return &output.UsageError{
				Err: &output.Error{
					Code:    "E_USAGE_INVALID",
					Message: fmt.Sprintf("service %q does not allow external mode, --ext is only valid for external services", svc),
					Hint:    "add the label cvctl.external: allowed to " + svc + " in compose.yml, or remove --ext",
				},
			}
		}

		if sd.Mode == "" {
			if !ds.ExternalAllowed {
				return &output.UsageError{
					Err: &output.Error{
						Code:    "E_USAGE_INVALID",
						Message: fmt.Sprintf("service %q does not allow external mode, --ext is only valid for external services", svc),
						Hint:    "remove --ext for this service",
					},
				}
			}
			sd.Mode = "external"
		}

		k := cat[ds.Kind]
		buckets := p.Buckets(svc)
		hasBuckets := len(buckets) > 0

		allowed := make(map[string]bool)
		for _, f := range k.External {
			if hasBuckets && f == "bucket" {
				continue
			}
			allowed[strings.ToLower(f)] = true
		}
		if hasBuckets {
			for _, b := range buckets {
				bLower := strings.ToLower(b.Name)
				allowed["bucket_"+bLower] = true
				if b.Public {
					allowed["public_url_"+bLower] = true
				}
			}
		}

		allowedList := make([]string, 0, len(allowed))
		for f := range allowed {
			allowedList = append(allowedList, f)
		}
		sort.Strings(allowedList)

		fieldNames := make([]string, 0, len(ext[svc]))
		for f := range ext[svc] {
			fieldNames = append(fieldNames, f)
		}
		sort.Strings(fieldNames)

		for _, f := range fieldNames {
			if !allowed[strings.ToLower(f)] {
				return &output.UsageError{
					Err: &output.Error{
						Code:    "E_USAGE_EXT",
						Message: fmt.Sprintf("unknown external field %q", svc+"."+f),
						Hint:    fmt.Sprintf("valid fields for %s: %s", svc, strings.Join(allowedList, ", ")),
					},
				}
			}
		}

		if sd.External == nil {
			sd.External = make(map[string]string)
		}
		for k, v := range ext[svc] {
			sd.External[strings.ToLower(k)] = v
		}
		d.Services[svc] = sd
	}
	return nil
}

func checkDecisions(d *instance.Decisions, p *contract.Project, cat kinds.Catalog) error {
	knownData := make(map[string]bool, len(p.Data))
	for _, ds := range p.Data {
		knownData[ds.Name] = true
	}
	var checkIssues []output.Issue
	for _, iss := range instance.Check(d, p, cat) {
		if iss.Code == "E_S3_PUBLIC_LOCAL_PROD" {
			checkIssues = append(checkIssues, iss)
			continue
		}
		if strings.HasPrefix(iss.Code, "E_DECISION_") {
			if iss.Service == "" || knownData[iss.Service] {
				checkIssues = append(checkIssues, iss)
			}
		}
	}
	if len(checkIssues) > 0 {
		return &output.IssuesError{Issues: checkIssues}
	}
	return nil
}

func promptEnvSpec(ctx context.Context, e *Env, spec *envspec.Spec, appEnvValues map[string]string) error {
	if spec == nil {
		return nil
	}
	names := make([]string, 0, len(spec.Vars))
	for n := range spec.Vars {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		v := spec.Vars[name]
		if v.Source == "prompt" {
			if _, ok := appEnvValues[name]; ok {
				continue
			}
			def := ""
			if v.Default != nil {
				def = *v.Default
			}
			if e.Prompt != nil {
				ans, err := e.Prompt.Ask(hostcfg.Question{
					Key:    "env_" + name,
					Prompt: fmt.Sprintf("Value for %s", name),
					Secret: v.Secret,
				}, def)
				if err != nil {
					return err
				}
				if strings.ContainsAny(ans, "\r\n") {
					return &output.Error{
						Code:    "E_ENVSPEC_VALUE",
						Message: fmt.Sprintf("var %s: value cannot contain newline or carriage return", name),
						Hint:    "remove newline and carriage return characters from the value",
					}
				}
				appEnvValues[name] = ans
			} else if def != "" {
				if strings.ContainsAny(def, "\r\n") {
					return &output.Error{
						Code:    "E_ENVSPEC_VALUE",
						Message: fmt.Sprintf("var %s: default value cannot contain newline or carriage return", name),
						Hint:    "remove newline and carriage return characters from the value",
					}
				}
				appEnvValues[name] = def
			}
		}
	}
	return nil
}

// runDeploy resolves desired state and deploys it. With locked, the caller holds
// the instance lock and the deploy is forced, because an explicit update must
// redeploy even when the desired state hash is unchanged.
func runDeploy(ctx context.Context, e *Env, repo RepoLike, d *instance.Decisions, actor string, locked bool) (deploy.Result, error) {
	if e.Deployer == nil {
		return deploy.Result{Status: state.StatusOK}, nil
	}
	if e.Deployer.Repo == nil {
		e.Deployer.Repo = func(string) deploy.CheckoutAPI {
			return repo
		}
	}
	deps := deploy.SourceDepsWithRoots(d.Instance, e.runner(), e.Host, e.Secrets, e.deployRoot(), e.stateRoot(), e.fs())
	if repo != nil {
		deps.Repo = repo
	}
	if e.Resolver != nil {
		deps.Resolver = e.Resolver
	}
	src := deploy.SourceFor(d, deps)
	desired, err := src.Desired(ctx, d)
	if err != nil {
		return deploy.Result{}, err
	}
	if locked {
		return e.Deployer.DeployLocked(ctx, d.Instance, desired, deploy.Options{Actor: actor, Force: true})
	}
	return e.Deployer.Deploy(ctx, d.Instance, desired, deploy.Options{Actor: actor})
}

func Add(ctx context.Context, e *Env, repoURL string, opts AddOptions) (*AddResult, error) {
	if opts.Strategy == "blue-green" {
		return nil, &output.Error{
			Code:    "E_STRATEGY_UNSUPPORTED",
			Message: "blue-green strategy is not supported yet",
			Hint:    "use recreate strategy",
		}
	}
	if opts.Strategy == "" {
		opts.Strategy = "recreate"
	}
	if opts.Strategy != "recreate" {
		return nil, &output.Error{
			Code:    "E_STRATEGY_UNSUPPORTED",
			Message: fmt.Sprintf("strategy %q is not supported", opts.Strategy),
			Hint:    "use recreate strategy",
		}
	}

	envName := opts.Env
	if envName == "" {
		envName = "dev"
	}
	if envName != "dev" && envName != "prod" {
		return nil, &output.UsageError{
			Err: &output.Error{
				Code:    "E_PROJECT_ENV_INVALID",
				Message: fmt.Sprintf("invalid environment %q: must be dev or prod", opts.Env),
				Hint:    "use --env dev or --env prod",
			},
		}
	}

	for k, v := range opts.Set {
		if strings.ContainsAny(v, "\r\n") {
			return nil, &output.Error{
				Code:    "E_ENVSPEC_VALUE",
				Message: fmt.Sprintf("--set %s: value cannot contain newline or carriage return", k),
				Hint:    "remove newline and carriage return characters from the value",
			}
		}
	}

	if err := validateRepoURL(repoURL); err != nil {
		return nil, err
	}

	inst := DeriveInstanceName(repoURL, opts.Project, envName)
	if err := ValidateInstance(e.deployRoot(), e.stateRoot(), inst, envName); err != nil {
		return nil, err
	}

	instDir := filepath.Join(e.deployRoot(), inst)

	if _, err := e.fs().Stat(instDir); err == nil {
		return nil, &output.Error{
			Code:    "E_PROJECT_EXISTS",
			Message: fmt.Sprintf("instance %q already exists", inst),
			Hint:    fmt.Sprintf("cvctl project update %s", inst),
		}
	}

	isFileRepo := strings.HasPrefix(repoURL, "file://")

	keyPath := ""
	pubKey := ""
	if !isFileRepo {
		keyDir := filepath.Join(e.stateRoot(), inst)
		keyPath = filepath.Join(keyDir, "deploy_key")
		pubPath := keyPath + ".pub"

		if existingPub, err := e.fs().ReadFile(pubPath); err == nil {
			pubKey = string(existingPub)
		} else if existingPriv, err := e.fs().ReadFile(keyPath); err == nil {
			pub, err := gitrepo.PublicKeyFromPrivate(existingPriv, "cvctl-"+inst)
			if err != nil {
				return nil, err
			}
			if err := e.fs().MkdirAll(keyDir, 0o750, deployOwner); err != nil {
				return nil, err
			}
			if err := e.fs().WriteFile(pubPath, pub, 0o644, deployOwner); err != nil {
				return nil, err
			}
			pubKey = string(pub)
		} else {
			priv, pub, err := gitrepo.NewDeployKey("cvctl-" + inst)
			if err != nil {
				return nil, err
			}
			if err := e.fs().MkdirAll(keyDir, 0o750, deployOwner); err != nil {
				return nil, err
			}
			if err := e.fs().WriteFile(keyPath, priv, 0o600, deployOwner); err != nil {
				return nil, err
			}
			if err := e.fs().WriteFile(pubPath, pub, 0o644, deployOwner); err != nil {
				return nil, err
			}
			pubKey = string(pub)
		}

		repoForCheck := e.repo(filepath.Join(instDir, "infra"), keyPath)
		if !e.Interactive {
			if err := repoForCheck.LsRemote(ctx, repoURL); err != nil {
				return nil, &output.Error{
					Code:    "E_DEPLOY_KEY_PENDING",
					Message: "deploy key is not yet authorized on remote repo",
					Hint:    fmt.Sprintf("add this key as a read-only deploy key on the repo:\n%s", strings.TrimSpace(pubKey)),
				}
			}
		} else {
			if err := repoForCheck.LsRemote(ctx, repoURL); err != nil {
				cleanRepo := strings.TrimSuffix(repoURL, ".git")
				if e.Prompt != nil {
					ans, err := e.Prompt.Ask(hostcfg.Question{
						Key:    "deploy_key_ready",
						Prompt: fmt.Sprintf("Add it as a read-only deploy key at %s/settings/keys, then press enter", cleanRepo),
						Help:   strings.TrimSpace(pubKey),
					}, "")
					if err != nil {
						return nil, err
					}
					if strings.TrimSpace(strings.ToLower(ans)) == "abort" {
						return nil, &output.Error{
							Code:    "E_DEPLOY_KEY_ABORTED",
							Message: "deploy key authorization aborted",
							Hint:    "add the deploy key to the repo before re-running",
						}
					}
				}
				interval := e.PollInterval
				if interval == 0 {
					interval = 5 * time.Second
				}
				for {
					if err := repoForCheck.LsRemote(ctx, repoURL); err == nil {
						break
					}
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(interval):
					}
				}
			}
		}
	}

	cleanupInst := func() {
		_ = removeAll(e.fs(), instDir)
	}

	if err := e.fs().MkdirAll(instDir, 0o2775, deployOwner); err != nil {
		return nil, err
	}

	branch := opts.Branch
	if branch == "" {
		if envName == "prod" {
			branch = "main"
		} else {
			branch = "develop"
		}
	}

	infraDir := filepath.Join(instDir, "infra")
	repo := e.repo(infraDir, keyPath)
	if err := repo.Clone(ctx, repoURL, branch); err != nil {
		cleanupInst()
		return nil, err
	}

	cat, err := e.catalog()
	if err != nil {
		cleanupInst()
		return nil, err
	}

	p, spec, err := loadContractAndSpec(e.fs(), infraDir, cat)
	if err != nil {
		cleanupInst()
		return nil, err
	}

	validDomainServices := make(map[string]bool, len(p.Apps)+len(p.Data))
	for _, a := range p.Apps {
		validDomainServices[a.Name] = true
	}
	for _, ds := range p.Data {
		if ds.ExposePort > 0 {
			validDomainServices[ds.Name] = true
		}
	}
	for svc := range opts.Domains {
		if !validDomainServices[svc] {
			cleanupInst()
			return nil, &output.UsageError{
				Err: &output.Error{
					Code:    "E_PROJECT_UNKNOWN_SERVICE",
					Message: fmt.Sprintf("unknown service %q for --domain", svc),
					Hint:    "specify an app service defined in compose.yml",
				},
			}
		}
	}

	trackTag := opts.TrackTag
	if trackTag == "" && envName == "dev" {
		trackTag = "develop"
	}

	d := &instance.Decisions{
		Instance:    inst,
		Env:         envName,
		InfraBranch: branch,
		TrackTag:    trackTag,
		InfraPath:   ".",
		Strategy:    opts.Strategy,
		Services:    make(map[string]instance.ServiceDecision),
		Domains:     make(map[string]string),
		Digests:     make(map[string]string),
	}
	for k, v := range opts.Domains {
		d.Domains[k] = v
	}
	if err := applyExt(d, p, cat, opts.Ext); err != nil {
		cleanupInst()
		return nil, err
	}

	if err := promptDataServices(ctx, e, cat, p, d); err != nil {
		cleanupInst()
		return nil, err
	}

	if err := promptDomains(ctx, e, p, d); err != nil {
		cleanupInst()
		return nil, err
	}

	appEnvValues := make(map[string]string)
	for k, v := range opts.Set {
		appEnvValues[k] = v
	}

	if err := promptEnvSpec(ctx, e, spec, appEnvValues); err != nil {
		cleanupInst()
		return nil, err
	}

	if err := checkDecisions(d, p, cat); err != nil {
		cleanupInst()
		return nil, err
	}

	cvctlEnvPath := filepath.Join(instDir, "cvctl.env")
	appEnvPath := filepath.Join(instDir, "app.env")

	addEntries := d.Entries()
	storedURL, stripped := storableInfraURL(repoURL)
	addEntries[keyInfraURL] = storedURL
	var warnings []string
	if stripped {
		warnings = append(warnings, strippedURLWarning)
	}
	if err := e.fs().WriteFile(cvctlEnvPath, instance.FormatEnv(addEntries), 0o640, deployOwner); err != nil {
		cleanupInst()
		return nil, err
	}
	if err := e.fs().WriteFile(appEnvPath, instance.FormatEnv(appEnvValues), 0o640, deployOwner); err != nil {
		cleanupInst()
		return nil, err
	}

	if e.Deployer != nil && e.Deployer.Store != nil {
		// Persisted before the first deploy so the agent adopts the instance
		// even when that deploy fails before Deploy saves any state.
		if err := e.Deployer.Store.Save(&state.State{
			Instance:  inst,
			Status:    state.StatusFailed,
			Detail:    "initial deploy pending",
			UpdatedAt: time.Now(),
		}); err != nil {
			cleanupInst()
			return nil, err
		}
	}

	deployRes, err := runDeploy(ctx, e, repo, d, "cli:project-add", false)
	if err != nil {
		return nil, err
	}

	return &AddResult{
		Instance:     d.Instance,
		Deploy:       deployRes,
		DeployKeyPub: pubKey,
		Decisions:    d,
		Warnings:     warnings,
	}, nil
}

// readRawEnv returns every key in a dotenv file. Parse errors never echo the
// content because cvctl.env holds credentials.
func readRawEnv(fsys sys.FS, path string) (map[string]string, error) {
	data, err := fsys.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := godotenv.Unmarshal(string(data))
	if err != nil {
		return nil, &output.Error{
			Code:    "E_DECISION_INVALID",
			Message: fmt.Sprintf("%s is malformed (details hidden: the file holds secrets)", path),
			Hint:    "fix the KEY=value syntax in cvctl.env",
		}
	}
	return m, nil
}

func Update(ctx context.Context, e *Env, inst string, opts UpdateOptions) (*AddResult, error) {
	if err := ValidateInstanceName(e.deployRoot(), e.stateRoot(), inst); err != nil {
		return nil, err
	}

	instDir := filepath.Join(e.deployRoot(), inst)
	cvctlEnvPath := filepath.Join(instDir, "cvctl.env")

	if _, err := e.fs().Stat(cvctlEnvPath); err != nil {
		return nil, &output.Error{
			Code:    "E_INSTANCE_NOT_FOUND",
			Message: fmt.Sprintf("instance %q not found", inst),
			Hint:    fmt.Sprintf("run cvctl project add <repo-url> --name %s", inst),
		}
	}

	locked := e.Deployer != nil && e.Deployer.Store != nil
	if locked {
		unlock, err := e.Deployer.Store.Lock(inst)
		if err != nil {
			return nil, err
		}
		defer unlock()
	}

	keyPath := filepath.Join(e.stateRoot(), inst, "deploy_key")
	pubKey := ""
	if pub, err := e.fs().ReadFile(keyPath + ".pub"); err == nil {
		pubKey = string(pub)
	}
	if _, err := e.fs().Stat(keyPath); err != nil {
		keyPath = ""
	}

	infraDir := filepath.Join(instDir, "infra")
	repo := e.repo(infraDir, keyPath)

	rawEnv, err := readRawEnv(e.fs(), cvctlEnvPath)
	if err != nil {
		return nil, err
	}
	branch := rawEnv["CVCTL_INFRA_BRANCH"]
	if branch == "" {
		branch = "develop"
		if rawEnv["CVCTL_ENV"] == "prod" {
			branch = "main"
		}
	}

	// Decisions must be asked against the contract that will be deployed, so the
	// working tree is moved to the origin tip before anything is loaded.
	commit, err := repo.Fetch(ctx, branch)
	if err != nil {
		return nil, err
	}
	if err := repo.Checkout(ctx, commit); err != nil {
		return nil, err
	}

	cat, err := e.catalog()
	if err != nil {
		return nil, err
	}

	p, spec, err := loadContractAndSpec(e.fs(), infraDir, cat)
	if err != nil {
		return nil, err
	}

	d, err := instance.Read(cvctlEnvPath, p)
	if err != nil {
		return nil, err
	}

	if err := applyExt(d, p, cat, opts.Ext); err != nil {
		return nil, err
	}

	appEnvPath := filepath.Join(instDir, "app.env")
	appEnvValues := make(map[string]string)
	if data, err := e.fs().ReadFile(appEnvPath); err == nil {
		parsed, pErr := godotenv.Unmarshal(string(data))
		if pErr != nil {
			return nil, &output.Error{
				Code:    "E_APPENV_READ",
				Message: fmt.Sprintf("%s is malformed (details hidden: the file holds secrets)", appEnvPath),
				Hint:    "fix syntax in app.env or remove the file",
			}
		}
		appEnvValues = parsed
	} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, os.ErrNotExist) {
		return nil, &output.Error{
			Code:    "E_APPENV_READ",
			Message: fmt.Sprintf("reading app.env: %v", err),
			Hint:    "fix permissions on app.env",
		}
	}

	if err := promptDataServices(ctx, e, cat, p, d); err != nil {
		return nil, err
	}

	if err := promptDomains(ctx, e, p, d); err != nil {
		return nil, err
	}

	if err := promptEnvSpec(ctx, e, spec, appEnvValues); err != nil {
		return nil, err
	}

	if err := checkDecisions(d, p, cat); err != nil {
		return nil, err
	}

	// Overlay onto the raw file: keys of services that left the contract (and any
	// key this version does not model) must survive an update.
	merged := make(map[string]string, len(rawEnv))
	for k, v := range rawEnv {
		merged[k] = v
	}
	for k, v := range d.Entries() {
		merged[k] = v
	}
	var warnings []string
	if merged[keyInfraURL] == "" {
		if url, stripped := recordedInfraURL(ctx, e, infraDir); url != "" {
			merged[keyInfraURL] = url
			if stripped {
				warnings = append(warnings, strippedURLWarning)
			}
		}
	}
	if err := e.fs().WriteFile(cvctlEnvPath, instance.FormatEnv(merged), 0o640, deployOwner); err != nil {
		return nil, err
	}
	if err := e.fs().WriteFile(appEnvPath, instance.FormatEnv(appEnvValues), 0o640, deployOwner); err != nil {
		return nil, err
	}

	deployRes, err := runDeploy(ctx, e, repo, d, "cli:project-update", locked)
	if err != nil {
		return nil, err
	}

	return &AddResult{
		Instance:     d.Instance,
		Deploy:       deployRes,
		DeployKeyPub: pubKey,
		Decisions:    d,
		Warnings:     warnings,
	}, nil
}
