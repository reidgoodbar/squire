//go:build !darwin && !linux

package main

import (
	"os"
	"strconv"
)

func preloadFileStatSignal(info os.FileInfo) (string, bool) {
	return strconv.FormatInt(info.Size(), 10) + "|" +
		strconv.FormatInt(info.ModTime().UnixNano(), 10) + "|" +
		info.Mode().String() + "|change:unsupported", true
}
