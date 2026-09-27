// Tessera CLI — command-line interface for Tessera decentralized storage.
//
// Commands:
//
//	tessera login       Connect this machine to Tessera
//	tessera logout      Remove local credentials
//	tessera whoami      Show current account identity
//	tessera status      Account info + total storage used
//	tessera list        List all files (path, size, date)
//	tessera upload      Upload a file or folder
//	tessera download    Download a file
//	tessera delete      Delete a file
//	tessera share       Create a share link for a file
//	tessera fetch       Download a shared file
//	tessera sync        Sync a local folder with the network
//	tessera folder      Manage the drop-in Tessera folder
//	tessera find        Search your files
//	tessera versions    List or restore retained versions
//	tessera trash       Soft delete, restore, empty
//	tessera audit       Verify durability
//	tessera usage       Storage usage and cost forecast
//	tessera index       Rebuild the local index
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "login":
		cmdLogin(args)
	case "logout":
		cmdLogout()
	case "whoami":
		cmdWhoami()
	case "status":
		cmdStatus()
	case "list", "ls":
		cmdList(args)
	case "upload", "put":
		if len(positional(args)) < 1 {
			usageError("upload <path> [--as <remote-path>]")
		}
		cmdUploadPath(positional(args)[0], args)
	case "download", "get":
		if len(positional(args)) < 1 {
			usageError("download <path> [--out <file>]")
		}
		cmdDownload(args)
	case "delete", "rm":
		if len(positional(args)) < 1 {
			usageError("delete <path> [--versions]")
		}
		cmdDelete(args)
	case "share":
		if len(positional(args)) < 1 {
			usageError("share <path>")
		}
		cmdShare(positional(args)[0])
	case "fetch":
		if len(positional(args)) < 1 {
			usageError("fetch <share-url> [output-name]")
		}
		pos := positional(args)
		outName := ""
		if len(pos) >= 2 {
			outName = pos[1]
		}
		cmdFetch(pos[0], outName)
	case "sync":
		cmdSync(args)
	case "folder":
		cmdFolder(args)
	case "find", "search":
		cmdFind(args)
	case "versions":
		cmdVersions(args)
	case "trash":
		cmdTrash(args)
	case "audit":
		cmdAudit(args)
	case "usage", "du":
		cmdUsage(args)
	case "index":
		cmdIndex(args)
	case "config":
		cmdConfig(args)
	case "service":
		cmdService(args)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

// usageError prints a one-line usage hint and exits.
func usageError(usage string) {
	fmt.Fprintf(os.Stderr, "Usage: %s %s\n", filepath.Base(os.Args[0]), usage)
	os.Exit(1)
}

func printUsage() {
	exe := filepath.Base(os.Args[0])
	fmt.Printf(`Tessera CLI — decentralized storage on Sia.

Usage:
  %s login                   Connect this machine to Tessera
  %s logout                  Remove local credentials
  %s whoami                  Show current account identity
  %s status                  Account info + total storage used
  %s list [prefix]           List files (path, size, date)
  %s upload <path>           Upload a file or folder
  %s download <path>         Download a file
  %s delete <path>           Delete a file (see also: trash)
  %s share <path>            Create a share link for a file
  %s fetch <url> [name]      Download a shared file

Folders & sync:
  %s sync add <folder> --as <remote-prefix>
                             Start syncing a folder (see 'sync add -h')
  %s sync [--dry-run]        Sync registered folders once
  %s sync watch              Keep folders in sync continuously
  %s sync status|list|remove|conflicts
  %s folder add <path>       Create a drop-in folder and start syncing
  %s folder open|status      Open it in your file manager

More:
  %s find <pattern>          Search your files (--content for text search)
  %s versions <path>         List retained versions
  %s trash list|restore|empty|rm <path>
  %s audit [path]            Verify durability of stored data
  %s usage [--by-folder]     Storage usage and cost forecast
  %s index --rebuild         Rebuild the local metadata index
  %s config [--json]         Show or change settings
  %s service install|start|stop|status
                             Run the watcher as a background service

Indexer: https://index.tessera.storage
Docs:    https://tessera.storage
`, exe, exe, exe, exe, exe, exe, exe, exe, exe, exe,
		exe, exe, exe, exe, exe, exe,
		exe, exe, exe, exe, exe, exe,
		exe, exe)
}
