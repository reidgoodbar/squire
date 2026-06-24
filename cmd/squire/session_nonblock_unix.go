//go:build !windows

package main

import (
	"os"
	"syscall"
)

func setSessionPipeNonblock(file *os.File) {
	if file == nil {
		return
	}
	_ = syscall.SetNonblock(int(file.Fd()), true)
}
