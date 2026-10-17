package state

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func TestDesiredHashStable(t *testing.T) {
	d1 := Desired{
		InfraCommit: "a1b2c3d4e5",
		Digests: map[string]string{
			"web": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
			"api": "sha256:2222222222222222222222222222222222222222222222222222222222222222",
			"db":  "sha256:3333333333333333333333333333333333333333333333333333333333333333",
		},
	}
	d2 := Desired{
		InfraCommit: "a1b2c3d4e5",
		Digests: map[string]string{
			"db":  "sha256:3333333333333333333333333333333333333333333333333333333333333333",
			"web": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
			"api": "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		},
	}

	h1 := d1.Hash()
	h2 := d2.Hash()

	if h1 != h2 {
		t.Fatalf("expected stable hash regardless of map ordering: %q != %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("expected 64-char sha256 hex string, got %d chars: %s", len(h1), h1)
	}

	dNil := Desired{InfraCommit: "a1b2c3d4e5", Digests: nil}
	dEmpty := Desired{InfraCommit: "a1b2c3d4e5", Digests: map[string]string{}}
	if dNil.Hash() != dEmpty.Hash() {
		t.Fatalf("expected nil and empty digests to produce same hash: %q != %q", dNil.Hash(), dEmpty.Hash())
	}

	dDiffCommit := Desired{InfraCommit: "different", Digests: d1.Digests}
	if dDiffCommit.Hash() == h1 {
		t.Fatalf("expected different commit to produce different hash")
	}

	dDiffDigest := Desired{
		InfraCommit: "a1b2c3d4e5",
		Digests: map[string]string{
			"web": "sha256:9999999999999999999999999999999999999999999999999999999999999999",
		},
	}
	if dDiffDigest.Hash() == h1 {
		t.Fatalf("expected different digest to produce different hash")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	fs := systest.NewFS()
	store := NewStore(fs, "/var/lib/cvctl")

	now := time.Date(2026, 10, 16, 14, 30, 0, 0, time.UTC)
	st := &State{
		Instance: "app-prod",
		Status:   StatusDeploying,
		Current: &Desired{
			InfraCommit: "commit-curr",
			Digests:     map[string]string{"web": "sha256:111"},
		},
		Previous: &Desired{
			InfraCommit: "commit-prev",
			Digests:     map[string]string{"web": "sha256:000"},
		},
		FailedHash:     "failedhash123",
		Blocked:        []string{"MISSING_KEY", "ANOTHER_KEY"},
		PausedReason:   "maintenance in progress",
		PausedWantHash: "hash-to-skip",
		RollbackFailed: false,
		UpdatedAt:      now,
	}

	if err := store.Save(st); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	files := fs.Files()
	dirInfo, ok := files["/var/lib/cvctl/app-prod"]
	if !ok {
		t.Fatalf("expected /var/lib/cvctl/app-prod directory to exist")
	}
	if !dirInfo.IsDir || dirInfo.Mode != 0o750 || dirInfo.User != "deployer" || dirInfo.Group != "deploy" {
		t.Fatalf("unexpected dir metadata: %+v", dirInfo)
	}

	fileInfo, ok := files["/var/lib/cvctl/app-prod/state.json"]
	if !ok {
		t.Fatalf("expected state.json to exist")
	}
	if fileInfo.IsDir || fileInfo.Mode != 0o640 || fileInfo.User != "deployer" || fileInfo.Group != "deploy" {
		t.Fatalf("unexpected state.json metadata: %+v", fileInfo)
	}

	loaded, err := store.Load("app-prod")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if loaded.Instance != st.Instance {
		t.Errorf("Instance: got %q, want %q", loaded.Instance, st.Instance)
	}
	if loaded.Status != st.Status {
		t.Errorf("Status: got %q, want %q", loaded.Status, st.Status)
	}
	if loaded.Current == nil || loaded.Current.InfraCommit != "commit-curr" || loaded.Current.Digests["web"] != "sha256:111" {
		t.Errorf("Current: unexpected %+v", loaded.Current)
	}
	if loaded.Previous == nil || loaded.Previous.InfraCommit != "commit-prev" || loaded.Previous.Digests["web"] != "sha256:000" {
		t.Errorf("Previous: unexpected %+v", loaded.Previous)
	}
	if loaded.FailedHash != st.FailedHash {
		t.Errorf("FailedHash: got %q, want %q", loaded.FailedHash, st.FailedHash)
	}
	if len(loaded.Blocked) != 2 || loaded.Blocked[0] != "MISSING_KEY" || loaded.Blocked[1] != "ANOTHER_KEY" {
		t.Errorf("Blocked: unexpected %v", loaded.Blocked)
	}
	if loaded.PausedReason != st.PausedReason {
		t.Errorf("PausedReason: got %q, want %q", loaded.PausedReason, st.PausedReason)
	}
	if loaded.PausedWantHash != st.PausedWantHash {
		t.Errorf("PausedWantHash: got %q, want %q", loaded.PausedWantHash, st.PausedWantHash)
	}
	if loaded.RollbackFailed != st.RollbackFailed {
		t.Errorf("RollbackFailed: got %v, want %v", loaded.RollbackFailed, st.RollbackFailed)
	}
	if !loaded.UpdatedAt.Equal(st.UpdatedAt) {
		t.Errorf("UpdatedAt: got %v, want %v", loaded.UpdatedAt, st.UpdatedAt)
	}
}

func TestHistoryNewestFirstAndCap(t *testing.T) {
	fs := systest.NewFS()
	store := NewStore(fs, "/var/lib/cvctl")

	baseTime := time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 510; i++ {
		ev := Event{
			At:     baseTime.Add(time.Duration(i) * time.Minute),
			ID:     fmt.Sprintf("deploy-%d", i),
			Kind:   "deploy",
			Result: "OK",
			Detail: fmt.Sprintf("deploy iteration %d", i),
			Actor:  "agent",
		}
		if err := store.Append("test-app", ev); err != nil {
			t.Fatalf("Append %d failed: %v", i, err)
		}
	}

	hist, err := store.History("test-app", 0)
	if err != nil {
		t.Fatalf("History failed: %v", err)
	}
	if len(hist) != 500 {
		t.Fatalf("expected history capped at 500, got %d", len(hist))
	}

	// Newest first:
	if hist[0].ID != "deploy-509" {
		t.Errorf("expected newest event deploy-509 at index 0, got %s", hist[0].ID)
	}
	if hist[499].ID != "deploy-10" {
		t.Errorf("expected oldest retained event deploy-10 at index 499, got %s", hist[499].ID)
	}
	if !hist[0].At.After(hist[1].At) {
		t.Errorf("expected strictly decreasing order: %v not after %v", hist[0].At, hist[1].At)
	}

	// Limited history query
	limited, err := store.History("test-app", 5)
	if err != nil {
		t.Fatalf("History(5) failed: %v", err)
	}
	if len(limited) != 5 {
		t.Fatalf("expected 5 events, got %d", len(limited))
	}
	if limited[0].ID != "deploy-509" || limited[4].ID != "deploy-505" {
		t.Errorf("unexpected limited history IDs: %s .. %s", limited[0].ID, limited[4].ID)
	}
}

func TestLockHeldReturnsInstanceLocked(t *testing.T) {
	fs := systest.NewFS()
	store := NewStore(fs, "/var/lib/cvctl")

	fl := systest.NewFakeLocker()
	oldLockFile := sys.LockFile
	t.Cleanup(func() { sys.LockFile = oldLockFile })
	sys.LockFile = fl.LockFile

	unlock1, err := store.Lock("my-app")
	if err != nil {
		t.Fatalf("first Lock failed: %v", err)
	}

	// Check instance dir creation
	files := fs.Files()
	dirInfo, ok := files["/var/lib/cvctl/my-app"]
	if !ok || !dirInfo.IsDir || dirInfo.Mode != 0o750 || dirInfo.User != "deployer" || dirInfo.Group != "deploy" {
		t.Fatalf("unexpected dir metadata: %+v", dirInfo)
	}

	// Second lock while held
	_, err2 := store.Lock("my-app")
	if err2 == nil {
		t.Fatalf("expected second lock to fail with E_INSTANCE_LOCKED, got nil error")
	}

	var oe *output.Error
	if !errors.As(err2, &oe) {
		t.Fatalf("expected *output.Error, got %T: %v", err2, err2)
	}
	if oe.Code != "E_INSTANCE_LOCKED" {
		t.Errorf("expected code E_INSTANCE_LOCKED, got %s", oe.Code)
	}
	expectedHint := "another cvctl operation is running on my-app; retry later"
	if oe.Hint != expectedHint {
		t.Errorf("expected hint %q, got %q", expectedHint, oe.Hint)
	}

	unlock1()

	unlock3, err3 := store.Lock("my-app")
	if err3 != nil {
		t.Fatalf("third Lock after unlock failed: %v", err3)
	}
	unlock3()
}

func TestLoadMissingDefaultsOK(t *testing.T) {
	fs := systest.NewFS()
	store := NewStore(fs, "/var/lib/cvctl")

	st, err := store.Load("non-existent")
	if err != nil {
		t.Fatalf("Load missing returned error: %v", err)
	}
	if st == nil {
		t.Fatalf("Load returned nil state")
	}
	if st.Instance != "non-existent" {
		t.Errorf("Instance: got %q, want 'non-existent'", st.Instance)
	}
	if st.Status != StatusOK {
		t.Errorf("Status: got %q, want %q", st.Status, StatusOK)
	}
	if st.Current != nil {
		t.Errorf("expected Current to be nil, got %+v", st.Current)
	}
}

func TestStateCorruptReturnsError(t *testing.T) {
	fs := systest.NewFS()
	store := NewStore(fs, "/var/lib/cvctl")

	badPath := "/var/lib/cvctl/broken/state.json"
	_ = fs.MkdirAll("/var/lib/cvctl/broken", 0o750, sys.Owner{User: "deployer", Group: "deploy"})
	_ = fs.WriteFile(badPath, []byte("{invalid json"), 0o640, sys.Owner{User: "deployer", Group: "deploy"})

	_, err := store.Load("broken")
	if err == nil {
		t.Fatalf("expected error loading corrupted state, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if oe.Code != "E_STATE_CORRUPT" {
		t.Errorf("expected code E_STATE_CORRUPT, got %s", oe.Code)
	}
	if !strings.Contains(oe.Message, badPath) {
		t.Errorf("expected error message to name the corrupt file %s, got: %s", badPath, oe.Message)
	}
}

func TestLockOtherErrorReturnsLockFailed(t *testing.T) {
	fs := systest.NewFS()
	store := NewStore(fs, "/var/lib/cvctl")

	oldLockFile := sys.LockFile
	t.Cleanup(func() { sys.LockFile = oldLockFile })
	sys.LockFile = func(p string, mode os.FileMode, o sys.Owner) (func(), error) {
		return nil, errors.New("permission denied")
	}

	_, err := store.Lock("failing-app")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if oe.Code != "E_INSTANCE_LOCK_FAILED" {
		t.Errorf("expected code E_INSTANCE_LOCK_FAILED, got %s", oe.Code)
	}
}

func TestInstances(t *testing.T) {
	fs := systest.NewFS()
	store := NewStore(fs, "/var/lib/cvctl")

	// Missing root returns empty list, no error
	list, err := store.Instances()
	if err != nil {
		t.Fatalf("expected no error for missing root, got: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty list, got: %v", list)
	}

	// Create instances
	st1 := &State{Instance: "beta", Status: StatusOK}
	st2 := &State{Instance: "alpha", Status: StatusOK}
	if err := store.Save(st1); err != nil {
		t.Fatalf("Save st1: %v", err)
	}
	if err := store.Save(st2); err != nil {
		t.Fatalf("Save st2: %v", err)
	}

	// Add directory without state.json
	_ = fs.MkdirAll("/var/lib/cvctl/gamma", 0o750, sys.Owner{User: "deployer", Group: "deploy"})
	// Add plain file in root
	_ = fs.WriteFile("/var/lib/cvctl/somefile.txt", []byte("hello"), 0o640, sys.Owner{User: "deployer", Group: "deploy"})

	list, err = store.Instances()
	if err != nil {
		t.Fatalf("Instances failed: %v", err)
	}
	if len(list) != 2 || list[0] != "alpha" || list[1] != "beta" {
		t.Fatalf("expected sorted instances [alpha beta], got: %v", list)
	}
}

func TestLockFileOwnershipDeployer(t *testing.T) {
	fs := systest.NewFS()
	store := NewStore(fs, "/var/lib/cvctl")

	fl := systest.NewFakeLocker()
	oldLockFile := sys.LockFile
	t.Cleanup(func() { sys.LockFile = oldLockFile })
	sys.LockFile = fl.LockFile

	unlock, err := store.Lock("my-app")
	if err != nil {
		t.Fatalf("Lock failed: %v", err)
	}
	defer unlock()

	if len(fl.Calls) != 1 {
		t.Fatalf("expected 1 call to LockFile, got %d", len(fl.Calls))
	}
	call := fl.Calls[0]
	if call.Path != "/var/lib/cvctl/my-app/lock" {
		t.Errorf("expected path /var/lib/cvctl/my-app/lock, got %s", call.Path)
	}
	if call.Mode != 0o660 {
		t.Errorf("expected mode 0660, got %o", call.Mode)
	}
	if call.Owner.User != "deployer" || call.Owner.Group != "deploy" {
		t.Errorf("expected owner deployer:deploy, got %s:%s", call.Owner.User, call.Owner.Group)
	}
}

func TestDesiredHashAmbiguity(t *testing.T) {
	d1 := Desired{
		InfraCommit: "commit1",
		Digests: map[string]string{
			"a=b": "c",
		},
	}
	d2 := Desired{
		InfraCommit: "commit1",
		Digests: map[string]string{
			"a": "b=c",
		},
	}
	if d1.Hash() == d2.Hash() {
		t.Fatalf("expected different hashes for %v and %v, got same: %s", d1.Digests, d2.Digests, d1.Hash())
	}

	dNil := Desired{InfraCommit: "commit1", Digests: nil}
	dEmpty := Desired{InfraCommit: "commit1", Digests: map[string]string{}}
	if dNil.Hash() != dEmpty.Hash() {
		t.Fatalf("expected nil and empty digests to produce same hash: %q != %q", dNil.Hash(), dEmpty.Hash())
	}
}

func TestHistoryCorruptLine(t *testing.T) {
	fs := systest.NewFS()
	store := NewStore(fs, "/var/lib/cvctl")

	histPath := "/var/lib/cvctl/my-app/history.jsonl"
	_ = fs.MkdirAll("/var/lib/cvctl/my-app", 0o750, sys.Owner{User: "deployer", Group: "deploy"})
	_ = fs.WriteFile(histPath, []byte("{\"at\":\"2026-10-16T10:00:00Z\",\"id\":\"deploy-1\"}\n{not valid json\n"), 0o640, sys.Owner{User: "deployer", Group: "deploy"})

	_, err := store.History("my-app", 0)
	if err == nil {
		t.Fatalf("expected error on corrupt history line, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if oe.Code != "E_STATE_CORRUPT" {
		t.Errorf("expected code E_STATE_CORRUPT, got %s", oe.Code)
	}
	if !strings.Contains(oe.Message, "history.jsonl") {
		t.Errorf("expected error message to name history.jsonl, got %s", oe.Message)
	}
	if oe.Hint != "inspect or move the file" {
		t.Errorf("expected hint 'inspect or move the file', got %q", oe.Hint)
	}
}

type failingFS struct {
	*systest.FS
	readErr    error
	readDirErr error
}

func (f *failingFS) ReadFile(p string) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.FS.ReadFile(p)
}

func (f *failingFS) ReadDir(p string) ([]os.DirEntry, error) {
	if f.readDirErr != nil {
		return nil, f.readDirErr
	}
	return f.FS.ReadDir(p)
}

func TestStoreFSErrorsWrappedInStateIO(t *testing.T) {
	fakeFS := &failingFS{
		FS:      systest.NewFS(),
		readErr: errors.New("disk read error"),
	}
	store := NewStore(fakeFS, "/var/lib/cvctl")

	_, err := store.Load("my-app")
	if err == nil {
		t.Fatalf("expected error from Load on FS error, got nil")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if oe.Code != "E_STATE_IO" {
		t.Errorf("expected code E_STATE_IO, got %s", oe.Code)
	}
	expectedPath := "/var/lib/cvctl/my-app/state.json"
	if !strings.Contains(oe.Message, expectedPath) {
		t.Errorf("expected message to contain path %s, got: %s", expectedPath, oe.Message)
	}
	if oe.Hint != "check permissions on /var/lib/cvctl" {
		t.Errorf("expected hint 'check permissions on /var/lib/cvctl', got %q", oe.Hint)
	}

	fakeFS.readErr = nil
	fakeFS.readDirErr = errors.New("permission denied")
	_, err = store.Instances()
	if err == nil {
		t.Fatalf("expected error from Instances on FS error, got nil")
	}
	if !errors.As(err, &oe) {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if oe.Code != "E_STATE_IO" {
		t.Errorf("expected code E_STATE_IO, got %s", oe.Code)
	}
	if !strings.Contains(oe.Message, "/var/lib/cvctl") {
		t.Errorf("expected message to contain root /var/lib/cvctl, got: %s", oe.Message)
	}
	if oe.Hint != "check permissions on /var/lib/cvctl" {
		t.Errorf("expected hint 'check permissions on /var/lib/cvctl', got %q", oe.Hint)
	}
}

func TestAppendCapsHugeDetail(t *testing.T) {
	store := NewStore(systest.NewFS(), "/var/lib/cvctl")
	big := strings.Repeat("x", 100*1024)
	if err := store.Append("app", Event{ID: "a", Kind: "failed", Detail: big}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.Append("app", Event{ID: "b", Kind: "deploy", Result: "OK"}); err != nil {
		t.Fatalf("second Append: %v", err)
	}
	hist, err := store.History("app", 0)
	if err != nil || len(hist) != 2 {
		t.Fatalf("History: %v (%d events)", err, len(hist))
	}
	d := hist[1].Detail
	if len(d) > 4096+len("…[truncated]") || !strings.HasSuffix(d, "…[truncated]") {
		t.Fatalf("detail not capped: len %d", len(d))
	}
}

func TestHistoryReadsLongLines(t *testing.T) {
	fs := systest.NewFS()
	line := `{"id":"a","kind":"deploy","result":"OK","detail":"` + strings.Repeat("y", 200*1024) + `"}` + "\n"
	if err := fs.WriteFile("/var/lib/cvctl/app/history.jsonl", []byte(line), 0o640, sys.Owner{}); err != nil {
		t.Fatal(err)
	}
	store := NewStore(fs, "/var/lib/cvctl")
	if hist, err := store.History("app", 0); err != nil || len(hist) != 1 {
		t.Fatalf("History: %v %d", err, len(hist))
	}
	if err := store.Append("app", Event{ID: "b", Kind: "deploy"}); err != nil {
		t.Fatalf("Append over long line: %v", err)
	}
}

func TestLastTransientRoundTrip(t *testing.T) {
	store := NewStore(systest.NewFS(), "/var/lib/cvctl")
	if err := store.Save(&State{Instance: "app", Status: StatusFailed, LastTransient: "pull timeout"}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Load("app")
	if err != nil || st.LastTransient != "pull timeout" {
		t.Fatalf("got %+v %v", st, err)
	}
}

func TestLockDataIsHostWideAndFailsBusy(t *testing.T) {
	store := NewStore(systest.NewFS(), "/var/lib/cvctl")
	fl := systest.NewFakeLocker()
	oldLockFile := sys.LockFile
	t.Cleanup(func() { sys.LockFile = oldLockFile })
	sys.LockFile = fl.LockFile

	unlock, err := store.LockData()
	if err != nil {
		t.Fatal(err)
	}
	c := fl.Calls[0]
	if c.Path != "/var/lib/cvctl/data.lock" || c.Mode != 0o640 || c.Owner != (sys.Owner{User: "deployer", Group: "deploy"}) {
		t.Fatalf("lock call: %+v", c)
	}
	_, err = store.LockData()
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_DATA_BUSY" || !strings.Contains(oe.Hint, "a backup or verify is running; retry when it finishes") {
		t.Fatalf("err=%v", err)
	}
	unlock()
	if _, err := store.LockData(); err != nil {
		t.Fatalf("after unlock: %v", err)
	}
}
