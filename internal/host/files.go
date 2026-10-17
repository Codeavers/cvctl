package host

import (
	"errors"
	"io/fs"
	"os"

	"github.com/codeavers/cvctl/internal/sys"
)

var rootOwner = sys.Owner{User: "root", Group: "root"}

func fileNeedsWrite(fsys sys.FS, p string, want []byte, mode os.FileMode, owner sys.Owner) (bool, error) {
	data, err := fsys.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	fi, err := fsys.Stat(p)
	if err != nil {
		return false, err
	}
	if fi.IsDir || fi.Mode != mode || fi.User != owner.User || fi.Group != owner.Group || string(data) != string(want) {
		return true, nil
	}
	return false, nil
}
