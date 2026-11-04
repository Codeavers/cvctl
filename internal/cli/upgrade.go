package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/upgrade"
)

const (
	defaultUpgradeRepo = "Codeavers/cvctl"
	githubAPI          = "https://api.github.com"
	// Covers the binary download, which the Upgrader bounds again per fetch.
	upgradeHTTPTimeout = 5 * time.Minute
	noticeTimeout      = 30 * time.Second
)

type upgradeEnv struct {
	Upgrader    *upgrade.Upgrader
	EUID        int
	Interactive bool
	Notifier    func() notify.Notifier
}

var newUpgradeEnv = func(current string) (*upgradeEnv, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	repo := os.Getenv("CVCTL_UPGRADE_REPO")
	if repo == "" {
		repo = defaultUpgradeRepo
	}
	return &upgradeEnv{
		Upgrader: &upgrade.Upgrader{
			Run:       sys.NewExecRunner(),
			FS:        sys.NewOSFS(),
			Client:    upgrade.Client{HTTP: &http.Client{Timeout: upgradeHTTPTimeout}, BaseURL: githubAPI, Repo: repo},
			Arch:      runtime.GOARCH,
			Current:   current,
			Exe:       exe,
			Deploying: deployingInstances,
			Sleep:     time.Sleep,
		},
		EUID:        os.Geteuid(),
		Interactive: term.IsTerminal(int(os.Stdin.Fd())),
		Notifier:    hostNotifier,
	}, nil
}

func deployingInstances() ([]string, error) {
	e, err := newOpsEnv(opsOpts{NonInteractive: true, Out: io.Discard})
	if err != nil {
		return nil, err
	}
	var busy []string
	for _, inst := range e.allInstances() {
		st, err := e.Store.Load(inst)
		if err != nil {
			return nil, err
		}
		if st.Status == state.StatusDeploying {
			busy = append(busy, inst)
		}
	}
	return busy, nil
}

// hostNotifier degrades to a no-op: a broken notifier must not fail an upgrade.
func hostNotifier() notify.Notifier {
	cfg, secrets, err := hostcfg.Load(sys.NewOSFS(), hostCfgDir)
	if err != nil {
		fmt.Fprintf(notifyLog, "notify: notifications disabled: %v\n", err)
		return notify.NoopNotifier{}
	}
	n, err := notify.FromHost(cfg, secrets)
	if err != nil {
		fmt.Fprintf(notifyLog, "notify: notifications disabled: %v\n", err)
		return notify.NoopNotifier{}
	}
	return n
}

type upgradeChange struct {
	Path string `json:"path"`
	From string `json:"from"`
	To   string `json:"to"`
}

type upgradeReport struct {
	From       string          `json:"from"`
	To         string          `json:"to"`
	Notes      string          `json:"notes"`
	Changes    []upgradeChange `json:"changes"`
	PlanFailed bool            `json:"plan_failed"`
	UpToDate   bool            `json:"up_to_date"`
	Installed  bool            `json:"installed"`
}

func newUpgradeCmd(current string) *cobra.Command {
	var target string
	var check, yes, accept bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade cvctl to the latest release (or --version)",
		Example: `  sudo cvctl upgrade
  cvctl upgrade --check
  sudo cvctl upgrade --version v0.8.2 --yes --accept-changes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			asJSON, _ := cmd.Flags().GetBool("json")
			e, err := newUpgradeEnv(current)
			if err != nil {
				return err
			}
			if e.EUID != 0 {
				return &output.Error{Code: "E_UPGRADE_ROOT", Message: "cvctl upgrade necesita root",
					Hint: "sudo cvctl upgrade"}
			}
			return doUpgrade(cmd, e, upgradeOpts{target: target, check: check, yes: yes, accept: accept, json: asJSON})
		},
	}
	cmd.Flags().StringVar(&target, "version", "", "target release tag (default: latest)")
	cmd.Flags().BoolVar(&check, "check", false, "show what would change and exit without installing")
	cmd.Flags().BoolVar(&yes, "yes", false, "answer the plain confirmation")
	cmd.Flags().BoolVar(&accept, "accept-changes", false, "with --yes, also accept behaviour changes between versions")
	return cmd
}

type upgradeOpts struct {
	target                   string
	check, yes, accept, json bool
}

func doUpgrade(cmd *cobra.Command, e *upgradeEnv, o upgradeOpts) error {
	ctx := cmd.Context()
	p, err := e.Upgrader.Prepare(ctx, o.target)
	if err != nil {
		return err
	}
	defer p.Cleanup()

	rep := upgradeReport{From: p.From, To: p.To, Notes: p.Notes, Changes: []upgradeChange{},
		PlanFailed: p.PlanFailed, UpToDate: p.UpToDate}
	for _, c := range p.Changes {
		rep.Changes = append(rep.Changes, upgradeChange{Path: c.Path, From: c.From, To: c.To})
	}
	pr := printerFrom(cmd)
	if p.UpToDate {
		pr.Result(rep, func(w io.Writer) { fmt.Fprintf(w, "cvctl ya está en %s\n", p.To) })
		return nil
	}
	if o.check {
		pr.Result(rep, func(w io.Writer) { writeUpgradeReview(w, p) })
		return nil
	}
	if !o.json {
		writeUpgradeReview(cmd.OutOrStdout(), p)
	}
	if err := confirmUpgrade(cmd, e, o, p); err != nil {
		return err
	}
	if err := e.Upgrader.Apply(ctx, p); err != nil {
		noticeFailure(ctx, e, p, err)
		return err
	}
	rep.Installed = true
	sendNotice(ctx, e, notify.Message{Level: "info", Title: "upgrade",
		Status: fmt.Sprintf("upgraded %s -> %s", p.From, p.To)})
	pr.Result(rep, func(w io.Writer) { fmt.Fprintf(w, "cvctl actualizado: %s -> %s\n", p.From, p.To) })
	return nil
}

func writeUpgradeReview(w io.Writer, p *upgrade.Prepared) {
	fmt.Fprintf(w, "Versión actual: %s\nVersión nueva:  %s\n", p.From, p.To)
	if strings.TrimSpace(p.Notes) != "" {
		fmt.Fprintf(w, "\nNotas de la release:\n%s\n", strings.TrimSpace(p.Notes))
	}
	switch {
	case p.PlanFailed:
		fmt.Fprintln(w, "\nNo se han podido comparar los planes de ambas versiones (cvctl plan falló o una instancia no se pudo leer): no se puede descartar un cambio de comportamiento.")
	case len(p.Changes) == 0:
		fmt.Fprintln(w, "\nSin diferencias de comportamiento por instancia.")
	default:
		fmt.Fprintln(w, "\nDiferencias de comportamiento:")
		for _, c := range p.Changes {
			fmt.Fprintf(w, "  %s: %s -> %s\n", c.Path, c.From, c.To)
		}
	}
}

func confirmUpgrade(cmd *cobra.Command, e *upgradeEnv, o upgradeOpts, p *upgrade.Prepared) error {
	strong := len(p.Changes) > 0 || p.PlanFailed
	if o.yes && (!strong || o.accept) {
		return nil
	}
	if !e.Interactive || o.json {
		hint := "añade --yes para confirmar, o --check para solo ver los cambios"
		if strong {
			hint = "revisa las diferencias con --check; si las aceptas, añade --yes --accept-changes"
		}
		return &output.Error{Code: "E_UPGRADE_CONFIRM", Message: "hace falta confirmación y no hay terminal ni opciones que la den",
			Hint: hint}
	}
	out := cmd.ErrOrStderr()
	if strong {
		fmt.Fprintf(out, "\nEscribe %s para instalar: ", p.To)
	} else {
		fmt.Fprintf(out, "\n¿Instalar %s? [y/N]: ", p.To)
	}
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && line == "" {
		return cancelled()
	}
	ans := strings.TrimSpace(line)
	if strong && ans == p.To {
		return nil
	}
	if !strong && (strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes")) {
		return nil
	}
	return cancelled()
}

func cancelled() error {
	return &output.Error{Code: "E_UPGRADE_CONFIRM", Message: "actualización cancelada; no se ha instalado nada",
		Hint: "reintenta cuando quieras"}
}

// noticeFailure covers only the two codes that mean the binary changed or an
// install was attempted; E_UPGRADE_INSTALL means nothing was touched.
func noticeFailure(ctx context.Context, e *upgradeEnv, p *upgrade.Prepared, err error) {
	var oe *output.Error
	if !errors.As(err, &oe) {
		return
	}
	var status string
	switch oe.Code {
	case "E_UPGRADE_FAILED":
		status = fmt.Sprintf("upgrade %s -> %s failed; restore of %s attempted", p.From, p.To, p.From)
	case "E_UPGRADE_INSTALL":
		status = fmt.Sprintf("upgrade %s -> %s failed; nothing installed (still %s)", p.From, p.To, p.From)
	default:
		return
	}
	sendNotice(ctx, e, notify.Message{Level: "error", Title: "upgrade", Status: status, Error: oe.Message, Text: oe.Message})
}

// sendNotice never fails the upgrade; the cancel-proof context keeps a Ctrl-C
// during the watch from swallowing the notice.
func sendNotice(ctx context.Context, e *upgradeEnv, m notify.Message) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), noticeTimeout)
	defer cancel()
	if err := e.Notifier().Send(ctx, m); err != nil {
		fmt.Fprintf(notifyLog, "notify: %q not sent: %v\n", m.Title, err)
	}
}
