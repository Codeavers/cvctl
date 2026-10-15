package envspec

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/codeavers/cvctl/internal/output"
)

func code(t *testing.T, err error) string {
	t.Helper()
	var e *output.Error
	if !errors.As(err, &e) {
		t.Fatalf("not an output.Error: %v", err)
	}
	if e.Hint == "" {
		t.Fatalf("empty hint for %s", e.Code)
	}
	return e.Code
}

func TestResolveAllSources(t *testing.T) {
	def := "d"
	s := &Spec{Vars: map[string]Var{
		"DATABASE_URL": {Source: "service", Service: "pg"},
		"JWT":          {Source: "random", Length: 4},
		"MODE":         {Source: "fixed", Value: "x"},
		"API":          {Source: "template", Value: "https://{domain:api}/{var:MODE}"},
		"OPT":          {Source: "prompt", Default: &def},
	}}
	res, err := Resolve(s, Input{
		Env:         "prod",
		ServiceVars: map[string]map[string]string{"pg": {"DATABASE_URL": "postgres://u"}},
		Domains:     map[string]string{"api": "api.x.com"},
		Rand:        bytes.NewReader([]byte{0, 1, 2, 3}),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"DATABASE_URL": "postgres://u", "JWT": "ABCD", "MODE": "x",
		"API": "https://api.x.com/x", "OPT": "d",
	}
	if !reflect.DeepEqual(res.Values, want) {
		t.Fatalf("got %v", res.Values)
	}
	if !reflect.DeepEqual(res.Generated, []string{"JWT"}) || len(res.Missing) != 0 {
		t.Fatalf("generated %v missing %v", res.Generated, res.Missing)
	}
}

func TestRandomKeepsExisting(t *testing.T) {
	s := &Spec{Vars: map[string]Var{"JWT": {Source: "random"}}}
	res, err := Resolve(s, Input{Existing: map[string]string{"JWT": "keep"}, Rand: bytes.NewReader(nil)})
	if err != nil || res.Values["JWT"] != "keep" || len(res.Generated) != 0 {
		t.Fatalf("%v %v", res, err)
	}
}

func TestRandomDefaultLengthAndRejection(t *testing.T) {
	s := &Spec{Vars: map[string]Var{"K": {Source: "random"}}}
	b := append([]byte{255, 248}, bytes.Repeat([]byte{61}, 48)...)
	res, err := Resolve(s, Input{Rand: bytes.NewReader(b)})
	if err != nil || res.Values["K"] != string(bytes.Repeat([]byte{'9'}, 48)) {
		t.Fatalf("%v %v", res, err)
	}
}

func TestPromptMissing(t *testing.T) {
	s := &Spec{Vars: map[string]Var{"SMTP_PASSWORD": {Source: "prompt", Secret: true}}}
	res, err := Resolve(s, Input{})
	if err != nil || !reflect.DeepEqual(res.Missing, []string{"SMTP_PASSWORD"}) {
		t.Fatalf("%v %v", res, err)
	}
	if _, ok := res.Values["SMTP_PASSWORD"]; ok {
		t.Fatal("value set")
	}
}

func TestPromptEmptyDefault(t *testing.T) {
	empty := ""
	s := &Spec{Vars: map[string]Var{"X": {Source: "prompt", Default: &empty}}}
	res, _ := Resolve(s, Input{})
	if v, ok := res.Values["X"]; !ok || v != "" || len(res.Missing) != 0 {
		t.Fatalf("%v", res)
	}
}

func TestFixedPerEnv(t *testing.T) {
	s := &Spec{Vars: map[string]Var{"E": {Source: "fixed", Value: map[string]string{"dev": "development", "prod": "production"}}}}
	res, err := Resolve(s, Input{Env: "prod"})
	if err != nil || res.Values["E"] != "production" {
		t.Fatalf("%v %v", res, err)
	}
	_, err = Resolve(s, Input{Env: "stg"})
	if c := code(t, err); c != "E_ENVSPEC_FIXED_ENV" {
		t.Fatal(c)
	}
}

func TestServiceVarMissing(t *testing.T) {
	s := &Spec{Vars: map[string]Var{"A": {Source: "service", Service: "pg"}}}
	_, err := Resolve(s, Input{})
	if c := code(t, err); c != "E_ENVSPEC_SERVICE_VAR" {
		t.Fatal(c)
	}
}

func TestTemplateDomain(t *testing.T) {
	s := &Spec{Vars: map[string]Var{"U": {Source: "template", Value: "https://{domain:api}"}}}
	res, err := Resolve(s, Input{Domains: map[string]string{"api": "api.x.com"}})
	if err != nil || res.Values["U"] != "https://api.x.com" {
		t.Fatalf("%v %v", res, err)
	}
}

func TestTemplateChainAndErrors(t *testing.T) {
	s := &Spec{Vars: map[string]Var{
		"A": {Source: "template", Value: "{var:B}!"},
		"B": {Source: "template", Value: "b{var:C}"},
		"C": {Source: "fixed", Value: "c"},
	}}
	res, err := Resolve(s, Input{})
	if err != nil || res.Values["A"] != "bc!" {
		t.Fatalf("%v %v", res, err)
	}
	for name, v := range map[string]string{"unknown var": "{var:NOPE}", "unknown domain": "{domain:zz}", "bad syntax": "{what:x}"} {
		s := &Spec{Vars: map[string]Var{"T": {Source: "template", Value: v}}}
		_, err := Resolve(s, Input{})
		if c := code(t, err); c != "E_ENVSPEC_TEMPLATE_REF" {
			t.Fatalf("%s: %s", name, c)
		}
	}
	cyc := &Spec{Vars: map[string]Var{
		"A": {Source: "template", Value: "{var:B}"},
		"B": {Source: "template", Value: "{var:A}"},
	}}
	_, err = Resolve(cyc, Input{})
	if c := code(t, err); c != "E_ENVSPEC_TEMPLATE_REF" {
		t.Fatal(c)
	}
}

func TestTemplateRefMissingPrompt(t *testing.T) {
	s := &Spec{Vars: map[string]Var{
		"P":  {Source: "prompt"},
		"T":  {Source: "template", Value: "x{var:P}"},
		"T2": {Source: "template", Value: "y{var:T}"},
		"OK": {Source: "fixed", Value: "ok"},
	}}
	res, err := Resolve(s, Input{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Missing, []string{"P"}) {
		t.Fatalf("missing %v", res.Missing)
	}
	for _, k := range []string{"P", "T", "T2"} {
		if _, ok := res.Values[k]; ok {
			t.Fatalf("%s should be absent", k)
		}
	}
	if res.Values["OK"] != "ok" {
		t.Fatalf("%v", res.Values)
	}
}

func TestUnknownKeysKept(t *testing.T) {
	s := &Spec{Vars: map[string]Var{"A": {Source: "fixed", Value: "a"}}}
	res, _ := Resolve(s, Input{Existing: map[string]string{"EXTRA": "e"}})
	if res.Values["EXTRA"] != "e" || res.Values["A"] != "a" {
		t.Fatalf("%v", res.Values)
	}
}

func write(t *testing.T, body string) string {
	p := filepath.Join(t.TempDir(), "env.spec.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadOK(t *testing.T) {
	s, err := Load(write(t, `vars:
  A: {source: service, service: pg}
  B: {source: random}
  C: {source: fixed, value: {dev: d, prod: p}}
  D: {source: template, value: "x{var:A}"}
  E: {source: prompt, default: "", secret: true}
`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Vars["B"].Length != 48 || s.Vars["E"].Default == nil || !s.Vars["E"].Secret {
		t.Fatalf("%+v", s.Vars)
	}
	if !reflect.DeepEqual(s.Vars["C"].Value, map[string]string{"dev": "d", "prod": "p"}) {
		t.Fatalf("%#v", s.Vars["C"].Value)
	}
}

func TestLoadInvalid(t *testing.T) {
	for name, body := range map[string]string{
		"magic":      "vars:\n  A: {source: magic}\n",
		"no service": "vars:\n  A: {source: service}\n",
		"no value":   "vars:\n  A: {source: fixed}\n",
		"bad value":  "vars:\n  A: {source: fixed, value: [1]}\n",
		"bad yaml":   "vars: [",
	} {
		_, err := Load(write(t, body))
		if c := code(t, err); c != "E_ENVSPEC_INVALID" {
			t.Fatalf("%s: %s", name, c)
		}
	}
	_, err := Load(filepath.Join(t.TempDir(), "missing.yml"))
	if c := code(t, err); c != "E_ENVSPEC_INVALID" {
		t.Fatal(c)
	}
}
