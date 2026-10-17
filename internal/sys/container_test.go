package sys_test

import (
	"context"
	"testing"

	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

const probeCmd = "docker ps -a --filter label=com.docker.compose.project=app-prod --filter label=com.docker.compose.service=db --format {{.State}}"

func TestContainerState(t *testing.T) {
	for name, tc := range map[string]struct {
		out             string
		exists, running bool
	}{
		"none":    {"", false, false},
		"exited":  {"exited\n", true, false},
		"running": {"running\n", true, true},
		"mixed":   {"exited\nrunning\n", true, true},
	} {
		r := &systest.Runner{}
		r.On(probeCmd, sys.Result{Stdout: []byte(tc.out)})
		e, ru, err := sys.ContainerState(context.Background(), r, "app-prod", "db")
		if err != nil || e != tc.exists || ru != tc.running {
			t.Errorf("%s: %v %v %v", name, e, ru, err)
		}
	}
}

func TestContainerStateDockerFailure(t *testing.T) {
	r := &systest.Runner{}
	r.On(probeCmd, sys.Result{ExitCode: 1, Stderr: []byte("daemon down")})
	if _, _, err := sys.ContainerState(context.Background(), r, "app-prod", "db"); err == nil {
		t.Fatal("expected error")
	}
}
