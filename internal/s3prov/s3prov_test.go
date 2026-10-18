package s3prov

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/contract"
)

var update = flag.Bool("update", false, "rewrite golden files")

var testBuckets = []contract.Bucket{{Name: "prescriptions"}, {Name: "products", Public: true}}

func TestScriptGolden(t *testing.T) {
	got := Script(testBuckets, "media-app", "appsecret1234567", PolicyName)
	if again := Script(testBuckets, "media-app", "appsecret1234567", PolicyName); again != got {
		t.Fatal("Script is not deterministic")
	}
	path := filepath.Join("testdata", "script.sh")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
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

func TestScriptSortsBucketsAndNeverEmbedsRootKeys(t *testing.T) {
	s := Script([]contract.Bucket{{Name: "zeta"}, {Name: "alpha", Public: true}}, "media-app", "pw", PolicyName)
	if strings.Index(s, "local/alpha") > strings.Index(s, "local/zeta") {
		t.Fatal("buckets not sorted")
	}
	if !strings.Contains(s, `"$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD"`) {
		t.Fatal("root keys must come from the container env")
	}
	if !strings.Contains(s, `"Resource":["arn:aws:s3:::alpha/*"]`) || strings.Contains(s, "arn:aws:s3:::zeta/*\"]}]}\nCVCTL_EOF\nmc anonymous set-json") {
		t.Fatal("anonymous policy must cover only the public bucket objects")
	}
	if !strings.Contains(s, "mc anonymous set none local/zeta") {
		t.Fatal("private bucket must be set to none")
	}
}

func TestScriptQuotesPassword(t *testing.T) {
	s := Script(testBuckets, "media-app", "it's", PolicyName)
	if !strings.Contains(s, `mc admin user add local 'media-app' 'it'\''s'`) {
		t.Fatalf("password not quoted:\n%s", s)
	}
}

func TestOrphans(t *testing.T) {
	out := []byte(`Added user ` + "`media-app`" + ` successfully.
@@buckets
{"status":"success","type":"folder","key":"old/","url":"http://localhost:9000/"}
{"status":"success","type":"folder","key":"products/"}
{"status":"success","type":"folder","key":"prescriptions/"}
{"status":"success","type":"folder","key":"archive/"}
not json
`)
	got := Orphans(out, testBuckets)
	if want := []string{"archive", "old"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := Orphans([]byte("no marker\n"), testBuckets); got != nil {
		t.Fatalf("got %v without marker", got)
	}
}
