package project

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"

	"github.com/codeavers/cvctl/internal/envspec"
	"github.com/codeavers/cvctl/internal/output"
)

// ReadEnvFile parses an existing .env with docker compose rules. References to the host
// environment are not expanded: only keys defined earlier in the same file are.
func ReadEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &output.UsageError{Err: &output.Error{
			Code:    "E_USAGE_ENV_FILE",
			Message: fmt.Sprintf("cannot read env file %s: %v", path, err),
			Hint:    "pass the path of an existing .env readable by this user (usually with sudo)",
		}}
	}
	vals, err := dotenv.UnmarshalBytesWithLookup(data, func(string) (string, bool) { return "", false })
	if err != nil {
		// The parser error can quote file content, which may be a secret: it is not shown.
		return nil, &output.UsageError{Err: &output.Error{
			Code:    "E_USAGE_ENV_FILE",
			Message: fmt.Sprintf("cannot parse env file %s", path),
			Hint:    "check it loads with docker compose (KEY=VALUE lines, matching quotes)",
		}}
	}
	return vals, nil
}

// EnvFileReport lists the env-file keys used and ignored, never their values.
type EnvFileReport struct {
	Used    []string `json:"used"`
	Ignored []string `json:"ignored"`
}

// selectEnvFile keeps the keys env.spec fills from operator input (random and prompt).
// service, fixed and template values come from cvctl and the contract; keys outside the
// spec would otherwise reach app.env. A --set value wins over the file.
func selectEnvFile(spec *envspec.Spec, vals, set map[string]string) (map[string]string, EnvFileReport, error) {
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	taken := make(map[string]string)
	rep := EnvFileReport{Used: []string{}, Ignored: []string{}}
	for _, k := range keys {
		v, ok := spec.Vars[k]
		switch {
		case !ok:
			rep.Ignored = append(rep.Ignored, k+" (not in env.spec)")
		case v.Source != "random" && v.Source != "prompt":
			rep.Ignored = append(rep.Ignored, fmt.Sprintf("%s (source %s)", k, v.Source))
		case hasKey(set, k):
			rep.Ignored = append(rep.Ignored, k+" (--set)")
		default:
			if strings.ContainsAny(vals[k], "\r\n") {
				return nil, rep, &output.Error{
					Code:    "E_ENVSPEC_VALUE",
					Message: fmt.Sprintf("env file %s: value cannot contain newline or carriage return", k),
					Hint:    "put the value on one line, or pass it with --set",
				}
			}
			taken[k] = vals[k]
			rep.Used = append(rep.Used, k)
		}
	}
	return taken, rep, nil
}

func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}
