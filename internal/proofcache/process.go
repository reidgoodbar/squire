package proofcache

import (
	"os"
	"path/filepath"
)

type ProcessGuardReport struct {
	Mode           string   `json:"mode"`
	CurrentPID     int      `json:"current_pid"`
	ParentPID      int      `json:"parent_pid"`
	CurrentFDCount int      `json:"current_fd_count"`
	CleanupActions int      `json:"cleanup_actions"`
	Diagnostics    []string `json:"diagnostics,omitempty"`
}

func ProcessGuardStatus() ProcessGuardReport {
	report := ProcessGuardReport{
		Mode:           "observe_only",
		CurrentPID:     os.Getpid(),
		ParentPID:      os.Getppid(),
		CurrentFDCount: -1,
		CleanupActions: 0,
		Diagnostics: []string{
			"process guard never kills or cleans processes",
			"raw process command lines are not persisted",
		},
	}
	for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
		count, ok := countOpenFDs(dir)
		if ok {
			report.CurrentFDCount = count
			return report
		}
	}
	report.Diagnostics = append(report.Diagnostics, "fd count unavailable on this platform")
	return report
}

func countOpenFDs(dir string) (int, bool) {
	entries, err := os.ReadDir(filepath.Clean(dir))
	if err != nil {
		return -1, false
	}
	var count int
	for _, entry := range entries {
		if entry.Name() == "." || entry.Name() == ".." {
			continue
		}
		count++
	}
	return count, true
}
