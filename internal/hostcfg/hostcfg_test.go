package hostcfg_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/hostcfg/hostcfgtest"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func code(t *testing.T, err error) string {
	t.Helper()
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want *output.Error, got %v", err)
	}
	if oe.Hint == "" {
		t.Fatalf("error %s has no hint", oe.Code)
	}
	return oe.Code
}

func TestLoadSaveRoundTrip(t *testing.T) {
	fs := systest.NewFS()
	cfg, sec, err := hostcfg.Load(fs, "/etc/cvctl")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"HOST_NAME": "box1", "EDGE_MODE": "behind-proxy", "PROXY_PORT": "8081", "SSH_MANAGE": "false"} {
		if err := cfg.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	sec.Set("telegram_token", "tok-123")
	if err := hostcfg.Save(fs, "/etc/cvctl", cfg, sec); err != nil {
		t.Fatal(err)
	}
	files := fs.Files()
	if fi := files["/etc/cvctl/host.env"]; fi.Mode != 0o640 || fi.User != "root" || fi.Group != "deploy" {
		t.Fatalf("host.env info %+v", fi)
	}
	if fi := files["/etc/cvctl/secrets"]; fi.Mode != 0o750 || !fi.IsDir {
		t.Fatalf("secrets dir info %+v", fi)
	}
	if fi := files["/etc/cvctl/secrets/telegram_token"]; fi.Mode != 0o640 || fi.Group != "deploy" {
		t.Fatalf("secret info %+v", fi)
	}
	if _, ok := files["/etc/cvctl/secrets/s3_secret_key"]; ok {
		t.Fatal("unset secret was written")
	}
	cfg2, sec2, err := hostcfg.Load(fs, "/etc/cvctl")
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.HostName != "box1" || cfg2.EdgeMode != "behind-proxy" || cfg2.ProxyPort != 8081 || cfg2.SSHManage {
		t.Fatalf("typed fields: %+v", cfg2)
	}
	if v, ok := sec2.Get("telegram_token"); !ok || v != "tok-123" {
		t.Fatalf("secret = %q %v", v, ok)
	}
	if _, ok := sec2.Get("s3_access_key"); ok {
		t.Fatal("unset secret reported set")
	}
}

func TestLoadInvalidEnum(t *testing.T) {
	fs := systest.NewFS()
	if err := fs.WriteFile("/etc/cvctl/host.env", []byte("CVCTL_EDGE_MODE=both\n"), 0o640, sys.Owner{}); err != nil {
		t.Fatal(err)
	}
	_, _, err := hostcfg.Load(fs, "/etc/cvctl")
	if code(t, err) != "E_HOSTCFG_INVALID" || !strings.Contains(err.Error(), "CVCTL_EDGE_MODE") {
		t.Fatalf("err = %v", err)
	}
}

func fullCfg(t *testing.T) (*hostcfg.Config, *hostcfg.Secrets) {
	t.Helper()
	cfg, sec, _ := hostcfg.Load(systest.NewFS(), "/x")
	for k, v := range map[string]string{
		"HOST_NAME": "box", "EDGE_MODE": "edge", "ADMIN_USER": "andres", "SSH_MANAGE": "true",
		"FIREWALL_MANAGE": "true", "ACME_EMAIL": "a@b.co", "ACME_CHALLENGE": "http", "STORAGE": "local",
		"NOTIFY": "none", "REGISTRIES": "ghcr.io/codeavers", "REGISTRY_USER": "bot",
	} {
		if err := cfg.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	sec.Set("registry_token", "rt")
	return cfg, sec
}

func TestResolveSkipsExisting(t *testing.T) {
	cfg, sec := fullCfg(t)
	p := hostcfgtest.NewPrompter(t, nil)
	if err := hostcfg.Resolve(cfg, sec, hostcfg.Facts{}, p, true); err != nil {
		t.Fatal(err)
	}
	if len(p.Asked) != 0 {
		t.Fatalf("asked %v", p.Asked)
	}
}

func asked(p *hostcfgtest.Prompter, key string) bool {
	for _, k := range p.Asked {
		if k == key {
			return true
		}
	}
	return false
}

func TestResolveWhen(t *testing.T) {
	cfg, sec := fullCfg(t)
	p := hostcfgtest.NewPrompter(t, nil)
	if err := hostcfg.Resolve(cfg, sec, hostcfg.Facts{}, p, true); err != nil {
		t.Fatal(err)
	}
	if asked(p, "s3_access_key") {
		t.Fatal("asked s3 key with local storage")
	}

	cfg, sec = fullCfg(t)
	_ = cfg.Set("STORAGE", "s3")
	_ = cfg.Set("S3_ENDPOINT", "https://s3.example.com")
	p = hostcfgtest.NewPrompter(t, map[string]string{"S3_BUCKET": "cvctl", "S3_REGION": "auto", "s3_access_key": "ak", "s3_secret_key": "sk"})
	if err := hostcfg.Resolve(cfg, sec, hostcfg.Facts{}, p, true); err != nil {
		t.Fatal(err)
	}
	if !asked(p, "s3_access_key") || !asked(p, "s3_secret_key") {
		t.Fatalf("asked %v", p.Asked)
	}
	if v, _ := sec.Get("s3_secret_key"); v != "sk" {
		t.Fatal("secret not stored")
	}
	if cfg.S3Bucket != "cvctl" || cfg.S3Region != "auto" {
		t.Fatalf("defaults not applied: %q %q", cfg.S3Bucket, cfg.S3Region)
	}
}

func TestResolveNonInteractiveMissing(t *testing.T) {
	cfg, sec := fullCfg(t)
	_ = cfg.Set("NOTIFY", "telegram")
	err := hostcfg.Resolve(cfg, sec, hostcfg.Facts{}, nil, false)
	if code(t, err) != "E_INIT_MISSING" || !strings.Contains(err.Error(), "telegram_token") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "REGISTRY") {
		t.Fatalf("optional key reported missing: %v", err)
	}
}

func TestResolveAdoptDefaults(t *testing.T) {
	cfg, sec, _ := hostcfg.Load(systest.NewFS(), "/x")
	f := hostcfg.Facts{Hostname: "box", SudoUser: "andres", Adopt: true}
	// ACME_EMAIL is not asked in behind-proxy mode, so only optional keys remain.
	if err := hostcfg.Resolve(cfg, sec, f, nil, false); err != nil {
		t.Fatal(err)
	}
	if cfg.EdgeMode != "behind-proxy" || cfg.FirewallManage || cfg.ProxyPort != 8081 || cfg.HostName != "box" || cfg.AdminUser != "andres" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestSecretsNotInHostEnv(t *testing.T) {
	fs := systest.NewFS()
	cfg, sec := fullCfg(t)
	sec.Set("s3_secret_key", "SUPER-SECRET-VALUE")
	if err := hostcfg.Save(fs, "/etc/cvctl", cfg, sec); err != nil {
		t.Fatal(err)
	}
	b, err := fs.ReadFile("/etc/cvctl/host.env")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "SUPER-SECRET-VALUE") || strings.Contains(string(b), "s3_secret_key") {
		t.Fatalf("secret leaked into host.env:\n%s", b)
	}
}

func TestResolveValidatesAnswer(t *testing.T) {
	cfg, sec, _ := hostcfg.Load(systest.NewFS(), "/x")
	p := hostcfgtest.NewPrompter(t, map[string]string{"HOST_NAME": "Bad_Name"})
	err := hostcfg.Resolve(cfg, sec, hostcfg.Facts{}, p, true)
	if code(t, err) != "E_HOSTCFG_INVALID" {
		t.Fatalf("err = %v", err)
	}
}

func TestNewHuhPrompter(t *testing.T) {
	if hostcfg.NewHuhPrompter(strings.NewReader(""), &strings.Builder{}) == nil {
		t.Fatal("nil prompter")
	}
}
