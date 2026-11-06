package sys

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPipeStreams(t *testing.T) {
	res, err := NewExecPiper().Pipe(context.Background(),
		Cmd{Name: "sh", Args: []string{"-c", "head -c 8388608 /dev/zero"}},
		Cmd{Name: "wc", Args: []string{"-c"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(res.To.Stdout)); got != "8388608" {
		t.Fatalf("wc -c = %q", got)
	}
}

func TestPipeReportsBothExitCodes(t *testing.T) {
	for _, tc := range []struct{ from, to int }{{3, 0}, {0, 4}} {
		res, err := NewExecPiper().Pipe(context.Background(),
			Cmd{Name: "sh", Args: []string{"-c", "echo hi; exit " + string(rune('0'+tc.from))}},
			Cmd{Name: "sh", Args: []string{"-c", "cat >/dev/null; exit " + string(rune('0'+tc.to))}})
		if err != nil {
			t.Fatal(err)
		}
		if res.From.ExitCode != tc.from || res.To.ExitCode != tc.to {
			t.Fatalf("codes = %d,%d want %d,%d", res.From.ExitCode, res.To.ExitCode, tc.from, tc.to)
		}
	}
}

func TestPipeConsumerExitStopsProducer(t *testing.T) {
	start := time.Now()
	res, err := NewExecPiper().Pipe(context.Background(),
		Cmd{Name: "yes"}, Cmd{Name: "head", Args: []string{"-c", "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
	if res.To.ExitCode != 0 {
		t.Fatalf("head exit = %d", res.To.ExitCode)
	}
}

func TestPipeContextCancelKillsBoth(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := NewExecPiper().Pipe(ctx,
		Cmd{Name: "sleep", Args: []string{"30"}}, Cmd{Name: "sleep", Args: []string{"30"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestPipeStderrCappedAndEnv(t *testing.T) {
	res, err := NewExecPiper().Pipe(context.Background(),
		Cmd{Name: "sh", Args: []string{"-c", "head -c 200000 /dev/zero >&2"}},
		Cmd{Name: "sh", Args: []string{"-c", "echo $PIPE_VAR"}, Env: []string{"PIPE_VAR=ok"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.From.Stderr) != 64<<10 {
		t.Fatalf("stderr len = %d", len(res.From.Stderr))
	}
	if strings.TrimSpace(string(res.To.Stdout)) != "ok" {
		t.Fatalf("env not applied: %q", res.To.Stdout)
	}
}

func TestPipeStartFailure(t *testing.T) {
	_, err := NewExecPiper().Pipe(context.Background(),
		Cmd{Name: "sleep", Args: []string{"30"}}, Cmd{Name: "/nonexistent/cmd"})
	if err == nil {
		t.Fatal("want start error")
	}
}
