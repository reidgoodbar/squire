//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly || solaris

package proofcache

import (
	"errors"
	"os"
	"syscall"
)

func mapHotSnapshotFile(path string) ([]byte, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, func() {}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, func() {}, err
	}
	size := info.Size()
	if size <= 0 || size > hotSnapshotMaxBytes {
		return nil, func() {}, errors.New("invalid hot snapshot file size")
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, func() {}, err
	}
	return data, func() { _ = syscall.Munmap(data) }, nil
}
