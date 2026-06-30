//go:build darwin

package main

import (
	"os"
	"strconv"
	"syscall"
)

func preloadFileStatSignal(info os.FileInfo) (string, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return strconv.FormatInt(info.Size(), 10) + "|" +
		strconv.FormatInt(info.ModTime().UnixNano(), 10) + "|" +
		info.Mode().String() + "|" +
		"dev=" + strconv.FormatUint(uint64(st.Dev), 10) + "|" +
		"ino=" + strconv.FormatUint(uint64(st.Ino), 10) + "|" +
		"ctime=" + strconv.FormatInt(st.Ctimespec.Sec, 10) + "." +
		strconv.FormatInt(st.Ctimespec.Nsec, 10), true
}
