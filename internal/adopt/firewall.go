package adopt

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// Rule is a ufw rule cvctl added for an adopted service. Only recorded rules
// are ever deleted, so a rule the operator created stays untouched.
type Rule struct {
	Subnet string `yaml:"subnet" json:"subnet"`
	Port   int    `yaml:"port" json:"port"`
	Proto  string `yaml:"proto" json:"proto"`
}

func RuleFor(subnet string, port int) Rule {
	return Rule{Subnet: subnet, Port: port, Proto: "tcp"}
}

func (r Rule) Validate() error {
	if _, err := netip.ParsePrefix(r.Subnet); err != nil {
		return fmt.Errorf("firewall subnet %q is not a CIDR", r.Subnet)
	}
	if r.Port < 1 || r.Port > 65535 {
		return fmt.Errorf("firewall port %d is out of range", r.Port)
	}
	if r.Proto != "tcp" {
		return fmt.Errorf("firewall proto %q is not tcp", r.Proto)
	}
	return nil
}

// Args are the ufw arguments after the verb (allow, delete allow).
func (r Rule) Args() []string {
	return []string{"from", r.Subnet, "to", "any", "port", strconv.Itoa(r.Port), "proto", r.Proto}
}

func (r Rule) String() string { return strings.Join(r.Args(), " ") }

// ParseAdded reads `ufw show added`, which lists user rules as ufw commands
// whether or not ufw is active. Only plain incoming allow rules to any address
// count: a narrower rule (interface, destination) does not cover the service.
func ParseAdded(out []byte) []Rule {
	var rules []Rule
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "ufw" || f[1] != "allow" {
			continue
		}
		f = f[2:]
		if len(f) > 0 && f[0] == "in" {
			f = f[1:]
		}
		kv := map[string]string{}
		ok := true
		for i := 0; i < len(f) && ok; i += 2 {
			if f[i] == "comment" {
				break
			}
			switch f[i] {
			case "from", "to", "port", "proto":
				if _, dup := kv[f[i]]; dup || i+1 >= len(f) {
					ok = false
					break
				}
				kv[f[i]] = f[i+1]
			default:
				ok = false
			}
		}
		if !ok || kv["to"] != "any" || kv["from"] == "" || kv["proto"] == "" {
			continue
		}
		p, err := strconv.Atoi(kv["port"])
		if err != nil {
			continue
		}
		rules = append(rules, Rule{Subnet: kv["from"], Port: p, Proto: kv["proto"]})
	}
	return rules
}

// Recorded reports whether a service other than except owns rule r.
func Recorded(svcs []Service, r Rule, except string) bool {
	return slices.ContainsFunc(svcs, func(o Service) bool {
		return o.Name != except && o.Firewall != nil && *o.Firewall == r
	})
}
