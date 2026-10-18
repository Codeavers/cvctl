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
			"main-db": {Mode: "local", Version: "16", BackupGen: 3, BackupGenMax: 3, BackupKeyGen: 2, Creds: kinds.Credentials{User: "u", Password: "p\"w $x #y \\z\nline2 'q'", DB: "d"}},
			"cache":   {Mode: "external", External: map[string]string{"url": "redis://h:6379/0?a=b#c"}},
			"media":   {Mode: "external", External: map[string]string{"endpoint": "https://s3", "bucket": "b", "access_key": "ak", "secret_key": "sk", "region": "r"}},
		},
		Domains: map[string]string{"api": "api.example.com"},
		Digests: map[string]string{"api": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Backup: BackupDecision{
			AccessKey: "bk-proj-dev", SecretKey: "s3cr3t", LibsodiumKey: "abcd", ResticPassword: "pw",
			Ext: map[string]string{"bucket": "own", "access_key": "eak", "secret_key": "esk", "endpoint": "https://r2", "region": "auto"},
		},
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
		{"double hyphen", "proj--dev"},
	} {
		d := full()
		d.Instance = tc.inst
		is := Check(d, p, cat)
		if len(is) != 1 || is[0].Code != "E_INSTANCE_NAME" || is[0].Hint == "" {
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

func TestCheckDir(t *testing.T) {
	d := &Decisions{Instance: "demo-dev"}
	// Matching directory
	if err := CheckDir(d, "/srv/deploy/demo-dev/cvctl.env"); err != nil {
		t.Fatalf("expected nil for matching directory, got %v", err)
	}
	// Mismatching directory
	err := CheckDir(d, "/srv/deploy/other-dev/cvctl.env")
	if err == nil {
		t.Fatal("expected error for mismatching directory, got nil")
	}
	if err.Code != "E_INSTANCE_MISMATCH" {
		t.Fatalf("expected E_INSTANCE_MISMATCH, got %s: %v", err.Code, err)
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

func TestVolumeDecisionEntries(t *testing.T) {
	d := &Decisions{Services: map[string]ServiceDecision{"db": {Mode: "local", Volume: "my_vol"}}}
	if got := d.Entries()["CVCTL_SVC_DB_VOLUME"]; got != "my_vol" {
		t.Fatalf("entry = %q", got)
	}
	d2 := &Decisions{Services: map[string]ServiceDecision{"db": {Mode: "local"}}}
	if _, ok := d2.Entries()["CVCTL_SVC_DB_VOLUME"]; ok {
		t.Fatal("empty volume must not be written")
	}
}

func TestVolumeReadAndCheck(t *testing.T) {
	p, cat := load(t)
	d := full()
	sd := d.Services["main-db"]
	sd.Volume = "restored_vol-1"
	d.Services["main-db"] = sd
	path := filepath.Join(t.TempDir(), "cvctl.env")
	if err := Write(path, d); err != nil {
		t.Fatal(err)
	}
	back, err := Read(path, p)
	if err != nil {
		t.Fatal(err)
	}
	if back.Services["main-db"].Volume != "restored_vol-1" {
		t.Fatalf("volume lost: %+v", back.Services["main-db"])
	}
	if is := Check(back, p, cat); len(is) != 0 {
		t.Fatalf("valid volume rejected: %v", is)
	}
	sd.Volume = "bad name/../x"
	d.Services["main-db"] = sd
	got := codes(Check(d, p, cat))
	if !reflect.DeepEqual(got, []string{"E_DECISION_INVALID:main-db"}) {
		t.Fatalf("got %v", got)
	}
}

func TestBackupGenerationKeysRoundTrip(t *testing.T) {
	p := &contract.Project{Data: []contract.DataService{{Name: "db", Kind: "postgres"}}}
	path := filepath.Join(t.TempDir(), "cvctl.env")
	d := &Decisions{Instance: "a-dev", Services: map[string]ServiceDecision{"db": {Mode: "local", BackupGen: 2, BackupGenMax: 5}}}
	if err := Write(path, d); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path, p)
	if err != nil {
		t.Fatal(err)
	}
	if s := got.Services["db"]; s.BackupGen != 2 || s.BackupGenMax != 5 {
		t.Fatalf("got %+v", s)
	}
	d.Services["db"] = ServiceDecision{Mode: "local"}
	if m := d.Entries(); m["CVCTL_SVC_DB_BACKUP_GEN"] != "" || m["CVCTL_SVC_DB_BACKUP_GEN_MAX"] != "" {
		t.Fatalf("zero generation must stay absent: %v", m)
	}
}

func loadWithBuckets(t *testing.T) (*contract.Project, kinds.Catalog) {
	t.Helper()
	p, cat := load(t)
	p.X.S3 = map[string][]contract.Bucket{
		"media": {
			{Name: "photos-raw", Public: true},
			{Name: "cache", Public: false},
		},
	}
	for i := range p.Data {
		if p.Data[i].Name == "media" {
			p.Data[i].ExposePort = 9000
		}
	}
	return p, cat
}

func TestAppCredentialsAndBucketExternalRoundTrip(t *testing.T) {
	p, _ := loadWithBuckets(t)
	path := filepath.Join(t.TempDir(), "cvctl.env")
	d := full()
	d.Services["media"] = ServiceDecision{
		Mode: "external",
		Creds: kinds.Credentials{
			User: "root-u", Password: "root-p", DB: "root-db",
			AppUser: "media-app", AppPassword: "app-secret-pw",
		},
		External: map[string]string{
			"endpoint":              "https://s3.example.com",
			"access_key":            "ak-val",
			"secret_key":            "sk-val",
			"region":                "us-east-1",
			"bucket_photos-raw":     "my-bucket-raw",
			"public_url_photos-raw": "https://cdn.example.com/raw",
			"bucket_cache":          "my-bucket-cache",
		},
	}
	d.Domains["media"] = "s3.example.com"
	if err := Write(path, d); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, want := range []string{
		"CVCTL_SVC_MEDIA_APP_USER=media-app",
		"CVCTL_SVC_MEDIA_APP_PASSWORD=app-secret-pw",
		"CVCTL_SVC_MEDIA_EXT_BUCKET_PHOTOS_RAW=my-bucket-raw",
		"CVCTL_SVC_MEDIA_EXT_PUBLIC_URL_PHOTOS_RAW=https://cdn.example.com/raw",
		"CVCTL_SVC_MEDIA_EXT_BUCKET_CACHE=my-bucket-cache",
		"CVCTL_DOMAIN_MEDIA=s3.example.com",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("file missing %q:\n%s", want, content)
		}
	}

	got, err := Read(path, p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, d) {
		t.Fatalf("mismatch\n got %#v\nwant %#v", got, d)
	}
}

func TestCheckLocalAppCredentials(t *testing.T) {
	p, cat := load(t)
	d := full()
	d.Services["media"] = ServiceDecision{
		Mode: "local",
		Creds: kinds.Credentials{
			User: "media_root", Password: "root-password", DB: "mediadb",
		},
	}
	// Missing app creds
	is := Check(d, p, cat)
	missing := issueMsgs(is, "E_DECISION_MISSING")
	var mediaMissing []output.Issue
	for _, i := range missing {
		if i.Service == "media" {
			mediaMissing = append(mediaMissing, i)
		}
	}
	if len(mediaMissing) != 1 || !strings.Contains(mediaMissing[0].Hint, "cvctl project update") {
		t.Fatalf("expected 1 missing app creds issue with hint, got: %v", mediaMissing)
	}

	// Invalid AppUser
	s := d.Services["media"]
	s.Creds.AppUser = "media.app"
	s.Creds.AppPassword = "pw"
	d.Services["media"] = s
	invalid := issueMsgs(Check(d, p, cat), "E_DECISION_INVALID")
	var mediaInvalid []output.Issue
	for _, i := range invalid {
		if i.Service == "media" {
			mediaInvalid = append(mediaInvalid, i)
		}
	}
	if len(mediaInvalid) != 1 || !strings.Contains(mediaInvalid[0].Message, "app_user") {
		t.Fatalf("expected invalid app_user issue, got: %v", mediaInvalid)
	}

	// Valid AppUser with hyphen
	s.Creds.AppUser = "media-app"
	d.Services["media"] = s
	if is := Check(d, p, cat); len(is) != 0 {
		t.Fatalf("unexpected issues: %v", is)
	}

	// Without declared buckets the DB name is the bucket: it must be a valid one.
	for _, db := range []string{"media_db", "s3", "Media"} {
		s.Creds.DB = db
		d.Services["media"] = s
		bad := issueMsgs(Check(d, p, cat), "E_DECISION_INVALID")
		if len(bad) != 1 || !strings.Contains(bad[0].Message, "bucket name") {
			t.Fatalf("DB %q: expected one bucket-name issue, got %v", db, bad)
		}
	}
}

func TestCheckExposedBucketServiceDomain(t *testing.T) {
	p, cat := loadWithBuckets(t)
	d := full()
	d.Services["media"] = ServiceDecision{
		Mode: "local",
		Creds: kinds.Credentials{
			User: "media_root", Password: "root-password", DB: "media_db",
			AppUser: "media-app", AppPassword: "app-password",
		},
	}
	// Missing domain
	delete(d.Domains, "media")
	is := Check(d, p, cat)
	missing := issueMsgs(is, "E_DECISION_MISSING")
	var mediaDomain []output.Issue
	for _, i := range missing {
		if i.Service == "media" {
			mediaDomain = append(mediaDomain, i)
		}
	}
	if len(mediaDomain) != 1 || !strings.Contains(mediaDomain[0].Hint, "CVCTL_DOMAIN_MEDIA") {
		t.Fatalf("expected missing domain issue, got: %v", mediaDomain)
	}

	// Invalid hostname
	d.Domains["media"] = "media..example.com"
	is = Check(d, p, cat)
	inv := issueMsgs(is, "E_DECISION_INVALID")
	var mediaInv []output.Issue
	for _, i := range inv {
		if i.Service == "media" {
			mediaInv = append(mediaInv, i)
		}
	}
	if len(mediaInv) != 1 || !strings.Contains(mediaInv[0].Message, "domain") {
		t.Fatalf("expected invalid domain issue, got: %v", mediaInv)
	}

	// Valid domain
	d.Domains["media"] = "s3.example.com"
	if is := Check(d, p, cat); len(is) != 0 {
		t.Fatalf("unexpected issues: %v", is)
	}

	// External mode does not require domain for media
	d.Services["media"] = ServiceDecision{
		Mode: "external",
		External: map[string]string{
			"endpoint":              "https://s3.example.com",
			"access_key":            "ak",
			"secret_key":            "sk",
			"region":                "us-east-1",
			"bucket_photos-raw":     "b1",
			"public_url_photos-raw": "https://cdn.example.com",
			"bucket_cache":          "b2",
		},
	}
	delete(d.Domains, "media")
	if is := Check(d, p, cat); len(is) != 0 {
		t.Fatalf("unexpected issues in external mode without domain: %v", is)
	}
}

func TestCheckExternalDeclaredBuckets(t *testing.T) {
	p, cat := loadWithBuckets(t)
	d := full()
	d.Services["media"] = ServiceDecision{
		Mode:     "external",
		External: map[string]string{},
	}
	is := Check(d, p, cat)
	missing := issueMsgs(is, "E_DECISION_MISSING")
	var mediaMissing []string
	for _, i := range missing {
		if i.Service == "media" {
			mediaMissing = append(mediaMissing, i.Hint)
		}
	}
	joined := strings.Join(mediaMissing, " ")
	for _, want := range []string{
		"EXT_ENDPOINT", "EXT_ACCESS_KEY", "EXT_SECRET_KEY", "EXT_REGION",
		"EXT_BUCKET_PHOTOS_RAW", "EXT_PUBLIC_URL_PHOTOS_RAW", "EXT_BUCKET_CACHE",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing required field %s in hints: %v", want, mediaMissing)
		}
	}
	if strings.Contains(joined, "EXT_BUCKET ") || strings.HasSuffix(joined, "EXT_BUCKET") {
		t.Errorf("plain bucket should not be required when buckets are declared: %v", mediaMissing)
	}
	if strings.Contains(joined, "EXT_PUBLIC_URL_CACHE") {
		t.Errorf("private bucket should not require public_url: %v", mediaMissing)
	}

	// URL validation on public_url_*
	d.Services["media"] = ServiceDecision{
		Mode: "external",
		External: map[string]string{
			"endpoint":              "https://s3.example.com",
			"access_key":            "ak",
			"secret_key":            "sk",
			"region":                "us-east-1",
			"bucket_photos-raw":     "b1",
			"public_url_photos-raw": "https://cdn.example.com",
			"bucket_cache":          "b2",
		},
	}
	if is := Check(d, p, cat); len(is) != 0 {
		t.Fatalf("valid external buckets failed check: %v", is)
	}

	for _, badURL := range []string{
		"https://cdn.example.com/",
		"cdn.example.com",
		"ftp://cdn.example.com",
		"https:///",
		"https://cdn.example.com/p?x=1",
		"https://cdn.example.com/p#frag",
		"https://user@cdn.example.com/p",
		"/relative/path",
		"http://",
	} {
		d.Services["media"].External["public_url_photos-raw"] = badURL
		is := Check(d, p, cat)
		inv := issueMsgs(is, "E_DECISION_INVALID")
		var urlInv []output.Issue
		for _, i := range inv {
			if i.Service == "media" && strings.Contains(i.Message, "public_url_photos-raw") {
				urlInv = append(urlInv, i)
			}
		}
		if len(urlInv) != 1 {
			t.Errorf("bad url %q: expected 1 invalid issue, got %v", badURL, is)
		}
	}
}

func TestCheckProdPublicLocalRule(t *testing.T) {
	p, cat := loadWithBuckets(t)
	d := full()
	d.Env = "prod"
	d.Instance = "proj-prod"
	d.Domains["media"] = "s3.example.com"
	d.Services["media"] = ServiceDecision{
		Mode: "local",
		Creds: kinds.Credentials{
			User: "media_root", Password: "root-password", DB: "media_db",
			AppUser: "media-app", AppPassword: "app-password",
		},
	}

	// Prod with public bucket in local mode: fails with E_S3_PUBLIC_LOCAL_PROD
	is := Check(d, p, cat)
	prodIssues := issueMsgs(is, "E_S3_PUBLIC_LOCAL_PROD")
	if len(prodIssues) != 1 || prodIssues[0].Service != "media" {
		t.Fatalf("expected 1 E_S3_PUBLIC_LOCAL_PROD issue, got: %v", is)
	}
	if !strings.Contains(prodIssues[0].Hint, "cvctl project update proj-prod --ext media.endpoint=") {
		t.Errorf("unexpected hint: %q", prodIssues[0].Hint)
	}

	// Dev with public bucket in local mode: clean
	d.Env = "dev"
	d.Instance = "proj-dev"
	if is := Check(d, p, cat); len(is) != 0 {
		t.Fatalf("dev with public bucket in local mode should pass: %v", is)
	}

	// Prod with only private buckets in local mode: clean
	d.Env = "prod"
	d.Instance = "proj-prod"
	p.X.S3["media"] = []contract.Bucket{{Name: "cache", Public: false}}
	if is := Check(d, p, cat); len(is) != 0 {
		t.Fatalf("prod with only private buckets should pass: %v", is)
	}
}

func TestBackupKeysAreStoredUnderTheirKeys(t *testing.T) {
	m := full().Entries()
	for k, want := range map[string]string{
		"CVCTL_BACKUP_ACCESS_KEY": "bk-proj-dev", "CVCTL_BACKUP_SECRET_KEY": "s3cr3t",
		"CVCTL_BACKUP_LIBSODIUM_KEY": "abcd", "CVCTL_BACKUP_RESTIC_PASSWORD": "pw",
		"CVCTL_BACKUP_EXT_ACCESS_KEY": "eak", "CVCTL_BACKUP_EXT_BUCKET": "own",
		"CVCTL_SVC_MAIN_DB_BACKUP_KEY_GEN": "2",
	} {
		if m[k] != want {
			t.Errorf("%s = %q, want %q", k, m[k], want)
		}
	}
}

func TestInfraSourceRoundTrip(t *testing.T) {
	p, _ := load(t)
	path := filepath.Join(t.TempDir(), "cvctl.env")
	d := full()
	d.InfraSource, d.InfraRef = SourceOCI, "ghcr.io/org/proj-infra"
	if err := Write(path, d); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "CVCTL_INFRA_SOURCE=oci\n") || !strings.Contains(string(raw), "CVCTL_INFRA_REF=ghcr.io/org/proj-infra\n") {
		t.Fatalf("cvctl.env:\n%s", raw)
	}
	got, err := Read(path, p)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsOCI() || got.InfraRef != d.InfraRef {
		t.Fatalf("read back %q %q", got.InfraSource, got.InfraRef)
	}

	g := full()
	g.InfraSource = SourceGit
	if _, ok := g.Entries()["CVCTL_INFRA_SOURCE"]; ok {
		t.Fatal("git is the default and must not be written")
	}
}

func TestCheckInfraSource(t *testing.T) {
	p, cat := load(t)
	d := full()
	d.InfraSource = SourceOCI
	if !hasCode(Check(d, p, cat), "E_DECISION_MISSING") {
		t.Fatal("oci without CVCTL_INFRA_REF must be rejected")
	}
	d.InfraSource = "svn"
	if !hasCode(Check(d, p, cat), "E_DECISION_INVALID") {
		t.Fatal("unknown infra source must be rejected")
	}
}

func hasCode(is []output.Issue, code string) bool {
	for _, i := range is {
		if i.Code == code {
			return true
		}
	}
	return false
}
