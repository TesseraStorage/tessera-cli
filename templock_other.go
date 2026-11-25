//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package main

import "os"

// lockTempFile is a no-op here. On Windows a file another process still has
// open (as a running download does) cannot be deleted, so removeStaleTemps
// cannot remove a live download's partial file anyway.
func lockTempFile(f *os.File) {}

// removeUnlockedTemp removes path; it fails while another process has it open.
func removeUnlockedTemp(path string) bool {
	return os.Remove(path) == nil
}
