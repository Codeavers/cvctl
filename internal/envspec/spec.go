package envspec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/codeavers/cvctl/internal/output"
)

const defaultRandomLength = 48

type Var struct {
	Source  string  `yaml:"source"`
	Service string  `yaml:"service"`
	Length  int     `yaml:"length"`
	Value   any     `yaml:"-"`
	Default *string `yaml:"default"`
	Secret  bool    `yaml:"secret"`
}

type Spec struct {
	Vars map[string]Var `yaml:"vars"`
}

var knownVarKeys = map[string]bool{"source": true, "service": true, "length": true, "value": true, "default": true, "secret": true}

func invalid(msg, hint string) error {
	return &output.Error{Code: "E_ENVSPEC_INVALID", Message: msg, Hint: hint}
}

// UnmarshalYAML decodes Value separately because it is either a scalar or an env->string map.
func (v *Var) UnmarshalYAML(n *yaml.Node) error {
	// Node.Decode ignores the caller's KnownFields, so unknown keys (typos such
	// as "secert") are rejected here.
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if k := n.Content[i].Value; !knownVarKeys[k] {
				return fmt.Errorf("unknown key %q (valid keys: source, service, length, value, default, secret)", k)
			}
		}
	}
	type plain Var
	aux := struct {
		plain `yaml:",inline"`
		Value any `yaml:"value"`
	}{}
	if err := n.Decode(&aux); err != nil {
		return err
	}
	*v = Var(aux.plain)
	switch val := aux.Value.(type) {
	case nil:
	case string:
		v.Value = val
	case map[string]any:
		m := make(map[string]string, len(val))
		for k, e := range val {
			s, ok := e.(string)
			if !ok {
				return fmt.Errorf("value for env %q must be a string", k)
			}
			m[k] = s
		}
		v.Value = m
	default:
		// Unquoted numbers/bools are rejected so that values never change meaning silently.
		return fmt.Errorf("value must be a string or a map of env to string")
	}
	return nil
}

func Load(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, invalid(fmt.Sprintf("cannot read env spec %s: %v", path, err), "check the path to env.spec.yml")
	}
	// Vars are decoded one by one so errors name the offending var, in sorted
	// order so the same file always reports the same first error.
	var raw struct {
		Vars map[string]yaml.Node `yaml:"vars"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, invalid(fmt.Sprintf("cannot parse env spec %s: %v", path, err), "fix the YAML syntax; the only top-level key is vars")
	}
	names := make([]string, 0, len(raw.Vars))
	for n := range raw.Vars {
		names = append(names, n)
	}
	sort.Strings(names)
	s := Spec{Vars: make(map[string]Var, len(raw.Vars))}
	for _, name := range names {
		node := raw.Vars[name]
		var v Var
		if err := node.Decode(&v); err != nil {
			return nil, invalid(fmt.Sprintf("var %s: %v", name, err), "fix the var definition; values must be strings or env->string maps")
		}
		s.Vars[name] = v
	}
	for _, name := range names {
		v := s.Vars[name]
		switch v.Source {
		case "service":
			if v.Service == "" {
				return nil, invalid(fmt.Sprintf("var %s: source service requires 'service'", name), "set service: <data service name>")
			}
		case "fixed":
			if v.Value == nil {
				return nil, invalid(fmt.Sprintf("var %s: source fixed requires 'value'", name), "set value: a string or an env->string map")
			}
		case "template":
			if _, ok := v.Value.(string); !ok {
				return nil, invalid(fmt.Sprintf("var %s: source template requires a string 'value'", name), "set value: \"https://{domain:api}\"")
			}
		case "random", "prompt":
		default:
			return nil, invalid(fmt.Sprintf("var %s: unknown source %q", name, v.Source), "use one of: service, random, fixed, template, prompt")
		}
		if v.Length < 0 {
			return nil, invalid(fmt.Sprintf("var %s: negative length", name), "use a positive length")
		}
		if v.Source == "random" && v.Length == 0 {
			v.Length = defaultRandomLength
			s.Vars[name] = v
		}
	}
	return &s, nil
}
