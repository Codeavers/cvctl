package s3prov

import (
	"fmt"
	"strings"
)

// BackupUserScript returns the POSIX sh script that creates or updates an
// instance's backup user and its policy inside the silo container. The policy
// grants object access under <bucket>/<prefix>/ only and lists the bucket only
// below that prefix, so one instance can neither read nor delete another's
// backups. Like Script, it runs on stdin with the root keys taken from the
// container's environment and the secret never in argv; every step is
// idempotent.
func BackupUserScript(user, secret, bucket, prefix, policy string) string {
	prefix = strings.Trim(prefix, "/")
	doc := mustJSON(policyDoc{Version: "2012-10-17", Statement: []statement{
		{Effect: "Allow", Action: []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject"},
			Resource: []string{"arn:aws:s3:::" + bucket + "/" + prefix + "/*"}},
		{Effect: "Allow", Action: []string{"s3:ListBucket"},
			Resource:  []string{"arn:aws:s3:::" + bucket},
			Condition: map[string]map[string][]string{"StringLike": {"s3:prefix": {prefix + "/*", prefix}}}},
	}})
	file := "/tmp/" + policy + "-policy.json"

	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString(`export MC_HOST_local="http://${MINIO_ROOT_USER}:${MINIO_ROOT_PASSWORD}@localhost:9000"` + "\n")
	fmt.Fprintf(&b, "cat > %s <<'CVCTL_EOF'\n%s\nCVCTL_EOF\n", shQuote(file), doc)
	fmt.Fprintf(&b, "mc admin policy create local %s %s\n", shQuote(policy), shQuote(file))
	fmt.Fprintf(&b, "printf '%%s\\n' %s | mc admin user add local %s\n", shQuote(secret), shQuote(user))
	fmt.Fprintf(&b, "case \"$(mc admin user info local %s --json)\" in\n", shQuote(user))
	fmt.Fprintf(&b, "  *'\"policyName\":\"%[1]s\"'* | *'\"policyName\":\"%[1]s,'* | *',%[1]s\"'* | *',%[1]s,'*) ;;\n", policy)
	fmt.Fprintf(&b, "  *) mc admin policy attach local %s --user %s ;;\n", shQuote(policy), shQuote(user))
	b.WriteString("esac\n")
	fmt.Fprintf(&b, "rm -f %s\n", shQuote(file))
	return b.String()
}
