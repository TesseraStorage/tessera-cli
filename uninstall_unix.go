//go:build !windows

package main

import "os"

// selfDeleteBinary removes the running executable. On Unix this is safe:
// unlinking an open/running file just removes the directory entry, the
// process keeps running off its already-mapped inode until it exits.
func selfDeleteBinary(exe string) error {
	return os.Remove(exe)
}
