package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// TrashPrefix is the reserved remote namespace for soft-deleted files. They
// stay pinned (and therefore recoverable) until the retention window passes or
// the user purges them.
const TrashPrefix = ".tessera-trash"

// TrashedEntry records a soft deletion so restore does not need a rescan.
type TrashedEntry struct {
	OriginalPath string    `json:"original_path"`
	TrashPath    string    `json:"trash_path"`
	ObjectID     string    `json:"object_id"`
	SHA256       string    `json:"sha256,omitempty"`
	Size         int64     `json:"size"`
	DeletedAt    time.Time `json:"deleted_at"`
}

// TrashIndex is the on-disk list of soft-deleted files.
type TrashIndex struct {
	Schema  int            `json:"schema"`
	Entries []TrashedEntry `json:"entries"`
}

const trashSchema = 1

func trashIndexPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "trash.json"), nil
}

func loadTrash() (*TrashIndex, error) {
	p, err := trashIndexPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &TrashIndex{Schema: trashSchema}, nil
		}
		return nil, err
	}
	t := &TrashIndex{Schema: trashSchema}
	if err := json.Unmarshal(data, t); err != nil {
		return nil, fmt.Errorf("corrupt trash index: %w", err)
	}
	return t, nil
}

func (t *TrashIndex) save() error {
	p, err := trashIndexPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p, data, 0600)
}

// recordTrash appends a soft deletion.
func recordTrash(e TrashedEntry) {
	t, err := loadTrash()
	if err != nil {
		return
	}
	t.Entries = append(t.Entries, e)
	_ = t.save()
}

// recordTrashEntry is an alias used by the sync engine, which needs no
// distinction from a user-initiated soft delete.
func recordTrashEntry(e TrashedEntry) { recordTrash(e) }

// trashRetention reads the retention window from config.
func trashRetention() time.Duration {
	cfg, err := loadConfig()
	if err != nil {
		return 30 * 24 * time.Hour
	}
	if cfg.TrashRetentionDays < 0 {
		return 0
	}
	if cfg.TrashRetentionDays == 0 {
		return 30 * 24 * time.Hour
	}
	return time.Duration(cfg.TrashRetentionDays) * 24 * time.Hour
}

// cmdTrash implements list, restore, empty and rm.
func cmdTrash(args []string) {
	pos := positional(args)
	sub := "list"
	if len(pos) > 0 {
		sub = pos[0]
		pos = pos[1:]
	}

	switch sub {
	case "list", "ls":
		trashList(args)
	case "restore":
		if len(pos) < 1 {
			usageError("trash restore <original-path>")
		}
		trashRestore(pos[0])
	case "empty", "purge":
		trashEmpty(hasFlag(args, "--yes"))
	case "rm", "delete":
		if len(pos) < 1 {
			usageError("trash rm <original-path>")
		}
		trashRemove(pos[0])
	default:
		usageError("trash list|restore <path>|empty|rm <path>")
	}
}

func trashList(args []string) {
	t, err := loadTrash()
	if err != nil {
		fatal("%v", err)
	}
	if hasFlag(args, "--json") {
		emitJSON(t)
		return
	}
	if len(t.Entries) == 0 {
		fmt.Println("Trash is empty.")
		return
	}
	retention := trashRetention()
	fmt.Printf("%-44s  %10s  %-20s  %s\n", "ORIGINAL PATH", "SIZE", "DELETED", "EXPIRES")
	fmt.Println(strings.Repeat("-", 90))
	for _, e := range t.Entries {
		expires := "never"
		if retention > 0 {
			expires = e.DeletedAt.Add(retention).Format("2006-01-02")
		}
		fmt.Printf("%-44s  %10s  %-20s  %s\n",
			truncate(e.OriginalPath, 44), formatBytes(uint64(e.Size)),
			e.DeletedAt.Format("2006-01-02 15:04:05"), expires)
	}
	fmt.Printf("\n%d item(s). Restore with: tessera trash restore <path>\n", len(t.Entries))
}

func trashRestore(original string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}
	t, err := loadTrash()
	if err != nil {
		fatal("%v", err)
	}

	idxMatch := -1
	for i, e := range t.Entries {
		if e.OriginalPath == original || path.Base(e.OriginalPath) == original {
			idxMatch = i
			break
		}
	}
	if idxMatch < 0 {
		fatal("not in trash: %s (see 'tessera trash list')", original)
	}
	entry := t.Entries[idxMatch]

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rt, idx, cleanup, err := connectForWrite(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	info, err := rt.HeadPath(ctx, entry.TrashPath)
	if err != nil {
		fatal("trashed object not found (%s): %v", entry.TrashPath, err)
	}
	if _, err := rt.Reattach(ctx, info, entry.OriginalPath, info.Root); err != nil {
		fatal("restore: %v", err)
	}

	idx.put(PathInfo{
		ObjectID: info.ObjectID, Path: entry.OriginalPath, Name: path.Base(entry.OriginalPath),
		Root: info.Root, SHA256: info.SHA256, Size: info.Size, ModTime: info.ModTime,
	})
	idx.remove(entry.TrashPath)
	_ = idx.save()

	t.Entries = append(t.Entries[:idxMatch], t.Entries[idxMatch+1:]...)
	_ = t.save()

	fmt.Printf("Restored: %s\n", entry.OriginalPath)
	fmt.Println("Run 'tessera sync' to bring it back into a synced folder.")
}

// trashRemove permanently deletes one trashed item.
func trashRemove(original string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}
	t, err := loadTrash()
	if err != nil {
		fatal("%v", err)
	}
	idxMatch := -1
	for i, e := range t.Entries {
		if e.OriginalPath == original || path.Base(e.OriginalPath) == original {
			idxMatch = i
			break
		}
	}
	if idxMatch < 0 {
		fatal("not in trash: %s", original)
	}
	entry := t.Entries[idxMatch]

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	sdk, cleanup, err := connectSDK(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()
	rt := NewRemote(sdk)
	if err := rt.DeleteObject(ctx, entry.ObjectID); err != nil {
		fatal("purge: %v", err)
	}
	t.Entries = append(t.Entries[:idxMatch], t.Entries[idxMatch+1:]...)
	_ = t.save()
	fmt.Printf("Permanently deleted: %s\n", entry.OriginalPath)
}

// trashEmpty purges everything past (and optionally within) the window.
func trashEmpty(yes bool) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}
	t, err := loadTrash()
	if err != nil {
		fatal("%v", err)
	}
	if len(t.Entries) == 0 {
		fmt.Println("Trash is already empty.")
		return
	}
	if !yes {
		fmt.Printf("Permanently delete %d trashed item(s)? [y/N] ", len(t.Entries))
		var a string
		fmt.Scanln(&a)
		if strings.ToLower(a) != "y" {
			fmt.Println("Aborted.")
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	sdk, cleanup, err := connectSDK(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()
	rt := NewRemote(sdk)

	var kept []TrashedEntry
	var purged int
	for _, e := range t.Entries {
		if err := rt.DeleteObject(ctx, e.ObjectID); err != nil {
			fmt.Fprintf(os.Stderr, "  ! %s: %v\n", e.OriginalPath, err)
			kept = append(kept, e)
			continue
		}
		purged++
	}
	t.Entries = kept
	_ = t.save()
	fmt.Printf("Purged %d item(s).\n", purged)
}

// expireTrash purges items whose retention window has elapsed. It is called
// opportunistically at the start of a sync so the window is enforced without a
// daemon of its own.
func expireTrash(ctx context.Context, rt *Remote) int {
	retention := trashRetention()
	if retention <= 0 {
		return 0
	}
	t, err := loadTrash()
	if err != nil || len(t.Entries) == 0 {
		return 0
	}
	cutoff := time.Now().Add(-retention)
	var kept []TrashedEntry
	var purged int
	for _, e := range t.Entries {
		if e.DeletedAt.After(cutoff) {
			kept = append(kept, e)
			continue
		}
		if err := rt.DeleteObject(ctx, e.ObjectID); err != nil {
			kept = append(kept, e)
			continue
		}
		purged++
	}
	if purged > 0 {
		t.Entries = kept
		_ = t.save()
	}
	return purged
}
