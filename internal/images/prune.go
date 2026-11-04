package images

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/codeavers/cvctl/internal/sys"
)

// Item is one repo@digest reference considered for removal. In a dry run
// Removed is false and Reason is empty. InUse marks a rejection because a
// container uses the image, the one failure that is routine.
type Item struct {
	Ref     string
	Size    int64
	Removed bool
	InUse   bool
	Reason  string
}

type Pruner struct{ Run sys.Runner }

var dockerEnv = []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"}

type listed struct {
	Repository string
	Digest     string
	Size       string
}

// Prune removes only references whose digest is a candidate and not kept.
// Images of other stacks are never named, so they never reach the output.
// Removal is by repo@digest without --force: Docker refuses an image in use,
// which is expected and recorded, not an error.
func (p Pruner) Prune(ctx context.Context, plan Plan, dryRun bool) ([]Item, error) {
	res, err := p.Run.Run(ctx, sys.Cmd{Name: "docker", Args: []string{"image", "ls", "--digests", "--format", "{{json .}}"}, Env: dockerEnv})
	if err != nil {
		return nil, fmt.Errorf("listing images: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("listing images: docker exited %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}

	var items []Item
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(res.Stdout))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var l listed
		if json.Unmarshal(line, &l) != nil {
			continue
		}
		if l.Digest == "" || l.Digest == "<none>" || l.Repository == "" || l.Repository == "<none>" {
			continue
		}
		if !plan.Candidates[l.Digest] || plan.Keep[l.Digest] {
			continue
		}
		ref := l.Repository + "@" + l.Digest
		if seen[ref] {
			continue
		}
		seen[ref] = true
		it := Item{Ref: ref, Size: parseSize(l.Size)}
		if !dryRun {
			r, err := p.Run.Run(ctx, sys.Cmd{Name: "docker", Args: []string{"image", "rm", ref}, Env: dockerEnv})
			switch {
			case err != nil:
				it.Reason = err.Error()
			case r.ExitCode != 0:
				it.Reason = strings.TrimSpace(string(r.Stderr))
				it.InUse = usedByContainer(it.Reason)
			default:
				it.Removed = true
			}
		}
		items = append(items, it)
	}
	if err := sc.Err(); err != nil {
		return items, fmt.Errorf("reading image list: %w", err)
	}
	return items, nil
}

// usedByContainer matches the daemon's wording, which has no stable error code
// in the CLI output. Other conflicts, such as child images, are not routine.
func usedByContainer(stderr string) bool {
	for _, s := range []string{
		"is using its referenced image",
		"is being used by running container",
		"is being used by stopped container",
	} {
		if strings.Contains(stderr, s) {
			return true
		}
	}
	return false
}

// parseSize reads Docker's decimal units ("12.5MB"); 0 when unparseable.
func parseSize(s string) int64 {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   float64
	}{{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"B", 1}}
	for _, u := range units {
		if num, ok := strings.CutSuffix(s, u.suffix); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
			if err != nil || f < 0 {
				return 0
			}
			return int64(f*u.mult + 0.5)
		}
	}
	return 0
}
