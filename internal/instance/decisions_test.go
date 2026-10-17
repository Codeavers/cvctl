package instance

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/kinds"
)

func load(t *testing.T) (*contract.Project, kinds.Catalog) {
	t.Helper()
	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := contract.Load("../contract/testdata/valid", cat)
	if err != nil {
		t.Fatal(err)
	}
	return p, cat
}

func full() *Decisions {
	return &Decisions{
		Instance: "proj-dev", Env: "dev", InfraBranch: "develop", TrackTag: "dev",
		InfraPath: ".", Strategy: "recreate",
		Services: map[string]ServiceDecision{
			"main-db": {Mode: "local", Version: "16", Creds: kinds.Credentials{User: "u", Password: "p\"w $x #y \\z\nline2 'q'", DB: "d"}},
			"cache":   {Mode: "external", External: map[string]string{"url": "redis://h:6379/0?a=b#c"}},
			"media":   {Mode: "external", External: map[string]string{"endpoint": "https://s3", "bucket": "b", "access_key": "ak", "secret_key": "sk", "region": "r"}},
		},
		Domains: map[string]string{"api": "api.example.com"},
		Digests: map[string]string{"api": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
}

func TestRoundTrip(t *testing.T) {
	p, _ := load(t)
	path := filepath.Join(t.TempDir(), "cvctl.env")
	d := full()
	if err := Write(path, d); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path, p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, d) {
		t.Fatalf("mismatch\n got %#v\nwant %#v", got, d)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestWriteStable(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := Write(a, full()); err != nil {
		t.Fatal(err)
	}
	if err := Write(b, full()); err != nil {
		t.Fatal(err)
	}
	x, _ := os.ReadFile(a)
	y, _ := os.ReadFile(b)
	if string(x) != string(y) {
		t.Fatal("writes differ")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("leftover temp files: %v", entries)
	}
}

func TestReadIgnoresUnknown(t *testing.T) {
	p, _ := load(t)
	path := filepath.Join(t.TempDir(), "cvctl.env")
	os.WriteFile(path, []byte("CVCTL_ENV=dev\nOTHER=1\nCVCTL_SVC_GONE_MODE=local\nCVCTL_DOMAIN_GONE=x\nCVCTL_DIGEST_GONE=y\n"), 0o640)
	d, err := Read(path, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Services) != 0 || len(d.Domains) != 0 || len(d.Digests) != 0 {
		t.Fatalf("unexpected: %#v", d)
	}
}

func TestReadMissingFile(t *testing.T) {
	p, _ := load(t)
	_, err := Read(filepath.Join(t.TempDir(), "nope"), p)
	var e *output.Error
	if err == nil || !asErr(err, &e) || e.Code != "E_DECISION_READ" || e.Hint == "" {
		t.Fatalf("got %v", err)
	}
}

func asErr(err error, e **output.Error) bool {
	x, ok := err.(*output.Error)
	*e = x
	return ok
}

func codes(is []output.Issue) []string {
	var s []string
	for _, i := range is {
		s = append(s, i.Code+":"+i.Service)
	}
	return s
}

func TestCheckComplete(t *testing.T) {
	p, cat := load(t)
	d := full()
	if is := Check(d, p, cat); len(is) != 0 {
		t.Fatalf("unexpected %v", is)
	}
}

func TestCheckMissing(t *testing.T) {
	p, cat := load(t)
	is := Check(&Decisions{}, p, cat)
	want := []string{
		"E_DECISION_MISSING:", // instance
		"E_DECISION_MISSING:", // env
		"E_DECISION_MISSING:", // strategy
		"E_DECISION_MISSING:api",
		"E_DECISION_MISSING:api",
		"E_DECISION_MISSING:cache",
		"E_DECISION_MISSING:main-db",
		"E_DECISION_MISSING:media",
	}
	got := codes(is)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	for _, i := range is {
		if i.Hint == "" {
			t.Fatalf("no hint: %v", i)
		}
	}
}

func TestCheckLocalNoCredsAndExternalFields(t *testing.T) {
	p, cat := load(t)
	d := full()
	d.Services["main-db"] = ServiceDecision{Mode: "local", Version: "16"}
	d.Services["media"] = ServiceDecision{Mode: "external", External: map[string]string{"endpoint": "e"}}
	got := codes(Check(d, p, cat))
	want := []string{"E_DECISION_MISSING:main-db", "E_DECISION_MISSING:main-db", "E_DECISION_MISSING:main-db", "E_DECISION_MISSING:media", "E_DECISION_MISSING:media", "E_DECISION_MISSING:media", "E_DECISION_MISSING:media"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestCheckExternalNotAllowed(t *testing.T) {
	p, cat := load(t)
	d := full()
	d.Services["main-db"] = ServiceDecision{Mode: "external", External: map[string]string{"url": "postgres://x"}}
	is := Check(d, p, cat)
	if len(is) != 1 || is[0].Code != "E_DECISION_EXTERNAL_NOT_ALLOWED" || is[0].Service != "main-db" {
		t.Fatalf("got %v", is)
	}
}

func TestCheckInvalid(t *testing.T) {
	p, cat := load(t)
	d := full()
	d.Env = "staging"
	d.Strategy = "yolo"
	s := d.Services["main-db"]
	s.Version = "99"
	d.Services["main-db"] = s
	c := d.Services["cache"]
	c.Mode = "weird"
	d.Services["cache"] = c
	got := codes(Check(d, p, cat))
	want := []string{"E_DECISION_INVALID:", "E_DECISION_INVALID:", "E_DECISION_INVALID:cache", "E_DECISION_INVALID:main-db"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestWriteQuoting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e")
	d := &Decisions{Instance: "a b"}
	if err := Write(path, d); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `CVCTL_INSTANCE="a b"`) {
		t.Fatalf("%s", b)
	}
}

func TestCheckPartialCreds(t *testing.T) {
	p, cat := load(t)
	d := full()
	d.Services["main-db"] = ServiceDecision{Mode: "local", Version: "16", Creds: kinds.Credentials{User: "u"}}
	is := Check(d, p, cat)
	if len(is) != 2 || is[0].Service != "main-db" || !strings.Contains(is[0].Hint, "_DB") || !strings.Contains(is[1].Hint, "_PASSWORD") {
		t.Fatalf("got %v", is)
	}
}

func TestCheckRedisOnlyPassword(t *testing.T) {
	p, cat := load(t)
	d := full()
	d.Services["cache"] = ServiceDecision{Mode: "local", Version: "7", Creds: kinds.Credentials{Password: "x"}}
	if is := Check(d, p, cat); len(is) != 0 {
		t.Fatalf("got %v", is)
	}
	d.Services["cache"] = ServiceDecision{Mode: "local", Version: "7"}
	is := Check(d, p, cat)
	if len(is) != 1 || !strings.Contains(is[0].Hint, "_PASSWORD") {
		t.Fatalf("got %v", is)
	}
}

func issueMsgs(is []output.Issue, code string) []output.Issue {
	var out []output.Issue
	for _, i := range is {
		if i.Code == code {
			out = append(out, i)
		}
	}
	return out
}

func TestCheckInstance(t *testing.T) {
	p, cat := load(t)
	for _, tc := range []struct{ name, inst string }{
		{"not a label", "Proj_dev"},
		{"wrong env suffix", "proj-prod"},
		{"no suffix", "proj"},
		{"leading hyphen", "-dev"},
	} {
		d := full()
		d.Instance = tc.inst
		is := Check(d, p, cat)
		if len(is) != 1 || is[0].Code != "E_DECISION_INVALID" || is[0].Hint == "" {
			t.Fatalf("%s: got %v", tc.name, is)
		}
	}
	d := full()
	d.Instance = ""
	is := Check(d, p, cat)
	if len(is) != 1 || is[0].Code != "E_DECISION_MISSING" || !strings.Contains(is[0].Hint, "CVCTL_INSTANCE") {
		t.Fatalf("empty: got %v", is)
	}
}

func TestCheckCredCharset(t *testing.T) {
	p, cat := load(t)
	d := full()
	s := d.Services["main-db"]
	s.Creds.User = "u;drop"
	s.Creds.DB = "d b"
	d.Services["main-db"] = s
	is := issueMsgs(Check(d, p, cat), "E_DECISION_INVALID")
	if len(is) != 2 {
		t.Fatalf("got %v", is)
	}
	joined := is[0].Hint + is[1].Hint
	if !strings.Contains(joined, "CVCTL_SVC_MAIN_DB_USER") || !strings.Contains(joined, "CVCTL_SVC_MAIN_DB_DB") {
		t.Fatalf("hints: %v", is)
	}
}

func TestCheckDomain(t *testing.T) {
	p, cat := load(t)
	for _, bad := range []string{"a.com`) || Host(`b", "-a.com", "a-.com", "A.com", "a..com", "a b.com", strings.Repeat("a", 254)} {
		d := full()
		d.Domains["api"] = bad
		is := Check(d, p, cat)
		if len(is) != 1 || is[0].Code != "E_DECISION_INVALID" || is[0].Service != "api" {
			t.Fatalf("%q: got %v", bad, is)
		}
	}
	d := full()
	d.Domains["api"] = "api-1.example.com"
	if is := Check(d, p, cat); len(is) != 0 {
		t.Fatalf("got %v", is)
	}
}

func TestCheckDigest(t *testing.T) {
	p, cat := load(t)
	for _, bad := range []string{"sha256:abc", "sha256:" + strings.Repeat("A", 64), "md5:" + strings.Repeat("a", 64), "latest"} {
		d := full()
		d.Digests["api"] = bad
		is := Check(d, p, cat)
		if len(is) != 1 || is[0].Code != "E_DECISION_INVALID" || is[0].Service != "api" {
			t.Fatalf("%q: got %v", bad, is)
		}
	}
}

func TestReadMalformedHidesSecrets(t *testing.T) {
	p, _ := load(t)
	path := filepath.Join(t.TempDir(), "cvctl.env")
	os.WriteFile(path, []byte("CVCTL_SVC_X_PASSWORD=\"hunter2hunter2\n"), 0o640)
	_, err := Read(path, p)
	var e *output.Error
	if err == nil || !asErr(err, &e) || e.Code != "E_DECISION_READ" {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(e.Message, "hunter2") || !strings.Contains(e.Message, "malformed") {
		t.Fatalf("message leaks or lacks marker: %q", e.Message)
	}
}

func TestVolumeDecisionRoundTripAndCheck(t *testing.T) {
	d := &Decisions{Services: map[string]ServiceDecision{"db": {Mode: "local", Volume: "my_vol"}}}
	if got := d.Entries()["CVCTL_SVC_DB_VOLUME"]; got != "my_vol" {
		t.Fatalf("entry = %q", got)
	}
	d2 := &Decisions{Services: map[string]ServiceDecision{"db": {Mode: "local"}}}
	if _, ok := d2.Entries()["CVCTL_SVC_DB_VOLUME"]; ok {
		t.Fatal("empty volume must not be written")
	}
}
