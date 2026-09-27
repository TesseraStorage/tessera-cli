package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// cmdSync dispatches the sync subcommands.
//
//	tessera sync add <folder> --as <remote-prefix>
//	tessera sync [--root <id>] [--dry-run] [--json]
//	tessera sync watch [--interval 30s]
//	tessera sync list | status | remove | conflicts
func cmdSync(args []string) {
	if hasFlag(args, "--help") || hasFlag(args, "-h") {
		syncUsage()
		return
	}
	pos := positional(args)
	sub := ""
	if len(pos) > 0 {
		sub = pos[0]
	}
	switch sub {
	case "add":
		syncAdd(args)
	case "remove", "rm":
		syncRemove(args)
	case "list", "ls":
		syncList(args)
	case "status":
		syncStatus(args)
	case "watch", "daemon":
		syncWatch(args)
	case "conflicts":
		syncConflicts(args)
	case "help":
		syncUsage()
	case "":
		syncRun(args)
	default:
		// Allow `tessera sync <folder>` as shorthand for a one-shot sync of a
		// specific folder.
		if fi, err := os.Stat(sub); err == nil && fi.IsDir() {
			cfg, err := findRootByPath(sub)
			if err != nil {
				fatal("%v", err)
			}
			args = append(args, "--root", cfg.ID)
			syncRun(args)
			return
		}
		syncUsage()
	}
}

func syncUsage() {
	exe := filepath.Base(os.Args[0])
	fmt.Print(strings.ReplaceAll(`Sync — keep local folders in step across machines.

Usage:
  EXE sync add <folder> --as <remote-prefix>
        Register a folder and start tracking it.
        Example: EXE sync add ~/Documents --as tessera/macos/Documents

  EXE sync                    Reconcile every registered folder once
  EXE sync --dry-run          Show what would change, without changing it
  EXE sync --root <id>        Reconcile one folder
  EXE sync --json             Machine-readable summary
  EXE sync --policy keep-both Keep both copies when a file changed twice
  EXE sync --adopt local|remote
                             First-run policy when both sides already have files

  EXE sync watch              Stay running and sync continuously
  EXE sync watch --interval 15s --max-rate 20MB

  EXE sync list               Show registered folders
  EXE sync status             Ahead/behind counts and last error
  EXE sync conflicts          List conflict copies
  EXE sync remove <id>        Stop tracking a folder

Files are identified by content hash, so renames are cheap and deletes
propagate. When both sides change, the newest copy wins by default; pass
--policy keep-both (or set it per folder) to keep a conflict copy instead.
`, "EXE", exe))
}

// syncAdd registers a folder.
func syncAdd(args []string) {
	pos := positional(args)
	if len(pos) < 2 {
		usageError("sync add <folder> --as <remote-prefix>")
	}
	folder := pos[1]

	prefix, ok := flagValue(args, "--as")
	if !ok || prefix == "" {
		fatal("--as <remote-prefix> is required, e.g. --as tessera/macos/Documents")
	}

	cfg, err := addRoot(folder, prefix)
	if err != nil {
		fatal("%v", err)
	}
	if p, ok := flagValue(args, "--policy"); ok {
		cfg.ConflictPolicy = p
		_ = saveSyncConfig(cfg)
	}
	if hasFlag(args, "--json") {
		emitJSON(cfg)
		return
	}
	fmt.Printf("Sync folder added.\n\n")
	fmt.Printf("  Local:   %s\n", cfg.LocalPath)
	fmt.Printf("  Remote:  %s/\n", cfg.RemotePrefix)
	fmt.Printf("  Root ID: %s\n", cfg.ID)
	fmt.Println()
	fmt.Println("Run 'tessera sync' to reconcile it now, or 'tessera sync watch'")
	fmt.Println("to keep it in sync continuously.")
}

func syncRemove(args []string) {
	pos := positional(args)
	if len(pos) < 2 {
		usageError("sync remove <root-id>")
	}
	id := pos[1]
	cfg, err := loadSyncConfig(id)
	if err != nil {
		fatal("unknown sync folder %q (see 'tessera sync list')", id)
	}
	fmt.Printf("Stop syncing %s (%s → %s)?\n", cfg.LocalPath, cfg.ID, cfg.RemotePrefix)
	fmt.Print("Local files are kept. Remote copies are kept. [y/N] ")
	var a string
	fmt.Scanln(&a)
	if strings.ToLower(a) != "y" {
		fmt.Println("Aborted.")
		return
	}
	if err := removeRoot(id); err != nil {
		fatal("remove: %v", err)
	}
	fmt.Println("Removed. The folder and its remote files were left untouched.")
}

func syncList(args []string) {
	roots, err := listSyncRoots()
	if err != nil {
		fatal("%v", err)
	}
	if hasFlag(args, "--json") {
		emitJSON(roots)
		return
	}
	if len(roots) == 0 {
		fmt.Println("No sync folders. Add one with:")
		fmt.Println("  tessera sync add ~/Documents --as tessera/macos/Documents")
		return
	}
	fmt.Printf("%-18s  %-30s  %-28s  %s\n", "ROOT ID", "LOCAL FOLDER", "REMOTE PREFIX", "LAST SYNC")
	fmt.Println(strings.Repeat("-", 100))
	for _, r := range roots {
		fmt.Printf("%-18s  %-30s  %-28s  %s\n",
			r.ID, truncate(r.LocalPath, 30), truncate(r.RemotePrefix+"/", 28), formatAge(r.LastSync))
	}
}

// syncStatus reports ahead/behind counts without transferring anything.
func syncStatus(args []string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}
	roots, err := listSyncRoots()
	if err != nil {
		fatal("%v", err)
	}
	if len(roots) == 0 {
		fmt.Println("No sync folders registered.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sdk, cleanup, err := connectSDK(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()
	rt := NewRemote(sdk)

	type status struct {
		Root      string `json:"root_id"`
		LocalPath string `json:"local_path"`
		Remote    string `json:"remote_prefix"`
		Uploads   int    `json:"to_upload"`
		Downloads int    `json:"to_download"`
		Deletes   int    `json:"to_delete"`
		Conflicts int    `json:"conflicts"`
		LastSync  string `json:"last_sync"`
		Error     string `json:"error,omitempty"`
	}
	var out []status
	for _, r := range roots {
		s := status{Root: r.ID, LocalPath: r.LocalPath, Remote: r.RemotePrefix, LastSync: formatAge(r.LastSync)}
		eng, err := loadEngine(&r, rt, SyncOptions{})
		if err != nil {
			s.Error = err.Error()
			out = append(out, s)
			continue
		}
		if err := eng.RefreshRemote(ctx); err != nil {
			s.Error = err.Error()
			out = append(out, s)
			continue
		}
		if err := eng.ScanLocal(); err != nil {
			s.Error = err.Error()
			out = append(out, s)
			continue
		}
		for _, op := range eng.Plan() {
			switch op.Kind {
			case OpUpload, OpConflictRemote:
				s.Uploads++
			case OpDownload, OpConflictLocal:
				s.Downloads++
			case OpDeleteLocal, OpDeleteRemote:
				s.Deletes++
			}
			if op.Kind == OpConflictLocal || op.Kind == OpConflictRemote {
				s.Conflicts++
			}
		}
		out = append(out, s)
	}

	if hasFlag(args, "--json") {
		emitJSON(out)
		return
	}
	for _, s := range out {
		fmt.Printf("%s\n", s.LocalPath)
		fmt.Printf("  remote:     %s/\n", s.Remote)
		fmt.Printf("  to upload:  %d\n", s.Uploads)
		fmt.Printf("  to fetch:   %d\n", s.Downloads)
		fmt.Printf("  to delete:  %d\n", s.Deletes)
		if s.Conflicts > 0 {
			fmt.Printf("  conflicts:  %d\n", s.Conflicts)
		}
		fmt.Printf("  last sync:  %s\n", s.LastSync)
		if s.Error != "" {
			fmt.Printf("  error:      %s\n", s.Error)
		}
	}
}

// syncRun performs one reconcile pass over the selected roots.
func syncRun(args []string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	roots, err := listSyncRoots()
	if err != nil {
		fatal("%v", err)
	}
	if id, ok := flagValue(args, "--root"); ok {
		var picked []SyncConfig
		for _, r := range roots {
			if r.ID == id {
				picked = append(picked, r)
			}
		}
		if len(picked) == 0 {
			fatal("unknown sync folder %q (see 'tessera sync list')", id)
		}
		roots = picked
	}
	if len(roots) == 0 {
		fmt.Println("No sync folders registered.")
		fmt.Println("Add one with: tessera sync add ~/Documents --as tessera/macos/Documents")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
	defer cancel()

	sdk, cleanup, err := connectSDK(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()
	rt := NewRemote(sdk)

	opts := SyncOptions{
		DryRun:  hasFlag(args, "--dry-run"),
		Verbose: hasFlag(args, "--verbose") || hasFlag(args, "-v"),
		Jobs:    intFlag(args, "--jobs", cfg.DefaultJobs),
		MaxRate: parseRate(args),
		Adopt:   adoptFlag(args),
	}
	if p, ok := flagValue(args, "--policy"); ok {
		opts.ConflictMode = p
	}

	if purged := expireTrash(ctx, rt); purged > 0 {
		fmt.Printf("Purged %d expired trash item(s).\n", purged)
	}

	jsonOut := hasFlag(args, "--json")
	var results []*Result
	var failed bool
	for i := range roots {
		r := roots[i]
		fmt.Printf("Syncing %s → %s/\n", r.LocalPath, r.RemotePrefix)
		rel, err := acquireLock(r.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %v\n", err)
			failed = true
			continue
		}
		eng, err := loadEngine(&r, rt, opts)
		if err != nil {
			fatal("%v", err)
		}
		if opts.DryRun {
			res, err := eng.SyncOnce(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  error: %v\n", err)
				failed = true
				rel()
				continue
			}
			results = append(results, res)
			rel()
			continue
		}
		res, err := eng.SyncOnce(ctx)
		rel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "  error: %v\n", err)
			failed = true
			continue
		}
		results = append(results, res)
		if len(res.Errors) > 0 {
			failed = true
		}
	}

	if jsonOut {
		emitJSON(results)
	} else {
		var up, down, del, conf, adopt int
		var bytes int64
		for _, r := range results {
			up += r.Uploaded
			down += r.Downloaded
			del += r.Deleted
			conf += r.Conflicts
			adopt += r.Adopted
			bytes += r.BytesUp + r.BytesDown
		}
		if len(results) > 0 && !opts.DryRun {
			summary := fmt.Sprintf("Done: %d uploaded, %d downloaded, %d deleted", up, down, del)
			if adopt > 0 {
				summary += fmt.Sprintf(", %d already in sync", adopt)
			}
			if conf > 0 {
				summary += fmt.Sprintf(", %d conflict copy(ies)", conf)
			}
			fmt.Printf("%s, %s transferred\n", summary, formatBytes(uint64(bytes)))
		}
	}
	if failed {
		os.Exit(1)
	}
}

// Adopt policies for the first sync of a folder that already has files.
const (
	adoptMerge  = "merge"
	adoptLocal  = "local"
	adoptRemote = "remote"
)

// parseRate converts --max-rate into bytes/second.
func parseRate(args []string) int64 {
	v, ok := flagValue(args, "--max-rate")
	if !ok || v == "" {
		return 0
	}
	n, err := parseByteSize(v)
	if err != nil {
		fatal("--max-rate: %v", err)
	}
	return n
}

func adoptFlag(args []string) string {
	if v, ok := flagValue(args, "--adopt"); ok {
		switch v {
		case adoptLocal, adoptRemote, adoptMerge:
			return v
		default:
			fatal("--adopt must be one of: local, remote, merge")
		}
	}
	return ""
}

// parseByteSize parses values like "20MB", "1.5G", "512k" or a plain number.
func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "TB"):
		mult, s = 1<<40, strings.TrimSuffix(s, "TB")
	case strings.HasSuffix(s, "GB"), strings.HasSuffix(s, "G"):
		mult, s = 1<<30, strings.TrimSuffix(strings.TrimSuffix(s, "GB"), "G")
	case strings.HasSuffix(s, "MB"), strings.HasSuffix(s, "M"):
		mult, s = 1<<20, strings.TrimSuffix(strings.TrimSuffix(s, "MB"), "M")
	case strings.HasSuffix(s, "KB"), strings.HasSuffix(s, "K"):
		mult, s = 1<<10, strings.TrimSuffix(strings.TrimSuffix(s, "KB"), "K")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}
	var f float64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%f", &f); err != nil {
		return 0, fmt.Errorf("cannot parse %q", s)
	}
	return int64(f * float64(mult)), nil
}

// syncWatch keeps all registered roots in sync until interrupted.
func syncWatch(args []string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}
	interval := durationFlag(args, "--interval", 30*time.Second)

	roots, err := listSyncRoots()
	if err != nil {
		fatal("%v", err)
	}
	if len(roots) == 0 {
		fatal("No sync folders registered. Add one with 'tessera sync add'.")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := SyncOptions{
		Verbose: hasFlag(args, "--verbose") || hasFlag(args, "-v"),
		Jobs:    intFlag(args, "--jobs", cfg.DefaultJobs),
		MaxRate: parseRate(args),
	}
	if p, ok := flagValue(args, "--policy"); ok {
		opts.ConflictMode = p
	}

	fmt.Printf("Tessera watch — %d folder(s), every %s. Press Ctrl-C to stop.\n", len(roots), interval)

	// Hold one SDK for the whole session so host connections stay warm.
	sdk, cleanup, err := connectSDK(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()
	rt := NewRemote(sdk)

	tick := time.NewTicker(interval)
	defer tick.Stop()

	engines := make(map[string]*SyncEngine, len(roots))
	for i := range roots {
		eng, err := loadEngine(&roots[i], rt, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ! %s: %v\n", roots[i].LocalPath, err)
			continue
		}
		engines[roots[i].ID] = eng
	}

	// Track directory mtimes so an unchanged tree costs one cheap stat per
	// file rather than a full hash pass.
	for {
		start := time.Now()
		changedAny := false
		for _, r := range roots {
			eng, ok := engines[r.ID]
			if !ok {
				continue
			}
			release, err := acquireLock(r.ID)
			if err != nil {
				continue
			}
			res, err := eng.SyncOnce(ctx)
			release()
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ! %s: %v\n", r.LocalPath, err)
				continue
			}
			if res.Uploaded+res.Downloaded+res.Deleted+res.Conflicts > 0 {
				changedAny = true
				fmt.Printf("[%s] %s: ↑%d ↓%d ✗%d ⚑%d (%s)\n",
					time.Now().Format("15:04:05"), filepath.Base(r.LocalPath),
					res.Uploaded, res.Downloaded, res.Deleted, res.Conflicts, res.Elapsed)
			}
		}
		if opts.Verbose && !changedAny {
			fmt.Printf("[%s] in sync (%.2fs)\n", time.Now().Format("15:04:05"), time.Since(start).Seconds())
		}

		select {
		case <-ctx.Done():
			fmt.Println("\nStopped.")
			return
		case <-tick.C:
		}
	}
}
