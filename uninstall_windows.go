//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"syscall"
)

// selfDeleteBinary schedules removal of the running executable. Windows
// keeps a lock on an executable's own file while it is running, so this
// process cannot delete itself directly (unlike Unix). Instead it starts a
// detached helper that waits a moment for this process to exit, then
// deletes the file -- the standard Windows self-delete pattern.
func selfDeleteBinary(exe string) error {
	cmd := exec.Command("cmd", "/c", "ping 127.0.0.1 -n 2 >nul & del /f /q \""+exe+"\"")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x00000008, // DETACHED_PROCESS: outlives this process
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Println("(Windows will finish removing the binary a couple of seconds after this process exits.)")
	return nil
}
