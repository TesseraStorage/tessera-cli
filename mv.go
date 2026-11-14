package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// cmdMv renames or moves a stored file (or folder) without transferring data.
// The indexer keeps the same object id; a tombstone at the old path tells
// tessera sync on every machine to drop the local copy there instead of
// uploading it again.
func cmdMv(args []string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	pos := positional(args)
	if len(pos) < 2 {
		usageError("mv <src> <dst> [--force] [--json]")
	}
	force := hasFlag(args, "--force")
	asJSON := hasFlag(args, "--json")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	rt, idx, cleanup, err := connectForWrite(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	res, err := resolveMv(idx, pos[0], pos[1], pendingMvPrefix(cfg.AppID, pos[0], pos[1]))
	if err != nil {
		fatal("%v", err)
	}
	moves := res.moves
	if len(moves) == 0 {
		fatal("nothing to move")
	}
	// A folder move is one indexer write per file. Remember where this one
	// is going so re-running the same command after an interruption sends
	// the remaining files to the same folder instead of nesting them inside
	// the part that already moved.
	if res.isDir {
		savePendingMv(cfg.AppID, pos[0], pos[1], res.dstPrefix)
	}

	type mvItem struct {
		Src      string `json:"src"`
		Dst      string `json:"dst"`
		ObjectID string `json:"object_id"`
		SHA256   string `json:"sha256,omitempty"`
		Size     int64  `json:"size"`
		Replaced bool   `json:"replaced,omitempty"`
	}

	var items []mvItem
	seenDst := map[string]string{}
	for _, m := range moves {
		if prev, ok := seenDst[m.dst]; ok && prev != m.src {
			fatal("cannot move both %s and %s to %s", prev, m.src, m.dst)
		}
		seenDst[m.dst] = m.src
		if m.src == m.dst {
			if !asJSON {
				fmt.Printf("Already at %s\n", m.src)
			}
			continue
		}
		srcEnt := idx.Paths[m.src]
		if srcEnt == nil {
			fatal("not found: %s", m.src)
		}
		replaced := false
		if destEnt := idx.Paths[m.dst]; destEnt != nil {
			if !force {
				fatal("%s already exists (use --force to replace it)", m.dst)
			}
			if err := trashOrPurgeEntry(ctx, rt, idx, destEnt); err != nil {
				fatal("replace %s: %v", m.dst, err)
			}
			replaced = true
		}
		info, err := rt.Reattach(ctx, pathInfoFromIndex(srcEnt), m.dst, srcEnt.RootID)
		if err != nil {
			fatal("move %s → %s: %v", m.src, m.dst, err)
		}
		idx.remove(m.src)
		idx.put(info)
		recordMoveInSyncState(m.src, m.dst, info)
		items = append(items, mvItem{
			Src:      m.src,
			Dst:      m.dst,
			ObjectID: info.ObjectID,
			SHA256:   info.SHA256,
			Size:     info.Size,
			Replaced: replaced,
		})
	}
	if err := updateIndex(func(fresh *Index) error {
		for _, it := range items {
			if ent := idx.Paths[it.Dst]; ent != nil {
				fresh.Paths[it.Dst] = ent
			}
			delete(fresh.Paths, it.Src)
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not update local index: %v\n", err)
	}
	if res.isDir {
		clearPendingMv(cfg.AppID, pos[0], pos[1])
	}

	if asJSON {
		emitJSON(map[string]any{
			"src":   strings.Trim(filepath.ToSlash(pos[0]), "/"),
			"dst":   strings.Trim(filepath.ToSlash(pos[1]), "/"),
			"count": len(items),
			"files": items,
		})
		return
	}
	if len(items) == 0 {
		return
	}
	if len(items) == 1 {
		line := fmt.Sprintf("Moved %s → %s", items[0].Src, items[0].Dst)
		if items[0].Replaced {
			line += " (replaced existing file)"
		}
		fmt.Println(line)
		return
	}
	fmt.Printf("Moved %d files\n", len(items))
	for _, it := range items {
		extra := ""
		if it.Replaced {
			extra = " (replaced existing file)"
		}
		fmt.Printf("  %s → %s%s\n", it.Src, it.Dst, extra)
	}
}

type mvTarget struct {
	src, dst string
}

// resolveMvTargets turns user-supplied src/dst into concrete file moves.
// A destination ending in / or an existing remote folder keeps the file
// (or folder) name. A source that names a remote prefix moves every file
// under that prefix.
func resolveMvTargets(idx *Index, srcRaw, dstRaw string) ([]mvTarget, error) {
	res, err := resolveMv(idx, srcRaw, dstRaw, "")
	return res.moves, err
}

type mvResolution struct {
	moves     []mvTarget
	isDir     bool
	dstPrefix string
}

// resolveMv is resolveMvTargets for cmdMv. resumePrefix, when set, is the
// destination folder an earlier interrupted run of the same folder move
// chose; the remaining files go there.
func resolveMv(idx *Index, srcRaw, dstRaw, resumePrefix string) (mvResolution, error) {
	var res mvResolution
	srcTrail := hasTrailingSlash(srcRaw)
	dstTrail := hasTrailingSlash(dstRaw)
	src, err := normalizeUserRemotePath(srcRaw)
	if err != nil {
		return res, fmt.Errorf("source: %w", err)
	}
	dst, err := normalizeUserRemotePath(dstRaw)
	if err != nil {
		return res, fmt.Errorf("destination: %w", err)
	}

	files, isDir, err := collectMvSource(idx, src, srcTrail)
	if err != nil {
		return res, err
	}

	dstIsDir := dstTrail || remoteFolderExists(idx, dst)
	if !dstIsDir && dstTrail {
		// Trailing slash asked for a folder that does not exist yet; treat
		// the destination as a folder prefix anyway (mkdir-style).
		dstIsDir = true
	}
	if dstTrail && idx.Paths[dst] != nil && !remoteFolderExists(idx, dst) {
		return res, fmt.Errorf("%s is a file, not a folder", dst)
	}

	var out []mvTarget
	if isDir {
		base := path.Base(src)
		dstPrefix := dst
		if dstIsDir {
			dstPrefix = path.Join(dst, base)
		}
		if resumePrefix != "" {
			dstPrefix = resumePrefix
		}
		if dstPrefix == src || strings.HasPrefix(dstPrefix+"/", src+"/") {
			return res, fmt.Errorf("cannot move %s into itself", src)
		}
		for _, e := range files {
			rel := strings.TrimPrefix(e.Path, src+"/")
			if rel == e.Path {
				return res, fmt.Errorf("internal path error moving %s", e.Path)
			}
			out = append(out, mvTarget{src: e.Path, dst: path.Join(dstPrefix, rel)})
		}
		return mvResolution{moves: out, isDir: true, dstPrefix: dstPrefix}, nil
	}

	destPath := dst
	if dstIsDir {
		destPath = path.Join(dst, path.Base(files[0].Path))
	}
	if destPath == src || strings.HasPrefix(destPath+"/", src+"/") {
		return res, fmt.Errorf("cannot move %s into itself", src)
	}
	out = append(out, mvTarget{src: files[0].Path, dst: destPath})
	return mvResolution{moves: out}, nil
}

func hasTrailingSlash(p string) bool {
	p = strings.ReplaceAll(filepath.ToSlash(p), `\`, "/")
	return strings.HasSuffix(p, "/")
}

func normalizeUserRemotePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(filepath.ToSlash(p), `\`, "/")
	p = strings.Trim(p, "/")
	if p == "" || p == "." {
		return "", fmt.Errorf("empty path")
	}
	return NormalizeRelPath(p)
}

func collectMvSource(idx *Index, src string, trail bool) ([]*IndexEntry, bool, error) {
	var under []*IndexEntry
	prefix := src + "/"
	for _, e := range idx.SortedPaths() {
		if strings.HasPrefix(e.Path, prefix) && !isReservedRemotePath(e.Path) {
			under = append(under, e)
		}
	}
	if trail {
		if len(under) == 0 {
			return nil, false, fmt.Errorf("folder not found: %s", src)
		}
		return under, true, nil
	}
	if e := idx.Paths[src]; e != nil {
		return []*IndexEntry{e}, false, nil
	}
	if len(under) > 0 {
		return under, true, nil
	}
	e, err := idx.lookup(src)
	if err != nil {
		return nil, false, err
	}
	return []*IndexEntry{e}, false, nil
}

func remoteFolderExists(idx *Index, p string) bool {
	prefix := p + "/"
	for _, e := range idx.Paths {
		if strings.HasPrefix(e.Path, prefix) {
			return true
		}
	}
	return false
}

// trashOrPurgeEntry removes a live destination using the same semantics as
// tessera delete (trash when retention is on, otherwise a permanent delete).
func trashOrPurgeEntry(ctx context.Context, rt *Remote, idx *Index, entry *IndexEntry) error {
	if trashRetention() > 0 {
		trashPath := path.Join(TrashPrefix, shortHash(entry.SHA256), entry.Path)
		if _, err := rt.Reattach(ctx, pathInfoFromIndex(entry), trashPath, entry.RootID); err != nil {
			return err
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
		recordDeleteInSyncState(entry.Path)
		return nil
	}
	if err := rt.DeleteObject(ctx, entry.ObjectID); err != nil {
		return err
	}
	if err := deleteVersions(ctx, rt, entry); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove some versions: %v\n", err)
	}
	idx.remove(entry.Path)
	recordDeleteInSyncState(entry.Path)
	return nil
}

// recordMoveInSyncState writes a tombstone at the old path and records the
// new one in every sync folder that owns either side. When both sides live in
// the same local folder, the file is renamed so the next sync does not
// download it again.
func recordMoveInSyncState(srcRemote, dstRemote string, info PathInfo) {
	roots, err := listSyncRoots()
	if err != nil {
		return
	}
	for i := range roots {
		r := &roots[i]
		eng := &SyncEngine{cfg: r}
		srcRel, srcOurs := eng.localPathForRemote(srcRemote)
		dstRel, dstOurs := eng.localPathForRemote(dstRemote)
		if !srcOurs && !dstOurs {
			continue
		}
		st, err := loadSyncState(r.ID)
		if err != nil {
			continue
		}
		now := time.Now()
		if srcOurs {
			old := st.Remote[srcRel]
			tomb := &RemoteEntry{Deleted: true, Updated: now, SHA256: info.SHA256, Size: info.Size}
			if old != nil && tomb.SHA256 == "" {
				tomb.SHA256 = old.SHA256
				tomb.Size = old.Size
			}
			st.Remote[srcRel] = tomb
			ent := st.entry(srcRel)
			ent.BaseSHA256 = ""
			ent.ObjectID = ""
			ent.RemotePath = ""
			ent.DeletedLocal = true
			ent.DeletedRemote = true
		}
		if dstOurs {
			copied := info
			copied.Path = dstRel
			st.applyRemoteEvent(dstRel, copied)
			ent := st.entry(dstRel)
			ent.BaseSHA256 = info.SHA256
			ent.ObjectID = info.ObjectID
			ent.RemotePath = dstRemote
			ent.Size = info.Size
			ent.ModTime = info.ModTime
			ent.DeletedLocal = false
			ent.DeletedRemote = false
		}

		srcAbs := filepath.Join(r.LocalPath, filepath.FromSlash(srcRel))
		dstAbs := filepath.Join(r.LocalPath, filepath.FromSlash(dstRel))
		switch {
		case srcOurs && dstOurs:
			if _, err := os.Stat(srcAbs); err == nil {
				if err := os.MkdirAll(filepath.Dir(dstAbs), 0755); err == nil {
					if err := os.Rename(srcAbs, dstAbs); err != nil {
						fmt.Fprintf(os.Stderr, "warning: could not rename local %s → %s: %v\n", srcRel, dstRel, err)
					} else {
						eng.pruneEmptyDirs(filepath.Dir(srcAbs))
					}
				}
			}
		case srcOurs && !dstOurs:
			if err := os.Remove(srcAbs); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "warning: could not remove local %s: %v\n", srcRel, err)
			} else {
				eng.pruneEmptyDirs(filepath.Dir(srcAbs))
			}
		}
		_ = st.save()
	}
}
