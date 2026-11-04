package notify

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

const repo = "https://github.com/acme/app"

func okMsg(d *Deploy) Message {
	return Message{Host: "demo-server", Project: "demo-dev", Level: "info", Status: "OK, deployed in 42s", Deploy: d}
}

func b(svc, pr string) Build {
	return Build{Service: svc, Repo: repo, Commit: "a1b2c3d4e5f6", PR: pr, Title: "Fix login redirect", Author: "ana"}
}

func TestRenderHTML(t *testing.T) {
	tests := []struct {
		name    string
		m       Message
		want    string
		buttons []Button
	}{
		{
			name: "one image with labels",
			m:    okMsg(&Deploy{Changed: []string{"api"}, Builds: []Build{b("api", "123")}}),
			want: "<b>Server:</b> demo-server\n<b>Project:</b> demo-dev\n<b>Status:</b> OK, deployed in 42s\n" +
				`<b>PR:</b> <a href="https://github.com/acme/app/pull/123">#123 Fix login redirect</a>` + "\n" +
				`<b>Commit:</b> <a href="https://github.com/acme/app/commit/a1b2c3d4e5f6">a1b2c3d</a> by ana` + "\n" +
				"<b>Images:</b> api",
			buttons: []Button{
				{"PR", "https://github.com/acme/app/pull/123"},
				{"Commit", "https://github.com/acme/app/commit/a1b2c3d4e5f6"},
			},
		},
		{
			name: "two services same PR and commit",
			m:    okMsg(&Deploy{Changed: []string{"api", "web"}, Builds: []Build{b("api", "123"), b("web", "123")}}),
			want: "<b>Server:</b> demo-server\n<b>Project:</b> demo-dev\n<b>Status:</b> OK, deployed in 42s\n" +
				`<b>PR:</b> <a href="https://github.com/acme/app/pull/123">#123 Fix login redirect</a>` + "\n" +
				`<b>Commit:</b> <a href="https://github.com/acme/app/commit/a1b2c3d4e5f6">a1b2c3d</a> by ana` + "\n" +
				"<b>Images:</b> api, web",
			buttons: []Button{
				{"PR", "https://github.com/acme/app/pull/123"},
				{"Commit", "https://github.com/acme/app/commit/a1b2c3d4e5f6"},
			},
		},
		{
			name: "different PRs",
			m:    okMsg(&Deploy{Changed: []string{"api", "web"}, Builds: []Build{b("api", "1")}}),
			want: "<b>Server:</b> demo-server\n<b>Project:</b> demo-dev\n<b>Status:</b> OK, deployed in 42s\n" +
				`<b>api:</b> <a href="https://github.com/acme/app/pull/1">#1 Fix login redirect</a> (<a href="https://github.com/acme/app/commit/a1b2c3d4e5f6">a1b2c3d</a>, ana)` + "\n" +
				"<b>web:</b> no build info\n" +
				"<b>Images:</b> api, web",
		},
		{
			name: "no labels",
			m:    okMsg(&Deploy{Changed: []string{"api"}}),
			want: "<b>Server:</b> demo-server\n<b>Project:</b> demo-dev\n<b>Status:</b> OK, deployed in 42s\n<b>Images:</b> api",
		},
		{
			name: "no PR label",
			m: okMsg(&Deploy{Changed: []string{"api"}, Builds: []Build{
				{Service: "api", Repo: repo, Commit: "a1b2c3d4e5f6", Author: "ana"}}}),
			want: "<b>Server:</b> demo-server\n<b>Project:</b> demo-dev\n<b>Status:</b> OK, deployed in 42s\n" +
				`<b>Commit:</b> <a href="https://github.com/acme/app/commit/a1b2c3d4e5f6">a1b2c3d</a> by ana` + "\n" +
				"<b>Images:</b> api",
			buttons: []Button{{"Commit", "https://github.com/acme/app/commit/a1b2c3d4e5f6"}},
		},
		{
			name: "no repo: values without link",
			m: okMsg(&Deploy{Changed: []string{"api"}, Builds: []Build{
				{Service: "api", Commit: "a1b2c3d4e5f6", PR: "7", Title: "T", Author: "ana"}}}),
			want: "<b>Server:</b> demo-server\n<b>Project:</b> demo-dev\n<b>Status:</b> OK, deployed in 42s\n" +
				"<b>PR:</b> #7 T\n<b>Commit:</b> a1b2c3d by ana\n<b>Images:</b> api",
		},
		{
			name: "unchanged and one button per app",
			m: okMsg(&Deploy{Links: []Link{
				{"api", "https://api.example.com"}, {"bad", "http://bad.example.com"}, {"web", "https://web.example.com"}}}),
			want:    "<b>Server:</b> demo-server\n<b>Project:</b> demo-dev\n<b>Status:</b> OK, deployed in 42s\n<b>Images:</b> unchanged",
			buttons: []Button{{"Open api", "https://api.example.com"}, {"Open web", "https://web.example.com"}},
		},
		{
			name: "failure",
			m: Message{Host: "demo-server", Project: "demo-dev", Level: "error", Status: "FAILED, will retry",
				Text:   "E_COMPOSE_FAILED: pull failed\nImage x Pulling\nnot found: will retry next cycle",
				Deploy: &Deploy{Changed: []string{"api"}, Builds: []Build{b("api", "123")}, Logs: "sudo cvctl history demo-dev"}},
			want: "<b>Server:</b> demo-server\n<b>Project:</b> demo-dev\n<b>Status:</b> FAILED, will retry\n" +
				`<b>PR:</b> <a href="https://github.com/acme/app/pull/123">#123 Fix login redirect</a>` + "\n" +
				`<b>Commit:</b> <a href="https://github.com/acme/app/commit/a1b2c3d4e5f6">a1b2c3d</a> by ana` + "\n" +
				"<b>Images:</b> api\n" +
				"<b>Error:</b> <code>E_COMPOSE_FAILED: not found</code>\n" +
				"<b>Logs:</b> sudo cvctl history demo-dev",
			buttons: []Button{
				{"PR", "https://github.com/acme/app/pull/123"},
				{"Commit", "https://github.com/acme/app/commit/a1b2c3d4e5f6"},
			},
		},
		{
			name: "no deploy",
			m:    Message{Host: "h1", Level: "warn", Title: "Backup <failed>", Text: "disk & more"},
			want: "<b>Server:</b> h1\n<b>Status:</b> Backup &lt;failed&gt;\n\ndisk &amp; more",
		},
		{
			name: "host from argument, project omitted",
			m:    Message{Level: "info", Title: "t"},
			want: "<b>Server:</b> argh\n<b>Status:</b> t",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, btns := renderHTML(tc.m, "argh")
			if got != tc.want {
				t.Errorf("html mismatch\ngot:\n%s\nwant:\n%s", got, tc.want)
			}
			if len(btns) != len(tc.buttons) {
				t.Fatalf("buttons = %v, want %v", btns, tc.buttons)
			}
			for i := range btns {
				if btns[i] != tc.buttons[i] {
					t.Errorf("button %d = %v, want %v", i, btns[i], tc.buttons[i])
				}
			}
		})
	}
}

func TestRenderHTMLUntrustedLabels(t *testing.T) {
	cases := map[string]Build{
		"escapes title and author": {Service: "api", Repo: repo, Commit: "a1b2c3d", PR: "1", Title: `<script>"x"&`, Author: "<b>"},
		"http repo":                {Service: "api", Repo: "http://github.com/acme/app", Commit: "a1b2c3d", PR: "1", Title: "t"},
		"repo with query":          {Service: "api", Repo: repo + "?x=1", Commit: "a1b2c3d", PR: "1", Title: "t"},
		"repo with fragment":       {Service: "api", Repo: repo + "#x", Commit: "a1b2c3d", PR: "1", Title: "t"},
		"quote in repo":            {Service: "api", Repo: `https://github.com/a"onclick="x`, Commit: "a1b2c3d", PR: "1", Title: "t"},
		"non hex commit":           {Service: "api", Repo: repo, Commit: "zzzzzzz", PR: "1", Title: "t"},
		"non numeric PR":           {Service: "api", Repo: repo, Commit: "a1b2c3d", PR: "1/../x", Title: "t"},
	}
	for name, bd := range cases {
		t.Run(name, func(t *testing.T) {
			got, btns := renderHTML(okMsg(&Deploy{Changed: []string{"api"}, Builds: []Build{bd}}), "h")
			for _, bad := range []string{"<script>", `onclick="`, `"x"`, "<b>by"} {
				if strings.Contains(got, bad) {
					t.Errorf("output contains %q:\n%s", bad, got)
				}
			}
			if name == "escapes title and author" {
				if !strings.Contains(got, "&lt;script&gt;&#34;x&#34;&amp;") {
					t.Errorf("title not escaped:\n%s", got)
				}
				return
			}
			if strings.Contains(got, "<a href") && name != "non hex commit" && name != "non numeric PR" {
				t.Errorf("unexpected link:\n%s", got)
			}
			for _, bt := range btns {
				if bt.Text != "PR" && bt.Text != "Commit" {
					continue
				}
				if strings.ContainsAny(bt.URL, `?#"`) || !strings.HasPrefix(bt.URL, "https://") {
					t.Errorf("unsafe button %v", bt)
				}
			}
			if name == "non hex commit" && strings.Contains(got, "zzzzzzz") {
				t.Errorf("invalid commit shown:\n%s", got)
			}
			if name == "non numeric PR" && strings.Contains(got, "pull/") {
				t.Errorf("invalid PR linked:\n%s", got)
			}
		})
	}
}

func TestRenderHTMLLimits(t *testing.T) {
	long := strings.Repeat("<", 5000)
	got, _ := renderHTML(Message{Level: "warn", Title: "t", Text: long}, "h")
	// 1000 runes of "<" escape to "&lt;", never cut mid entity.
	if !strings.HasSuffix(got, "&lt;") && !strings.HasSuffix(got, "…") {
		t.Errorf("cut inside an entity: %q", got[len(got)-10:])
	}
	if strings.Count(got, "&lt;") > 1000 {
		t.Errorf("text not limited to 1000 runes")
	}
}

func TestRenderPlain(t *testing.T) {
	tests := []struct {
		name string
		m    Message
		want string
	}{
		{
			name: "one image",
			m:    okMsg(&Deploy{Changed: []string{"api"}, Builds: []Build{b("api", "123")}, Links: []Link{{"api", "https://app.example.com"}}}),
			want: "Server: demo-server\nProject: demo-dev\nStatus: OK, deployed in 42s\n" +
				"PR: #123 Fix login redirect\nCommit: a1b2c3d by ana\nImages: api",
		},
		{
			name: "different PRs",
			m:    okMsg(&Deploy{Changed: []string{"api", "web"}, Builds: []Build{b("api", "1")}}),
			want: "Server: demo-server\nProject: demo-dev\nStatus: OK, deployed in 42s\n" +
				"api: #1 Fix login redirect (a1b2c3d, ana)\nweb: no build info\nImages: api, web",
		},
		{
			name: "failure",
			m: Message{Host: "demo-server", Project: "demo-dev", Level: "error", Status: "FAILED",
				Text:   "E_COMPOSE_FAILED: x\nnot found",
				Deploy: &Deploy{Logs: "sudo cvctl history demo-dev"}},
			want: "Server: demo-server\nProject: demo-dev\nStatus: FAILED\nImages: unchanged\n" +
				"Error: E_COMPOSE_FAILED: not found\nLogs: sudo cvctl history demo-dev",
		},
		{
			name: "no deploy",
			m:    Message{Host: "h1", Level: "warn", Title: "Backup <failed>", Text: "disk & more"},
			want: "Server: h1\nStatus: Backup <failed>\n\ndisk & more",
		},
		{
			name: "info never shows Error",
			m:    Message{Host: "h", Level: "info", Status: "OK", Text: "E_X: boom", Deploy: &Deploy{}},
			want: "Server: h\nStatus: OK\nImages: unchanged",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderPlain(tc.m, "argh"); got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

func TestRenderPlainTotalLimit(t *testing.T) {
	var svcs []string
	var builds []Build
	for i := 0; i < 200; i++ {
		s := strings.Repeat("s", 40) + string(rune('a'+i%26)) + strings.Repeat("x", i)
		svcs = append(svcs, s)
		builds = append(builds, Build{Service: s, PR: "1", Title: strings.Repeat("t", 100)})
	}
	got := renderPlain(okMsg(&Deploy{Changed: svcs, Builds: builds}), "h")
	if n := len([]rune(got)); n > 4000 {
		t.Errorf("len = %d, want <= 4000", n)
	}
}

func TestSummary(t *testing.T) {
	failure := "E_COMPOSE_FAILED: docker compose -p demo-dev --env-file /x/.env pull api dashboard landing failed (exit 1): Image ghcr.io/x/api Pulling\n" +
		" Image ghcr.io/x/landing Pulling\n\n" +
		`Error response from daemon: failed to resolve reference "ghcr.io/x/landing@sha256:e754": ghcr.io/x/landing@sha256:e754: not found: will retry next cycle` + "\n"
	tests := []struct{ name, in, want string }{
		{"compose failure", failure,
			`E_COMPOSE_FAILED: Error response from daemon: failed to resolve reference "ghcr.io/x/landing@sha256:e754": ghcr.io/x/landing@sha256:e754: not found`},
		{"no code", "first\nlast line: will retry next cycle", "last line"},
		{"single line with code", "E_X: boom", "E_X: boom"},
		{"single line with code and retry", "E_X: boom: will retry next cycle", "E_X: boom"},
		{"trailing blanks", "E_X: a\nb\n\n  \n", "E_X: b"},
		{"empty", "", ""},
		{"blank", " \n\n", ""},
		{"bare retry line", "E_X: boom\n: will retry next cycle", "E_X: boom"},
		{"bare retry line after a blank", "real cause\n\n: will retry next cycle\n", "real cause"},
		{"only the retry suffix", ": will retry next cycle", ""},
		{"limit", "E_X: a\n" + strings.Repeat("é", 500), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Summary(tc.in)
			if tc.name == "limit" {
				if n := len([]rune(got)); n != 300 || !strings.HasPrefix(got, "E_X: é") {
					t.Errorf("limit: %d runes, %q", n, got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderHTMLEscapesServiceLabel(t *testing.T) {
	got, _ := renderHTML(okMsg(&Deploy{Changed: []string{"<i>a", "b"}, Builds: []Build{b("<i>a", "1")}}), "h")
	if strings.Contains(got, "<i>") {
		t.Errorf("service name not escaped:\n%s", got)
	}
}

func TestRenderHTMLLongTitleCutInRunes(t *testing.T) {
	bd := b("api", "1")
	bd.Title = strings.Repeat("ñ<&é世", 100)
	got, _ := renderHTML(okMsg(&Deploy{Changed: []string{"api"}, Builds: []Build{bd}}), "h")
	if !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8: %q", got)
	}
	if strings.Contains(got, "<&") || strings.Contains(got, "ñ<") {
		t.Errorf("unescaped markup:\n%s", got)
	}
	// every '&' must start a complete entity, every '<' a tag we wrote
	for _, m := range regexp.MustCompile(`&[^;\s]*.?`).FindAllString(got, -1) {
		if !regexp.MustCompile(`^&(lt|gt|amp|#34|#39);`).MatchString(m) {
			t.Errorf("broken entity %q", m)
		}
	}
	if !strings.Contains(got, "…") {
		t.Errorf("title not cut:\n%s", got)
	}
	if strings.Count(got, "<a ") != strings.Count(got, "</a>") || strings.Count(got, "<b>") != strings.Count(got, "</b>") {
		t.Errorf("unbalanced tags:\n%s", got)
	}
}

func TestRenderHTMLJavascriptRepo(t *testing.T) {
	bd := Build{Service: "api", Repo: "javascript:alert(1)", Commit: "a1b2c3d", PR: "5", Title: "t", Author: "ana"}
	got, btns := renderHTML(okMsg(&Deploy{Changed: []string{"api"}, Builds: []Build{bd}}), "h")
	if strings.Contains(got, "<a ") || strings.Contains(got, "javascript") {
		t.Errorf("unsafe repo linked or shown:\n%s", got)
	}
	if !strings.Contains(got, "#5 t") || !strings.Contains(got, "a1b2c3d") {
		t.Errorf("values missing:\n%s", got)
	}
	if len(btns) != 0 {
		t.Errorf("buttons = %v, want none", btns)
	}
}

func TestRenderErrorField(t *testing.T) {
	logs := "E_X: real cause\ncontainer log line"
	tests := []struct {
		name  string
		m     Message
		want  string
		drops string
	}{
		{"set wins over Text", Message{Level: "error", Text: logs, Error: "the cause", Deploy: &Deploy{}},
			"<b>Error:</b> <code>the cause</code>", "container log line"},
		{"unset falls back to Summary", Message{Level: "error", Text: logs, Deploy: &Deploy{}},
			"<b>Error:</b> <code>E_X: container log line</code>", "real cause"},
		{"blank falls back to Summary", Message{Level: "error", Text: logs, Error: " \n", Deploy: &Deploy{}},
			"<b>Error:</b> <code>E_X: container log line</code>", "real cause"},
		{"one line", Message{Level: "warn", Error: "a\nb", Deploy: &Deploy{}},
			"<b>Error:</b> <code>a b</code>", "a\nb"},
		{"escaped", Message{Level: "error", Error: "<b>x</b>", Deploy: &Deploy{}},
			"<code>&lt;b&gt;x&lt;/b&gt;</code>", "<b>x</b>"},
		{"info stays silent", Message{Level: "info", Error: "the cause", Deploy: &Deploy{}},
			"<b>Images:</b> unchanged", "Error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := renderHTML(tc.m, "h")
			if !strings.Contains(got, tc.want) || strings.Contains(got, tc.drops) {
				t.Errorf("want %q without %q in:\n%s", tc.want, tc.drops, got)
			}
		})
	}

	long := Message{Level: "error", Error: strings.Repeat("é", 500), Deploy: &Deploy{}}
	got := renderPlain(long, "h")
	if n := strings.Count(got, "é"); n != maxSummary-1 {
		t.Errorf("Error limited to %d runes, want %d", n, maxSummary-1)
	}
}

func TestRenderNext(t *testing.T) {
	const cmd = "sudo cvctl resume demo-dev"
	tests := []struct {
		name, want string
		m          Message
	}{
		{"after Logs", "<b>Logs:</b> sudo cvctl history demo-dev\n<b>Next:</b> <code>sudo cvctl resume demo-dev</code>",
			Message{Level: "error", Deploy: &Deploy{Logs: "sudo cvctl history demo-dev", Next: cmd}}},
		{"shown at info", "<b>Images:</b> unchanged\n<b>Next:</b> <code>sudo cvctl resume demo-dev</code>",
			Message{Level: "info", Deploy: &Deploy{Next: cmd}}},
		{"escaped", "<code>a &lt;b&gt;</code>",
			Message{Level: "info", Deploy: &Deploy{Next: "a <b>"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := renderHTML(tc.m, "h"); !strings.HasSuffix(got, tc.want) {
				t.Errorf("want suffix %q in:\n%s", tc.want, got)
			}
		})
	}
	if got := renderPlain(Message{Level: "info", Deploy: &Deploy{Next: cmd}}, "h"); !strings.HasSuffix(got, "\nNext: "+cmd) {
		t.Errorf("plain:\n%s", got)
	}
	if got, _ := renderHTML(Message{Level: "info", Deploy: &Deploy{}}, "h"); strings.Contains(got, "Next") {
		t.Errorf("unset Next rendered:\n%s", got)
	}
}
