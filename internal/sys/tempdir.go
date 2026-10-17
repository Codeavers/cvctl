package sys

import "os"

// MkdirTemp creates a private temporary directory and returns it with a cleanup func.
func MkdirTemp(pattern string) (string, func(), error) {
	dir, err := os.MkdirTemp("", pattern)
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}
