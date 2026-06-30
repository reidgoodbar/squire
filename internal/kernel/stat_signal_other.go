//go:build !darwin && !linux

package kernel

import "os"

func fileStatChangeSignal(info os.FileInfo) (string, bool) {
	return "", false
}
