package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/kinds"
)

// LockNoticeAfter is the duration an instance can remain locked before the agent
// reports an error notification.
var LockNoticeAfter = 10 * time.Minute

// FetchErrorNoticeAfter is how long a transient fetch error (unreachable
// registry, HTTP 429/5xx, fetch timeout) must persist before the agent sends
// an error notification.
var FetchErrorNoticeAfter = 10 * time.Minute

// Store abstracts state operations for instances.
type Store interface {
	Instances() ([]string, error)
	Load(inst string) (*state.State, error)
}

// Deployer abstracts the deployment execution for an instance.
type Deployer interface {
	Deploy(ctx context.Context, inst string, want state.Desired, opts deploy.Options) (deploy.Result, error)
}

// OnDiskRejected is returned by a loader, together with decisions read
// without the project, when the contract in infra/ fails validation.
type OnDiskRejected struct{ Err error }

func (e *OnDiskRejected) Error() string {
	return "contract on disk rejected, checking the published contract: " + e.Err.Error()
}

func (e *OnDiskRejected) Unwrap() error { return e.Err }

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
	Now        func() time.Time
	// ReloadHost, when set, runs at the start of every tick and returns the
	// host config, secrets and notifier to use for that tick. A long-running
	// agent must not deploy with a host.env older than the one on disk: a
	// stale CVCTL_EDGE_MODE renders routes for the wrong entry point.
	ReloadHost func() (*hostcfg.Config, *hostcfg.Secrets, notify.Notifier, error)
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

	host, secrets := cfg.Host, cfg.Secrets
	hostNameOf := func(h *hostcfg.Config) string {
		if h != nil && h.HostName != "" {
			return h.HostName
		}
		n, _ := os.Hostname()
		return n
	}
	hostName := hostNameOf(host)

	loadFn := cfg.Load
	if loadFn == nil {
		loadFn = func(inst string) (*instance.Decisions, *contract.Project, error) {
			instDir := filepath.Join(deployRoot, inst)
			cvctlEnvPath := filepath.Join(instDir, "cvctl.env")
			d0, err := instance.Read(cvctlEnvPath, &contract.Project{})
			if err != nil {
				return nil, nil, err
			}
			if err := instance.CheckDir(d0, cvctlEnvPath); err != nil {
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
			// The on-disk contract may fail rules added after it was deployed:
			// d0 still lets Desired fetch a fixed contract, which Desired and
			// the pipeline validate themselves.
			p, err := contract.Load(infraDir, cat)
			if err != nil {
				return d0, nil, &OnDiskRejected{Err: instance.WrapLoadErr(err)}
			}
			dec, err := instance.Read(cvctlEnvPath, p)
			if err != nil {
				return d0, nil, &OnDiskRejected{Err: err}
			}
			if err := instance.CheckDir(dec, cvctlEnvPath); err != nil {
				return nil, nil, err
			}
			if err := instance.Verify(infraDir, p, dec, cat, false); err != nil {
				return d0, nil, &OnDiskRejected{Err: err}
			}
			return dec, p, nil
		}
	}

	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	sourcesFn := cfg.Sources
	if sourcesFn == nil {
		sourcesFn = func(d *instance.Decisions) deploy.Source {
			deps := deploy.SourceDepsWithRoots(d.Instance, cfg.Run, host, secrets, deployRoot, stateRoot, fsys)
			return deploy.SourceFor(d, deps)
		}
	}

	logDecision := func(inst, action, reason string) {
		entry := DecisionLog{
			TS:       nowFn().UTC().Format(time.RFC3339),
			Instance: inst,
			Action:   action,
			Reason:   reason,
		}
		_ = json.NewEncoder(out).Encode(entry)
	}

	lastErrors := make(map[string]string)
	lastWarnings := make(map[string]string)
	lockedFirstSeen := make(map[string]time.Time)
	fetchFirstSeen := make(map[string]time.Time)
	fetchLastLogged := make(map[string]string)
	regFirstSeen := make(map[string]time.Time)
	regNotified := make(map[string]bool)

	recordWarning := func(inst, reason string) {
		if lastWarnings[inst] != reason {
			lastWarnings[inst] = reason
			logDecision(inst, "warn", reason)
			_ = notif.Send(ctx, notify.Message{
				Host:  hostName,
				Level: "warn",
				Title: fmt.Sprintf("%s running contract rejected", inst),
				Text:  reason,
			})
		}
	}

	recordError := func(inst, reason string) {
		if lastErrors[inst] != reason {
			lastErrors[inst] = reason
			logDecision(inst, "error", reason)
			_ = notif.Send(ctx, notify.Message{
				Host:  hostName,
				Level: "error",
				Title: fmt.Sprintf("%s agent error", inst),
				Text:  reason,
			})
		}
	}

	// A deploy cut short by a crash or kill leaves DEPLOYING behind.
	if rec, ok := cfg.Store.(interface {
		RecoverInterrupted(inst string, now time.Time) (bool, error)
	}); ok {
		if instances, err := cfg.Store.Instances(); err == nil {
			for _, inst := range instances {
				switch recovered, err := rec.RecoverInterrupted(inst, nowFn().UTC()); {
				case err != nil:
					logDecision(inst, "error", fmt.Sprintf("cannot recover interrupted deploy: %v", err))
				case recovered:
					logDecision(inst, "recover", "interrupted deploy: returned to last stable state")
				}
			}
		}
	}

	lastReloadErr := ""
	reloadHost := func() bool {
		if cfg.ReloadHost == nil {
			return true
		}
		h, s, n, err := cfg.ReloadHost()
		if err != nil {
			reason := fmt.Sprintf("cannot reload host config, skipping this tick: %v", err)
			if reason != lastReloadErr {
				lastReloadErr = reason
				logDecision("", "error", reason)
				_ = notif.Send(ctx, notify.Message{
					Host:  hostName,
					Level: "error",
					Title: "agent cannot reload host config",
					Text:  reason,
				})
			}
			return false
		}
		lastReloadErr = ""
		if !host.Equal(h) {
			logDecision("", "reload", "host config changed")
		}
		host, secrets = h, s
		if n != nil {
			notif = n
		}
		hostName = hostNameOf(host)
		return true
	}

	runTick := func() {
		if !reloadHost() {
			return
		}
		instances, err := cfg.Store.Instances()
		if err != nil {
			logDecision("", "error", fmt.Sprintf("cannot list instances: %v", err))
			return
		}

		regFailedThisTick := make(map[string]bool)
		for _, inst := range instances {
			if ctx.Err() != nil {
				return
			}

			func(inst string) {
				locked := false
				defer func() {
					if !locked {
						delete(lockedFirstSeen, inst)
					}
				}()

				transientFetch := false
				defer func() {
					if !transientFetch {
						delete(fetchFirstSeen, inst)
						delete(fetchLastLogged, inst)
					}
				}()

				handleFetchError := func(err error) {
					reason := err.Error()
					transient, host := classifyFetchError(err)
					if !transient {
						recordError(inst, reason)
						return
					}
					transientFetch = true
					if fetchLastLogged[inst] != reason {
						fetchLastLogged[inst] = reason
						logDecision(inst, "error", reason)
					}
					if host != "" {
						regFailedThisTick[host] = true
						firstSeen, ok := regFirstSeen[host]
						if !ok {
							firstSeen = nowFn()
							regFirstSeen[host] = firstSeen
						}
						if !nowFn().Before(firstSeen.Add(FetchErrorNoticeAfter)) && !regNotified[host] {
							regNotified[host] = true
							_ = notif.Send(ctx, notify.Message{
								Host:  hostName,
								Level: "error",
								Title: fmt.Sprintf("registry %s unreachable", host),
								Text:  reason,
							})
						}
						return
					}
					firstSeen, ok := fetchFirstSeen[inst]
					if !ok {
						firstSeen = nowFn()
						fetchFirstSeen[inst] = firstSeen
					}
					if !nowFn().Before(firstSeen.Add(FetchErrorNoticeAfter)) {
						if lastErrors[inst] != reason {
							lastErrors[inst] = reason
							_ = notif.Send(ctx, notify.Message{
								Host:  hostName,
								Level: "error",
								Title: fmt.Sprintf("%s agent error", inst),
								Text:  reason,
							})
						}
					}
				}

				defer func() {
					if r := recover(); r != nil {
						recordError(inst, fmt.Sprintf("panic: %v", r))
					}
				}()

				dec, _, err := loadFn(inst)
				var rejected *OnDiskRejected
				if errors.As(err, &rejected) && dec != nil {
					recordWarning(inst, rejected.Error())
				} else if err != nil {
					recordError(inst, err.Error())
					return
				} else {
					delete(lastWarnings, inst)
				}

				src := sourcesFn(dec)
				fetchCtx, fetchCancel := deploy.WithStepTimeout(ctx, host.TimeoutFetch(), "fetch")
				want, err := src.Desired(fetchCtx, dec)
				if err != nil {
					te := deploy.TimeoutError(fetchCtx)
					fetchCancel()
					if te != nil {
						handleFetchError(te)
						return
					}
					// A restart cancels the fetch in progress; that is not a failure to report.
					if ctx.Err() != nil {
						return
					}
					handleFetchError(err)
					return
				}
				fetchCancel()

				st, err := cfg.Store.Load(inst)
				if err != nil {
					recordError(inst, fmt.Sprintf("cannot load state: %v", err))
					return
				}

				wantHash := want.Hash()
				currentHash := ""
				if st.Current != nil {
					currentHash = st.Current.Hash()
				}

				if st.Status == state.StatusPaused {
					if st.PausedWantHash == "" || wantHash == st.PausedWantHash || (st.FailedHash != "" && wantHash == st.FailedHash) {
						delete(lastErrors, inst)
						logDecision(inst, "skip", "instance is paused")
						return
					}
				}

				isBlocked := st.Status == state.StatusBlocked
				hashChanged := (wantHash != currentHash) && (st.FailedHash == "" || wantHash != st.FailedHash)

				if !isBlocked && !hashChanged {
					delete(lastErrors, inst)
					reason := "already at desired state"
					if st.FailedHash != "" && wantHash == st.FailedHash {
						reason = "desired state previously failed"
					}
					logDecision(inst, "skip", reason)
					return
				}

				// ctx is cancelled by the shutdown signal; the pipeline stops at its
				// next step boundary and records "interrupted".
				deployCtx, deployCancel := deploy.WithStepTimeout(ctx, host.TimeoutDeploy(), "deploy")
				res, err := cfg.Deployer.Deploy(deployCtx, inst, want, deploy.Options{Actor: "agent"})
				if err != nil {
					if te := deploy.TimeoutError(deployCtx); te != nil {
						err = te
					}
					deployCancel()
					if ctx.Err() == nil {
						var outErr *output.Error
						if errors.As(err, &outErr) && outErr.Code == "E_INSTANCE_LOCKED" {
							locked = true
							firstSeen, ok := lockedFirstSeen[inst]
							if !ok {
								firstSeen = nowFn()
								lockedFirstSeen[inst] = firstSeen
							}
							if !nowFn().Before(firstSeen.Add(LockNoticeAfter)) {
								recordError(inst, err.Error())
							} else {
								delete(lastErrors, inst)
								logDecision(inst, "skip", "instance is locked")
							}
							return
						}
						recordError(inst, err.Error())
					}
					return
				}
				deployCancel()

				delete(lastErrors, inst)
				status := string(res.Status)
				if status == "" {
					status = "ok"
				}
				logDecision(inst, "deploy", status)
			}(inst)
		}
		if ctx.Err() == nil {
			for h := range regFirstSeen {
				if !regFailedThisTick[h] {
					delete(regFirstSeen, h)
					delete(regNotified, h)
				}
			}
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

// classifyFetchError reports whether a Desired fetch error is transient
// (network hiccup, rate limit, 5xx, timeout) and, when scoped to a registry,
// returns its host so failures coalesce across instances.
func classifyFetchError(err error) (transient bool, host string) {
	var oe *output.Error
	if !errors.As(err, &oe) {
		return false, ""
	}
	switch oe.Code {
	case "E_TIMEOUT":
		return true, ""
	case "E_REGISTRY_RESOLVE":
		if strings.HasPrefix(oe.Message, "cannot reach registry ") {
			if strings.Contains(oe.Hint, "syntax") {
				return false, ""
			}
			return true, strings.TrimSpace(strings.TrimPrefix(oe.Message, "cannot reach registry "))
		}
		if code, rest, ok := parseHTTPStatus(oe.Message); ok {
			if code == 429 || (code >= 500 && code <= 599) {
				return true, extractRegistryHost(rest)
			}
		}
		return false, ""
	case "E_INFRA_ARTIFACT":
		if code, rest, ok := parseHTTPStatus(oe.Message); ok {
			if code == 429 || (code >= 500 && code <= 599) {
				return true, extractRegistryHost(rest)
			}
			return false, ""
		}
		if strings.HasPrefix(oe.Message, "resolving ") || strings.HasPrefix(oe.Message, "pulling ") {
			return true, extractRegistryHost(oe.Message)
		}
		return false, ""
	default:
		return false, ""
	}
}

func parseHTTPStatus(msg string) (code int, rest string, ok bool) {
	s, found := strings.CutPrefix(msg, "HTTP ")
	if !found {
		return 0, "", false
	}
	codeStr, after, hasSpace := strings.Cut(s, " ")
	n, err := strconv.Atoi(codeStr)
	if err != nil || n < 100 || n > 599 {
		return 0, "", false
	}
	if hasSpace {
		rest = after
	}
	return n, rest, true
}

func extractRegistryHost(s string) string {
	s = strings.TrimPrefix(s, "resolving ")
	s = strings.TrimPrefix(s, "pulling ")
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	ref := strings.TrimSuffix(fields[0], ":")
	if idx := strings.IndexByte(ref, '@'); idx >= 0 {
		ref = ref[:idx]
	}
	host, _, ok := strings.Cut(ref, "/")
	if !ok {
		return ""
	}
	if host == "localhost" || strings.ContainsAny(host, ".:") {
		return host
	}
	return ""
}
