package sys

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/codeavers/cvctl/internal/output"
)

type osFS struct{}

func NewOSFS() FS { return osFS{} }

func (osFS) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (osFS) Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (osFS) Mkdir(path string, mode os.FileMode, o Owner) error {
	uid, gid, err := resolveOwner(o)
	if err != nil {
		return err
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	return chown(path, uid, gid)
}

func (osFS) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

func (osFS) RemoveAll(path string) error { return os.RemoveAll(path) }

func (osFS) ReadDir(path string) ([]os.DirEntry, error) {
	return os.ReadDir(path)
}

func (osFS) Stat(path string) (FileInfo, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return FileInfo{}, &fs.PathError{Op: "stat", Path: path, Err: err}
	}
	fi := FileInfo{
		Mode:  os.FileMode(st.Mode & 0o7777),
		IsDir: st.Mode&syscall.S_IFMT == syscall.S_IFDIR,
		User:  strconv.Itoa(int(st.Uid)),
		Group: strconv.Itoa(int(st.Gid)),
	}
	if u, err := user.LookupId(fi.User); err == nil {
		fi.User = u.Username
	}
	if g, err := user.LookupGroupId(fi.Group); err == nil {
		fi.Group = g.Name
	}
	return fi, nil
}

func (osFS) Lstat(path string) (FileInfo, error) {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return FileInfo{}, &fs.PathError{Op: "lstat", Path: path, Err: err}
	}
	fi := FileInfo{
		Mode:      os.FileMode(st.Mode & 0o7777),
		IsDir:     st.Mode&syscall.S_IFMT == syscall.S_IFDIR,
		IsSymlink: st.Mode&syscall.S_IFMT == syscall.S_IFLNK,
		User:      strconv.Itoa(int(st.Uid)),
		Group:     strconv.Itoa(int(st.Gid)),
	}
	if u, err := user.LookupId(fi.User); err == nil {
		fi.User = u.Username
	}
	if g, err := user.LookupGroupId(fi.Group); err == nil {
		fi.Group = g.Name
	}
	return fi, nil
}

func (osFS) WriteFile(path string, data []byte, mode os.FileMode, o Owner) (err error) {
	uid, gid, err := resolveOwner(o)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = syscall.Fchmod(int(tmp.Fd()), uint32(mode)); err != nil {
		return err
	}
	if err = chown(tmp.Name(), uid, gid); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (osFS) MkdirAll(path string, mode os.FileMode, o Owner) error {
	uid, gid, err := resolveOwner(o)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	// Mkdir applies the umask and drops setgid/sticky, so set the mode explicitly.
	if err := syscall.Chmod(path, uint32(mode)); err != nil {
		return err
	}
	return chown(path, uid, gid)
}

// resolveOwner returns -1 for parts to leave unchanged. Unprivileged callers
// cannot chown, so lookups are skipped entirely (tests run unprivileged).
// LookupOwner checks the names even when unprivileged, where WriteFile would
// skip chown, so an import fails identically for every caller.
func (osFS) LookupOwner(o Owner) error {
	if o.User != "" {
		if _, err := user.Lookup(o.User); err != nil {
			return ownerErr("user", o.User, err)
		}
	}
	if o.Group != "" {
		if _, err := user.LookupGroup(o.Group); err != nil {
			return ownerErr("group", o.Group, err)
		}
	}
	return nil
}

func resolveOwner(o Owner) (uid, gid int, err error) {
	uid, gid = -1, -1
	if os.Geteuid() != 0 {
		return
	}
	if o.User != "" {
		u, e := user.Lookup(o.User)
		if e != nil {
			return 0, 0, ownerErr("user", o.User, e)
		}
		uid, _ = strconv.Atoi(u.Uid)
	}
	if o.Group != "" {
		g, e := user.LookupGroup(o.Group)
		if e != nil {
			return 0, 0, ownerErr("group", o.Group, e)
		}
		gid, _ = strconv.Atoi(g.Gid)
	}
	return
}

func ownerErr(kind, name string, cause error) error {
	return &output.Error{
		Code:    "E_SYS_OWNER_LOOKUP",
		Message: kind + " " + name + " not found: " + cause.Error(),
		Hint:    "create the " + kind + " first or fix the owner in the host config",
	}
}

func chown(path string, uid, gid int) error {
	if uid == -1 && gid == -1 {
		return nil
	}
	return os.Chown(path, uid, gid)
}
