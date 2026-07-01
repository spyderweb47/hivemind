//go:build !linux && !darwin

package procscan

// Process discovery is implemented for Linux (/proc) and macOS (lsof+ps). On any
// other platform the feature degrades to a no-op: Scan returns nil and the
// dashboard simply never shows a BACKGROUND panel.
func available() bool { return false }

func collect() (map[int]rawProc, map[int][]int) { return nil, nil }
