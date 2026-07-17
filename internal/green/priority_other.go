//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd

package green

func lowerProcessPriority(pid int) {
	_ = pid
}
