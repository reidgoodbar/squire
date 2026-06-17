//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly || solaris)

package kernel

import (
	"errors"
	"os"
)

func mapHotSnapshotFile(path string) ([]byte, func(), error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, func() {}, err
	}
	if len(data) == 0 || len(data) > hotSnapshotMaxBytes {
		return nil, func() {}, errors.New("invalid hot snapshot file size")
	}
	return data, func() {}, nil
}
