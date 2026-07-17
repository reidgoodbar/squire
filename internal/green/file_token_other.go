//go:build !darwin && !linux

package green

import (
	"os"
	"strconv"
)

func fileChangeToken(info os.FileInfo) string {
	if info == nil {
		return "missing"
	}
	return strconv.FormatInt(info.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(info.Size(), 10)
}
