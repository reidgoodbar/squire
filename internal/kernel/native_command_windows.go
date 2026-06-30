//go:build windows

package kernel

import "os/exec"

func configureNativeCommandCleanup(cmd *exec.Cmd) {}
