//go:build windows

package proofcache

import (
	"os/user"
	"strings"
)

func processIdentitySignal() string {
	current, err := user.Current()
	if err != nil || current == nil {
		return hashString("windows-user:unknown")
	}
	return hashString(strings.Join([]string{current.Username, current.Uid, current.Gid, current.HomeDir}, "\n"))
}
