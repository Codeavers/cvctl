package envspec

import (
	"crypto/rand"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/codeavers/cvctl/internal/output"
)

type Input struct {
	Env         string
	Existing    map[string]string
	ServiceVars map[string]map[string]string
	Domains     map[string]string
	Rand        io.Reader
}

type Result struct {
	Values    map[string]string
	Missing   []string
	Generated []string
}

const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// maxUnbiased is the largest multiple of len(alphabet) that fits in a byte; bytes at or
// above it are discarded so every character is equally likely.
const maxUnbiased = 256 / len(alphabet) * len(alphabet)

func randomString(r io.Reader, n int) (string, error) {
	out := make([]byte, 0, n)
	for len(out) < n {
		// Reading only what is still needed keeps consumption deterministic for a given stream.
		buf := make([]byte, n-len(out))
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", &output.Error{Code: "E_ENVSPEC_RANDOM", Message: "cannot read random bytes: " + err.Error(), Hint: "check the entropy source"}
		}
		for _, b := range buf {
			if int(b) >= maxUnbiased {
				continue
			}
			out = append(out, alphabet[int(b)%len(alphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}

var tplRef = regexp.MustCompile(`\{([a-z]+):([^{}]*)\}`)

// TemplateRef is one {kind:name} reference inside a template value.
type TemplateRef struct{ Kind, Name string }

// TemplateRefs lists the references in a template value, in order.
func TemplateRefs(raw string) []TemplateRef {
	var out []TemplateRef
	for _, m := range tplRef.FindAllStringSubmatch(raw, -1) {
		out = append(out, TemplateRef{Kind: m[1], Name: m[2]})
	}
	return out
}

func tplErr(name, msg string) error {
	return &output.Error{Code: "E_ENVSPEC_TEMPLATE_REF", Message: fmt.Sprintf("var %s: %s", name, msg), Hint: "references are {domain:<service>} and {var:<NAME>}; referenced vars must resolve and not form a cycle"}
}

func Resolve(s *Spec, in Input) (Result, error) {
	rd := in.Rand
	if rd == nil {
		rd = rand.Reader
	}
	values := make(map[string]string, len(in.Existing)+len(s.Vars))
	for k, v := range in.Existing {
		values[k] = v
	}
	var missing, generated []string
	var templates []string

	names := make([]string, 0, len(s.Vars))
	for n := range s.Vars {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		v := s.Vars[name]
		switch v.Source {
		case "service":
			val, ok := in.ServiceVars[v.Service][name]
			if !ok {
				return Result{}, &output.Error{Code: "E_ENVSPEC_SERVICE_VAR", Message: fmt.Sprintf("var %s: service %q does not provide it", name, v.Service), Hint: "check the data service name and the variables it injects"}
			}
			values[name] = val
		case "random":
			if _, ok := in.Existing[name]; ok {
				continue
			}
			n := v.Length
			if n == 0 {
				n = defaultRandomLength
			}
			val, err := randomString(rd, n)
			if err != nil {
				return Result{}, err
			}
			values[name] = val
			generated = append(generated, name)
		case "fixed":
			switch val := v.Value.(type) {
			case string:
				values[name] = val
			case map[string]string:
				e, ok := val[in.Env]
				if !ok {
					return Result{}, &output.Error{Code: "E_ENVSPEC_FIXED_ENV", Message: fmt.Sprintf("var %s: no value for env %q", name, in.Env), Hint: "add that env to the value map"}
				}
				values[name] = e
			default:
				return Result{}, invalid(fmt.Sprintf("var %s: fixed value has an unsupported type", name), "use a string or an env->string map")
			}
		case "template":
			templates = append(templates, name)
		case "prompt":
			if e, ok := in.Existing[name]; ok {
				values[name] = e
			} else if v.Default != nil {
				values[name] = *v.Default
			} else {
				missing = append(missing, name)
			}
		default:
			return Result{}, invalid(fmt.Sprintf("var %s: unknown source %q", name, v.Source), "use one of: service, random, fixed, template, prompt")
		}
	}

	// Templates are resolved lazily so they can reference each other; state detects cycles.
	state := map[string]int{} // 1 = in progress, 2 = done
	// A template that depends on an unresolved prompt var is skipped, not failed: the root
	// prompt var is already in Missing and the caller blocks on it.
	missingSet := map[string]bool{}
	for _, m := range missing {
		missingSet[m] = true
	}
	skipped := map[string]bool{}
	var resolveTpl func(name string) error
	resolveTpl = func(name string) error {
		switch state[name] {
		case 1:
			return tplErr(name, "template reference cycle")
		case 2:
			return nil
		}
		state[name] = 1
		raw, ok := s.Vars[name].Value.(string)
		if !ok {
			return invalid(fmt.Sprintf("var %s: template value must be a string", name), "set value to a string")
		}
		var rerr error
		blocked := false
		out := tplRef.ReplaceAllStringFunc(raw, func(m string) string {
			if rerr != nil {
				return m
			}
			sub := tplRef.FindStringSubmatch(m)
			switch sub[1] {
			case "domain":
				d, ok := in.Domains[sub[2]]
				if !ok {
					rerr = tplErr(name, fmt.Sprintf("unknown domain for service %q", sub[2]))
					return m
				}
				return d
			case "var":
				if t, ok := s.Vars[sub[2]]; ok && t.Source == "template" {
					if err := resolveTpl(sub[2]); err != nil {
						rerr = err
						return m
					}
				}
				if missingSet[sub[2]] || skipped[sub[2]] {
					blocked = true
					return m
				}
				val, ok := values[sub[2]]
				if !ok {
					rerr = tplErr(name, fmt.Sprintf("unknown or unresolved var %q", sub[2]))
					return m
				}
				return val
			}
			rerr = tplErr(name, fmt.Sprintf("unknown reference kind %q", sub[1]))
			return m
		})
		if rerr != nil {
			return rerr
		}
		if blocked {
			skipped[name] = true
			state[name] = 2
			return nil
		}
		values[name] = out
		state[name] = 2
		return nil
	}
	for _, name := range templates {
		if err := resolveTpl(name); err != nil {
			return Result{}, err
		}
	}

	sortedValKeys := make([]string, 0, len(values))
	for k := range values {
		sortedValKeys = append(sortedValKeys, k)
	}
	sort.Strings(sortedValKeys)
	for _, k := range sortedValKeys {
		if strings.ContainsAny(values[k], "\r\n") {
			return Result{}, &output.Error{
				Code:    "E_ENVSPEC_VALUE",
				Message: fmt.Sprintf("var %s: value cannot contain newline or carriage return", k),
				Hint:    "remove newline and carriage return characters from the value",
			}
		}
	}

	sort.Strings(missing)
	sort.Strings(generated)
	return Result{Values: values, Missing: missing, Generated: generated}, nil
}
