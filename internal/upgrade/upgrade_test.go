package upgrade

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/host"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

const (
	nRestartsCmd = "systemctl show -p NRestarts --value cvctl-agent"
	isActiveCmd  = "systemctl is-active cvctl-agent"
	restartCmd   = "systemctl restart cvctl-agent"
	latestTmp    = "/usr/local/bin/.cvctl-upgrade-v0.9.0"
	oldBinary    = "old binary"
	newBinary    = "new binary"
	emptyPlan    = `{"ok":true,"data":{"schema":1,"instances":{}}}`
)

func versionOut(v string) sys.Result {
	return sys.Result{Stdout: []byte(`{"ok":true,"data":{"version":"` + v + `"}}`)}
}

// failFS makes selected writes and renames fail, which the in-memory FS never does.
type failFS struct {
	*systest.FS
	writeErr  map[string]error
	renameErr map[string]error
}

func (f *failFS) WriteFile(p string, data []byte, mode os.FileMode, o sys.Owner) error {
	if err := f.writeErr[p]; err != nil {
		return err
	}
	return f.FS.WriteFile(p, data, mode, o)
}

func (f *failFS) Rename(oldp, newp string) error {
	if err := f.renameErr[oldp]; err != nil {
		return err
	}
	return f.FS.Rename(oldp, newp)
}

// ctxRunner fails on a cancelled context, as the real runner does.
type ctxRunner struct{ *systest.Runner }

func (r ctxRunner) Run(ctx context.Context, c sys.Cmd) (sys.Result, error) {
	if err := ctx.Err(); err != nil {
		r.Calls = append(r.Calls, c)
		return sys.Result{}, err
	}
	return r.Runner.Run(ctx, c)
}

type runnerFunc func(context.Context, sys.Cmd) (sys.Result, error)

func (f runnerFunc) Run(ctx context.Context, c sys.Cmd) (sys.Result, error) { return f(ctx, c) }

// newTagServer answers every release query with tag.
func newTagServer(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	bin := []byte(newBinary)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dl/bin":
			w.Write(bin)
		case "/dl/sums":
			fmt.Fprint(w, sumsFor(bin))
		default:
			fmt.Fprint(w, releaseJSON(srv.URL, tag))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func hint(err error) string {
	var e *output.Error
	if errors.As(err, &e) {
		return e.Hint
	}
	return ""
}

type fixture struct {
	u     *Upgrader
	run   *systest.Runner
	fs    *failFS
	slept []time.Duration
	// onSleep runs on every watch step, to change what systemctl answers.
	onSleep func(step int)
}

func newFixture(t *testing.T, current string) *fixture {
	t.Helper()
	bin := []byte(newBinary)
	srv := newServer(t, bin, sumsFor(bin))
	f := &fixture{
		run: &systest.Runner{},
		fs:  &failFS{FS: systest.NewFS(), writeErr: map[string]error{}, renameErr: map[string]error{}},
	}
	if err := f.fs.WriteFile(host.AgentBinaryPath, []byte(oldBinary), 0o755, sys.Owner{}); err != nil {
		t.Fatal(err)
	}
	f.run.On(latestTmp+" version --json", versionOut("v0.9.0"))
	f.run.On(latestTmp+" plan --json", sys.Result{Stdout: []byte(emptyPlan)})
	f.run.On(host.AgentBinaryPath+" plan --json", sys.Result{Stdout: []byte(emptyPlan)})
	f.run.On(nRestartsCmd, sys.Result{Stdout: []byte("2\n")})
	f.run.On(isActiveCmd, sys.Result{Stdout: []byte("active\n")})
	f.run.On(restartCmd, sys.Result{})
	f.u = &Upgrader{
		Run:       f.run,
		FS:        f.fs,
		Client:    Client{BaseURL: srv.URL, Repo: "o/r"},
		Arch:      "amd64",
		Current:   current,
		Exe:       host.AgentBinaryPath,
		Deploying: func() ([]string, error) { return nil, nil },
		Sleep: func(d time.Duration) {
			f.slept = append(f.slept, d)
			if f.onSleep != nil {
				f.onSleep(len(f.slept))
			}
		},
	}
	return f
}

func (f *fixture) binary(t *testing.T) string {
	t.Helper()
	b, err := f.fs.ReadFile(host.AgentBinaryPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (f *fixture) noTemp(t *testing.T) {
	t.Helper()
	for p := range f.fs.Files() {
		if strings.HasPrefix(path.Base(p), ".cvctl-upgrade-") {
			t.Errorf("temporal sin borrar: %s", p)
		}
	}
}

func (f *fixture) count(cmd string) int {
	n := 0
	for _, c := range f.run.Calls {
		if c.String() == cmd {
			n++
		}
	}
	return n
}

// agentDiesOnRestart makes is-active answer res from the first watch step on;
// the read before the swap still sees an active agent.
func (f *fixture) agentDiesOnRestart(res sys.Result) {
	f.onSleep = func(int) { f.run.On(isActiveCmd, res) }
}

func (f *fixture) applyErr(p *Prepared) error {
	_, err := f.u.Apply(context.Background(), p)
	return err
}

func (f *fixture) prepare(t *testing.T) *Prepared {
	t.Helper()
	p, err := f.u.Prepare(context.Background(), "")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return p
}

func TestPrepareRejectsDevBuildWithoutTarget(t *testing.T) {
	f := newFixture(t, "dev")
	_, err := f.u.Prepare(context.Background(), "")
	if code(err) != "E_UPGRADE_DEV" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(hint(err), "--version") {
		t.Errorf("hint = %q", hint(err))
	}
}

func TestPrepareDevBuildWithTarget(t *testing.T) {
	f := newFixture(t, "dev-abc123")
	tmp := "/usr/local/bin/.cvctl-upgrade-v0.8.2"
	f.run.On(tmp+" version --json", versionOut("v0.8.2"))
	f.run.On(tmp+" plan --json", sys.Result{Stdout: []byte(emptyPlan)})
	p, err := f.u.Prepare(context.Background(), "v0.8.2")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Cleanup()
	if p.From != "dev-abc123" || p.To != "v0.8.2" {
		t.Errorf("%+v", p)
	}
}

func TestPrepareRejectsOtherPath(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	f.u.Exe = "/home/me/cvctl"
	_, err := f.u.Prepare(context.Background(), "")
	if code(err) != "E_UPGRADE_PATH" {
		t.Fatalf("err = %v", err)
	}
}

func TestPrepareRejectsInvalidTarget(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	_, err := f.u.Prepare(context.Background(), "../../evil")
	if code(err) != "E_UPGRADE_VERSION" {
		t.Fatalf("err = %v", err)
	}
}

func TestPrepareUpToDate(t *testing.T) {
	f := newFixture(t, "v0.9.0")
	f.u.Deploying = func() ([]string, error) { return []string{"cute-prod"}, nil }
	p := f.prepare(t)
	if !p.UpToDate || p.To != "v0.9.0" {
		t.Fatalf("%+v", p)
	}
	if len(f.run.Calls) != 0 {
		t.Errorf("calls = %v", f.run.Calls)
	}
	f.noTemp(t)
	p.Cleanup()
}

func TestPrepareDowngradeNeedsTarget(t *testing.T) {
	f := newFixture(t, "v1.0.0")
	if _, err := f.u.Prepare(context.Background(), ""); code(err) != "E_UPGRADE_DOWNGRADE" {
		t.Fatalf("err = %v", err)
	}
	f.noTemp(t)

	tmp := "/usr/local/bin/.cvctl-upgrade-v0.8.2"
	f.run.On(tmp+" version --json", versionOut("v0.8.2"))
	f.run.On(tmp+" plan --json", sys.Result{Stdout: []byte(emptyPlan)})
	p, err := f.u.Prepare(context.Background(), "v0.8.2")
	if err != nil {
		t.Fatal(err)
	}
	p.Cleanup()
}

func TestPrepareRefusesWhileDeploying(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	f.u.Deploying = func() ([]string, error) { return []string{"cute-prod"}, nil }
	_, err := f.u.Prepare(context.Background(), "")
	if code(err) != "E_UPGRADE_BUSY" || !strings.Contains(err.Error(), "cute-prod") {
		t.Fatalf("err = %v", err)
	}
	f.noTemp(t)
}

func TestPrepareStopsWhenDeployingFails(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	f.u.Deploying = func() ([]string, error) { return nil, errors.New("estado ilegible") }
	if _, err := f.u.Prepare(context.Background(), ""); err == nil {
		t.Fatal("se esperaba error")
	}
	f.noTemp(t)
}

func TestPrepareWritesExecutableTemp(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	p := f.prepare(t)
	info, ok := f.fs.Files()[latestTmp]
	if !ok || info.Mode != 0o755 || info.User != "root" || info.Group != "root" {
		t.Fatalf("temporal: %+v (existe: %v)", info, ok)
	}
	p.Cleanup()
	f.noTemp(t)
	if f.binary(t) != oldBinary {
		t.Error("Prepare no debe tocar el binario instalado")
	}
}

func TestPrepareChecksNewVersion(t *testing.T) {
	tests := []struct {
		name string
		res  sys.Result
	}{
		{"versión distinta", versionOut("v0.8.9")},
		{"sale con error", sys.Result{ExitCode: 1}},
		{"salida no JSON", sys.Result{Stdout: []byte("v0.9.0")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, "v0.8.2")
			f.run.On(latestTmp+" version --json", tt.res)
			_, err := f.u.Prepare(context.Background(), "")
			if code(err) != "E_UPGRADE_VERIFY" {
				t.Fatalf("err = %v", err)
			}
			f.noTemp(t)
		})
	}
}

func TestPrepareRemovesTempWhenBinaryCannotRun(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	f.run.OnError(latestTmp+" version --json", errors.New("exec format error"))
	if _, err := f.u.Prepare(context.Background(), ""); code(err) != "E_UPGRADE_VERIFY" {
		t.Fatalf("err = %v", err)
	}
	f.noTemp(t)
}

func TestPrepareWriteFailure(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	f.fs.writeErr[latestTmp] = errors.New("no space left on device")
	if _, err := f.u.Prepare(context.Background(), ""); code(err) != "E_UPGRADE_INSTALL" {
		t.Fatalf("err = %v", err)
	}
	f.noTemp(t)
}

func TestPrepareFetchFailureLeavesNothing(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	srv := newServer(t, []byte(newBinary), sumsFor([]byte("otro")))
	f.u.Client.BaseURL = srv.URL
	if _, err := f.u.Prepare(context.Background(), ""); code(err) != "E_UPGRADE_CHECKSUM" {
		t.Fatalf("err = %v", err)
	}
	f.noTemp(t)
	if len(f.run.Calls) != 0 {
		t.Errorf("calls = %v", f.run.Calls)
	}
}

// deadlineClient records how far away the deadline of the last request is.
func deadlineClient(bound *time.Duration) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if d, ok := r.Context().Deadline(); ok {
			*bound = time.Until(d)
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPrepareBoundsTheDownload(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	var bound time.Duration
	f.u.Client.HTTP = deadlineClient(&bound)
	f.prepare(t).Cleanup()
	if bound <= 4*time.Minute || bound > 5*time.Minute {
		t.Fatalf("plazo de descarga = %v", bound)
	}
}

func TestPrepareReportsChanges(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	f.run.On(host.AgentBinaryPath+" plan --json",
		sys.Result{Stdout: []byte(`{"ok":true,"data":{"schema":1,"instances":{"a":{"backup":"volume"}}}}`)})
	f.run.On(latestTmp+" plan --json",
		sys.Result{Stdout: []byte(`{"ok":true,"data":{"schema":1,"instances":{"a":{"backup":"none"}}}}`)})
	p := f.prepare(t)
	defer p.Cleanup()
	if p.PlanFailed || p.UpToDate {
		t.Fatalf("%+v", p)
	}
	if p.From != "v0.8.2" || p.To != "v0.9.0" || p.Notes != "notas" {
		t.Errorf("%+v", p)
	}
	want := Change{Path: "instances.a.backup", From: "volume", To: "none"}
	if len(p.Changes) != 1 || p.Changes[0] != want {
		t.Fatalf("changes = %+v", p.Changes)
	}
}

func TestPrepareNoChanges(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	p := f.prepare(t)
	defer p.Cleanup()
	if p.PlanFailed || len(p.Changes) != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestPreparePlanFailureCounts(t *testing.T) {
	failures := []struct {
		name string
		res  sys.Result
	}{
		{"sale con error", sys.Result{ExitCode: 1}},
		{"no es JSON", sys.Result{Stdout: []byte("usage: cvctl")}},
		{"ok false", sys.Result{Stdout: []byte(`{"ok":false,"error":{"code":"E_X"}}`)}},
	}
	for _, bin := range []string{host.AgentBinaryPath, latestTmp} {
		for _, tt := range failures {
			t.Run(path.Base(bin)+"/"+tt.name, func(t *testing.T) {
				f := newFixture(t, "v0.8.2")
				f.run.On(bin+" plan --json", tt.res)
				p := f.prepare(t)
				defer p.Cleanup()
				if !p.PlanFailed {
					t.Fatalf("%+v", p)
				}
				if _, ok := f.fs.Files()[latestTmp]; !ok {
					t.Error("el temporal debe seguir para Apply")
				}
			})
		}
		t.Run(path.Base(bin)+"/no arranca", func(t *testing.T) {
			f := newFixture(t, "v0.8.2")
			f.run.OnError(bin+" plan --json", errors.New("boom"))
			p := f.prepare(t)
			defer p.Cleanup()
			if !p.PlanFailed {
				t.Fatalf("%+v", p)
			}
		})
	}
}

func TestPreparePlanInstanceErrorCounts(t *testing.T) {
	broken := `{"ok":true,"data":{"schema":1,"instances":{"a":{"env":"prod"},"b":{"error":"contrato ilegible"}}}}`
	good := `{"ok":true,"data":{"schema":1,"instances":{"a":{"env":"staging"},"b":{"env":"prod"}}}}`
	for _, tt := range []struct{ name, from, to string }{
		{"en el actual", broken, good},
		{"en el nuevo", good, broken},
		{"en los dos", broken, broken},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, "v0.8.2")
			f.run.On(host.AgentBinaryPath+" plan --json", sys.Result{Stdout: []byte(tt.from)})
			f.run.On(latestTmp+" plan --json", sys.Result{Stdout: []byte(tt.to)})
			p := f.prepare(t)
			defer p.Cleanup()
			if !p.PlanFailed {
				t.Fatalf("%+v", p)
			}
		})
	}
}

func TestPrepareKeepsChangesWhenAnInstanceFailed(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	f.run.On(host.AgentBinaryPath+" plan --json",
		sys.Result{Stdout: []byte(`{"ok":true,"data":{"instances":{"a":{"env":"x"},"b":{"error":"e"}}}}`)})
	f.run.On(latestTmp+" plan --json",
		sys.Result{Stdout: []byte(`{"ok":true,"data":{"instances":{"a":{"env":"y"},"b":{"error":"e"}}}}`)})
	p := f.prepare(t)
	defer p.Cleanup()
	want := Change{Path: "instances.a.env", From: "x", To: "y"}
	if !p.PlanFailed || len(p.Changes) != 1 || p.Changes[0] != want {
		t.Fatalf("%+v", p)
	}
}

func TestPreparePlanWithoutInstanceShapeCounts(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	f.run.On(latestTmp+" plan --json", sys.Result{Stdout: []byte(`{"ok":true,"data":{"instances":{"a":"x"}}}`)})
	p := f.prepare(t)
	defer p.Cleanup()
	if !p.PlanFailed {
		t.Fatalf("%+v", p)
	}
}

// Each run must get a fresh minute, so a slow plan cannot eat the next one's.
func TestPrepareBoundsEachBinaryRun(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	bounds := map[string]time.Duration{}
	f.u.Run = runnerFunc(func(ctx context.Context, c sys.Cmd) (sys.Result, error) {
		if d, ok := ctx.Deadline(); ok {
			bounds[c.String()] = time.Until(d)
		}
		return f.run.Run(ctx, c)
	})
	f.prepare(t).Cleanup()
	for _, cmd := range []string{latestTmp + " version --json", host.AgentBinaryPath + " plan --json", latestTmp + " plan --json"} {
		if d := bounds[cmd]; d <= 50*time.Second || d > time.Minute {
			t.Errorf("%s: plazo = %v", cmd, d)
		}
	}
}

func TestPrepareVersionTimeoutFails(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	f.run.OnError(latestTmp+" version --json", context.DeadlineExceeded)
	if _, err := f.u.Prepare(context.Background(), ""); code(err) != "E_UPGRADE_VERIFY" {
		t.Fatalf("err = %v", err)
	}
	f.noTemp(t)
}

func TestPreparePlanTimeoutCounts(t *testing.T) {
	for _, bin := range []string{host.AgentBinaryPath, latestTmp} {
		t.Run(path.Base(bin), func(t *testing.T) {
			f := newFixture(t, "v0.8.2")
			f.run.OnError(bin+" plan --json", context.DeadlineExceeded)
			p := f.prepare(t)
			defer p.Cleanup()
			if !p.PlanFailed {
				t.Fatalf("%+v", p)
			}
		})
	}
}

func TestPrepareRejectsMalformedLatestTag(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	srv := newTagServer(t, "../../../etc/cron.d/x")
	f.u.Client.BaseURL = srv.URL
	if _, err := f.u.Prepare(context.Background(), ""); code(err) != "E_UPGRADE_VERSION" {
		t.Fatalf("err = %v", err)
	}
	f.noTemp(t)
	if got := len(f.fs.Files()); got != 1 {
		t.Errorf("ficheros = %v", f.fs.Files())
	}
}

func TestPrepareRejectsTagMismatch(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	srv := newTagServer(t, "v0.7.0")
	f.u.Client.BaseURL = srv.URL
	if _, err := f.u.Prepare(context.Background(), "v0.9.0"); code(err) != "E_UPGRADE_RELEASE" {
		t.Fatalf("err = %v", err)
	}
	f.noTemp(t)
}

func TestApplySucceeds(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	p := f.prepare(t)
	if _, err := f.u.Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if f.binary(t) != newBinary {
		t.Errorf("binario = %q", f.binary(t))
	}
	f.noTemp(t)
	var total time.Duration
	for _, d := range f.slept {
		if d != 5*time.Second {
			t.Errorf("paso = %v", d)
		}
		total += d
	}
	if total != 30*time.Second {
		t.Errorf("vigilancia = %v", total)
	}
	if n := f.count(isActiveCmd); n != 7 {
		t.Errorf("is-active = %d: una antes de sustituir y seis en la vigilancia", n)
	}
	if n := f.count(restartCmd); n != 1 {
		t.Errorf("restart = %d", n)
	}
	p.Cleanup()
	if f.binary(t) != newBinary {
		t.Error("Cleanup tras Apply no debe tocar el binario")
	}
}

func TestApplyKeepsPrevious(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	if _, err := f.u.Apply(context.Background(), f.prepare(t)); err != nil {
		t.Fatal(err)
	}
	b, err := f.fs.ReadFile("/usr/local/bin/cvctl.previous")
	if err != nil || string(b) != oldBinary {
		t.Fatalf("previous = %q, %v", b, err)
	}
	info := f.fs.Files()["/usr/local/bin/cvctl.previous"]
	if info.Mode != 0o755 || info.User != "root" || info.Group != "root" {
		t.Errorf("previous: %+v", info)
	}
}

func TestApplyReadsNRestartsBeforeRestarting(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	p := f.prepare(t)
	f.run.Calls = nil
	if _, err := f.u.Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := f.run.Calls[0].String(); got != isActiveCmd {
		t.Fatalf("primera llamada = %q", got)
	}
	if got := f.run.Calls[1].String(); got != nRestartsCmd {
		t.Fatalf("segunda llamada = %q", got)
	}
}

func TestApplyLeavesStoppedAgentStopped(t *testing.T) {
	for _, state := range []string{"inactive", "failed"} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t, "v0.8.2")
			p := f.prepare(t)
			f.run.On(isActiveCmd, sys.Result{ExitCode: 3, Stdout: []byte(state + "\n")})
			res, err := f.u.Apply(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			if !res.AgentStopped {
				t.Error("AgentStopped = false")
			}
			if f.binary(t) != newBinary {
				t.Errorf("binario = %q", f.binary(t))
			}
			prev, err := f.fs.ReadFile(previousPath)
			if err != nil || string(prev) != oldBinary {
				t.Errorf("previous = %q, %v", prev, err)
			}
			if n := f.count(restartCmd); n != 0 {
				t.Errorf("restart = %d: no se arranca un agente parado", n)
			}
			if n := f.count(isActiveCmd); n != 1 {
				t.Errorf("is-active = %d: sin reinicio no hay vigilancia", n)
			}
			if len(f.slept) != 0 {
				t.Errorf("vigilancia = %v", f.slept)
			}
			f.noTemp(t)
		})
	}
}

func TestApplyStoppedAgentNeedsNoCounter(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	p := f.prepare(t)
	f.run.On(isActiveCmd, sys.Result{ExitCode: 3, Stdout: []byte("inactive\n")})
	f.run.On(nRestartsCmd, sys.Result{ExitCode: 1})
	res, err := f.u.Apply(context.Background(), p)
	if err != nil || !res.AgentStopped {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestApplyActiveAgentIsNotReportedStopped(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	res, err := f.u.Apply(context.Background(), f.prepare(t))
	if err != nil || res.AgentStopped {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

// systemctl exits non-zero for a stopped agent and for one it could not read,
// and a starting agent would come up on the new binary with nobody watching.
func TestApplyUnclearAgentStateChangesNothing(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fixture)
	}{
		{"systemctl no responde", func(f *fixture) { f.run.On(isActiveCmd, sys.Result{ExitCode: 1}) }},
		{"systemctl no arranca", func(f *fixture) { f.run.OnError(isActiveCmd, errors.New("exec failed")) }},
		{"arrancando", func(f *fixture) { f.run.On(isActiveCmd, sys.Result{ExitCode: 3, Stdout: []byte("activating\n")}) }},
		{"parando", func(f *fixture) { f.run.On(isActiveCmd, sys.Result{ExitCode: 3, Stdout: []byte("deactivating\n")}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, "v0.8.2")
			p := f.prepare(t)
			tt.setup(f)
			_, err := f.u.Apply(context.Background(), p)
			if code(err) != "E_UPGRADE_INSTALL" {
				t.Fatalf("err = %v", err)
			}
			if f.binary(t) != oldBinary {
				t.Errorf("binario = %q", f.binary(t))
			}
			if _, err := f.fs.ReadFile(previousPath); err == nil {
				t.Error("previous escrito")
			}
			if n := f.count(restartCmd); n != 0 {
				t.Errorf("restart = %d", n)
			}
			f.noTemp(t)
		})
	}
}

func TestApplyFailureBeforeSwapChangesNothing(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fixture)
	}{
		{"NRestarts falla", func(f *fixture) { f.run.On(nRestartsCmd, sys.Result{ExitCode: 1}) }},
		{"NRestarts ilegible", func(f *fixture) { f.run.On(nRestartsCmd, sys.Result{Stdout: []byte("[not set]\n")}) }},
		{"previous no se escribe", func(f *fixture) {
			f.fs.writeErr["/usr/local/bin/cvctl.previous"] = errors.New("no space left on device")
		}},
		{"rename falla", func(f *fixture) { f.fs.renameErr[latestTmp] = errors.New("EIO") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, "v0.8.2")
			p := f.prepare(t)
			tt.setup(f)
			_, err := f.u.Apply(context.Background(), p)
			if code(err) != "E_UPGRADE_INSTALL" {
				t.Fatalf("err = %v", err)
			}
			if f.binary(t) != oldBinary {
				t.Errorf("binario = %q", f.binary(t))
			}
			f.noTemp(t)
			if n := f.count(restartCmd); n != 0 {
				t.Errorf("restart = %d", n)
			}
		})
	}
}

func TestApplyRefusesWhileDeploying(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	p := f.prepare(t)
	f.u.Deploying = func() ([]string, error) { return []string{"cute-prod"}, nil }
	f.run.Calls = nil
	_, err := f.u.Apply(context.Background(), p)
	if code(err) != "E_UPGRADE_BUSY" || !strings.Contains(err.Error(), "cute-prod") {
		t.Fatalf("err = %v", err)
	}
	if f.binary(t) != oldBinary {
		t.Errorf("binario = %q", f.binary(t))
	}
	f.noTemp(t)
	if _, ok := f.fs.Files()["/usr/local/bin/cvctl.previous"]; ok {
		t.Error("no debe escribir cvctl.previous")
	}
	if len(f.run.Calls) != 0 {
		t.Errorf("calls = %v", f.run.Calls)
	}
}

func TestApplyStopsWhenDeployingFails(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	p := f.prepare(t)
	f.u.Deploying = func() ([]string, error) { return nil, errors.New("estado ilegible") }
	if _, err := f.u.Apply(context.Background(), p); err == nil {
		t.Fatal("se esperaba error")
	}
	if f.binary(t) != oldBinary {
		t.Errorf("binario = %q", f.binary(t))
	}
	f.noTemp(t)
}

func TestApplyWithoutPreparedBinary(t *testing.T) {
	f := newFixture(t, "v0.9.0")
	_, err := f.u.Apply(context.Background(), f.prepare(t))
	if code(err) != "E_UPGRADE_INSTALL" {
		t.Fatalf("err = %v", err)
	}
	if f.binary(t) != oldBinary || len(f.run.Calls) != 0 {
		t.Errorf("binario = %q, calls = %v", f.binary(t), f.run.Calls)
	}
}

func assertRestored(t *testing.T, f *fixture, err error) {
	t.Helper()
	if code(err) != "E_UPGRADE_FAILED" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "restaurado y agente reiniciado") {
		t.Errorf("mensaje = %v", err)
	}
	if f.binary(t) != oldBinary {
		t.Errorf("binario = %q", f.binary(t))
	}
	if n := f.count(restartCmd); n != 2 {
		t.Errorf("restart = %d, se esperaban 2", n)
	}
	f.noTemp(t)
}

func TestApplyRestoresWhenAgentFails(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	p := f.prepare(t)
	f.agentDiesOnRestart(sys.Result{ExitCode: 3, Stdout: []byte("failed\n")})
	_, err := f.u.Apply(context.Background(), p)
	assertRestored(t, f, err)
	if !strings.Contains(err.Error(), "el agente no está activo") {
		t.Errorf("mensaje = %v", err)
	}
	if n := f.count(isActiveCmd); n != 3 {
		t.Errorf("is-active = %d: antes de sustituir, en la vigilancia y tras restaurar", n)
	}
}

func TestApplyRollbackReportsActiveAgent(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	p := f.prepare(t)
	restarts := 0
	f.u.Run = runnerFunc(func(ctx context.Context, c sys.Cmd) (sys.Result, error) {
		if c.String() == restartCmd {
			switch restarts++; restarts {
			case 1:
				f.run.On(isActiveCmd, sys.Result{ExitCode: 3})
			case 2:
				f.run.On(isActiveCmd, sys.Result{Stdout: []byte("active\n")})
			}
		}
		return f.run.Run(ctx, c)
	})
	_, err := f.u.Apply(context.Background(), p)
	assertRestored(t, f, err)
	if !strings.Contains(err.Error(), "el agente está activo") {
		t.Errorf("mensaje = %v", err)
	}
}

func TestApplyRestoresWhenAgentDiesLate(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	p := f.prepare(t)
	f.onSleep = func(step int) {
		if step == 6 {
			f.run.On(isActiveCmd, sys.Result{ExitCode: 3})
		}
	}
	assertRestored(t, f, f.applyErr(p))
}

func TestApplyRestoresWhenRestartFails(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	p := f.prepare(t)
	// Only the first restart fails: the one on the restored binary works.
	restarts := 0
	f.u.Run = runnerFunc(func(ctx context.Context, c sys.Cmd) (sys.Result, error) {
		res, err := f.run.Run(ctx, c)
		if c.String() == restartCmd {
			if restarts++; restarts == 1 {
				return sys.Result{ExitCode: 1, Stderr: []byte("unit failed")}, nil
			}
		}
		return res, err
	})
	assertRestored(t, f, f.applyErr(p))
	if len(f.slept) != 0 {
		t.Errorf("no debe vigilar tras un reinicio fallido: %v", f.slept)
	}
}

func TestApplyRestoresOnRestartLoop(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	p := f.prepare(t)
	f.onSleep = func(step int) {
		if step == 3 {
			f.run.On(nRestartsCmd, sys.Result{Stdout: []byte("3\n")})
		}
	}
	assertRestored(t, f, f.applyErr(p))
}

// A manual restart zeroes NRestarts in systemd, so a loop can stay below the
// value read beforehand and only shows as growth between readings.
func TestApplyRestoresOnRestartLoopAfterCounterReset(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	f.run.On(nRestartsCmd, sys.Result{Stdout: []byte("7\n")})
	p := f.prepare(t)
	f.onSleep = func(step int) {
		n := "0\n"
		if step >= 3 {
			n = "1\n"
		}
		f.run.On(nRestartsCmd, sys.Result{Stdout: []byte(n)})
	}
	assertRestored(t, f, f.applyErr(p))
}

func TestApplyCounterResetAloneIsFine(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	f.run.On(nRestartsCmd, sys.Result{Stdout: []byte("7\n")})
	p := f.prepare(t)
	f.onSleep = func(int) { f.run.On(nRestartsCmd, sys.Result{Stdout: []byte("0\n")}) }
	if _, err := f.u.Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if f.binary(t) != newBinary {
		t.Errorf("binario = %q", f.binary(t))
	}
}

func TestApplyRestoresWhenWatchCannotReadCounter(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	p := f.prepare(t)
	f.onSleep = func(int) { f.run.On(nRestartsCmd, sys.Result{ExitCode: 1}) }
	assertRestored(t, f, f.applyErr(p))
}

func TestApplyRestoresEvenIfInterrupted(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	p := f.prepare(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.u.Run = ctxRunner{f.run}
	f.onSleep = func(int) { cancel() }
	_, err := f.u.Apply(ctx, p)
	assertRestored(t, f, err)
	if !strings.Contains(err.Error(), "el agente está activo") {
		t.Errorf("mensaje = %v", err)
	}
}

func TestApplyReportsFailedRestoreOfBinary(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	p := f.prepare(t)
	f.agentDiesOnRestart(sys.Result{ExitCode: 3})
	f.fs.renameErr["/usr/local/bin/cvctl.previous"] = errors.New("EIO")
	_, err := f.u.Apply(context.Background(), p)
	if code(err) != "E_UPGRADE_FAILED" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "no se pudo restaurar") {
		t.Errorf("mensaje = %v", err)
	}
	if !strings.Contains(hint(err), "/usr/local/bin/cvctl.previous") {
		t.Errorf("hint = %q", hint(err))
	}
	if n := f.count(restartCmd); n != 1 {
		t.Errorf("restart = %d: sin restaurar no hay nada que reiniciar", n)
	}
}

func TestApplyReportsFailedRestartAfterRestore(t *testing.T) {
	f := newFixture(t, "v0.8.2")
	p := f.prepare(t)
	f.run.On(restartCmd, sys.Result{ExitCode: 1, Stderr: []byte("unit failed")})
	_, err := f.u.Apply(context.Background(), p)
	if code(err) != "E_UPGRADE_FAILED" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "el agente no se pudo reiniciar") {
		t.Errorf("mensaje = %v", err)
	}
	if n := f.count(isActiveCmd); n != 2 {
		t.Errorf("is-active = %d: debe comprobarse aunque falle el reinicio", n)
	}
	if f.binary(t) != oldBinary {
		t.Errorf("binario = %q", f.binary(t))
	}
}
