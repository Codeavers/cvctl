package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
)

// siloRunner logs docker commands into the fake compose call list and plays
// the silo container.
type siloRunner struct {
	log      *fakeCompose
	failExec bool
	stdin    []string
}

func (n *siloRunner) Run(_ context.Context, c sys.Cmd) (sys.Result, error) {
	n.log.mu.Lock()
	defer n.log.mu.Unlock()
	cmd := c.String()
	n.log.calls = append(n.log.calls, "docker:"+strings.TrimPrefix(cmd, "docker "))
	switch {
	case strings.HasPrefix(cmd, "docker network inspect"):
		return sys.Result{ExitCode: 1}, nil
	case strings.HasPrefix(cmd, "docker ps "):
		return sys.Result{Stdout: []byte("silo1\n")}, nil
	case strings.HasPrefix(cmd, "docker exec -i silo1"):
		n.stdin = append(n.stdin, string(c.Stdin))
		if n.failExec {
			return sys.Result{ExitCode: 1, Stderr: []byte("denied INSTSECRETKEY123")}, nil
		}
	}
	return sys.Result{}, nil
}

func keyedRig(t *testing.T) (*testRig, *siloRunner) {
	t.Helper()
	rig := setupTestRig(t, "dev", "demo-dev")
	b := testBackupTarget()
	b.Local = true
	rig.deployer.Backup = b
	path := filepath.Join(rig.instDir, "cvctl.env")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("CVCTL_BACKUP_ACCESS_KEY=bk-demo-dev\nCVCTL_BACKUP_SECRET_KEY=INSTSECRETKEY123\nCVCTL_BACKUP_LIBSODIUM_KEY=INSTLIBSODIUM123\nCVCTL_BACKUP_RESTIC_PASSWORD=INSTRESTIC123\n")...)
	if err := os.WriteFile(path, raw, 0o640); err != nil {
		t.Fatal(err)
	}
	run := &siloRunner{log: rig.compose}
	rig.deployer.Run = run
	return rig, run
}

func TestDeployProvisionsBackupUserThenConnectsThenUpData(t *testing.T) {
	rig, run := keyedRig(t)
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{Actor: "agent"})
	if err != nil || res.Status != state.StatusOK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	calls := rig.compose.calls
	iNet := indexOf(calls, "docker:network create --label cvctl.managed=true --internal demo-dev_backup")
	iExec := indexOf(calls, "docker:exec -i silo1 sh -s")
	iConn := indexOf(calls, "docker:network connect --alias cvctl-storage demo-dev_backup silo1")
	iData := -1
	for i, c := range calls {
		if strings.HasPrefix(c, "updata:") {
			iData = i
			break
		}
	}
	if iNet < 0 || iExec < iNet || iConn < iExec || iData < iConn {
		t.Fatalf("order wrong (net=%d exec=%d connect=%d updata=%d): %v", iNet, iExec, iConn, iData, calls)
	}
	if len(run.stdin) != 1 || !strings.Contains(run.stdin[0], "mc admin user add local 'bk-demo-dev'") {
		t.Fatalf("script: %v", run.stdin)
	}
	env, err := os.ReadFile(filepath.Join(rig.instDir, "backup-main-db.env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "AWS_ACCESS_KEY_ID=bk-demo-dev") || !strings.Contains(string(env), "WALG_LIBSODIUM_KEY=INSTLIBSODIUM123") ||
		strings.Contains(string(env), "LIBSODIUMSECRET") || strings.Contains(string(env), "AKIDSECRET1") {
		t.Fatalf("backup env must hold only the instance identity and key:\n%s", env)
	}
}

func TestDeployBackupProvisionFailureIsTransientAndRedacted(t *testing.T) {
	rig, run := keyedRig(t)
	run.failExec = true
	saveOK(t, rig)
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", newWant(), Options{Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := rig.store.Load("demo-dev")
	if res.Status != state.StatusFailed || st.FailedHash != "" || !strings.Contains(res.Detail, "denied") {
		t.Fatalf("res=%+v state=%+v", res, st)
	}
	if strings.Contains(res.Detail, "INSTSECRETKEY123") {
		t.Fatalf("secret leaked: %s", res.Detail)
	}
	for _, c := range rig.compose.calls {
		if strings.HasPrefix(c, "updata:") || c == "up" {
			t.Fatalf("data or apps started after a failed provision: %v", rig.compose.calls)
		}
	}
}

func TestExecuteSwapProvisionsBackupUserBeforeUp(t *testing.T) {
	rig, _ := keyedRig(t)
	target := defaultDesired()
	if err := rig.deployer.executeSwap(context.Background(), "demo-dev", &target, map[string]string{}, false); err != nil {
		t.Fatal(err)
	}
	calls := rig.compose.calls
	iExec := indexOf(calls, "docker:exec -i silo1 sh -s")
	iConn := indexOf(calls, "docker:network connect --alias cvctl-storage demo-dev_backup silo1")
	iUp := indexOf(calls, "up")
	if iExec < 0 || iConn < iExec || iUp < iConn {
		t.Fatalf("order wrong: %v", calls)
	}
}

func TestCvctlSecretsRedactsBackupKeys(t *testing.T) {
	rig, _ := keyedRig(t)
	s := rig.deployer.cvctlSecrets(filepath.Join(rig.instDir, "cvctl.env"))
	for _, k := range []string{"CVCTL_BACKUP_SECRET_KEY", "CVCTL_BACKUP_LIBSODIUM_KEY", "CVCTL_BACKUP_RESTIC_PASSWORD", "CVCTL_BACKUP_ACCESS_KEY"} {
		if s[k] == "" {
			t.Errorf("%s not redacted", k)
		}
	}
}
