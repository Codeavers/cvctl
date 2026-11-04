package upgrade

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/output"
)

func releaseJSON(srvURL, tag string) string {
	return fmt.Sprintf(`{"tag_name":%q,"body":"notas","assets":[
{"name":"cvctl-linux-amd64","browser_download_url":"%s/dl/bin"},
{"name":"sha256sums.txt","browser_download_url":"%s/dl/sums"}]}`, tag, srvURL, srvURL)
}

func newServer(t *testing.T, bin []byte, sums string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases/latest":
			fmt.Fprint(w, releaseJSON(srv.URL, "v0.9.0"))
		case "/repos/o/r/releases/tags/v0.8.2":
			fmt.Fprint(w, releaseJSON(srv.URL, "v0.8.2"))
		case "/dl/bin":
			w.Write(bin)
		case "/dl/sums":
			fmt.Fprint(w, sums)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sumsFor(b []byte) string {
	return fmt.Sprintf("%x  cvctl-linux-amd64\n", sha256.Sum256(b))
}

func code(err error) string {
	var e *output.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestLatestUsesReleasesLatest(t *testing.T) {
	srv := newServer(t, nil, "")
	r, err := Client{BaseURL: srv.URL, Repo: "o/r"}.Latest(context.Background())
	if err != nil || r.Tag != "v0.9.0" || r.Notes != "notas" {
		t.Fatalf("got %+v, %v", r, err)
	}
}

func TestByTagUsesTagsPath(t *testing.T) {
	srv := newServer(t, nil, "")
	c := Client{BaseURL: srv.URL, Repo: "o/r"}
	r, err := c.ByTag(context.Background(), "v0.8.2")
	if err != nil || r.Tag != "v0.8.2" {
		t.Fatalf("got %+v, %v", r, err)
	}
	if _, err := c.ByTag(context.Background(), "v9.9.9"); code(err) != "E_UPGRADE_RELEASE" {
		t.Fatalf("missing tag: %v", err)
	}
}

func TestFetch(t *testing.T) {
	bin := []byte("binary-content")
	cases := []struct {
		name string
		bin  []byte
		sums string
		arch string
		want string
	}{
		{"ok", bin, sumsFor(bin), "amd64", ""},
		{"bad checksum", bin, sumsFor([]byte("other")), "amd64", "E_UPGRADE_CHECKSUM"},
		{"no sums entry", bin, "abc  other\n", "amd64", "E_UPGRADE_CHECKSUM"},
		{"missing asset", bin, sumsFor(bin), "arm64", "E_UPGRADE_ASSET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServer(t, tc.bin, tc.sums)
			c := Client{BaseURL: srv.URL, Repo: "o/r"}
			r, err := c.Latest(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.Fetch(context.Background(), r, tc.arch)
			if code(err) != tc.want {
				t.Fatalf("code %q want %q (%v)", code(err), tc.want, err)
			}
			if tc.want == "" && string(got) != string(bin) {
				t.Fatalf("got %q", got)
			}
		})
	}
}

func TestFetchTooLarge(t *testing.T) {
	big := []byte(strings.Repeat("x", maxBinary+1))
	srv := newServer(t, big, sumsFor(big))
	c := Client{BaseURL: srv.URL, Repo: "o/r"}
	r, _ := c.Latest(context.Background())
	if _, err := c.Fetch(context.Background(), r, "amd64"); code(err) != "E_UPGRADE_TOO_LARGE" {
		t.Fatalf("got %v", err)
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b    string
		want    bool
		wantErr bool
	}{
		{"v0.8.2", "v0.8.10", true, false},
		{"v0.8.10", "v0.8.2", false, false},
		{"v0.8.2", "v0.8.2", false, false},
		{"v1.0.0-rc1", "v1.0.0", true, false},
		{"v1.0.0", "v1.0.0-rc1", false, false},
		{"dev", "v0.8.2", false, true},
		{"v0.8.2", "latest", false, true},
	}
	for _, tc := range cases {
		got, err := Newer(tc.a, tc.b)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("Newer(%q,%q)=%v,%v", tc.a, tc.b, got, err)
		}
	}
}
