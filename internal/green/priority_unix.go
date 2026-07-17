//go:build darwin || linux || freebsd || netbsd || openbsd

package green

import "golang.org/x/sys/unix"

func lowerProcessPriority(pid int) {
	_ = unix.Setpriority(unix.PRIO_PROCESS, pid, 10)
}
