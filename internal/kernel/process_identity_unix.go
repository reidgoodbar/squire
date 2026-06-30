//go:build !windows

package kernel

import (
	"os"
	"sort"
	"strconv"
	"strings"
)

func processIdentitySignal() string {
	groups, _ := os.Getgroups()
	sort.Ints(groups)
	parts := []string{
		"uid=" + strconv.Itoa(os.Getuid()),
		"euid=" + strconv.Itoa(os.Geteuid()),
		"gid=" + strconv.Itoa(os.Getgid()),
		"egid=" + strconv.Itoa(os.Getegid()),
	}
	for _, group := range groups {
		parts = append(parts, "group="+strconv.Itoa(group))
	}
	return hashString(strings.Join(parts, "\n"))
}
