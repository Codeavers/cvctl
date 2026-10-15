package output

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrinterResultJSON(t *testing.T) {
	var out, errb bytes.Buffer
	p := &Printer{JSON: true, Out: &out, Err: &errb}
	p.Result(map[string]int{"a": 1}, nil)
	if got, want := out.String(), `{"ok":true,"data":{"a":1}}`+"\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestPrinterFailJSON(t *testing.T) {
	var out, errb bytes.Buffer
	p := &Printer{JSON: true, Out: &out, Err: &errb}
	code := p.Fail(&Error{Code: "E_X", Message: "m", Hint: "h"})
	if code != 1 {
		t.Fatalf("code %d", code)
	}
	want := `{"ok":false,"error":{"code":"E_X","message":"m","hint":"h"}}` + "\n"
	if out.String() != want {
		t.Fatalf("got %q want %q", out.String(), want)
	}
}

func TestPrinterFailIssuesJSON(t *testing.T) {
	var out, errb bytes.Buffer
	p := &Printer{JSON: true, Out: &out, Err: &errb}
	code := p.Fail(&IssuesError{Issues: []Issue{{Code: "E_A", Service: "s", Message: "m", Hint: "h"}}})
	if code != 1 || !strings.Contains(out.String(), `"code":"E_LINT_FAILED"`) || !strings.Contains(out.String(), `"issues":[{"code":"E_A","service":"s","message":"m","hint":"h"}]`) {
		t.Fatalf("code %d out %q", code, out.String())
	}
}

func TestPrinterFailUsageExit2(t *testing.T) {
	var out, errb bytes.Buffer
	p := &Printer{Out: &out, Err: &errb}
	if code := p.Fail(&UsageError{Err: errString("bad")}); code != 2 {
		t.Fatalf("code %d", code)
	}
}

func TestPrinterFailHuman(t *testing.T) {
	var out, errb bytes.Buffer
	p := &Printer{Out: &out, Err: &errb}
	p.Fail(&Error{Code: "E_X", Message: "m", Hint: "h"})
	s := errb.String()
	if !strings.Contains(s, "error E_X: m") || !strings.Contains(s, "hint: h") {
		t.Fatalf("got %q", s)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
