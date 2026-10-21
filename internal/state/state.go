package state

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

var deployOwner = sys.Owner{User: "deployer", Group: "deploy"}

type Status string

const (
	StatusOK        Status = "OK"
	StatusDeploying Status = "DEPLOYING"
	StatusBlocked   Status = "BLOCKED"
	StatusFailed    Status = "FAILED"
	StatusPaused    Status = "PAUSED"
)

type Desired struct {
	InfraCommit string `json:"infra_commit"`
	// InfraDigest is the contract artifact manifest digest of an OCI source.
	InfraDigest string            `json:"infra_digest,omitempty"`
	Digests     map[string]string `json:"digests,omitempty"`
}

type canonicalDesired struct {
	InfraCommit string `json:"infra_commit"`
	// omitempty keeps the hash of git-sourced states unchanged.
	InfraDigest string      `json:"infra_digest,omitempty"`
	Digests     [][2]string `json:"digests"`
}

func (d Desired) Hash() string {
	digests := make([][2]string, 0, len(d.Digests))
	for svc, dig := range d.Digests {
		digests = append(digests, [2]string{svc, dig})
	}
	slices.SortFunc(digests, func(a, b [2]string) int {
		return strings.Compare(a[0], b[0])
	})
	payload, _ := json.Marshal(canonicalDesired{
		InfraCommit: d.InfraCommit,
		InfraDigest: d.InfraDigest,
		Digests:     digests,
	})
	h := sha256.Sum256(payload)
	return hex.EncodeToString(h[:])
}

type State struct {
	Instance string `json:"instance"`
	Status   Status `json:"status"`
	// PriorStatus is the status before DEPLOYING, so an interrupted deploy can return to it.
	PriorStatus Status `json:"prior_status,omitempty"`
	// Promoted marks a DEPLOYING deploy past the point where files and
	// containers may differ from Current.
	Promoted       bool                    `json:"promoted,omitempty"`
	Current        *Desired                `json:"current,omitempty"`
	Previous       *Desired                `json:"previous,omitempty"`
	FailedHash     string                  `json:"failed_hash,omitempty"`
	Detail         string                  `json:"detail,omitempty"`
	Blocked        []string                `json:"blocked,omitempty"`
	PausedReason   string                  `json:"paused_reason,omitempty"`
	PausedWantHash string                  `json:"paused_want_hash,omitempty"`
	RollbackFailed bool                    `json:"rollback_failed,omitempty"`
	LastTransient  string                  `json:"last_transient,omitempty"`
	Backups        map[string]BackupStatus `json:"backups,omitempty"`
	UpdatedAt      time.Time               `json:"updated_at"`
}

// BackupStatus is the last known backup state of one data service. Errors are
// stored already redacted.
type BackupStatus struct {
	LastOK          time.Time `json:"last_ok,omitempty"`
	LastAttempt     time.Time `json:"last_attempt,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
	LastVerifyOK    time.Time `json:"last_verify_ok,omitempty"`
	LastVerifyError string    `json:"last_verify_error,omitempty"`
}

type Event struct {
	At     time.Time `json:"at"`
	ID     string    `json:"id"`
	Kind   string    `json:"kind"`
	From   *Desired  `json:"from,omitempty"`
	To     *Desired  `json:"to,omitempty"`
	Result string    `json:"result"`
	Detail string    `json:"detail,omitempty"`
	Actor  string    `json:"actor"`
	// BlueGreen lists the services swapped without downtime in this deploy.
	BlueGreen []string `json:"blue_green,omitempty"`
}

type Store struct {
	fs   sys.FS
	root string
}

func NewStore(fs sys.FS, root string) *Store {
	return &Store{fs: fs, root: root}
}

func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

func (s *Store) Load(inst string) (*State, error) {
	statePath := filepath.Join(s.root, inst, "state.json")
	data, err := s.fs.ReadFile(statePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &State{
				Instance: inst,
				Status:   StatusOK,
			}, nil
		}
		return nil, &output.Error{
			Code:    "E_STATE_IO",
			Message: fmt.Sprintf("cannot read state file %s: %v", statePath, err),
			Hint:    "check permissions on /var/lib/cvctl",
		}
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, &output.Error{
			Code:    "E_STATE_CORRUPT",
			Message: fmt.Sprintf("corrupt state file %s: %v", statePath, err),
			Hint:    "inspect or repair the state file",
		}
	}
	if st.Instance == "" {
		st.Instance = inst
	}
	return &st, nil
}

func (s *Store) Save(st *State) error {
	if st.Status != StatusDeploying {
		st.PriorStatus = ""
		st.Promoted = false
	}
	instDir := filepath.Join(s.root, st.Instance)
	if err := s.fs.MkdirAll(instDir, 0o750, deployOwner); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	statePath := filepath.Join(instDir, "state.json")
	return s.fs.WriteFile(statePath, data, 0o640, deployOwner)
}

const (
	maxDetailBytes   = 4096
	truncatedMarker  = "…[truncated]"
	historyLineLimit = 1 << 20
)

func newHistoryScanner(data []byte) *bufio.Scanner {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), historyLineLimit)
	return sc
}

func capDetail(d string) string {
	if len(d) <= maxDetailBytes {
		return d
	}
	cut := maxDetailBytes
	// do not split a multi-byte rune
	for cut > 0 && !utf8.RuneStart(d[cut]) {
		cut--
	}
	return d[:cut] + truncatedMarker
}

func (s *Store) Append(inst string, ev Event) error {
	ev.Detail = capDetail(ev.Detail)
	instDir := filepath.Join(s.root, inst)
	if err := s.fs.MkdirAll(instDir, 0o750, deployOwner); err != nil {
		return err
	}
	histPath := filepath.Join(instDir, "history.jsonl")
	var lines []string
	data, err := s.fs.ReadFile(histPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err == nil && len(data) > 0 {
		sc := newHistoryScanner(data)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line != "" {
				lines = append(lines, line)
			}
		}
		if scErr := sc.Err(); scErr != nil {
			return scErr
		}
	}

	evBytes, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	lines = append(lines, string(evBytes))
	if len(lines) > 500 {
		lines = lines[len(lines)-500:]
	}

	var buf bytes.Buffer
	for _, l := range lines {
		buf.WriteString(l)
		buf.WriteByte('\n')
	}
	return s.fs.WriteFile(histPath, buf.Bytes(), 0o640, deployOwner)
}

func (s *Store) History(inst string, n int) ([]Event, error) {
	histPath := filepath.Join(s.root, inst, "history.jsonl")
	data, err := s.fs.ReadFile(histPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []Event{}, nil
		}
		return nil, &output.Error{
			Code:    "E_STATE_IO",
			Message: fmt.Sprintf("cannot read history file %s: %v", histPath, err),
			Hint:    "check permissions on /var/lib/cvctl",
		}
	}
	var lines []string
	sc := newHistoryScanner(data)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	if scErr := sc.Err(); scErr != nil {
		return nil, scErr
	}

	var events []Event
	for i := len(lines) - 1; i >= 0; i-- {
		if n > 0 && len(events) >= n {
			break
		}
		var ev Event
		if err := json.Unmarshal([]byte(lines[i]), &ev); err != nil {
			return nil, &output.Error{
				Code:    "E_STATE_CORRUPT",
				Message: fmt.Sprintf("corrupt history file %s: %v", histPath, err),
				Hint:    "inspect or move the file",
			}
		}
		events = append(events, ev)
	}
	if events == nil {
		events = []Event{}
	}
	return events, nil
}

func (s *Store) Instances() ([]string, error) {
	entries, err := s.fs.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []string{}, nil
		}
		return nil, &output.Error{
			Code:    "E_STATE_IO",
			Message: fmt.Sprintf("cannot read directory %s: %v", s.root, err),
			Hint:    "check permissions on /var/lib/cvctl",
		}
	}
	var instances []string
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		stateFile := filepath.Join(s.root, ent.Name(), "state.json")
		fi, err := s.fs.Stat(stateFile)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, &output.Error{
				Code:    "E_STATE_IO",
				Message: fmt.Sprintf("cannot stat %s: %v", stateFile, err),
				Hint:    "check permissions on /var/lib/cvctl",
			}
		}
		if !fi.IsDir {
			instances = append(instances, ent.Name())
		}
	}
	slices.Sort(instances)
	if instances == nil {
		instances = []string{}
	}
	return instances, nil
}

// LockData takes the host-wide data lock that keeps backups, verifies and
// restores apart: wal-g retention could otherwise delete the base backup a
// restore is fetching. It fails at once with E_DATA_BUSY when held.
func (s *Store) LockData() (func(), error) {
	if err := s.fs.MkdirAll(s.root, 0o750, deployOwner); err != nil {
		return nil, &output.Error{
			Code:    "E_DATA_LOCK_FAILED",
			Message: fmt.Sprintf("cannot create %s: %v", s.root, err),
			Hint:    "check permissions on " + s.root,
		}
	}
	unlock, err := sys.LockFile(filepath.Join(s.root, "data.lock"), 0o640, deployOwner)
	if err != nil {
		if errors.Is(err, sys.ErrLocked) {
			return nil, &output.Error{
				Code:    "E_DATA_BUSY",
				Message: "another backup, verify or restore holds the data lock",
				Hint:    "a backup or verify is running; retry when it finishes",
			}
		}
		return nil, &output.Error{
			Code:    "E_DATA_LOCK_FAILED",
			Message: fmt.Sprintf("cannot take the data lock: %v", err),
			Hint:    "check permissions on " + s.root,
		}
	}
	return unlock, nil
}

func (s *Store) Lock(inst string) (func(), error) {
	instDir := filepath.Join(s.root, inst)
	if err := s.fs.MkdirAll(instDir, 0o750, deployOwner); err != nil {
		return nil, &output.Error{
			Code:    "E_INSTANCE_LOCK_FAILED",
			Message: fmt.Sprintf("cannot create instance directory %s: %v", instDir, err),
			Hint:    "check permissions on " + s.root,
		}
	}
	lockPath := filepath.Join(instDir, "lock")
	unlock, err := sys.LockFile(lockPath, 0o660, deployOwner)
	if err != nil {
		if errors.Is(err, sys.ErrLocked) {
			return nil, &output.Error{
				Code:    "E_INSTANCE_LOCKED",
				Message: fmt.Sprintf("instance %q is locked", inst),
				Hint:    fmt.Sprintf("another cvctl operation is running on %s; retry later", inst),
			}
		}
		return nil, &output.Error{
			Code:    "E_INSTANCE_LOCK_FAILED",
			Message: fmt.Sprintf("cannot lock instance %q: %v", inst, err),
			Hint:    "check permissions or filesystem status",
		}
	}
	return unlock, nil
}

// Interrupt returns an instance left in DEPLOYING to its last stable status
// and records an "interrupted" history entry. The status is the one held
// before the deploy started; when that is unknown it is OK if Current is set,
// else FAILED. A Promoted deploy always ends FAILED with an empty FailedHash
// and RollbackFailed set, so the next tick redeploys. The caller holds the
// instance lock.
func (s *Store) Interrupt(st *State, want *Desired, id, actor, detail string, now time.Time) error {
	prev := st.PriorStatus
	switch {
	case st.Promoted:
		// The new release may be half applied and Current was not restored:
		// force the next tick to redeploy.
		prev = StatusFailed
		st.FailedHash = ""
		st.RollbackFailed = true
	case prev == "" || prev == StatusDeploying:
		prev = StatusFailed
		if st.Current != nil {
			prev = StatusOK
		}
	}
	st.Status = prev
	st.UpdatedAt = now
	var errs []error
	if err := s.Save(st); err != nil {
		errs = append(errs, err)
	}
	if err := s.Append(st.Instance, Event{
		ID:     id,
		At:     now,
		Kind:   "interrupted",
		From:   st.Current,
		To:     want,
		Result: "interrupted",
		Detail: detail,
		Actor:  actor,
	}); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// RecoverInterrupted fixes an instance whose state is DEPLOYING because the
// process died mid-deploy. It skips instances whose lock is held: someone is
// deploying them right now.
func (s *Store) RecoverInterrupted(inst string, now time.Time) (bool, error) {
	if st, err := s.Load(inst); err != nil || st.Status != StatusDeploying {
		return false, err
	}
	unlock, err := s.Lock(inst)
	if err != nil {
		var oe *output.Error
		if errors.As(err, &oe) && oe.Code == "E_INSTANCE_LOCKED" {
			return false, nil
		}
		return false, err
	}
	defer unlock()
	st, err := s.Load(inst)
	if err != nil {
		return false, err
	}
	if st.Status != StatusDeploying {
		return false, nil
	}
	id := fmt.Sprintf("recover-%d", now.UnixNano())
	return true, s.Interrupt(st, nil, id, "agent", "interrupted: deploy was running when the agent stopped", now)
}
