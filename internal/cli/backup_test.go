package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
)

type cliRecNotifier struct {
	mu   sync.Mutex
	msgs []notify.Message
}

func (n *cliRecNotifier) Send(_ context.Context, m notify.Message) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.msgs = append(n.msgs, m)
	return nil
}

func TestBackupRunNotifiesFailureBeforeRecord(t *testing.T) {
	for _, sub := range []string{"run", "verify"} {
		_, env := setupOpsFixture(t, "myapp-dev")
		note := &cliRecNotifier{}
		env.Deployer.Notify = note
		if err := env.Store.Save(&state.State{Instance: "myapp-dev", Status: state.StatusOK}); err != nil {
			t.Fatal(err)
		}
		oldEnv := newOpsEnv
		t.Cleanup(func() { newOpsEnv = oldEnv })
		newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }
		// no backup destination: the instance fails before any status is recorded
		code, _, _ := run(t, "backup", sub, "--json")
		if code == 0 {
			t.Fatalf("%s: expected failure", sub)
		}
		if len(note.msgs) != 1 || note.msgs[0].Level != "error" || !strings.Contains(note.msgs[0].Title, "myapp-dev") {
			t.Fatalf("%s: notify: %+v", sub, note.msgs)
		}
	}
}

type readDirErrFS struct{ sys.FS }

func (readDirErrFS) ReadDir(string) ([]os.DirEntry, error) { return nil, errors.New("io error") }

func TestBackupRunNotifiesTopLevelFailure(t *testing.T) {
	for _, sub := range []string{"run", "verify"} {
		tmp, env := setupOpsFixture(t, "myapp-dev")
		note := &cliRecNotifier{}
		env.Deployer.Notify = note
		env.Store = state.NewStore(readDirErrFS{sys.NewOSFS()}, tmp+"/state")
		oldEnv := newOpsEnv
		t.Cleanup(func() { newOpsEnv = oldEnv })
		newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }
		code, out, _ := run(t, "backup", sub, "--json")
		if code == 0 || !strings.Contains(out, "E_STATE_IO") {
			t.Fatalf("%s: code=%d out=%s", sub, code, out)
		}
		if len(note.msgs) != 1 || note.msgs[0].Level != "error" || !strings.Contains(note.msgs[0].Text, "E_STATE_IO") {
			t.Fatalf("%s: notify: %+v", sub, note.msgs)
		}
	}
}
