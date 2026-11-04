package upgrade

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/codeavers/cvctl/internal/output"
)

const (
	maxBinary = 100 << 20
	maxAPI    = 4 << 20
	maxSums   = 1 << 20
)

var versionRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(-[0-9A-Za-z.-]+)?$`)

// Release is a published GitHub release. Assets maps asset name to download
// URL; the binary name depends on the architecture, known only to Fetch.
type Release struct {
	Tag, Notes string
	Assets     map[string]string
}

// Client queries the public GitHub API without a token. A nil HTTP uses
// http.DefaultClient; deadlines come from the caller's context.
type Client struct {
	HTTP    *http.Client
	BaseURL string
	Repo    string
}

func (c Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c Client) Latest(ctx context.Context) (Release, error) {
	return c.release(ctx, "releases/latest")
}

func (c Client) ByTag(ctx context.Context, tag string) (Release, error) {
	return c.release(ctx, "releases/tags/"+url.PathEscape(tag))
}

func (c Client) release(ctx context.Context, path string) (Release, error) {
	u := strings.TrimRight(c.BaseURL, "/") + "/repos/" + c.Repo + "/" + path
	body, err := c.get(ctx, u, maxAPI, "E_UPGRADE_RELEASE")
	if err != nil {
		return Release{}, err
	}
	var raw struct {
		Tag    string `json:"tag_name"`
		Body   string `json:"body"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.Tag == "" {
		return Release{}, &output.Error{Code: "E_UPGRADE_RELEASE",
			Message: "respuesta de GitHub no válida",
			Hint:    "reintenta más tarde o indica la versión con --version"}
	}
	r := Release{Tag: raw.Tag, Notes: raw.Body, Assets: map[string]string{}}
	for _, a := range raw.Assets {
		r.Assets[a.Name] = a.URL
	}
	return r, nil
}

// Fetch downloads the binary for arch and verifies it against sha256sums.txt.
// The checksum guards against a corrupt download, not a compromised release.
func (c Client) Fetch(ctx context.Context, r Release, arch string) ([]byte, error) {
	name := "cvctl-linux-" + arch
	binURL, sumsURL := r.Assets[name], r.Assets["sha256sums.txt"]
	if binURL == "" || sumsURL == "" {
		return nil, &output.Error{Code: "E_UPGRADE_ASSET",
			Message: fmt.Sprintf("la release %s no incluye %s y sha256sums.txt", r.Tag, name),
			Hint:    "usa otra versión con --version o comprueba la arquitectura"}
	}
	sums, err := c.get(ctx, sumsURL, maxSums, "E_UPGRADE_DOWNLOAD")
	if err != nil {
		return nil, err
	}
	bin, err := c.get(ctx, binURL, maxBinary, "E_UPGRADE_DOWNLOAD")
	if err != nil {
		return nil, err
	}
	want := checksumFor(sums, name)
	got := sha256.Sum256(bin)
	if want == "" || !strings.EqualFold(want, hex.EncodeToString(got[:])) {
		return nil, &output.Error{Code: "E_UPGRADE_CHECKSUM",
			Message: "el checksum de " + name + " no coincide con sha256sums.txt",
			Hint:    "la descarga está corrupta; reintenta"}
	}
	return bin, nil
}

func checksumFor(sums []byte, name string) string {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return f[0]
		}
	}
	return ""
}

// get reads at most limit bytes; a longer body is an error, not a truncation.
func (c Client) get(ctx context.Context, u string, limit int64, code string) ([]byte, error) {
	fail := func(msg string) error {
		return &output.Error{Code: code, Message: msg, Hint: "comprueba la conexión y reintenta"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fail("URL no válida")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fail("no se pudo contactar con GitHub")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fail("GitHub respondió " + resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fail("falló la lectura de la respuesta")
	}
	if int64(len(b)) > limit {
		return nil, &output.Error{Code: "E_UPGRADE_TOO_LARGE",
			Message: "la descarga supera el tamaño máximo permitido",
			Hint:    "comprueba que la release es la correcta"}
	}
	return b, nil
}

// Newer reports whether b is a newer version than a. Both must be vX.Y.Z with
// an optional prerelease suffix; a prerelease sorts before its release.
func Newer(a, b string) (bool, error) {
	pa, err := parseVersion(a)
	if err != nil {
		return false, err
	}
	pb, err := parseVersion(b)
	if err != nil {
		return false, err
	}
	for i := 0; i < 3; i++ {
		if pa.n[i] != pb.n[i] {
			return pb.n[i] > pa.n[i], nil
		}
	}
	if pa.pre == pb.pre {
		return false, nil
	}
	if pb.pre == "" {
		return true, nil
	}
	if pa.pre == "" {
		return false, nil
	}
	return pb.pre > pa.pre, nil
}

type version struct {
	n   [3]int
	pre string
}

func parseVersion(s string) (version, error) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return version{}, &output.Error{Code: "E_UPGRADE_VERSION",
			Message: fmt.Sprintf("versión %q no válida", s),
			Hint:    "se esperaba vX.Y.Z; un binario de desarrollo no se puede comparar"}
	}
	v := version{pre: strings.TrimPrefix(m[4], "-")}
	for i := 0; i < 3; i++ {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return version{}, &output.Error{Code: "E_UPGRADE_VERSION",
				Message: fmt.Sprintf("versión %q no válida", s), Hint: "se esperaba vX.Y.Z"}
		}
		v.n[i] = n
	}
	return v, nil
}
