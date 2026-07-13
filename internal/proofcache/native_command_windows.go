//go:build windows

package proofcache

import "os/exec"

func configureNativeCommandCleanup(cmd *exec.Cmd) {}
