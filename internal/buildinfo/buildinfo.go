package buildinfo

import "strings"

var Version = "dev"

func CurrentVersion() string {
	version := strings.TrimSpace(Version)
	if version == "" {
		return "dev"
	}
	return version
}
