//go:build !windows

package buildbench

import "os/exec"

// watchPeakRSS reports peak resident memory for the whole process tree.
// Unix accounting for descendants is not wired up yet, so it reports 0 and
// the runner prints "n/a" rather than a misleading number.
func watchPeakRSS(_ *exec.Cmd) func() uint64 {
	return func() uint64 { return 0 }
}
