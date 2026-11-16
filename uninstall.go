package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cmdUninstall removes tessera-cli itself: a background watcher left by an
// older version, optionally all local state (login, file index, trash), and
// finally the binary. Split into separate confirmations because removing
// the watcher is always safe to reverse (reinstall) but removing local
// state is not -- losing the synced-folder list or a still-valid login
// silently is exactly the kind of thing an uninstaller should never do
// by accident.
func cmdUninstall(args []string) {
	if hasFlag(args, "--help") || hasFlag(args, "-h") {
		uninstallUsage()
		return
	}

	exe, err := watcherExecutable()
	if err != nil {
		fatal("cannot determine the path to this binary: %v", err)
	}
	dir, _ := configDir()
	dryRun := hasFlag(args, "--dry-run")

	plan, planErr := planService("")
	watcher := false
	if planErr == nil {
		installed, known := watcherInstalled(plan)
		watcher = installed || !known
	}

	fmt.Println("This will:")
	step := 1
	if watcher {
		fmt.Printf("  %d. Stop and remove the background sync watcher installed by an older version\n", step)
		step++
	}
	fmt.Printf("  %d. Remove this binary: %s\n", step, exe)
	fmt.Println()
	fmt.Printf("Your login, local file index and trash list in %s are kept\n", dir)
	fmt.Println("unless you also confirm removing them below (or pass --purge).")
	fmt.Println("Your stored files and your account are not affected either way.")
	fmt.Println()

	if dryRun {
		fmt.Println("Dry run -- nothing was changed.")
		return
	}

	if !hasFlag(args, "--yes") && !hasFlag(args, "-y") {
		ok, asked := promptYesNo("Continue?", false)
		if !asked {
			fmt.Println("Non-interactive: re-run with --yes to proceed.")
			return
		}
		if !ok {
			fmt.Println("Aborted.")
			return
		}
	}

	// 1. Watcher -- same removal this binary would do for `service uninstall`,
	// just tolerant of "nothing was installed" so uninstall always succeeds.
	if watcher {
		fmt.Println("Stopping the watcher:")
		if err := serviceRun(plan.RemoveCmd[0], plan.RemoveCmd[1:]...); err != nil {
			fmt.Printf("  (%v)\n", err)
		}
		if plan.Content != "" {
			if rmErr := os.Remove(plan.DefPath); rmErr != nil && !os.IsNotExist(rmErr) {
				fmt.Printf("  could not remove %s: %v\n", plan.DefPath, rmErr)
			} else if rmErr == nil {
				fmt.Printf("  removed %s\n", plan.DefPath)
			}
		}
		if defDir, derr := serviceDefinitionDir(); derr == nil {
			_ = os.RemoveAll(defDir)
		}
	}

	// 2. Local state -- ask separately, default no.
	purge := hasFlag(args, "--purge")
	if !purge && dir != "" {
		if ok, asked := promptYesNo(
			fmt.Sprintf("Also remove %s (login, local file index, trash list, all local state)?", dir),
			false,
		); asked && ok {
			purge = true
		} else if asked {
			fmt.Printf("Keeping %s.\n", dir)
		} else {
			fmt.Printf("Non-interactive: local state at %s was kept. Re-run with --purge to remove it too.\n", dir)
		}
	}
	if purge && dir != "" {
		if err := os.RemoveAll(dir); err != nil {
			fmt.Printf("  could not remove %s: %v\n", dir, err)
		} else {
			fmt.Printf("Removed %s\n", dir)
		}
	}

	// 3. The binary. Last, so every step above still ran even if this fails.
	fmt.Println()
	if err := selfDeleteBinary(exe); err != nil {
		fmt.Printf("Could not remove the binary automatically: %v\n", err)
		fmt.Printf("Remove it yourself: %s\n", exe)
		return
	}
	fmt.Println("Tessera CLI uninstalled. Thanks for trying it.")
}

func uninstallUsage() {
	exe := filepath.Base(os.Args[0])
	fmt.Printf(`Uninstall — remove this binary and (optionally) local state.

Usage:
  %s uninstall            Ask before removing anything
  %s uninstall --yes      Skip the main confirmation (still asks about local state)
  %s uninstall --purge    Also remove %s without asking (login, file index, trash list)
  %s uninstall --dry-run  Show what would happen; change nothing

Local state (login, local file index, trash list) is kept by default so
reinstalling can pick up right where you left off. A background sync watcher
installed by an older version is stopped and removed too. Your stored files
and your account are not affected; to delete them use
'tessera account purge-data' or 'tessera account delete'.
`, exe, exe, exe, strings.TrimSuffix(mustConfigDirForUsage(), "\n"), exe)
}

// mustConfigDirForUsage is a best-effort helper purely for the usage text;
// it never fails the command over a cosmetic detail.
func mustConfigDirForUsage() string {
	dir, err := configDir()
	if err != nil {
		return "~/.tessera"
	}
	return dir
}
