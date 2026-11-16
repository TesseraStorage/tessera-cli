package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// Tessera launches as storage only: put bytes, get bytes, pay. Folder sync,
// the drop-in folder, its background watcher and share links are not part of
// it. The commands stay recognised so scripts and older habits get a clear
// answer instead of "Unknown command", and they exit non-zero. Their code is
// kept in the tree but is not reachable from the command line.
var notAvailableYetCommands = map[string]bool{
	"sync":   true,
	"folder": true,
	"share":  true,
	"fetch":  true,
}

// serviceNotAvailableYet lists the service subcommands that would install or
// run the sync watcher. stop, status and uninstall still work so a watcher
// installed by an older version can be inspected and removed.
var serviceNotAvailableYet = map[string]bool{
	"install":  true,
	"start":    true,
	"restart":  true,
	"print":    true,
	"template": true,
}

func notAvailableYetMessage(cmd string) string {
	exe := filepath.Base(os.Args[0])
	return fmt.Sprintf("'%s %s' is not available yet.\n"+
		"Use '%s upload <path>' and '%s download <path>' to store and get back files.",
		exe, cmd, exe, exe)
}

// notAvailableYet prints the message for a retired-for-now command and exits 1.
func notAvailableYet(cmd string) {
	fmt.Fprintln(os.Stderr, "ERROR: "+notAvailableYetMessage(cmd))
	exitWithNotices(1)
}
