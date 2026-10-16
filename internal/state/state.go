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
	InfraCommit string            `json:"infra_commit"`
	Digests     map[string]string `json:"digests,omitempty"`
}

type canonicalDesired struct {
	InfraCommit string      `json:"infra_commit"`
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
		Digests:     digests,
	})
	h := sha256.Sum256(payload)
	return hex.EncodeToString(h[:])
}

type State struct {
	Instance       string    `json:"instance"`
	Status         Status    `json:"status"`
	Current        *Desired  `json:"current,omitempty"`
	Previous       *Desired  `json:"previous,omitempty"`
	FailedHash     string    `json:"failed_hash,omitempty"`
	Blocked        []string  `json:"blocked,omitempty"`
	PausedReason   string    `json:"paused_reason,omitempty"`
	PausedWantHash string    `json:"paused_want_hash,omitempty"`
	RollbackFailed bool      `json:"rollback_failed,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
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
}

type Store struct {
	fs   sys.FS
	root string
}

func NewStore(fs sys.FS, root string) *Store {
	return &Store{fs: fs, root: root}
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

func (s *Store) Append(inst string, ev Event) error {
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
		sc := bufio.NewScanner(bytes.NewReader(data))
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
	sc := bufio.NewScanner(bytes.NewReader(data))
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
