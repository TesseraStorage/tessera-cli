package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// cmdFolder implements the drop-in folder experience: a normal directory that
// behaves like a cloud folder because a watcher keeps it in sync.
//
//	tessera folder add <path> [--as <remote-prefix>]
//	tessera folder list | open | status | watch | remove
func cmdFolder(args []string) {
	if hasFlag(args, "--help") || hasFlag(args, "-h") {
		folderUsage()
		return
	}
	pos := positional(args)
	sub := "status"
	if len(pos) > 0 {
		sub = pos[0]
	}
	switch sub {
	case "add", "link":
		folderAdd(args)
	case "list", "ls":
		syncList(args)
	case "open":
		folderOpen(args)
	case "status":
		folderStatus(args)
	case "watch", "start", "daemon":
		syncWatch(args)
	case "remove", "unlink":
		folderRemove(args)
	case "help":
		folderUsage()
	default:
		folderUsage()
	}
}

func folderUsage() {
	exe := filepath.Base(os.Args[0])
	const usage = `Folder — a normal folder that stays in sync, like a cloud drive folder.

Usage:
  %s folder add [path] [--as <remote-prefix>] [--install-service]
        Create/link a Tessera folder and run a first sync.
        --as               Where it lives remotely (default: tessera/<name>)
        --install-service  Also install the background watcher, so the folder
                           stays in sync without a terminal open

  %s folder open [path]      Open the folder in your file manager
  %s folder status           Show sync state and recent activity
  %s folder watch            Keep syncing in this terminal (--interval, --verbose)
  %s folder remove <path>    Stop syncing (files are kept)

Drag files into the folder with Finder/Explorer and they upload. Files
changed on another machine appear automatically. Deleting removes them
everywhere.

IMPORTANT: 'folder add' syncs once and then returns. Nothing watches the
folder until you install the watcher, which it will offer to do.
`
	fmt.Printf(usage, exe, exe, exe, exe, exe)
}

// defaultFolderPath picks a sensible drop-in folder location.
func defaultFolderPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "Tessera"
	}
	return filepath.Join(home, "Tessera")
}

func folderAdd(args []string) {
	pos := positional(args)
	folder := defaultFolderPath()
	if len(pos) > 1 {
		folder = pos[1]
	}
	abs, err := filepath.Abs(folder)
	if err != nil {
		fatal("%v", err)
	}

	prefix, ok := flagValue(args, "--as")
	if !ok || prefix == "" {
		prefix = "tessera/" + filepath.Base(abs)
	}

	cfg, err := addRoot(abs, prefix)
	if err != nil {
		fatal("%v", err)
	}
	if err := os.MkdirAll(abs, 0755); err != nil {
		fatal("create %s: %v", abs, err)
	}

	fmt.Printf("Tessera folder ready: %s\n", abs)
	fmt.Printf("  remote:  %s/\n", cfg.RemotePrefix)
	fmt.Printf("  root id: %s\n", cfg.ID)
	fmt.Println()

	// First sync so the folder is immediately meaningful.
	runFolderFirstSync(cfg, args)

	fmt.Println()
	if err := openPath(abs); err != nil {
		fmt.Printf("Open it with: %s folder open\n", filepath.Base(os.Args[0]))
	} else {
		fmt.Println("Opened in your file manager — drag files in to upload them.")
	}

	// Be explicit: this command has now finished, so nothing is watching the
	// folder. Claiming otherwise is how a sync tool loses people's trust.
	fmt.Println()
	fmt.Println("This folder has synced once. Nothing is watching it yet — files you")
	fmt.Println("add now will only sync the next time a sync runs.")
	fmt.Println()

	if hasFlag(args, "--install-service") {
		folderInstallService()
		return
	}

	// Offer it directly when we can ask. A typed "yes" here is the difference
	// between a folder that works and one that quietly stops syncing.
	if yes, asked := promptYesNo("Keep this folder in sync automatically?", true); asked {
		if yes {
			folderInstallService()
		} else {
			fmt.Println()
			fmt.Printf("  Sync on demand:   %s sync\n", filepath.Base(os.Args[0]))
			fmt.Printf("  Set it up later:  %s service install --yes\n", filepath.Base(os.Args[0]))
		}
		return
	}

	// Non-interactive: never leave the user guessing what to run next.
	fmt.Printf("  Keep it in sync:  %s service install --yes\n", filepath.Base(os.Args[0]))
	fmt.Printf("  Or in this shell: %s folder watch\n", filepath.Base(os.Args[0]))
	fmt.Printf("  (next time, add --install-service to do it in one step)\n")
}

// folderInstallService activates the background watcher from within
// `folder add`, so a single command can leave the folder genuinely live.
func folderInstallService() {
	exe, err := watcherExecutable()
	if err != nil {
		fmt.Printf("\nCould not install the watcher: %v\n", err)
		return
	}
	plan, err := planService(exe)
	if err != nil {
		fmt.Printf("\n%v\n", err)
		fmt.Println("Your folder still syncs on demand with 'tessera sync'.")
		return
	}
	if err := installService(exe, plan, true); err != nil {
		fmt.Printf("\nCould not start the watcher: %v\n", err)
		fmt.Println("Your folder still syncs on demand with 'tessera sync'.")
		return
	}
	fmt.Println("Watcher installed and started — the folder is now live.")
}

// runFolderFirstSync performs an initial reconcile and reports anything that
// needs the user's attention.
func runFolderFirstSync(cfg *SyncConfig, args []string) bool {
	cfgLoaded, err := loadConfig()
	if err != nil {
		fmt.Println("Not logged in yet — run 'tessera login', then 'tessera sync'.")
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	sdk, cleanup, err := connectSDK(ctx, cfgLoaded)
	if err != nil {
		fmt.Printf("Could not reach the network yet: %v\n", err)
		fmt.Println("Run 'tessera sync' once you are back online.")
		return false
	}
	defer cleanup()

	release, err := acquireLock(cfg.ID)
	if err != nil {
		fmt.Printf("%v\n", err)
		return false
	}
	defer release()

	eng, err := loadEngine(cfg, NewRemote(sdk), SyncOptions{Verbose: true})
	if err != nil {
		fmt.Printf("%v\n", err)
		return false
	}
	res, err := eng.SyncOnce(ctx)
	if err != nil {
		fmt.Printf("First sync: %v\n", err)
		return true
	}
	fmt.Printf("First sync: ↑%d ↓%d ✗%d ⚑%d in %s\n",
		res.Uploaded, res.Downloaded, res.Deleted, res.Conflicts, res.Elapsed)
	return true
}

func folderOpen(args []string) {
	target := ""
	if pos := positional(args); len(pos) > 1 {
		target = pos[1]
	}
	var cfg *SyncConfig
	var err error
	if target != "" {
		cfg, err = findRootByPath(target)
	} else {
		roots, lerr := listSyncRoots()
		if lerr != nil || len(roots) == 0 {
			fatal("No Tessera folder yet. Create one with 'tessera folder add'.")
		}
		cfg = &roots[0]
	}
	if err != nil {
		fatal("%v", err)
	}
	if err := openPath(cfg.LocalPath); err != nil {
		fatal("could not open %s: %v", cfg.LocalPath, err)
	}
}

func folderStatus(args []string) {
	roots, err := listSyncRoots()
	if err != nil {
		fatal("%v", err)
	}
	if len(roots) == 0 {
		fmt.Println("No Tessera folder yet.")
		fmt.Println("Create one with: tessera folder add")
		return
	}
	fmt.Printf("Tessera folder(s):\n\n")
	for _, r := range roots {
		fmt.Printf("  %s\n", r.LocalPath)
		fmt.Printf("    remote:    %s/\n", r.RemotePrefix)
		fmt.Printf("    root id:   %s\n", r.ID)
		fmt.Printf("    last sync: %s\n", formatAge(r.LastSync))
		st, err := loadSyncState(r.ID)
		if err != nil {
			continue
		}
		pending := 0
		for _, e := range st.Entries {
			if e.DeletedLocal || e.DeletedRemote {
				pending++
			}
		}
		conflicts := st.Conflicts
		fmt.Printf("    tracked:   %d file(s)\n", len(st.Entries))
		if pending > 0 {
			fmt.Printf("    pending:   %d change(s) — run 'tessera sync'\n", pending)
		}
		if len(conflicts) > 0 {
			fmt.Printf("    conflicts: %d — run 'tessera sync conflicts'\n", len(conflicts))
		}
	}
	fmt.Println()
	fmt.Println("Watching continuously keeps this fresh: tessera folder watch")
}

func folderRemove(args []string) {
	pos := positional(args)
	if len(pos) < 2 {
		usageError("folder remove <path>")
	}
	cfg, err := findRootByPath(pos[1])
	if err != nil {
		fatal("%v", err)
	}
	if err := removeRoot(cfg.ID); err != nil {
		fatal("remove: %v", err)
	}
	fmt.Printf("Stopped syncing %s. Files were kept in place.\n", cfg.LocalPath)
}

// openPath opens a directory in the platform file manager.
func openPath(p string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", p).Start()
	case "windows":
		return exec.Command("explorer", p).Start()
	case "linux":
		if _, err := exec.LookPath("xdg-open"); err == nil {
			return exec.Command("xdg-open", p).Start()
		}
		return fmt.Errorf("xdg-open not found")
	default:
		return fmt.Errorf("unsupported platform")
	}
}

// cmdConflictsSurface lists conflict copies recorded in sync state.
func syncConflicts(args []string) {
	roots, err := listSyncRoots()
	if err != nil {
		fatal("%v", err)
	}

	type conflictRow struct {
		Root     string `json:"root_id"`
		Folder   string `json:"folder"`
		Path     string `json:"path"`
		KeptAs   string `json:"kept_as"`
		Detected string `json:"detected"`
	}
	var rows []conflictRow
	for _, r := range roots {
		st, err := loadSyncState(r.ID)
		if err != nil {
			continue
		}
		for original, copyPath := range st.Conflicts {
			rows = append(rows, conflictRow{
				Root: r.ID, Folder: r.LocalPath, Path: original, KeptAs: copyPath,
			})
		}
	}

	if hasFlag(args, "--json") {
		emitJSON(rows)
		return
	}
	if len(rows) == 0 {
		fmt.Println("No conflicts recorded.")
		return
	}
	fmt.Printf("%-40s  %s\n", "ORIGINAL", "KEPT AS")
	fmt.Println(strings.Repeat("-", 100))
	for _, r := range rows {
		fmt.Printf("%-40s  %s\n", truncate(r.Path, 40), r.KeptAs)
	}
	fmt.Printf("\n%d conflict copy(ies). Both versions are present in the folder.\n", len(rows))
	fmt.Println("Resolve by keeping the file you want and deleting the other, then run 'tessera sync'.")
}
