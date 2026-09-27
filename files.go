package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	dataShards   = 10
	parityShards = 20
)

// connectForWrite opens an SDK plus a local index rebuilt from the network.
// Mutating commands must not act on a cached index: a file may have moved,
// been deleted, or been uploaded elsewhere since the cache was written, and a
// stale lookup fails with a confusing "object not found".
//
// Read-only commands use connectAPI instead, which avoids host warmup.
func connectForWrite(ctx context.Context, cfg *Config) (*Remote, *Index, func(), error) {
	sdk, cleanup, err := connectSDK(ctx, cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	rt := NewRemote(sdk)
	idx, err := refreshIndex(ctx, rt, cfg)
	if err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	return rt, idx, cleanup, nil
}

func cmdList(args []string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sdk, cleanup, err := connectSDK(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	rt := NewRemote(sdk)
	idx, err := rebuildIndex(ctx, rt, cfg.AppID)
	if err != nil {
		fatal("list: %v", err)
	}

	pos := positional(args)
	prefix := ""
	if len(pos) > 0 {
		prefix = strings.Trim(filepath.ToSlash(pos[0]), "/")
	}

	entries := idx.SortedPaths()
	var shown []*IndexEntry
	var total uint64
	for _, e := range entries {
		if prefix != "" && !strings.HasPrefix(e.Path, prefix) {
			continue
		}
		shown = append(shown, e)
		total += uint64(e.Size)
	}

	if hasFlag(args, "--json") {
		emitJSON(map[string]interface{}{
			"prefix": prefix,
			"count":  len(shown),
			"bytes":  total,
			"files":  shown,
		})
		return
	}

	if len(shown) == 0 {
		if prefix != "" {
			fmt.Printf("No files under %q.\n", prefix)
		} else {
			fmt.Println("No files.")
		}
		return
	}

	fmt.Printf("%-44s  %10s  %s\n", "PATH", "SIZE", "MODIFIED")
	fmt.Println(strings.Repeat("-", 78))
	for _, e := range shown {
		fmt.Printf("%-44s  %10s  %s\n", truncate(e.Path, 44), formatBytes(uint64(e.Size)), displayTime(e))
	}
	fmt.Println(strings.Repeat("-", 78))
	fmt.Printf("%d file(s), %s\n", len(shown), formatBytes(total))
}

// cmdUploadPath uploads a file or, when given a directory, uploads every file
// inside it preserving relative paths.
func cmdUploadPath(src string, args []string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	fi, err := os.Stat(src)
	if err != nil {
		fatal("cannot read %s: %v", src, err)
	}

	asFlag, _ := flagValue(args, "--as")

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()

	rt, idx, cleanup, err := connectForWrite(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	if fi.IsDir() {
		uploadDirectory(ctx, rt, idx, src, asFlag)
		return
	}

	remotePath := path.Base(src)
	if asFlag != "" {
		rel, err := NormalizeRelPath(asFlag)
		if err != nil {
			fatal("--as: %v", err)
		}
		remotePath = rel
	}

	if err := uploadOne(ctx, rt, idx, src, remotePath); err != nil {
		fatal("%v", err)
	}
}

// uploadDirectory walks a local directory and uploads each regular file under
// the remote prefix, preserving relative paths.
func uploadDirectory(ctx context.Context, rt *Remote, idx *Index, root, prefix string) {
	// A directory upload treats --as as a prefix so that nested files keep
	// their structure instead of all collapsing onto one remote path.
	clean, err := NormalizeRelPath(prefix)
	switch {
	case err != nil:
		// No --as given: namespace it under tessera/ so a plain directory
		// upload cannot collide with another folder of the same name.
		prefix = path.Join("tessera", path.Base(filepath.ToSlash(root)))
	default:
		prefix = clean
	}

	ig, _ := newIgnore(root, nil)

	var files []string
	var skipped int
	if err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel := p
		if r, rerr := filepath.Rel(root, p); rerr == nil {
			rel = filepath.ToSlash(r)
		}
		if info.IsDir() {
			if rel != "." && ig.Match(rel, true) {
				skipped++
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if ig.Match(rel, false) || strings.HasPrefix(filepath.Base(p), ".tessera-tmp-") {
			skipped++
			return nil
		}
		files = append(files, p)
		return nil
	}); err != nil {
		fatal("walk %s: %v", root, err)
	}
	if skipped > 0 {
		fmt.Printf("Skipped %d ignored file(s) (see .tesseraignore).\n", skipped)
	}
	sort.Strings(files)

	fmt.Printf("Uploading %d file(s) from %s → %s/\n", len(files), root, prefix)
	var uploaded int
	var bytes int64
	for i, p := range files {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			continue
		}
		remotePath := path.Join(prefix, filepath.ToSlash(rel))
		if err := uploadOne(ctx, rt, idx, p, remotePath); err != nil {
			fmt.Fprintf(os.Stderr, "  ! %s: %v\n", rel, err)
			continue
		}
		uploaded++
		if fi, err := os.Stat(p); err == nil {
			bytes += fi.Size()
		}
		if (i+1)%10 == 0 || i+1 == len(files) {
			fmt.Printf("  %d/%d (%.1f%%)\n", i+1, len(files), float64(i+1)/float64(len(files))*100)
		}
	}
	fmt.Printf("Done: %d/%d uploaded, %s\n", uploaded, len(files), formatBytes(uint64(bytes)))
}

// uploadOne uploads a single file and updates the local index.
func uploadOne(ctx context.Context, rt *Remote, idx *Index, localPath, remotePath string) error {
	fi, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	hash, err := hashFile(localPath)
	if err != nil {
		return fmt.Errorf("hash %s: %w", localPath, err)
	}

	// Content-addressed short circuit: an object has exactly one logical path,
	// so the only case that needs no upload is when the identical content is
	// already stored at this very path. Re-pointing an object that lives at
	// another path would delete that other path, silently removing a file the
	// user still expects to exist.
	if existing, ok := idx.byHash(hash); ok && existing.ObjectID != "" && existing.Path == remotePath {
		if _, err := rt.Reattach(ctx, pathInfoFromIndex(existing), remotePath, existing.Root); err == nil {
			dinfo := PathInfo{
				ObjectID: existing.ObjectID, Path: remotePath, Name: path.Base(remotePath),
				Root: existing.Root, SHA256: hash, Size: fi.Size(), ModTime: fi.ModTime(),
				Legacy: existing.Legacy,
			}
			idx.put(dinfo)
			_ = idx.save()
			recordUploadInSyncState(dinfo, fi.ModTime())
			fmt.Printf("  = %s already stored with identical content\n", remotePath)
			return nil
		}
	}

	start := time.Now()
	info, err := rt.Upload(ctx, uploadRequest{
		LocalPath: localPath,
		RelPath:   remotePath,
		Name:      path.Base(remotePath),
		Hash:      hash,
		Size:      fi.Size(),
		Mode:      fi.Mode(),
		ModTime:   fi.ModTime(),
	}, nil)
	if err != nil {
		return err
	}
	idx.put(info)
	if err := idx.save(); err != nil {
		return err
	}
	// If this path belongs to a registered sync folder, record it there as
	// well. Otherwise the next `tessera sync` would treat the file as both new
	// locally and new remotely and download its own upload back.
	recordUploadInSyncState(info, fi.ModTime())
	fmt.Printf("  ↑ %s (%s, %.1fs)\n", remotePath, formatBytes(uint64(fi.Size())), time.Since(start).Seconds())
	return nil
}

// recordUploadInSyncState updates a sync root's state after an upload made
// through the plain CLI, so sync and upload share one source of truth.
func recordUploadInSyncState(info PathInfo, modTime time.Time) {
	roots, err := listSyncRoots()
	if err != nil {
		return
	}
	for i := range roots {
		r := &roots[i]
		rel, ours := (&SyncEngine{cfg: r}).localPathForRemote(info.Path)
		if !ours {
			continue
		}
		// The path belongs to this root, so tag the object with the root's id.
		// Version and trash namespaces are derived from it.
		info.Root = r.ID
		st, err := loadSyncState(r.ID)
		if err != nil {
			continue
		}
		ent := st.entry(rel)
		ent.BaseSHA256 = info.SHA256
		ent.ObjectID = info.ObjectID
		ent.RemotePath = info.Path
		ent.Size = info.Size
		ent.ModTime = modTime
		ent.DeletedLocal = false
		ent.DeletedRemote = false
		ent.Untracked = false
		st.applyRemoteEvent(rel, info)
		_ = st.save()
	}
}

// byHash finds any indexed object with the given content hash. The map is
// rebuilt lazily per batch of uploads rather than scanned per file, which
// matters when uploading a folder with thousands of entries.
func (i *Index) byHash(h string) (*IndexEntry, bool) {
	if h == "" {
		return nil, false
	}
	if i.byHashCache == nil {
		i.byHashCache = make(map[string]*IndexEntry, len(i.Paths))
		for _, e := range i.Paths {
			if e.SHA256 != "" && !e.Legacy {
				if _, exists := i.byHashCache[e.SHA256]; !exists {
					i.byHashCache[e.SHA256] = e
				}
			}
		}
	}
	e, ok := i.byHashCache[h]
	return e, ok
}

func pathInfoFromIndex(e *IndexEntry) PathInfo {
	return PathInfo{
		ObjectID: e.ObjectID, Path: e.Path, Name: e.Name, Root: e.RootID,
		SHA256: e.SHA256, Size: e.Size, ModTime: e.ModTime, Machine: e.Machine,
		Legacy: e.Legacy,
	}
}

func cmdDownload(args []string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	pos := positional(args)
	name := pos[0]
	outName, _ := flagValue(args, "--out")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	rt, idx, cleanup, err := connectForWrite(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	entry, err := idx.lookup(name)
	if err != nil {
		fatal("%v", err)
	}

	dest := outName
	if dest == "" {
		dest = filepath.Base(entry.Path)
	}

	fmt.Printf("Downloading %s (%s)...\n", entry.Path, formatBytes(uint64(entry.Size)))

	start := time.Now()
	n, err := rt.Download(ctx, pathInfoFromIndex(entry), dest, 0644)
	if err != nil {
		fatal("download: %v", err)
	}
	elapsed := time.Since(start)

	got, err := hashFile(dest)
	if err != nil {
		fatal("verify %s: %v", dest, err)
	}
	if entry.SHA256 != "" && got != entry.SHA256 {
		fatal("integrity check failed for %s: expected %s, got %s", entry.Path, shortHash(entry.SHA256), shortHash(got))
	}

	speed := float64(n) / elapsed.Seconds() / (1 << 20)
	fmt.Printf("Downloaded %s in %.1fs (%.2f MB/s)\n", formatBytes(uint64(n)), elapsed.Seconds(), speed)
	if entry.SHA256 != "" {
		fmt.Printf("SHA-256 verified: %s\n", entry.SHA256)
	} else {
		fmt.Printf("SHA-256: %s (no reference hash stored)\n", got)
	}
	fmt.Printf("Saved as: %s\n", dest)
}

func cmdDelete(args []string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	pos := positional(args)
	name := pos[0]
	purge := hasFlag(args, "--purge")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	rt, idx, cleanup, err := connectForWrite(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	entry, err := idx.lookup(name)
	if err != nil {
		fatal("%v", err)
	}

	// Trashed objects are recoverable for the retention window unless the
	// caller explicitly purges.
	assumeYes := hasFlag(args, "--yes") || hasFlag(args, "-y")
	if !purge && trashRetention() > 0 {
		trashPath := path.Join(TrashPrefix, shortHash(entry.SHA256), entry.Path)
		info := pathInfoFromIndex(entry)
		if _, err := rt.Reattach(ctx, info, trashPath, entry.Root); err != nil {
			fatal("trash %s: %v", entry.Path, err)
		}
		recordTrash(TrashedEntry{
			OriginalPath: entry.Path,
			TrashPath:    trashPath,
			ObjectID:     entry.ObjectID,
			SHA256:       entry.SHA256,
			Size:         entry.Size,
			DeletedAt:    time.Now(),
		})
		idx.remove(entry.Path)
		idx.put(PathInfo{
			ObjectID: entry.ObjectID, Path: trashPath, Name: path.Base(trashPath),
			Root: entry.RootID, SHA256: entry.SHA256, Size: entry.Size, ModTime: entry.ModTime,
		})
		_ = idx.save()
		recordDeleteInSyncState(entry.Path)
		fmt.Printf("Moved to trash: %s\n", entry.Path)
		fmt.Printf("Restore with:   tessera trash restore %s\n", entry.Path)
		return
	}

	if !assumeYes {
		fmt.Printf("Permanently delete %s (%s)? This cannot be undone. [y/N] ", entry.Path, formatBytes(uint64(entry.Size)))
		var a string
		fmt.Scanln(&a)
		if strings.ToLower(a) != "y" {
			fmt.Println("Aborted.")
			return
		}
	}

	if err := rt.DeleteObject(ctx, entry.ObjectID); err != nil {
		fatal("delete: %v", err)
	}
	if err := deleteVersions(ctx, rt, entry); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove some versions: %v\n", err)
	}
	idx.remove(entry.Path)
	_ = idx.save()
	recordDeleteInSyncState(entry.Path)
	fmt.Printf("Deleted: %s\n", entry.Path)
}

// recordDeleteInSyncState marks a path as deleted in whichever sync root owns
// it, so the next sync propagates the deletion instead of restoring the file.
func recordDeleteInSyncState(remotePath string) {
	roots, err := listSyncRoots()
	if err != nil {
		return
	}
	for i := range roots {
		r := &roots[i]
		rel, ours := (&SyncEngine{cfg: r}).localPathForRemote(remotePath)
		if !ours {
			continue
		}
		st, err := loadSyncState(r.ID)
		if err != nil {
			continue
		}
		ent := st.entry(rel)
		ent.DeletedLocal = true
		ent.BaseSHA256 = ""
		st.save()
	}
}

// relativeToPrefix strips a remote prefix from a remote-absolute path.
func relativeToPrefix(prefix, p string) (string, bool) {
	prefix = strings.Trim(prefix, "/")
	p = strings.Trim(p, "/")
	if prefix == "" {
		return p, true
	}
	if p == prefix {
		return "", false
	}
	if !strings.HasPrefix(p, prefix+"/") {
		return "", false
	}
	return strings.TrimPrefix(p, prefix+"/"), true
}

// deleteVersions removes retained copies that belong to a deleted path.
func deleteVersions(ctx context.Context, rt *Remote, entry *IndexEntry) error {
	if entry.Root == "" {
		return nil
	}
	// Match the namespace by marker so this works whichever spelling the
	// record used.
	all, err := rt.LivePaths(ctx)
	if err != nil {
		return err
	}
	marker := VersionsPrefix + "/" + entry.Path + "/"
	for p, info := range all {
		if i := strings.Index(p, marker); i >= 0 && isVersionPath(p[:i], p) {
			if err := rt.DeleteObject(ctx, info.ObjectID); err != nil {
				return err
			}
		}
	}
	return nil
}

func cmdShare(name string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	rt, idx, cleanup, err := connectForWrite(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	entry, err := idx.lookup(name)
	if err != nil {
		fatal("%v", err)
	}

	sharedURL, err := rt.sdk.CreateSharedObjectURL(ctx, mustObjectID(entry.ObjectID), time.Now().Add(30*24*time.Hour))
	if err != nil {
		fatal("share: %v", err)
	}

	fmt.Printf("Share link (valid 30 days):\n  %s\n", sharedURL)
	fmt.Println("Anyone with this link can download the file.")
	fmt.Println()
	fmt.Printf("Download with:  tessera fetch \"%s\" %s\n", sharedURL, filepath.Base(entry.Path))
}

func cmdFetch(sharedURL, outName string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	if outName == "" {
		parts := strings.Split(sharedURL, "/")
		for i, p := range parts {
			if p == "objects" && i+1 < len(parts) {
				outName = "download-" + parts[i+1][:12]
				break
			}
		}
	}
	if outName == "" {
		outName = "download"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	sdk, cleanup, err := connectSDK(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	fmt.Printf("Downloading from shared link...\n")

	start := time.Now()
	rc, err := sdk.DownloadSharedObject(ctx, sharedURL)
	if err != nil {
		fatal("download: %v", err)
	}
	defer rc.Close()

	out, err := os.Create(outName)
	if err != nil {
		fatal("create %s: %v", outName, err)
	}
	defer out.Close()

	hasher := sha256.New()
	tee := io.TeeReader(rc, hasher)
	n, err := io.Copy(out, tee)
	if err != nil {
		fatal("read: %v", err)
	}

	elapsed := time.Since(start)
	speed := float64(n) / elapsed.Seconds() / (1 << 20)
	fmt.Printf("Downloaded %s in %.1fs (%.2f MB/s)\n", formatBytes(uint64(n)), elapsed.Seconds(), speed)
	fmt.Printf("SHA-256: %s\n", hex.EncodeToString(hasher.Sum(nil)))
	fmt.Printf("Saved as: %s\n", outName)
}

// cmdIndex rebuilds or reports on the local index.
func cmdIndex(args []string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sdk, cleanup, err := connectSDK(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	if hasFlag(args, "--rebuild") || func() bool { _, err := loadIndex(); return err != nil }() {
		idx, err := rebuildIndex(ctx, NewRemote(sdk), cfg.AppID)
		if err != nil {
			fatal("rebuild index: %v", err)
		}
		if hasFlag(args, "--json") {
			emitJSON(map[string]interface{}{"rebuilt": true, "files": len(idx.Paths), "built_at": idx.BuiltAt})
			return
		}
		fmt.Printf("Index rebuilt: %d file(s)\n", len(idx.Paths))
		return
	}

	idx, err := loadIndex()
	if err != nil {
		fatal("%v", err)
	}
	if hasFlag(args, "--json") {
		emitJSON(idx)
		return
	}
	fmt.Printf("Index:    %s\n", mustIndexPath())
	fmt.Printf("Files:    %d\n", len(idx.Paths))
	fmt.Printf("Size:     %s\n", formatBytes(idx.TotalSize()))
	fmt.Printf("Built at: %s\n", idx.BuiltAt.Format(time.RFC3339))
}

func mustIndexPath() string {
	p, _ := indexPath()
	return p
}

// cmdFind searches file names, paths and (optionally) content.
func cmdFind(args []string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}
	pos := positional(args)
	if len(pos) == 0 {
		usageError("find <pattern> [--content] [--json]")
	}
	pattern := pos[0]
	byContent := hasFlag(args, "--content")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	sdk, cleanup, err := connectSDK(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	rt := NewRemote(sdk)
	idx, err := ensureIndex(ctx, rt, cfg)
	if err != nil {
		fatal("%v", err)
	}

	lower := strings.ToLower(pattern)
	var matches []*IndexEntry
	for _, e := range idx.SortedPaths() {
		if strings.Contains(strings.ToLower(e.Path), lower) {
			matches = append(matches, e)
		}
	}

	if byContent && len(matches) == 0 {
		// Content search downloads candidates of plausible text type and size.
		fmt.Fprintf(os.Stderr, "Searching content across %d file(s) (this downloads data)...\n", len(idx.Paths))
		for _, e := range idx.SortedPaths() {
			if e.Size > 4<<20 {
				continue
			}
			if !plausibleText(e.Path) {
				continue
			}
			ok, err := objectContains(ctx, rt, e, pattern)
			if err != nil || !ok {
				continue
			}
			matches = append(matches, e)
		}
	}

	if hasFlag(args, "--json") {
		emitJSON(map[string]interface{}{"pattern": pattern, "count": len(matches), "files": matches})
		return
	}
	if len(matches) == 0 {
		fmt.Printf("No matches for %q.\n", pattern)
		return
	}
	fmt.Printf("%-52s  %10s  %s\n", "PATH", "SIZE", "MODIFIED")
	fmt.Println(strings.Repeat("-", 86))
	for _, e := range matches {
		fmt.Printf("%-52s  %10s  %s\n", truncate(e.Path, 52), formatBytes(uint64(e.Size)), displayTime(e))
	}
	fmt.Printf("%d match(es)\n", len(matches))
}

// plausibleText applies a cheap extension filter before downloading content.
func plausibleText(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".txt", ".md", ".markdown", ".json", ".yaml", ".yml", ".toml", ".csv", ".tsv",
		".log", ".go", ".rs", ".py", ".js", ".ts", ".tsx", ".jsx", ".c", ".h", ".cc", ".cpp",
		".java", ".rb", ".sh", ".bash", ".zsh", ".sql", ".html", ".htm", ".css", ".xml", ".ini", ".cfg", ".conf":
		return true
	}
	return false
}

// objectContains streams an object and reports whether needle appears in it.
func objectContains(ctx context.Context, rt *Remote, e *IndexEntry, needle string) (bool, error) {
	obj, err := rt.sdk.Object(ctx, mustObjectID(e.ObjectID))
	if err != nil {
		return false, err
	}
	rc, err := rt.sdk.Download(obj)
	if err != nil {
		return false, err
	}
	defer rc.Close()

	want := []byte(strings.ToLower(needle))
	buf := make([]byte, 0, 64<<10)
	chunk := make([]byte, 32<<10)
	for {
		n, err := rc.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			lowered := strings.ToLower(string(buf))
			if strings.Contains(lowered, string(want)) {
				return true, nil
			}
			// Keep a tail so matches spanning a boundary still register.
			if len(buf) > len(want)+64<<10 {
				buf = buf[len(buf)-(len(want)+1024):]
			}
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// cmdVersions lists or restores retained versions of a path.
func cmdVersions(args []string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}
	pos := positional(args)
	if len(pos) == 0 {
		usageError("versions <path> [--restore <n>] [--json]")
	}
	target := pos[0]

	restoreAt, hasRestore := flagValue(args, "--restore")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	rt, idx, cleanup, err := connectForWrite(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	entry, err := idx.lookup(target)
	if err != nil {
		fatal("%v", err)
	}
	if entry.RootID == "" {
		// The index is rebuilt from object metadata, which carries the sync
		// root id. Without it there is no namespace to look in, but the object
		// may still have versions recorded by state.
		fmt.Fprintf(os.Stderr, "note: %s is not tagged with a sync folder; checking recorded history\n", entry.Path)
	}

	// Relative path inside the root, used for every namespace derivation.
	relPath := entry.Path
	if entry.RootID != "" {
		if rel, ok := relativeToPrefix(entry.Root, entry.Path); ok {
			relPath = rel
		}
	}

	// Find the retained copies of this path. The namespace is matched against
	// several spellings because a version entry holds "<remote prefix>/
	// .tessera-versions/<path>/<stamp>" while the sync root's id is a separate
	// value; matching on the marker keeps this working regardless of which one
	// a given record used.
	var nsCandidates []string
	if entry.RootID != "" {
		nsCandidates = append(nsCandidates,
			path.Join(entry.RootID, VersionsPrefix, relPath),
			path.Join(entry.RootID, VersionsPrefix, path.Base(relPath)))
	}
	// Legacy versions sit in a namespace beside the file, derived from the
	// remote prefix: "<prefix>/.tessera-versions/<name>/<stamp>-<sha>".
	nsCandidates = append(nsCandidates,
		path.Join(path.Dir(entry.Path), VersionsPrefix, path.Base(entry.Path)))
	if entry.Root != "" {
		nsCandidates = append(nsCandidates, path.Join(entry.Root, VersionsPrefix, relPath))
	}
	live := make(map[string]PathInfo)
	var listErr error
	for _, ns := range nsCandidates {
		found, perr := rt.Paths(ctx, ns)
		if perr != nil {
			listErr = perr
			continue
		}
		for p, info := range found {
			live[p] = info
		}
	}
	if len(live) == 0 {
		// Depending on when a version was recorded its stored path is either
		// "<remote prefix>/.tessera-versions/<path>" or
		// "<root id>/.tessera-versions/<path>", so recognise both by their
		// namespace suffix rather than by an exact prefix.
		suffix := VersionsPrefix + "/" + relPath
		all, aerr := rt.LivePaths(ctx)
		if aerr != nil {
			listErr = aerr
		}
		for p, info := range all {
			base := p
			if i := strings.LastIndex(p, "/"+VersionsPrefix+"/"); i >= 0 {
				base = p[i+1:]
			}
			if base == suffix || strings.HasPrefix(base, suffix+"/") {
				live[p] = info
			}
		}
	}
	if listErr != nil {
		// A retained object can disappear between indexing and fetching (it is
		// unpinned elsewhere, or a tombstone has not propagated yet). That is a
		// reason to report fewer versions, not to fail the command.
		fmt.Fprintf(os.Stderr, "note: could not fully list versions for %s: %v\n", entry.Path, listErr)
		live = nil
	}
	if len(live) == 0 {
		fmt.Printf("No retained versions for %s yet.\n", entry.Path)
		fmt.Printf("The previous copy is kept the next time this file is replaced (retention: %d).\n", versionRetention())
		fmt.Println("Keep more with: tessera config --version-retention 5")
		return
	}

	versions := make([]PathInfo, 0, len(live))
	for _, v := range live {
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].ModTime.Before(versions[j].ModTime) })

	if hasFlag(args, "--json") {
		emitJSON(versions)
		return
	}

	if hasRestore {
		var n int
		fmt.Sscanf(restoreAt, "%d", &n)
		if n < 1 || n > len(versions) {
			fatal("--restore expects a number between 1 and %d", len(versions))
		}
		chosen := versions[n-1]
		if _, err := rt.Reattach(ctx, chosen, entry.Path, entry.Root); err != nil {
			if isNotFound(err) {
				fatal("version %d of %s is no longer on the network (it may have been purged); run 'tessera versions %s' again", n, entry.Path, entry.Path)
			}
			fatal("restore: %v", err)
		}
		// The current copy becomes a version so the restore is reversible.
		restoreNamespace := path.Join(entry.Root, VersionsPrefix, relPath)
		if entry.RootID != "" {
			restoreNamespace = path.Join(entry.RootID, VersionsPrefix, relPath)
		}
		_, _ = rt.Reattach(ctx, pathInfoFromIndex(entry),
			path.Join(restoreNamespace, fmt.Sprintf("%s-restored-over", time.Now().UTC().Format("20060102-150405"))), entry.RootID)
		idx.put(PathInfo{
			ObjectID: chosen.ObjectID, Path: entry.Path, Name: path.Base(entry.Path),
			Root: entry.RootID, SHA256: chosen.SHA256, Size: chosen.Size, ModTime: chosen.ModTime,
		})
		_ = idx.save()
		fmt.Printf("Restored %s from version %d.\n", entry.Path, n)
		fmt.Println("Run 'tessera sync' to bring it back into any synced folder.")
		return
	}

	fmt.Printf("Versions of %s (oldest first):\n\n", entry.Path)
	for i, v := range versions {
		fmt.Printf("  [%d]  %s  %10s  %s\n", i+1, v.ModTime.Format("2006-01-02 15:04:05"),
			formatBytes(uint64(v.Size)), shortHash(v.SHA256))
	}
	fmt.Printf("\nRestore with: tessera versions %s --restore <n>\n", entry.Path)
}

// displayTime prefers the file's own mtime but falls back to the indexer's
// event time for objects uploaded before sync metadata existed.
func displayTime(e *IndexEntry) string {
	t := e.ModTime
	if t.IsZero() {
		t = e.Updated
	}
	if t.IsZero() {
		return "unknown"
	}
	return t.Format("2006-01-02 15:04")
}

// mustObjectID parses a hex object id, fataling on corruption. Index entries
// are machine-written, so a bad id means the index is damaged.
func mustObjectID(s string) (out hash256) {
	if err := out.UnmarshalText([]byte(s)); err != nil {
		fatal("corrupt object id in local index: %q (run 'tessera index --rebuild')", s)
	}
	return out
}

var _ = json.Marshal
