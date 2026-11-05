package backup_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/backup"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/kinds"
)

const (
	appPass = "app-pass-SECRET-654"
	// The start of both, so a password cut by a length limit is caught too.
	enginePrefix = "engine-pass"
	appPrefix    = "app-pass"
)

var leaky = []byte("fatal: login failed with " + enginePass + " and " + appPass)

func assertNoCreds(t *testing.T, g *rig, texts ...string) {
	t.Helper()
	for _, m := range g.note.msgs {
		texts = append(texts, m.Title, m.Text, m.Error)
	}
	hist, err := g.store.History("app-prod", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range hist {
		texts = append(texts, ev.Detail)
	}
	st, err := g.store.Load("app-prod")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range st.Backups {
		texts = append(texts, b.LastError, b.LastVerifyError)
	}
	for _, s := range texts {
		if strings.Contains(s, enginePrefix) || strings.Contains(s, appPrefix) {
			t.Fatalf("credential leaked: %s", s)
		}
	}
}

func withAppPass(t *testing.T, g *rig) {
	t.Helper()
	_, d, err := g.r.Load("app-prod")
	if err != nil {
		t.Fatal(err)
	}
	sd := d.Services["db"]
	sd.Creds.AppPassword = appPass
	d.Services["db"] = sd
}

func TestDumpBackupRedactsServiceCredentials(t *testing.T) {
	for name, breakIt := range map[string]func(*dumpRig){
		"producer": func(g *dumpRig) { g.pipe.Result.From = sys.Result{ExitCode: 1, Stderr: leaky} },
		"restic":   func(g *dumpRig) { g.pipe.Result.To.ExitCode, g.pipe.Result.To.Stderr = 1, leaky },
		"forget": func(g *dumpRig) {
			g.pipe.Result.From = sys.Result{ExitCode: 1, Stderr: leaky}
			g.run.OnPrefix(tagCmd[:strings.LastIndex(tagCmd, " tag")]+" forget", sys.Result{ExitCode: 1, Stderr: leaky})
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newDumpRig(t)
			withAppPass(t, g.rig)
			breakIt(g)
			res, err := g.backup(t)
			if err == nil || len(res) != 1 || res[0].OK {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			assertNoCreds(t, g.rig, err.Error(), res[0].Error)
		})
	}
}

func TestDumpRestoreRedactsServiceCredentials(t *testing.T) {
	for name, breakIt := range map[string]func(*dumpRestoreRig){
		"restore_command": func(g *dumpRestoreRig) { g.pipe.Result.To = sys.Result{ExitCode: 1, Stderr: leaky} },
		"reader":          func(g *dumpRestoreRig) { g.pipe.Result.From = sys.Result{ExitCode: 1, Stderr: leaky} },
		"engine run":      func(g *dumpRestoreRig) { g.run.OnPrefix("docker run -d", sys.Result{ExitCode: 125, Stderr: leaky}) },
		"engine exited": func(g *dumpRestoreRig) {
			g.run.OnPrefix("docker inspect", sys.Result{Stdout: []byte("false\n")})
			g.run.OnPrefix("docker logs", sys.Result{Stdout: leaky})
		},
		"shutdown": func(g *dumpRestoreRig) {
			g.run.On("docker stop -t 120 "+dumpTemp, sys.Result{ExitCode: 1, Stderr: leaky})
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newDumpRestoreRig(t)
			withAppPass(t, g.rig)
			breakIt(g)
			_, err := g.restore("latest")
			if err == nil {
				t.Fatal("want error")
			}
			assertNoCreds(t, g.rig, err.Error())
		})
	}
}

func TestDumpRestoreFreshRedactsServiceCredentials(t *testing.T) {
	g := newDumpRestoreRig(t)
	withAppPass(t, g.rig)
	g.run.On("docker volume inspect fresh-vol", sys.Result{ExitCode: 1})
	g.pipe.Result.To = sys.Result{ExitCode: 1, Stderr: leaky}
	p, d, _ := g.r.Load("app-prod")
	err := g.r.RestoreFresh(context.Background(), "app-prod", "db", p, d, "fresh-vol")
	if err == nil {
		t.Fatal("want error")
	}
	assertNoCreds(t, g.rig, err.Error())
}

func TestRestorePointDumpRedactsServiceCredentials(t *testing.T) {
	g := newDumpRig(t)
	withAppPass(t, g.rig)
	g.run.On(psAll, sys.Result{Stdout: []byte("running\n")})
	g.pipe.Result.From = sys.Result{ExitCode: 1, Stderr: leaky}
	rp := &backup.RestorePoint{Compose: g.r.Compose, Load: g.r.Load, Data: g.r}
	err := rp.Create(context.Background(), "app-prod", "deploy-5")
	if err == nil || errors.Is(err, backup.ErrFreshInstance) {
		t.Fatalf("err: %v", err)
	}
	assertNoCreds(t, g.rig, err.Error())
}

func TestDumpVerifyRedactsBeforeCutting(t *testing.T) {
	// 20 characters first, so the limit of 32 falls inside the password.
	g, _ := dumpVerifyRig(t, sys.Result{Stdout: []byte(strings.Repeat("x", 20) + enginePass + "\n")})
	res, err := g.r.Verify(context.Background(), "app-prod", "db")
	if err == nil || len(res) != 1 || res[0].OK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	assertNoCreds(t, g.rig, err.Error(), res[0].Error)
}

// A sqlite service has no credentials of its own, but its helper shares an
// instance with services that do.
func sqliteRedactRig(t *testing.T, probeOut string) *sqliteRig {
	t.Helper()
	g := newSqliteRig(t, probeOut)
	g.d.Services["pg"] = instance.ServiceDecision{Mode: "local", Creds: kinds.Credentials{Password: enginePass, AppPassword: appPass}}
	return g
}

func TestSqliteHelperOutputRedactsServiceCredentials(t *testing.T) {
	t.Run("helper fails", func(t *testing.T) {
		g := sqliteRedactRig(t, "")
		g.run.OnPrefix(sqliteProbe, sys.Result{ExitCode: 1, Stderr: leaky})
		res, err := g.backup(t)
		if err == nil {
			t.Fatal("want error")
		}
		assertNoCreds(t, g.rig, err.Error(), res[0].Error)
	})
	t.Run("probe output", func(t *testing.T) {
		g := sqliteRedactRig(t, strings.Repeat("x", 52)+enginePass+"\n")
		res, err := g.backup(t)
		if err == nil {
			t.Fatal("want error")
		}
		assertNoCreds(t, g.rig, err.Error(), res[0].Error)
	})
	t.Run("integrity output", func(t *testing.T) {
		g, _ := sqliteVerifyRig(t, sys.Result{Stdout: []byte(strings.Repeat("x", 188) + enginePass + "\n")})
		g.d.Services["pg"] = instance.ServiceDecision{Mode: "local", Creds: kinds.Credentials{Password: enginePass}}
		res, err := g.r.Verify(context.Background(), "app-prod", "db")
		if err == nil {
			t.Fatal("want error")
		}
		assertNoCreds(t, g.rig, err.Error(), res[0].Error)
	})
	t.Run("prepare", func(t *testing.T) {
		g := sqliteRedactRig(t, "")
		g.run.OnPrefix(sqliteProbe, sys.Result{ExitCode: 1, Stderr: leaky})
		err := g.r.PrepareSqlite(context.Background(), g.p, g.d)
		if err == nil {
			t.Fatal("want error")
		}
		assertNoCreds(t, g.rig, err.Error())
	})
}
