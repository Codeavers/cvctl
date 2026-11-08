package host

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"path/filepath"

	"github.com/codeavers/cvctl/internal/adopt"
)

const dynamicDir = "/etc/traefik/dynamic"

// AdoptedStep keeps adopted routes in line with the edge mode, which only init changes.
type AdoptedStep struct{}

func (AdoptedStep) Name() string { return "adopted" }

func (AdoptedStep) Check(_ context.Context, e *Env) (Plan, error) {
	svcs, err := adopt.Load(e.FS, adopt.Dir)
	if err != nil {
		return Plan{}, err
	}
	want, err := adopt.Render(svcs, effectiveEdgeMode(e.Cfg), e.Cfg.EdgeGateway())
	if err != nil {
		return Plan{}, err
	}
	dst := filepath.Join(dynamicDir, adopt.RouteFile)
	have, err := e.FS.ReadFile(dst)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Plan{}, err
	}
	if bytes.Equal(want, have) {
		return Plan{}, nil
	}
	return Plan{Changes: []string{"regenerate " + dst}}, nil
}

func (AdoptedStep) Apply(_ context.Context, e *Env, _ Plan) (string, error) {
	return "", adopt.Sync(e.FS, adopt.Dir, dynamicDir, effectiveEdgeMode(e.Cfg), e.Cfg.EdgeGateway())
}
