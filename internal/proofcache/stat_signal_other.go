//go:build !darwin && !linux

package proofcache

import "os"

func fileStatChangeSignal(info os.FileInfo) (string, bool) {
	return "", false
}
