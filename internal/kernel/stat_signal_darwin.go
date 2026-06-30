//go:build darwin

package kernel

import (
	"fmt"
	"os"
	"syscall"
)

func fileStatChangeSignal(info os.FileInfo) (string, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("dev=%d|ino=%d|ctime=%d.%d", stat.Dev, stat.Ino, stat.Ctimespec.Sec, stat.Ctimespec.Nsec), true
}
