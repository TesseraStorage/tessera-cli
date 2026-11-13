//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCopyFileAtomicRefusesFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if _, err := copyFileAtomic(fifo, strings.NewReader("x"), 0644); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v", err)
	}
	if fi, err := os.Lstat(fifo); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("fifo replaced: %v", err)
	}
	if l := tempLeftovers(t, dir); len(l) != 0 {
		t.Fatalf("temp files left: %v", l)
	}
}

// TestCopyFileAtomicRefusesDevNull checks a device target is refused before any
// temp file is created. It never runs as root, where the old code path could
// replace /dev/null itself.
func TestCopyFileAtomicRefusesDevNull(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("not run as root")
	}
	_, err := copyFileAtomic(os.DevNull, strings.NewReader("x"), 0600)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v, want a refusal (not a /dev permission error)", err)
	}
}
