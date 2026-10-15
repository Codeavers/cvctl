package envspec

import (
	"fmt"
	"os"

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

func invalid(msg, hint string) error {
	return &output.Error{Code: "E_ENVSPEC_INVALID", Message: msg, Hint: hint}
}

// UnmarshalYAML decodes Value separately because it is either a scalar or an env->string map.
func (v *Var) UnmarshalYAML(n *yaml.Node) error {
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
	var s Spec
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, invalid(fmt.Sprintf("cannot parse env spec %s: %v", path, err), "fix the YAML syntax; values must be strings or env->string maps")
	}
	for name, v := range s.Vars {
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
