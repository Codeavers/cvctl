package contract

import (
	"regexp"
	"slices"
	"strings"
)

// "$$" is matched first so an escaped dollar never starts a reference.
var refRe = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)|\$([A-Za-z_][A-Za-z0-9_]*)`)

// EnvRefs returns the sorted, unique variable names referenced as $VAR or ${VAR...}.
func EnvRefs(raw []byte) []string {
	seen := map[string]bool{}
	for _, m := range refRe.FindAllSubmatch(raw, -1) {
		name := string(m[1])
		if name == "" {
			name = string(m[2])
		}
		if name != "" {
			seen[name] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// Normalize maps a service name to its env-var form.
func Normalize(name string) string {
	return strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToUpper(name))
}
