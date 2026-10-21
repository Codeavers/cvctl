// Package secrets builds and restores the age-encrypted bundle of every
// host and instance secret. Plaintext only ever lives in memory.
package secrets

import (
	"archive/tar"
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

const (
	hostEnvPath = "/etc/cvctl/host.env"
	secretsDir  = "/etc/cvctl/secrets"
	deployDir   = "/srv/deploy"
	stateDir    = "/var/lib/cvctl"

	// maxEntrySize bounds a single decrypted entry so a hostile bundle cannot
	// exhaust memory.
	maxEntrySize = 4 << 20
	maxEntries   = 4096
	// maxTotalSize bounds the whole decrypted bundle.
	maxTotalSize = 64 << 20
)

var (
	instRe       = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	secretNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	instFileRe   = regexp.MustCompile(`^(cvctl\.env|app\.env|backup-[A-Za-z0-9._-]+\.env|restic-[A-Za-z0-9._-]+\.env)$`)
	keyFileRe    = regexp.MustCompile(`^deploy_key(\.pub)?$`)
)

// Entry is one file of the bundle. Path is the logical absolute path, without
// any test root prefix.
type Entry struct {
	Path        string
	Mode        os.FileMode
	User, Group string
	Data        []byte
}

// Allowed reports whether p is one of the paths a bundle may contain.
func Allowed(p string) bool {
	if !strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	if p == hostEnvPath {
		return true
	}
	dir, name := path.Split(p)
	dir = strings.TrimSuffix(dir, "/")
	switch {
	case dir == secretsDir:
		return secretNameRe.MatchString(name)
	case path.Dir(dir) == deployDir:
		return instRe.MatchString(path.Base(dir)) && instFileRe.MatchString(name)
	case path.Dir(dir) == stateDir:
		return instRe.MatchString(path.Base(dir)) && keyFileRe.MatchString(name)
	}
	return false
}

// Collect gathers every bundle file under root ("" for the real system).
// Symlinks and non-regular files are skipped with a warning.
func Collect(fsys sys.FS, root string) ([]Entry, []string, error) {
	c := collector{fsys: fsys, root: strings.TrimRight(root, "/")}
	c.file(hostEnvPath)
	c.dirFiles(secretsDir, secretNameRe.MatchString)
	for _, inst := range c.subdirs(deployDir) {
		c.dirFiles(path.Join(deployDir, inst), instFileRe.MatchString)
	}
	for _, inst := range c.subdirs(stateDir) {
		c.dirFiles(path.Join(stateDir, inst), keyFileRe.MatchString)
	}
	if c.err != nil {
		return nil, c.warns, c.err
	}
	sort.Slice(c.entries, func(i, j int) bool { return c.entries[i].Path < c.entries[j].Path })
	return c.entries, c.warns, nil
}

// CollectHost gathers only host.env and the files under /etc/cvctl/secrets/.
func CollectHost(fsys sys.FS, root string) ([]Entry, []string, error) {
	c := collector{fsys: fsys, root: strings.TrimRight(root, "/")}
	c.file(hostEnvPath)
	c.dirFiles(secretsDir, secretNameRe.MatchString)
	if c.err != nil {
		return nil, c.warns, c.err
	}
	sort.Slice(c.entries, func(i, j int) bool { return c.entries[i].Path < c.entries[j].Path })
	return c.entries, c.warns, nil
}

// CollectInstance gathers only the files under /srv/deploy/<inst>/ that Collect
// would include.
func CollectInstance(fsys sys.FS, root, inst string) ([]Entry, []string, error) {
	if !instRe.MatchString(inst) {
		return nil, nil, &output.Error{
			Code:    "E_INSTANCE_NAME",
			Message: fmt.Sprintf("invalid instance name %q", inst),
			Hint:    "instance names must contain only lowercase letters, digits, and single hyphens",
		}
	}
	r := strings.TrimRight(root, "/")
	instDir := path.Join(deployDir, inst)
	fi, err := fsys.Lstat(r + instDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, &output.Error{
			Code:    "E_SECRETS_NO_INSTANCE",
			Message: fmt.Sprintf("instance %q not found under %s", inst, deployDir),
			Hint:    "check the instance name",
		}
	}
	if err != nil {
		return nil, nil, &output.Error{
			Code:    "E_SECRETS_READ",
			Message: fmt.Sprintf("cannot read %s: %v", instDir, err),
			Hint:    "run as root (sudo cvctl secrets export)",
		}
	}
	if fi.IsSymlink || !fi.IsDir {
		if fi.IsSymlink {
			return nil, nil, &output.Error{
				Code:    "E_SECRETS_UNSAFE_PATH",
				Message: fmt.Sprintf("%s is a symlink", instDir),
				Hint:    "instance directories must be real directories",
			}
		}
		return nil, nil, &output.Error{
			Code:    "E_SECRETS_NO_INSTANCE",
			Message: fmt.Sprintf("instance directory %s is not a directory", instDir),
			Hint:    "check the instance name",
		}
	}
	c := collector{fsys: fsys, root: r}
	c.dirFiles(instDir, instFileRe.MatchString)
	if c.err != nil {
		return nil, c.warns, c.err
	}
	sort.Slice(c.entries, func(i, j int) bool { return c.entries[i].Path < c.entries[j].Path })
	return c.entries, c.warns, nil
}

type collector struct {
	fsys    sys.FS
	root    string
	entries []Entry
	warns   []string
	err     error
}

func (c *collector) real(p string) string { return c.root + p }

func (c *collector) fail(p string, err error) {
	if c.err == nil {
		c.err = &output.Error{
			Code:    "E_SECRETS_READ",
			Message: fmt.Sprintf("cannot read %s: %v", p, err),
			Hint:    "run as root (sudo cvctl secrets export)",
		}
	}
}

func (c *collector) file(p string) {
	fi, err := c.fsys.Lstat(c.real(p))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		c.fail(p, err)
		return
	}
	if fi.IsSymlink || fi.IsDir {
		c.warns = append(c.warns, "skipped "+p+": not a regular file")
		return
	}
	data, err := c.fsys.ReadFile(c.real(p))
	if err != nil {
		c.fail(p, err)
		return
	}
	if numeric(fi.User) || numeric(fi.Group) {
		c.warns = append(c.warns, p+": owner has no name on this host (numeric id); it may not resolve on import")
	}
	c.entries = append(c.entries, Entry{Path: p, Mode: fi.Mode, User: fi.User, Group: fi.Group, Data: data})
}

func numeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (c *collector) readDir(dir string) []os.DirEntry {
	ents, err := c.fsys.ReadDir(c.real(dir))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.fail(dir, err)
		}
		return nil
	}
	return ents
}

func (c *collector) dirFiles(dir string, match func(string) bool) {
	for _, e := range c.readDir(dir) {
		if match(e.Name()) {
			c.file(path.Join(dir, e.Name()))
		}
	}
}

func (c *collector) subdirs(dir string) []string {
	var out []string
	for _, e := range c.readDir(dir) {
		if !instRe.MatchString(e.Name()) {
			continue
		}
		p := path.Join(dir, e.Name())
		fi, err := c.fsys.Lstat(c.real(p))
		if err != nil {
			c.fail(p, err)
			continue
		}
		if fi.IsSymlink || !fi.IsDir {
			if fi.IsSymlink {
				c.warns = append(c.warns, "skipped "+p+": symlink")
			}
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

// Pack writes entries as an uncompressed tar archive. Every entry is validated
// against Allowed and the size limits before writing.
func Pack(entries []Entry) ([]byte, error) {
	if len(entries) > maxEntries {
		return nil, corrupt()
	}
	seen := map[string]bool{}
	total := 0
	for _, e := range entries {
		if !Allowed(e.Path) {
			return nil, invalidEntry(e.Path, "path not allowed")
		}
		if seen[e.Path] {
			return nil, invalidEntry(e.Path, "duplicate entry")
		}
		seen[e.Path] = true
		if len(e.Data) > maxEntrySize {
			return nil, corrupt()
		}
		total += len(e.Data)
		if total > maxTotalSize {
			return nil, &output.Error{
				Code:    "E_SECRETS_TOO_LARGE",
				Message: "the bundle content exceeds the size limit",
				Hint:    "a cvctl secrets bundle is far smaller; check the file",
			}
		}
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     e.Path,
			Mode:     int64(e.Mode),
			Size:     int64(len(e.Data)),
			Uname:    e.User,
			Gname:    e.Group,
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if _, err := tw.Write(e.Data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Unpack decodes an uncompressed tar archive into entries after validating every
// path against Allowed and checking the size limits.
func Unpack(b []byte) ([]Entry, error) {
	// Callers outside Decrypt (restic dump) hand over raw bytes; bound them here too.
	if len(b) > maxTotalSize+maxEntries*3*512 {
		return nil, corrupt()
	}
	tr := tar.NewReader(bytes.NewReader(b))
	var out []Entry
	seen := map[string]bool{}
	total := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, corrupt()
		}
		if len(out) >= maxEntries {
			return nil, corrupt()
		}
		if h.Typeflag != tar.TypeReg {
			return nil, invalidEntry(h.Name, "not a regular file")
		}
		if !Allowed(h.Name) {
			return nil, invalidEntry(h.Name, "path not allowed")
		}
		if seen[h.Name] {
			return nil, invalidEntry(h.Name, "duplicate entry")
		}
		seen[h.Name] = true
		data, err := io.ReadAll(io.LimitReader(tr, maxEntrySize+1))
		if err != nil || len(data) > maxEntrySize {
			return nil, corrupt()
		}
		total += len(data)
		if total > maxTotalSize {
			return nil, &output.Error{
				Code:    "E_SECRETS_TOO_LARGE",
				Message: "the bundle content exceeds the size limit",
				Hint:    "a cvctl secrets bundle is far smaller; check the file",
			}
		}
		out = append(out, Entry{
			Path: h.Name, Mode: os.FileMode(h.Mode & 0o777),
			User: h.Uname, Group: h.Gname, Data: data,
		})
	}
	return out, nil
}

// Encrypt writes entries as an age-encrypted bundle (binary or ASCII armor)
// encrypted with passphrase.
func Encrypt(w io.Writer, entries []Entry, passphrase string, asciiArmor bool) error {
	if passphrase == "" {
		return emptyPassphrase()
	}
	raw, err := Pack(entries)
	if err != nil {
		return err
	}
	rcpt, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return &output.Error{Code: "E_SECRETS_PASSPHRASE", Message: "invalid passphrase", Hint: "use a different passphrase"}
	}
	var aw io.WriteCloser
	if asciiArmor {
		aw = armor.NewWriter(w)
		w = aw
	}
	ew, err := age.Encrypt(w, rcpt)
	if err != nil {
		return encryptErr(err)
	}
	if _, err := ew.Write(raw); err != nil {
		return encryptErr(err)
	}
	if err := ew.Close(); err != nil {
		return encryptErr(err)
	}
	if aw != nil {
		if err := aw.Close(); err != nil {
			return encryptErr(err)
		}
	}
	return nil
}

func encryptErr(err error) error {
	return &output.Error{Code: "E_SECRETS_ENCRYPT", Message: "cannot build the encrypted bundle: " + err.Error(), Hint: "retry; no file was written"}
}

func emptyPassphrase() error {
	return &output.Error{
		Code:    "E_SECRETS_PASSPHRASE",
		Message: "the passphrase is empty",
		Hint:    "type a passphrase, or set CVCTL_SECRETS_PASSPHRASE for non-interactive use",
	}
}

// Decrypt reads an age bundle (binary or ASCII armor) and returns its
// entries after validating every path; nothing is written here.
func Decrypt(r io.Reader, passphrase string) ([]Entry, error) {
	if passphrase == "" {
		return nil, emptyPassphrase()
	}
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, &output.Error{Code: "E_SECRETS_PASSPHRASE", Message: "invalid passphrase", Hint: "use a different passphrase"}
	}
	br := bufio.NewReader(r)
	var src io.Reader = br
	if head, _ := br.Peek(len(armor.Header)); string(head) == armor.Header {
		src = armor.NewReader(br)
	}
	dr, err := age.Decrypt(src, id)
	if err != nil {
		return nil, &output.Error{
			Code:    "E_SECRETS_DECRYPT",
			Message: "cannot decrypt the bundle",
			Hint:    "check the passphrase and that the file is a cvctl secrets bundle",
		}
	}
	tarBytes, err := io.ReadAll(io.LimitReader(dr, maxTotalSize+(maxEntries*2048)+1))
	if err != nil {
		return nil, corrupt()
	}
	if len(tarBytes) > maxTotalSize+(maxEntries*2048) {
		return nil, &output.Error{
			Code:    "E_SECRETS_TOO_LARGE",
			Message: "the bundle content exceeds the size limit",
			Hint:    "a cvctl secrets bundle is far smaller; check the file",
		}
	}
	return Unpack(tarBytes)
}

func corrupt() error {
	return &output.Error{Code: "E_SECRETS_CORRUPT", Message: "the bundle content is malformed", Hint: "re-create the bundle with cvctl secrets export"}
}

func invalidEntry(name, why string) error {
	return &output.Error{
		Code:    "E_SECRETS_ENTRY_INVALID",
		Message: fmt.Sprintf("bundle entry %q rejected: %s", name, why),
		Hint:    "the bundle was not produced by cvctl secrets export; nothing was written",
	}
}

// Restore writes entries under root. Everything is checked before the first
// write so a refusal leaves the system untouched.
func Restore(fsys sys.FS, root string, entries []Entry, force bool) ([]string, error) {
	root = strings.TrimRight(root, "/")
	var existing []string
	for _, e := range entries {
		if !Allowed(e.Path) {
			return nil, invalidEntry(e.Path, "path not allowed")
		}
		fi, err := fsys.Lstat(root + e.Path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, &output.Error{Code: "E_SECRETS_WRITE", Message: fmt.Sprintf("cannot inspect %s: %v", e.Path, err), Hint: "run as root (sudo cvctl secrets import)"}
		case fi.IsSymlink || fi.IsDir:
			return nil, invalidEntry(e.Path, "destination is a symlink or directory")
		default:
			existing = append(existing, e.Path)
		}
	}
	if len(existing) > 0 && !force {
		return nil, &output.Error{
			Code:    "E_SECRETS_EXISTS",
			Message: "files already exist: " + strings.Join(existing, ", "),
			Hint:    "re-run with --force to overwrite them",
		}
	}
	owners := map[sys.Owner]bool{}
	instDirs := map[string]os.FileMode{}
	for _, e := range entries {
		owners[sys.Owner{User: e.User, Group: e.Group}] = true
		if dir, mode, ok := instanceDir(e.Path); ok {
			instDirs[dir] = mode
			owners[deployOwner] = true
		}
		if err := checkParents(fsys, root, e.Path); err != nil {
			return nil, err
		}
	}
	for o := range owners {
		if err := fsys.LookupOwner(o); err != nil {
			return nil, err
		}
	}
	// The deployer clones into and the agent reads these directories, so they
	// get the same owner and mode as project add and the state store give them.
	dirs := make([]string, 0, len(instDirs))
	for d := range instDirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		if err := fsys.MkdirAll(root+d, instDirs[d], deployOwner); err != nil {
			return nil, writeErr(d, err)
		}
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		dir := root + path.Dir(e.Path)
		if _, err := fsys.Stat(dir); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return paths, writeErr(e.Path, err)
			}
			if err := fsys.MkdirAll(dir, 0o750, sys.Owner{}); err != nil {
				return paths, writeErr(e.Path, err)
			}
		}
		if err := fsys.WriteFile(root+e.Path, e.Data, e.Mode, sys.Owner{User: e.User, Group: e.Group}); err != nil {
			return paths, writeErr(e.Path, err)
		}
		paths = append(paths, e.Path)
	}
	return paths, nil
}

var deployOwner = sys.Owner{User: "deployer", Group: "deploy"}

// instanceDir returns the per-instance directory p lives in and its mode.
func instanceDir(p string) (string, os.FileMode, bool) {
	for top, mode := range map[string]os.FileMode{deployDir: 0o2775, stateDir: 0o750} {
		rest, ok := strings.CutPrefix(p, top+"/")
		if !ok {
			continue
		}
		inst, _, nested := strings.Cut(rest, "/")
		if nested && inst != "" {
			return top + "/" + inst, mode, true
		}
	}
	return "", 0, false
}

// checkParents refuses a symlink anywhere between the allowed root and the
// file, which would redirect the write outside the intended tree.
func checkParents(fsys sys.FS, root, p string) error {
	var top string
	for _, r := range []string{"/etc/cvctl", deployDir, stateDir} {
		if strings.HasPrefix(p, r+"/") {
			top = r
		}
	}
	dir := path.Dir(p)
	var chain []string
	for d := dir; len(d) >= len(top) && d != "/" && d != "."; d = path.Dir(d) {
		chain = append(chain, d)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		fi, err := fsys.Lstat(root + chain[i])
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return writeErr(chain[i], err)
		}
		if fi.IsSymlink {
			return &output.Error{
				Code:    "E_SECRETS_UNSAFE_PATH",
				Message: "directory " + chain[i] + " is a symlink",
				Hint:    "replace the symlink with a real directory; nothing was written",
			}
		}
	}
	return nil
}

func writeErr(p string, err error) error {
	return &output.Error{
		Code:    "E_SECRETS_WRITE",
		Message: fmt.Sprintf("cannot write %s: %v", p, err),
		Hint:    "run as root (sudo cvctl secrets import)",
	}
}

// Dearmor extracts the first age armor block from raw (which may carry pty
// noise such as prompts and CRLF line endings) and returns the binary bundle.
func Dearmor(raw []byte) ([]byte, error) {
	const end = "-----END AGE ENCRYPTED FILE-----"
	i := bytes.Index(raw, []byte(armor.Header))
	if i < 0 {
		return nil, noBundle()
	}
	j := bytes.Index(raw[i:], []byte(end))
	if j < 0 {
		return nil, noBundle()
	}
	block := bytes.ReplaceAll(raw[i:i+j+len(end)], []byte("\r\n"), []byte("\n"))
	bin, err := io.ReadAll(armor.NewReader(bytes.NewReader(block)))
	if err != nil {
		return nil, noBundle()
	}
	return bin, nil
}

func noBundle() error {
	return &output.Error{Code: "E_REMOTE_EXPORT", Message: "the remote did not return a secrets bundle", Hint: "check the passphrase prompts on the remote and that cvctl is installed there"}
}
