package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/codeavers/cvctl/internal/host"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

const (
	previousPath = host.AgentBinaryPath + ".previous"
	agentUnit    = "cvctl-agent"
	fetchTimeout = 5 * time.Minute
	execTimeout  = time.Minute
	watchSteps   = 6
	watchStep    = 5 * time.Second
)

var rootOwner = sys.Owner{User: "root", Group: "root"}

// Upgrader replaces the installed binary. Exe is the path of the running binary.
// A nil Sleep uses time.Sleep.
type Upgrader struct {
	Run       sys.Runner
	FS        sys.FS
	Client    Client
	Arch      string
	Current   string
	Exe       string
	Deploying func() ([]string, error)
	Sleep     func(time.Duration)
}

// Prepared is a downloaded and verified release, ready for Apply. Callers must
// call Cleanup on every path; it does nothing once Apply has used the binary.
type Prepared struct {
	From, To, Notes      string
	Changes              []Change
	PlanFailed, UpToDate bool

	tmp string
	fs  sys.FS
}

// Cleanup removes the staged binary.
func (p *Prepared) Cleanup() {
	if p.tmp != "" {
		_ = p.fs.Remove(p.tmp)
		p.tmp = ""
	}
}

// Prepare resolves target ("" means latest), stages its binary next to the
// installed one and compares what both versions would do. It installs nothing.
func (u *Upgrader) Prepare(ctx context.Context, target string) (*Prepared, error) {
	if u.Exe != host.AgentBinaryPath {
		return nil, &output.Error{Code: "E_UPGRADE_PATH",
			Message: fmt.Sprintf("cvctl se ejecuta desde %s, pero solo se actualiza %s", u.Exe, host.AgentBinaryPath),
			Hint:    "ejecuta " + host.AgentBinaryPath + " upgrade"}
	}
	r, err := u.resolve(ctx, target)
	if err != nil {
		return nil, err
	}
	p := &Prepared{From: u.Current, To: r.Tag, Notes: r.Notes, fs: u.FS}
	if r.Tag == u.Current {
		p.UpToDate = true
		return p, nil
	}
	if target == "" {
		newer, err := Newer(u.Current, r.Tag)
		if err != nil {
			return nil, err
		}
		if !newer {
			return nil, &output.Error{Code: "E_UPGRADE_DOWNGRADE",
				Message: fmt.Sprintf("la última release %s es anterior a %s", r.Tag, u.Current),
				Hint:    "para bajar de versión indícala con --version"}
		}
	}
	if err := u.refuseBusy(); err != nil {
		return nil, err
	}
	if err := u.stage(ctx, p, r); err != nil {
		return nil, err
	}
	return p, nil
}

func (u *Upgrader) refuseBusy() error {
	busy, err := u.Deploying()
	if err != nil {
		return err
	}
	if len(busy) > 0 {
		return &output.Error{Code: "E_UPGRADE_BUSY",
			Message: "hay despliegues en curso: " + strings.Join(busy, ", "),
			Hint:    "reintenta cuando terminen"}
	}
	return nil
}

func (u *Upgrader) resolve(ctx context.Context, target string) (Release, error) {
	if target == "" {
		if _, err := parseVersion(u.Current); err != nil {
			return Release{}, &output.Error{Code: "E_UPGRADE_DEV",
				Message: fmt.Sprintf("%q es un binario de desarrollo", u.Current),
				Hint:    "indica la versión de destino con --version"}
		}
		return u.Client.Latest(ctx)
	}
	if _, err := parseVersion(target); err != nil {
		return Release{}, err
	}
	r, err := u.Client.ByTag(ctx, target)
	if err != nil {
		return Release{}, err
	}
	if r.Tag != target {
		return Release{}, &output.Error{Code: "E_UPGRADE_RELEASE",
			Message: fmt.Sprintf("GitHub devolvió %q al pedir %s", r.Tag, target),
			Hint:    "comprueba la versión indicada"}
	}
	return r, nil
}

func (u *Upgrader) stage(ctx context.Context, p *Prepared, r Release) (err error) {
	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	bin, err := u.Client.Fetch(fetchCtx, r, u.Arch)
	if err != nil {
		return err
	}
	// Same directory as the target, so the final rename is atomic.
	p.tmp = filepath.Join(filepath.Dir(host.AgentBinaryPath), ".cvctl-upgrade-"+r.Tag)
	defer func() {
		if err != nil {
			p.Cleanup()
		}
	}()
	if err := u.FS.WriteFile(p.tmp, bin, 0o755, rootOwner); err != nil {
		return installErr("no se pudo escribir el binario descargado", err)
	}
	if err := u.checkVersion(ctx, p.tmp, r.Tag); err != nil {
		return err
	}
	p.Changes, p.PlanFailed = u.comparePlans(ctx, p.tmp)
	return nil
}

func (u *Upgrader) checkVersion(ctx context.Context, bin, want string) error {
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	c := sys.Cmd{Name: bin, Args: []string{"version", "--json"}}
	res, err := u.Run.Run(ctx, c)
	if err != nil || res.ExitCode != 0 {
		return sys.CmdErr("E_UPGRADE_VERIFY", c, res, err, "el binario descargado no funciona en este host")
	}
	var doc struct {
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(res.Stdout, &doc); err != nil || doc.Data.Version != want {
		return &output.Error{Code: "E_UPGRADE_VERIFY",
			Message: fmt.Sprintf("el binario descargado dice ser %q, se esperaba %s", doc.Data.Version, want),
			Hint:    "la release no corresponde a su etiqueta; no se ha instalado nada"}
	}
	return nil
}

// comparePlans reports failed rather than an error: a binary without plan must
// not block the upgrade, only demand the stronger confirmation.
func (u *Upgrader) comparePlans(ctx context.Context, newBin string) ([]Change, bool) {
	from, ok := u.plan(ctx, u.Exe)
	if !ok {
		return nil, true
	}
	to, ok := u.plan(ctx, newBin)
	if !ok {
		return nil, true
	}
	changes, err := Diff(from, to)
	if err != nil {
		return nil, true
	}
	return changes, hasInstanceError(from) || hasInstanceError(to)
}

// hasInstanceError is the only field read from a plan: an instance that could
// not be described was not compared, so an empty diff says nothing about it.
func hasInstanceError(plan []byte) bool {
	var doc struct {
		Instances map[string]map[string]json.RawMessage `json:"instances"`
	}
	if err := json.Unmarshal(plan, &doc); err != nil {
		return true
	}
	for _, inst := range doc.Instances {
		if _, ok := inst["error"]; ok {
			return true
		}
	}
	return false
}

func (u *Upgrader) plan(ctx context.Context, bin string) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	res, err := u.Run.Run(ctx, sys.Cmd{Name: bin, Args: []string{"plan", "--json"}})
	if err != nil || res.ExitCode != 0 {
		return nil, false
	}
	var doc struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(res.Stdout, &doc); err != nil || !doc.OK {
		return nil, false
	}
	return doc.Data, true
}

// Apply swaps in the prepared binary and restarts the agent. If the agent does
// not hold, it puts the previous binary back and returns E_UPGRADE_FAILED.
func (u *Upgrader) Apply(ctx context.Context, p *Prepared) error {
	if p.tmp == "" {
		return installErr("no hay ningún binario preparado", nil)
	}
	before, err := u.swap(ctx, p)
	if err != nil {
		p.Cleanup()
		return err
	}
	p.tmp = ""
	if _, err := u.systemctl(ctx, "restart", agentUnit); err != nil {
		return u.rollback(ctx, p, err)
	}
	if err := u.watch(ctx, before); err != nil {
		return u.rollback(ctx, p, err)
	}
	return nil
}

// swap returns NRestarts as read before touching anything: an agent whose
// counter cannot be read could not be watched, so it must not be replaced.
func (u *Upgrader) swap(ctx context.Context, p *Prepared) (int, error) {
	// A deploy may have started while a person read the confirmation prompt.
	if err := u.refuseBusy(); err != nil {
		return 0, err
	}
	before, err := u.nRestarts(ctx)
	if err != nil {
		return 0, installErr("no se pudo leer NRestarts", err)
	}
	current, err := u.FS.ReadFile(host.AgentBinaryPath)
	if err != nil {
		return 0, installErr("no se pudo leer el binario actual", err)
	}
	if err := u.FS.WriteFile(previousPath, current, 0o755, rootOwner); err != nil {
		return 0, installErr("no se pudo guardar "+previousPath, err)
	}
	if err := u.FS.Rename(p.tmp, host.AgentBinaryPath); err != nil {
		return 0, installErr("no se pudo sustituir el binario", err)
	}
	return before, nil
}

// watch needs the whole window because Restart=always hides a crashing binary
// from a single is-active check.
func (u *Upgrader) watch(ctx context.Context, before int) error {
	sleep := u.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	last := before
	for range watchSteps {
		sleep(watchStep)
		if _, err := u.systemctl(ctx, "is-active", agentUnit); err != nil {
			return err
		}
		n, err := u.nRestarts(ctx)
		if err != nil {
			return err
		}
		// Compared with the last reading, not only with before: systemd zeroes
		// NRestarts on a manual restart, so a loop can stay below before.
		if n > last {
			return fmt.Errorf("el agente se ha reiniciado solo (NRestarts %d -> %d)", last, n)
		}
		last = n
	}
	return nil
}

func (u *Upgrader) rollback(ctx context.Context, p *Prepared, cause error) error {
	// Ctrl-C during the watch cancels ctx, and that must not stop the restore.
	ctx = context.WithoutCancel(ctx)
	if err := u.FS.Rename(previousPath, host.AgentBinaryPath); err != nil {
		return &output.Error{Code: "E_UPGRADE_FAILED",
			Message: fmt.Sprintf("%s falló (%v) y no se pudo restaurar %s: %v", p.To, cause, p.From, err),
			Hint: fmt.Sprintf("el agente sigue con el binario nuevo; ejecuta mv %s %s y systemctl restart %s",
				previousPath, host.AgentBinaryPath, agentUnit)}
	}
	restored := "restaurado y agente reiniciado"
	if _, err := u.systemctl(ctx, "restart", agentUnit); err != nil {
		restored = fmt.Sprintf("restaurado, pero el agente no se pudo reiniciar: %v", err)
	}
	state := "el agente está activo"
	if _, err := u.systemctl(ctx, "is-active", agentUnit); err != nil {
		state = "el agente no está activo"
	}
	return &output.Error{Code: "E_UPGRADE_FAILED",
		Message: fmt.Sprintf("%s falló (%v); %s %s; %s", p.To, cause, p.From, restored, state),
		Hint:    "revisa systemctl status " + agentUnit + " y journalctl -u " + agentUnit + " antes de reintentar"}
}

func (u *Upgrader) nRestarts(ctx context.Context) (int, error) {
	out, err := u.systemctl(ctx, "show", "-p", "NRestarts", "--value", agentUnit)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("NRestarts no numérico: %q", out)
	}
	return n, nil
}

func (u *Upgrader) systemctl(ctx context.Context, args ...string) (string, error) {
	c := sys.Cmd{Name: "systemctl", Args: args}
	res, err := u.Run.Run(ctx, c)
	if err != nil || res.ExitCode != 0 {
		return "", errors.New(sys.CmdErr("", c, res, err, "").Message)
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

func installErr(msg string, cause error) error {
	if cause != nil {
		msg += ": " + cause.Error()
	}
	return &output.Error{Code: "E_UPGRADE_INSTALL", Message: msg,
		Hint: "no se ha instalado nada; corrige la causa y reintenta"}
}
