//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"os"
	"syscall"
)

// lockTempFile takes a non-blocking exclusive advisory lock (flock) on a
// partial download file for as long as f stays open. removeStaleTemps skips
// files whose lock is held, so a download that is merely slow or stopped
// (Ctrl-Z, SIGSTOP) keeps its file however old its mtime gets. The kernel
// drops the lock when the process exits, even on SIGKILL, so files left by a
// killed download are still cleaned. Best effort: an error just means no
// lock (e.g. a filesystem without flock), which is the old behaviour.
func lockTempFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// removeUnlockedTemp removes path unless another process holds its lock.
func removeUnlockedTemp(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return false // a running (or stopped) download owns it
		}
		// No flock support here: fall back to the age check alone.
	}
	return os.Remove(path) == nil
}
