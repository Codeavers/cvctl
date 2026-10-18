package s3prov

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupUserScriptGolden(t *testing.T) {
	got := BackupUserScript("bk-media", "s3cr3tvalue", "cvctl", "vps1/media", "cvctl-bk-media")
	if again := BackupUserScript("bk-media", "s3cr3tvalue", "cvctl", "vps1/media", "cvctl-bk-media"); again != got {
		t.Fatal("BackupUserScript is not deterministic")
	}
	path := filepath.Join("testdata", "backupuser.sh")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("script mismatch:\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func TestBackupUserScriptScopesPolicyAndKeepsSecretsOutOfArgv(t *testing.T) {
	s := BackupUserScript("bk-media", "it's", "cvctl", "/vps1/media/", "cvctl-bk-media")
	for _, want := range []string{
		`"Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::cvctl/vps1/media/*"]`,
		`"Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::cvctl"],"Condition":{"StringLike":{"s3:prefix":["vps1/media/*","vps1/media"]}}`,
		`printf '%s\n' 'it'\''s' | mc admin user add local 'bk-media'`,
		`export MC_HOST_local="http://${MINIO_ROOT_USER}:${MINIO_ROOT_PASSWORD}@localhost:9000"`,
		"mc admin policy attach local 'cvctl-bk-media' --user 'bk-media'",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "mc ") && (strings.Contains(line, "MINIO_ROOT") || strings.Contains(line, "it'")) {
			t.Fatalf("secret in mc argv: %s", line)
		}
	}
	if strings.Contains(s, "s3:*") || strings.Contains(s, `"arn:aws:s3:::cvctl/*"`) {
		t.Fatal("policy must not grant the whole bucket")
	}
}
