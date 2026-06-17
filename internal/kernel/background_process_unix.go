//go:build !windows

package kernel

import (
	"os/exec"
	"syscall"
)

func detachBackgroundCommand(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
