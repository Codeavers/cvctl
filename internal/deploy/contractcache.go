package deploy

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/codeavers/cvctl/internal/infraoci"
	"github.com/codeavers/cvctl/internal/sys"
)

// cacheRecent is how many most recently used contracts survive a prune on top
// of the ones state references.
const cacheRecent = 5

var cacheOwner = sys.Owner{User: "deployer", Group: "deploy"}

var cacheFiles = []string{"compose.yml", "env.spec.yml", "prod.env"}

var cacheKeyRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ContractCache keeps validated contracts by digest under
// <stateRoot>/<inst>/contracts/<hex>/, so a rollback does not depend on the
// registry still serving an untagged digest.
type ContractCache struct {
	FS  sys.FS
	Dir string
}

// NewContractCache is the cache of one instance.
func NewContractCache(fsys sys.FS, stateRoot, inst string) *ContractCache {
	if fsys == nil {
		fsys = sys.NewOSFS()
	}
	return &ContractCache{FS: fsys, Dir: filepath.Join(stateRoot, inst, "contracts")}
}

func cacheKey(digest string) (string, bool) {
	h, ok := strings.CutPrefix(digest, "sha256:")
	return h, ok && cacheKeyRe.MatchString(h)
}

// Get returns the cached contract of digest.
func (c *ContractCache) Get(digest string) (*infraoci.Contract, bool) {
	key, ok := cacheKey(digest)
	if c == nil || !ok {
		return nil, false
	}
	dir := filepath.Join(c.Dir, key)
	if fi, err := c.FS.Lstat(dir); err != nil || !fi.IsDir || fi.IsSymlink {
		return nil, false
	}
	ct := &infraoci.Contract{Digest: digest, Files: map[string][]byte{}}
	for _, f := range cacheFiles {
		p := filepath.Join(dir, f)
		fi, err := c.FS.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || fi.IsDir || fi.IsSymlink {
			return nil, false
		}
		data, err := c.FS.ReadFile(p)
		if err != nil {
			return nil, false
		}
		ct.Files[f] = data
	}
	if ct.Files["compose.yml"] == nil || ct.Files["env.spec.yml"] == nil {
		return nil, false
	}
	return ct, true
}

// Put stores ct atomically (temp dir + rename) and marks it most recent.
func (c *ContractCache) Put(ct *infraoci.Contract) error {
	key, ok := cacheKey(ct.Digest)
	if c == nil || !ok {
		return nil
	}
	if err := c.FS.MkdirAll(c.Dir, 0o750, cacheOwner); err != nil {
		return err
	}
	dst := filepath.Join(c.Dir, key)
	if _, err := c.FS.Lstat(dst); err != nil {
		var rnd [8]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return err
		}
		tmp := filepath.Join(c.Dir, ".tmp-"+hex.EncodeToString(rnd[:]))
		if err := c.FS.Mkdir(tmp, 0o750, cacheOwner); err != nil {
			return err
		}
		for _, f := range cacheFiles {
			data, ok := ct.Files[f]
			if !ok {
				continue
			}
			if err := c.FS.WriteFile(filepath.Join(tmp, f), data, 0o640, cacheOwner); err != nil {
				_ = c.FS.RemoveAll(tmp)
				return err
			}
		}
		if err := c.FS.Rename(tmp, dst); err != nil {
			_ = c.FS.RemoveAll(tmp)
			return err
		}
	}
	return c.touch(key)
}

func (c *ContractCache) recent() []string {
	raw, err := c.FS.ReadFile(filepath.Join(c.Dir, "recent"))
	if err != nil {
		return nil
	}
	var keys []string
	for _, l := range strings.Split(string(raw), "\n") {
		if cacheKeyRe.MatchString(l) {
			keys = append(keys, l)
		}
	}
	return keys
}

func (c *ContractCache) writeRecent(keys []string) error {
	return c.FS.WriteFile(filepath.Join(c.Dir, "recent"), []byte(strings.Join(keys, "\n")+"\n"), 0o640, cacheOwner)
}

// touch moves key to the front of the recency list.
func (c *ContractCache) touch(key string) error {
	keys := []string{key}
	for _, k := range c.recent() {
		if k != key {
			keys = append(keys, k)
		}
	}
	return c.writeRecent(keys)
}

// Prune removes every cached contract except the digests in keep and the
// cacheRecent most recently used ones.
func (c *ContractCache) Prune(keep ...string) error {
	if c == nil {
		return nil
	}
	ents, err := c.FS.ReadDir(c.Dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	keepSet := map[string]bool{}
	for _, d := range keep {
		if k, ok := cacheKey(d); ok {
			keepSet[k] = true
		}
	}
	recent := c.recent()
	var kept []string
	for i, k := range recent {
		if i < cacheRecent {
			keepSet[k] = true
		}
		if keepSet[k] {
			kept = append(kept, k)
		}
	}
	var errs []error
	for _, e := range ents {
		n := e.Name()
		if n == "recent" || keepSet[n] {
			continue
		}
		if cacheKeyRe.MatchString(n) || strings.HasPrefix(n, ".tmp-") {
			errs = append(errs, c.FS.RemoveAll(filepath.Join(c.Dir, n)))
		}
	}
	errs = append(errs, c.writeRecent(kept))
	return errors.Join(errs...)
}
