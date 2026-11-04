package sys

import (
	"context"
	"strings"
	"testing"
)

func TestExecRunnerDir(t *testing.T) {
	res, err := NewExecRunner().Run(context.Background(), Cmd{Name: "pwd", Dir: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "/" {
		t.Fatalf("pwd = %q", got)
	}
}
