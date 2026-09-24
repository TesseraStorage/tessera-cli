// Tessera CLI — command-line interface for Tessera decentralized storage.
//
// Commands:
//   tessera login     Connect this machine to Tessera
//   tessera logout    Remove local credentials
//   tessera whoami    Show current account identity
//   tessera status    Account info + total storage used
//   tessera list      List all files (name, size, date)
//   tessera upload    Upload a file to Tessera
//   tessera download  Download a file from Tessera
//   tessera delete    Delete a file from Tessera
//   tessera share     Create a share link for a file
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
		cmdLogin()
	case "logout":
		cmdLogout()
	case "whoami":
		cmdWhoami()
	case "status":
		cmdStatus()
	case "list":
		cmdList()
	case "upload":
		if len(args) < 1 {
			fmt.Fprintf(os.Stderr, "Usage: %s upload <path>\n", filepath.Base(os.Args[0]))
			os.Exit(1)
		}
		cmdUpload(args[0])
	case "download":
		if len(args) < 1 {
			fmt.Fprintf(os.Stderr, "Usage: %s download <name>\n", filepath.Base(os.Args[0]))
			os.Exit(1)
		}
		cmdDownload(args[0])
	case "delete":
		if len(args) < 1 {
			fmt.Fprintf(os.Stderr, "Usage: %s delete <name>\n", filepath.Base(os.Args[0]))
			os.Exit(1)
		}
		cmdDelete(args[0])
	case "share":
		if len(args) < 1 {
			fmt.Fprintf(os.Stderr, "Usage: %s share <name>\n", filepath.Base(os.Args[0]))
			os.Exit(1)
		}
		cmdShare(args[0])
	case "fetch":
		if len(args) < 1 {
			fmt.Fprintf(os.Stderr, "Usage: %s fetch <share-url> [output-name]\n", filepath.Base(os.Args[0]))
			os.Exit(1)
		}
		outName := ""
		if len(args) >= 2 {
			outName = args[1]
		}
		cmdFetch(args[0], outName)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	exe := filepath.Base(os.Args[0])
	fmt.Printf(`Tessera CLI — decentralized storage on Sia.

Usage:
  %s login               Connect this machine to Tessera
  %s logout              Remove local credentials
  %s whoami              Show current account identity
  %s status              Account info + total storage used
  %s list                List all files (name, size, date)
  %s upload <path>       Upload a file to Tessera
  %s download <name>     Download a file from Tessera
  %s delete <name>        Delete a file from Tessera
  %s share <name>        Create a share link for a file
  %s fetch <url> [name]  Download a shared file

Indexer: https://index.tessera.storage
`, exe, exe, exe, exe, exe, exe, exe, exe, exe, exe)
}
