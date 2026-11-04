package notify

import (
	"html"
	"net/url"
	"regexp"
	"strings"
)

// Button is an inline URL button shown under a message.
type Button struct{ Text, URL string }

const (
	maxPlain   = 4000
	maxBody    = 1000
	maxSummary = 300
	maxField   = 100
)

var (
	commitRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	prRe     = regexp.MustCompile(`^[0-9]+$`)
	codeRe   = regexp.MustCompile(`^E_[A-Z0-9_]+`)
)

const retrySuffix = ": will retry next cycle"

// Summary reduces an error detail to one line: its E_* code, if any, and the
// last non-empty line, so a compose pull log does not reach the chat.
func Summary(text string) string {
	lines := strings.Split(text, "\n")
	last, code := "", ""
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			if code == "" {
				code = codeRe.FindString(l)
			}
			last = l
		}
	}
	if last == "" {
		return ""
	}
	last = strings.TrimSuffix(last, retrySuffix)
	if code != "" && !strings.HasPrefix(last, code) {
		last = code + ": " + last
	}
	return clip(last, maxSummary)
}

// clip limits s to n runes, marking the cut.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// field is a single-line value limited before escaping, so an HTML entity or
// tag is never cut in half.
func field(s string, n int) string {
	return clip(strings.Join(strings.Fields(s), " "), n)
}

// repoURL returns repo without a trailing slash when it is safe to link.
func repoURL(repo string) string {
	if strings.ContainsAny(repo, " \t\r\n\"'<>\\") {
		return ""
	}
	u, err := url.Parse(repo)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(repo, "#") {
		return ""
	}
	return strings.TrimRight(repo, "/")
}

func appURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.ContainsAny(s, " \t\r\n\"'<>\\") {
		return ""
	}
	return s
}

// ref is a build whose labels passed validation.
type ref struct {
	pr, commit, title, author string
	prURL, commitURL          string
}

func newRef(b Build) ref {
	r := ref{title: field(b.Title, maxField), author: field(b.Author, maxField)}
	base := repoURL(b.Repo)
	if prRe.MatchString(b.PR) {
		r.pr = b.PR
		if base != "" {
			r.prURL = base + "/pull/" + b.PR
		}
	}
	if commitRe.MatchString(b.Commit) {
		r.commit = b.Commit
		if base != "" {
			r.commitURL = base + "/commit/" + b.Commit
		}
	}
	return r
}

func (r ref) key() [2]string { return [2]string{r.prURL + "|" + r.pr, r.commitURL + "|" + r.commit} }

func (r ref) prText() string {
	t := "#" + r.pr
	if r.title != "" {
		t += " " + r.title
	}
	return t
}

func (r ref) short() string { return r.commit[:7] }

// line is one label/value pair rendered in both formats.
type line struct{ label, html, plain string }

func link(href, text string) string {
	if href == "" {
		return html.EscapeString(text)
	}
	return `<a href="` + html.EscapeString(href) + `">` + html.EscapeString(text) + `</a>`
}

func (r ref) commitHTML() string { return link(r.commitURL, r.short()) }

func serviceLine(svc string, r ref, ok bool) line {
	svc = field(svc, 50)
	if !ok || (r.pr == "" && r.commit == "") {
		return line{svc, "no build info", "no build info"}
	}
	var h, p []string
	if r.pr != "" {
		h, p = append(h, link(r.prURL, r.prText())), append(p, r.prText())
	}
	var hp, pp []string
	if r.commit != "" {
		hp, pp = append(hp, r.commitHTML()), append(pp, r.short())
	}
	if r.author != "" {
		hp, pp = append(hp, html.EscapeString(r.author)), append(pp, r.author)
	}
	hs, ps := strings.Join(h, ""), strings.Join(p, "")
	if len(hp) > 0 {
		if hs != "" {
			hs, ps = hs+" ("+strings.Join(hp, ", ")+")", ps+" ("+strings.Join(pp, ", ")+")"
		} else {
			hs, ps = strings.Join(hp, ", "), strings.Join(pp, ", ")
		}
	}
	return line{svc, hs, ps}
}

func build(m Message, host string) (lines []line, buttons []Button, body string) {
	if m.Host != "" {
		host = m.Host
	}
	status := m.Status
	if status == "" {
		status = m.Title
	}
	add := func(label, v string) {
		lines = append(lines, line{label, html.EscapeString(v), v})
	}
	add("Server", field(host, maxField))
	if m.Project != "" {
		add("Project", field(m.Project, maxField))
	}
	add("Status", field(status, maxField*2))

	d := m.Deploy
	if d == nil {
		return lines, nil, clip(strings.TrimSpace(m.Text), maxBody)
	}

	builds := map[string]Build{}
	for _, b := range d.Builds {
		builds[b.Service] = b
	}
	if len(d.Builds) > 0 && len(d.Changed) > 0 {
		refs := make([]ref, len(d.Changed))
		shared := true
		for i, s := range d.Changed {
			b, ok := builds[s]
			refs[i] = newRef(b)
			if !ok || refs[i].key() != refs[0].key() {
				shared = false
			}
		}
		if shared {
			r := refs[0]
			if r.pr != "" {
				lines = append(lines, line{"PR", link(r.prURL, r.prText()), r.prText()})
				if r.prURL != "" {
					buttons = append(buttons, Button{"PR", r.prURL})
				}
			}
			if r.commit != "" {
				h, p := r.commitHTML(), r.short()
				if r.author != "" {
					h, p = h+" by "+html.EscapeString(r.author), p+" by "+r.author
				}
				lines = append(lines, line{"Commit", h, p})
				if r.commitURL != "" {
					buttons = append(buttons, Button{"Commit", r.commitURL})
				}
			}
		} else {
			for i, s := range d.Changed {
				_, ok := builds[s]
				lines = append(lines, serviceLine(s, refs[i], ok))
			}
		}
	}
	if len(d.Changed) == 0 {
		add("Images", "unchanged")
	} else {
		names := make([]string, len(d.Changed))
		for i, s := range d.Changed {
			names[i] = field(s, 50)
		}
		add("Images", strings.Join(names, ", "))
	}
	if m.Level != "info" {
		if e := Summary(m.Text); e != "" {
			lines = append(lines, line{"Error", "<code>" + html.EscapeString(e) + "</code>", e})
		}
		if d.Logs != "" {
			add("Logs", field(d.Logs, maxField*2))
		}
	}
	if u := appURL(d.AppURL); u != "" {
		buttons = append(buttons, Button{"Open app", u})
	}
	return lines, buttons, ""
}

func renderHTML(m Message, host string) (string, []Button) {
	lines, buttons, body := build(m, host)
	var sb strings.Builder
	for i, l := range lines {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString("<b>" + html.EscapeString(l.label) + ":</b> " + l.html)
	}
	if body != "" {
		sb.WriteString("\n\n" + html.EscapeString(body))
	}
	return sb.String(), buttons
}

func renderPlain(m Message, host string) string {
	lines, _, body := build(m, host)
	var sb strings.Builder
	for i, l := range lines {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(l.label + ": " + l.plain)
	}
	if body != "" {
		sb.WriteString("\n\n" + body)
	}
	return clip(sb.String(), maxPlain)
}
