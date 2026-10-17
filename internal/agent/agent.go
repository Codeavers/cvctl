package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/kinds"
)

var (
	lastErrorsMu sync.Mutex
	lastErrors   = make(map[string]string)
)

// ResetErrors clears the in-memory error map (primarily for testing).
func ResetErrors() {
	lastErrorsMu.Lock()
	defer lastErrorsMu.Unlock()
	lastErrors = make(map[string]string)
}

func getLastError(inst string) string {
	lastErrorsMu.Lock()
	defer lastErrorsMu.Unlock()
	return lastErrors[inst]
}

func setLastError(inst, errStr string) {
	lastErrorsMu.Lock()
	defer lastErrorsMu.Unlock()
	lastErrors[inst] = errStr
}

func clearLastError(inst string) {
	lastErrorsMu.Lock()
	defer lastErrorsMu.Unlock()
	delete(lastErrors, inst)
}

// Store abstracts state operations for instances.
type Store interface {
	Instances() ([]string, error)
	Load(inst string) (*state.State, error)
}

// Deployer abstracts the deployment execution for an instance.
type Deployer interface {
	Deploy(ctx context.Context, inst string, want state.Desired, opts deploy.Options) (deploy.Result, error)
}

// DecisionLog records one structured line per decision.
type DecisionLog struct {
	TS       string `json:"ts"`
	Instance string `json:"instance"`
	Action   string `json:"action"`
	Reason   string `json:"reason"`
}

// Config specifies the runtime configuration for the agent loop.
type Config struct {
	Interval   time.Duration
	Store      Store
	Deployer   Deployer
	Sources    func(d *instance.Decisions) deploy.Source
	Load       func(inst string) (*instance.Decisions, *contract.Project, error)
	Notify     notify.Notifier
	Out        io.Writer
	Once       bool
	Run        sys.Runner
	Host       *hostcfg.Config
	Secrets    *hostcfg.Secrets
	DeployRoot string
	StateRoot  string
	FS         sys.FS
	Catalog    kinds.Catalog
	LastErrors map[string]string
}

// Run executes the agent monitoring loop.
func Run(ctx context.Context, cfg Config) error {
	interval := cfg.Interval
	if interval <= 0 {
		interval = 60 * time.Second
	}

	out := cfg.Out
	if out == nil {
		out = os.Stdout
	}

	notif := cfg.Notify
	if notif == nil {
		notif = notify.NoopNotifier{}
	}

	fsys := cfg.FS
	if fsys == nil {
		fsys = sys.NewOSFS()
	}

	deployRoot := cfg.DeployRoot
	if deployRoot == "" {
		deployRoot = deploy.DefaultDeployRoot
	}

	stateRoot := cfg.StateRoot
	if stateRoot == "" {
		stateRoot = deploy.DefaultStateRoot
	}

	hostName := ""
	if cfg.Host != nil && cfg.Host.HostName != "" {
		hostName = cfg.Host.HostName
	} else {
		hostName, _ = os.Hostname()
	}

	loadFn := cfg.Load
	if loadFn == nil {
		loadFn = func(inst string) (*instance.Decisions, *contract.Project, error) {
			instDir := filepath.Join(deployRoot, inst)
			cvctlEnvPath := filepath.Join(instDir, "cvctl.env")
			d0, err := instance.Read(cvctlEnvPath, &contract.Project{})
			if err != nil {
				return nil, nil, err
			}
			infraPath := d0.InfraPath
			if infraPath == "" {
				infraPath = "."
			}
			infraDir := filepath.Join(instDir, "infra", infraPath)
			cat := cfg.Catalog
			if cat == nil {
				var cErr error
				cat, cErr = kinds.Load()
				if cErr != nil {
					return nil, nil, cErr
				}
			}
			p, err := contract.Load(infraDir, cat)
			if err != nil {
				return nil, nil, err
			}
			dec, err := instance.Read(cvctlEnvPath, p)
			if err != nil {
				return nil, nil, err
			}
			return dec, p, nil
		}
	}

	sourcesFn := cfg.Sources
	if sourcesFn == nil {
		sourcesFn = func(d *instance.Decisions) deploy.Source {
			deps := deploy.SourceDepsWithRoots(d.Instance, cfg.Run, cfg.Host, cfg.Secrets, deployRoot, stateRoot, fsys)
			return deploy.SourceFor(d, deps)
		}
	}

	logDecision := func(inst, action, reason string) {
		entry := DecisionLog{
			TS:       time.Now().UTC().Format(time.RFC3339),
			Instance: inst,
			Action:   action,
			Reason:   reason,
		}
		_ = json.NewEncoder(out).Encode(entry)
	}

	runTick := func() {
		instances, err := cfg.Store.Instances()
		if err != nil {
			logDecision("", "error", fmt.Sprintf("cannot list instances: %v", err))
			return
		}

		for _, inst := range instances {
			if ctx.Err() != nil {
				return
			}

			dec, p, err := loadFn(inst)
			if err != nil {
				errStr := err.Error()
				if getLastError(inst) != errStr {
					setLastError(inst, errStr)
					logDecision(inst, "error", errStr)
					_ = notif.Send(ctx, notify.Message{
						Host:  hostName,
						Level: "error",
						Title: fmt.Sprintf("%s agent error", inst),
						Text:  errStr,
					})
				}
				continue
			}

			src := sourcesFn(dec)
			want, err := src.Desired(ctx, dec, p)
			if err != nil {
				errStr := err.Error()
				if getLastError(inst) != errStr {
					setLastError(inst, errStr)
					logDecision(inst, "error", errStr)
					_ = notif.Send(ctx, notify.Message{
						Host:  hostName,
						Level: "error",
						Title: fmt.Sprintf("%s desired state error", inst),
						Text:  errStr,
					})
				}
				continue
			}

			clearLastError(inst)

			st, err := cfg.Store.Load(inst)
			if err != nil {
				logDecision(inst, "error", fmt.Sprintf("cannot load state: %v", err))
				continue
			}

			wantHash := want.Hash()
			currentHash := ""
			if st.Current != nil {
				currentHash = st.Current.Hash()
			}

			if st.Status == state.StatusPaused {
				if st.PausedWantHash == "" || wantHash == st.PausedWantHash || (st.FailedHash != "" && wantHash == st.FailedHash) {
					logDecision(inst, "skip", "instance is paused")
					continue
				}
			}

			isBlocked := st.Status == state.StatusBlocked
			hashChanged := (wantHash != currentHash) && (st.FailedHash == "" || wantHash != st.FailedHash)

			if !isBlocked && !hashChanged {
				reason := "already at desired state"
				if st.FailedHash != "" && wantHash == st.FailedHash {
					reason = "desired state previously failed"
				}
				logDecision(inst, "skip", reason)
				continue
			}

			reason := "new desired state"
			if isBlocked {
				reason = "retrying blocked instance"
			}
			logDecision(inst, "deploy", reason)

			_, _ = cfg.Deployer.Deploy(ctx, inst, want, deploy.Options{Actor: "agent"})
		}
	}

	runTick()
	if cfg.Once || ctx.Err() != nil {
		return nil
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			runTick()
		}
	}
}
