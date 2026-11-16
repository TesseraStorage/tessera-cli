package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
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
	shown := []*IndexEntry{}
	var total uint64
	for _, e := range entries {
		// Trash and version copies are listed by 'tessera trash list' and
		// 'tessera versions', not as files.
		if isInternalPath(e.Path) {
			continue
		}
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

	fmt.Printf("%-44s  %10s  %-19s  %s\n", "PATH", "SIZE", "UPLOADED", "MODIFIED")
	fmt.Println(strings.Repeat("-", 100))
	for _, e := range shown {
		fmt.Printf("%-44s  %10s  %-19s  %s\n", truncate(e.Path, 44), formatBytes(uint64(e.Size)), uploadedTime(e), modifiedTime(e))
	}
	fmt.Println(strings.Repeat("-", 100))
	fmt.Printf("%d file(s), %s\n", len(shown), formatBytes(total))
	fmt.Println("(UPLOADED = time stored on Tessera; MODIFIED = local file mtime when known)")
}

// cmdUploadPath uploads a file or, when given a directory, uploads every file
// inside it preserving relative paths.
func cmdUploadPath(src string, args []string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	// Input is checked first, all on this machine: a mistyped flag, a bad
	// --as or an unreadable file is reported as such, never as a balance
	// problem, and nothing is sent.
	if err := rejectUnknownFlags("upload", args, "--as", "--quiet", "-q", "--json"); err != nil {
		fatal("%v", err)
	}
	if extra := positional(args); len(extra) > 1 {
		fatal("upload takes one file or folder (got %d paths: %s); upload a folder, or run one upload per file", len(extra), strings.Join(extra, ", "))
	}
	asJSON := hasFlag(args, "--json")
	if asJSON {
		uploadOut = os.Stderr
	}

	fi, err := os.Stat(src)
	if err != nil {
		fatal("cannot read %s: %v", src, plainFileError(err))
	}
	if !fi.IsDir() {
		if !fi.Mode().IsRegular() {
			fatal("cannot upload %s: it is not a regular file", src)
		}
		f, err := os.Open(src)
		if err != nil {
			fatal("cannot read %s: %v", src, plainFileError(err))
		}
		f.Close()
	}

	asFlag, hasAs := flagValue(args, "--as")
	if hasAs {
		rel, err := checkRemotePath(asFlag)
		if err != nil {
			fatal("--as: %v", err)
		}
		asFlag = rel
	}

	// Size is known from the local stat. Refuse a technical-validation upload
	// that cannot fit in one slab before any host or indexer call, so the one
	// allowed upload is not started and then burned on the next slab.
	if !fi.IsDir() {
		if err := rejectTechnicalValidationOversize(cfg, fi.Size()); err != nil {
			fatal("%v", err)
		}
	} else if rel, size, found := firstOversizeUpload(src); found {
		if err := rejectTechnicalValidationOversizeNamed(cfg, size, rel); err != nil {
			fatal("%v", err)
		}
	}

	// Before anything is encoded or sent: an account that cannot upload
	// (technical validation used, no usable balance) fails here with the
	// top-up guidance. No host is dialled for this check.
	if err := refuseUploadIfBlocked(cfg); err != nil {
		fatal("%v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()

	wallStart := time.Now()

	rt, idx, cleanup, err := connectForWrite(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	if fi.IsDir() {
		err := uploadDir(ctx, rt, idx, src, asFlag)
		if asJSON {
			emitUploadJSON(err)
			if err == nil {
				return
			}
		}
		if err != nil {
			fatal("%v", explainTransferError(cfg, err, true))
		}
		if rem := secretsFileHint(); rem != "" {
			fmt.Fprintln(uploadOut, rem)
		}
		printStandingIfNotGood()
		return
	}

	remotePath := path.Base(src)
	if asFlag != "" {
		remotePath = asFlag
	}

	err = uploadOneWithWall(ctx, rt, idx, src, remotePath, wallStart)
	if asJSON {
		if err != nil {
			uploadResults.Failed = append(uploadResults.Failed, uploadJSONFailure{Path: remotePath, Error: explainTransferError(cfg, err, true).Error()})
		}
		emitUploadJSON(err)
		if err == nil {
			return
		}
	}
	if err != nil {
		fatal("%v", explainTransferError(cfg, err, true))
	}
	if rem := secretsFileHint(); rem != "" {
		fmt.Fprintln(uploadOut, rem)
	}
	printStandingIfNotGood()
}

// uploadOut receives upload's human progress lines: stdout normally, stderr
// with --json so stdout carries only the JSON document.
var uploadOut io.Writer = os.Stdout

// uploadJSONFile is one uploaded file in 'upload --json'.
type uploadJSONFile struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	ObjectID string `json:"object_id"`
}

// uploadJSONFailure is one file that failed in 'upload --json'.
type uploadJSONFailure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// uploadResults collects what 'upload --json' reports.
var uploadResults struct {
	Uploaded []uploadJSONFile
	Failed   []uploadJSONFailure
}

// emitUploadJSON prints the 'upload --json' document: every uploaded file,
// every failed one, and the totals. The exit status still follows err.
func emitUploadJSON(err error) {
	files := uploadResults.Uploaded
	if files == nil {
		files = []uploadJSONFile{}
	}
	failed := uploadResults.Failed
	if failed == nil {
		failed = []uploadJSONFailure{}
	}
	var bytes int64
	for _, f := range files {
		bytes += f.Size
	}
	emitJSON(map[string]interface{}{
		"ok":       err == nil,
		"uploaded": len(files),
		"failed":   len(failed),
		"bytes":    bytes,
		"files":    files,
		"errors":   failed,
	})
}

// rejectUnknownFlags fails on a flag the command does not take, so a typo is
// reported instead of silently ignored. --help/-h are always allowed.
func rejectUnknownFlags(cmd string, args []string, allowed ...string) error {
	ok := map[string]bool{"--help": true, "-h": true}
	for _, a := range allowed {
		ok[a] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return nil
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			continue
		}
		name := a
		if j := strings.IndexByte(a, '='); j > 0 {
			name = a[:j]
		}
		if !ok[name] {
			return fmt.Errorf("unknown option %s for '%s' (see 'tessera %s --help'); nothing was uploaded", name, cmd, cmd)
		}
		if !strings.Contains(a, "=") && takesValue(name) {
			i++
		}
	}
	return nil
}

// checkRemotePath validates a remote path given on the command line: relative,
// inside the account, and not in Tessera's own trash or version namespaces.
func checkRemotePath(p string) (string, error) {
	rel, err := NormalizeRelPath(p)
	if err != nil {
		return "", fmt.Errorf("%q is not a valid remote path (%v); use a relative path such as docs/report.pdf", p, err)
	}
	if isInternalPath(rel) {
		return "", fmt.Errorf("%q is inside %s or %s, which are reserved for Tessera's trash and versions; choose another remote path", p, TrashPrefix, VersionsPrefix)
	}
	return rel, nil
}

// plainFileError shortens an *os.PathError to its reason ("permission
// denied", "no such file or directory"); the caller already names the path.
func plainFileError(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// firstOversizeUpload returns the lexicographically first regular file under
// root that uploadDir would send and that is larger than one 40 MiB slab.
// Ignored paths are skipped the same way as the upload. A walk problem leaves
// found false so the later upload reports it instead of blocking.
func firstOversizeUpload(root string) (rel string, size int64, found bool) {
	ig, err := newIgnore(root, nil)
	if err != nil {
		return "", 0, false
	}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		relPath := p
		if r, rerr := filepath.Rel(root, p); rerr == nil {
			relPath = filepath.ToSlash(r)
		}
		if info.IsDir() {
			if relPath != "." && ig.Match(relPath, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if ig.Match(relPath, false) || strings.HasPrefix(filepath.Base(p), ".tessera-tmp-") {
			return nil
		}
		if info.Size() > slabBytes && (!found || relPath < rel) {
			rel = relPath
			size = info.Size()
			found = true
		}
		return nil
	})
	return rel, size, found
}

// prepareUpload hashes one file and builds its upload request. Every call
// produces a real upload: identical content at the same path is not skipped.
// Clients may upload multiple copies of the same file.
func prepareUpload(ctx context.Context, rt *Remote, idx *Index, localPath, remotePath string) (req uploadRequest, already bool, err error) {
	fi, err := os.Stat(localPath)
	if err != nil {
		return uploadRequest{}, false, err
	}
	hash, err := hashFile(localPath)
	if err != nil {
		return uploadRequest{}, false, fmt.Errorf("hash %s: %w", localPath, err)
	}

	return uploadRequest{
		LocalPath: localPath,
		RelPath:   remotePath,
		Name:      path.Base(remotePath),
		Hash:      hash,
		Size:      fi.Size(),
		Mode:      fi.Mode(),
		ModTime:   fi.ModTime(),
	}, false, nil
}

// recordUploaded stores one completed upload in the local index and sync
// bookkeeping, and prints the same line the single-file path always has.
func recordUploaded(rt *Remote, idx *Index, req uploadRequest, info PathInfo, started time.Time) error {
	// Update under the shared index lock so a concurrent command cannot
	// overwrite this entry. Also keep the in-memory idx consistent.
	if err := updateIndex(func(fresh *Index) error {
		fresh.put(info)
		return nil
	}); err != nil {
		return err
	}
	idx.put(info)
	// If this path belongs to a registered sync folder, record it there as
	// well. Otherwise the next `tessera sync` would treat the file as both new
	// locally and new remotely and download its own upload back.
	recordUploadInSyncState(info, req.ModTime)
	uploadResults.Uploaded = append(uploadResults.Uploaded, uploadJSONFile{Path: info.Path, Size: req.Size, SHA256: req.Hash, ObjectID: info.ObjectID})
	fmt.Fprintf(uploadOut, "  ↑ %s (%s, %.1fs)\n", req.RelPath, formatBytes(uint64(req.Size)), time.Since(started).Seconds())
	return nil
}

// uploadOne uploads a single file and updates the local index.
func uploadOne(ctx context.Context, rt *Remote, idx *Index, localPath, remotePath string) error {
	return uploadOneWithWall(ctx, rt, idx, localPath, remotePath, time.Time{})
}

// uploadOneWithWall is uploadOne that also reports total wall time when the
// caller passes a start time. The extra time is setup, not a funding wait.
func uploadOneWithWall(ctx context.Context, rt *Remote, idx *Index, localPath, remotePath string, wallStart time.Time) error {
	req, already, err := prepareUpload(ctx, rt, idx, localPath, remotePath)
	if err != nil {
		return err
	}
	if already {
		return nil
	}

	start := time.Now()
	info, err := rt.Upload(ctx, req, nil)
	if err != nil {
		return err
	}
	if err := recordUploaded(rt, idx, req, info, start); err != nil {
		return err
	}
	if !wallStart.IsZero() {
		wall := time.Since(wallStart).Seconds()
		xfer := time.Since(start).Seconds()
		if wall > xfer+0.5 {
			fmt.Fprintf(uploadOut, "  wall %.1fs total\n", wall)
		}
	}
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
	// Second positional is the destination path (TT run 5 #11). --out wins.
	if outName == "" && len(pos) >= 2 {
		outName = pos[1]
	}
	// --offset/--length: a partial read. Flag syntax is checked before any
	// network work; the bounds are checked once the file size is known.
	rng, err := parseDownloadRange(args)
	if err != nil {
		fatal("%v", err)
	}
	// An explicit destination is checked before anything is fetched.
	if outName != "" {
		if err := checkDownloadDest(outName); err != nil {
			fatal("%v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	rt, idx, cleanup, err := connectForWrite(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	// Look the file up first: a missing name must fail at once with "not
	// found", not after the host-funding wait.
	entry, err := idx.lookup(name)
	if err != nil {
		fatal("%v", err)
	}

	rng, clamped, err := rng.resolve(entry.Size)
	if err != nil {
		fatal("%s: %v", entry.Path, err)
	}

	dest := outName
	if dest == "" {
		dest = filepath.Base(entry.Path)
		if rng.Set {
			dest = defaultRangeName(entry.Path, rng)
		}
	}

	if outName == "" {
		if err := checkDownloadDest(dest); err != nil {
			fatal("%v", err)
		}
	}
	if n := removeStaleTemps(filepath.Dir(dest)); n > 0 {
		where := filepath.Dir(dest)
		if abs, err := filepath.Abs(where); err == nil {
			where = abs
		}
		fmt.Fprintf(os.Stderr, "Removed %d partial file(s) (%s*) left in %s by an earlier interrupted download.\n", n, tempFilePrefix, where)
	}

	if rng.Set {
		fmt.Printf("Downloading bytes %d-%d of %s (%s of %s)...\n", rng.Offset, rng.end()-1, entry.Path, formatBytes(rng.Length), formatBytes(uint64(entry.Size)))
		if clamped {
			fmt.Printf("Range clamped to the end of the file (%d bytes available from offset %d).\n", rng.Length, rng.Offset)
		}
	} else {
		fmt.Printf("Downloading %s (%s)...\n", entry.Path, formatBytes(uint64(entry.Size)))
	}

	start := time.Now()
	n, got, err := rt.downloadRangeHash(ctx, pathInfoFromIndex(entry), dest, 0644, rng)
	if err != nil {
		var ie *ErrIntegrity
		if errors.As(err, &ie) {
			fatal("%v", ie)
		}
		fatal("download: %v", explainTransferError(cfg, err, false))
	}
	elapsed := time.Since(start)

	if rng.Set {
		got, err := hashFile(dest)
		if err != nil {
			fatal("hash %s: %v", dest, err)
		}
		speed := float64(n) / elapsed.Seconds() / (1 << 20)
		fmt.Printf("Downloaded %s (bytes %d-%d) in %.1fs (%.2f MiB/s)\n", formatBytes(uint64(n)), rng.Offset, rng.end()-1, elapsed.Seconds(), speed)
		fmt.Printf("SHA-256 of range: %s (partial read; the whole-file hash is checked only on a full download)\n", got)
		fmt.Printf("Saved as: %s\n", dest)
		if rem := secretsFileHint(); rem != "" {
			fmt.Println(rem)
		}
		printStandingIfNotGood()
		return
	}

	// The whole-file hash was checked before dest was replaced (see
	// downloadRangeHash), so a mismatch never reaches this point.
	speed := float64(n) / elapsed.Seconds() / (1 << 20)
	fmt.Printf("Downloaded %s in %.1fs (%.2f MiB/s)\n", formatBytes(uint64(n)), elapsed.Seconds(), speed)
	if entry.SHA256 != "" {
		fmt.Printf("SHA-256 verified: %s\n", entry.SHA256)
	} else {
		fmt.Printf("SHA-256: %s (no reference hash stored)\n", got)
	}
	fmt.Printf("Saved as: %s\n", dest)
	if rem := secretsFileHint(); rem != "" {
		fmt.Println(rem)
	}
	printStandingIfNotGood()
}

func cmdDelete(args []string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	pos := positional(args)
	name := pos[0]
	purge := hasFlag(args, "--purge")
	assumeYes := hasFlag(args, "--yes") || hasFlag(args, "-y")
	toTrash := !purge && trashRetention() > 0

	// Without a terminal nobody can answer a prompt, so a delete needs an
	// explicit --yes (the same rule as 'account delete'). Checked before any
	// network work, so nothing changes.
	if !assumeYes && !stdinIsTerminal() {
		if toTrash {
			fatal("refusing to move %s to the trash without confirmation; pass --yes (non-interactive). Nothing was changed.", name)
		}
		fatal("refusing to permanently delete %s without confirmation; pass --yes (non-interactive). Nothing was changed.", name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rt, idx, cleanup, err := connectForWrite(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	// A name ending in "/" or naming a remote folder (a prefix with files
	// under it and no file of that exact name) deletes every file under it.
	var targets []*IndexEntry
	folder := ""
	if strings.HasSuffix(name, "/") {
		folder = strings.Trim(filepath.ToSlash(name), "/")
	} else if _, lerr := idx.lookup(name); lerr != nil && errors.Is(lerr, errNotFound) && len(filesUnder(idx, name)) > 0 {
		folder = strings.Trim(filepath.ToSlash(name), "/")
	}
	if folder != "" {
		targets = filesUnder(idx, folder)
		if len(targets) == 0 {
			fatal("%v: no files under %s/ (see 'tessera list')", errNotFound, folder)
		}
	} else {
		entry, err := idx.lookup(name)
		if err != nil {
			fatal("%v", err)
		}
		targets = []*IndexEntry{entry}
	}

	// Earlier copies kept at the same paths (an upload to an existing path
	// keeps the previous copy; see 'tessera versions').
	earlier := map[string][]PathInfo{}
	if all, err := rt.LiveObjects(ctx); err == nil {
		for _, t := range targets {
			earlier[t.Path] = earlierCopies(all, t)
		}
	}
	nEarlier := 0
	for _, list := range earlier {
		nEarlier += len(list)
	}
	var total int64
	for _, t := range targets {
		total += t.Size
	}
	what := targets[0].Path
	if folder != "" {
		what = fmt.Sprintf("%d file(s) under %s/", len(targets), folder)
	}

	if !assumeYes {
		var q string
		switch {
		case toTrash:
			q = fmt.Sprintf("Move %s (%s) to the trash? It can be restored with 'tessera trash restore' for %s. [y/N] ", what, formatBytes(uint64(total)), describeTrashWindow())
		case nEarlier > 0:
			q = fmt.Sprintf("Permanently delete %s (%s) and %d earlier version(s)? This cannot be undone. [y/N] ", what, formatBytes(uint64(total)), nEarlier)
		default:
			q = fmt.Sprintf("Permanently delete %s (%s)? This cannot be undone. [y/N] ", what, formatBytes(uint64(total)))
		}
		fmt.Print(q)
		var a string
		fmt.Scanln(&a)
		if strings.ToLower(strings.TrimSpace(a)) != "y" {
			fmt.Println("Aborted; nothing was deleted.")
			return
		}
	}

	failed := 0
	for _, entry := range targets {
		var err error
		if toTrash {
			err = trashOne(ctx, rt, idx, entry)
		} else {
			err = purgeOne(ctx, rt, idx, entry, earlier[entry.Path])
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ! %s: %v\n", entry.Path, err)
			failed++
			continue
		}
		if toTrash {
			fmt.Printf("Moved to trash: %s\n", entry.Path)
			if n := len(earlier[entry.Path]); n > 0 {
				fmt.Printf("  Note: %d earlier version(s) of %s are still stored; the newest of them is now the current file at this path ('tessera versions %s').\n", n, entry.Path, entry.Path)
			}
		} else {
			fmt.Printf("Deleted: %s\n", entry.Path)
			if n := len(earlier[entry.Path]); n > 0 {
				fmt.Printf("  Also deleted %d earlier version(s) of it.\n", n)
			}
		}
	}
	if toTrash && failed < len(targets) {
		if folder != "" {
			fmt.Printf("Restore with:   tessera trash restore <path>   (see 'tessera trash list')\n")
		} else {
			fmt.Printf("Restore with:   tessera trash restore %s\n", targets[0].Path)
		}
	}
	if failed > 0 {
		fatal("%d of %d file(s) could not be deleted (listed above)", failed, len(targets))
	}
	printStandingIfNotGood()
}

// describeTrashWindow is the trash retention in words ("30 days").
func describeTrashWindow() string {
	d := trashRetention()
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

// filesUnder lists the user files (not trash or version copies) under a
// remote folder, sorted by path.
func filesUnder(idx *Index, folder string) []*IndexEntry {
	folder = strings.Trim(filepath.ToSlash(folder), "/")
	if folder == "" {
		return nil
	}
	var out []*IndexEntry
	for _, e := range idx.SortedPaths() {
		if isInternalPath(e.Path) {
			continue
		}
		if strings.HasPrefix(e.Path, folder+"/") {
			out = append(out, e)
		}
	}
	return out
}

// trashOne moves one file into the trash namespace and records it.
func trashOne(ctx context.Context, rt *Remote, idx *Index, entry *IndexEntry) error {
	trashPath := path.Join(TrashPrefix, shortHash(entry.SHA256), entry.Path)
	info := pathInfoFromIndex(entry)
	if _, err := rt.Reattach(ctx, info, trashPath, entry.Root); err != nil {
		return fmt.Errorf("move to trash: %w", err)
	}
	recordTrash(TrashedEntry{
		OriginalPath: entry.Path,
		TrashPath:    trashPath,
		ObjectID:     entry.ObjectID,
		SHA256:       entry.SHA256,
		Size:         entry.Size,
		DeletedAt:    time.Now(),
	})
	trashed := PathInfo{
		ObjectID: entry.ObjectID, Path: trashPath, Name: path.Base(trashPath),
		Root: entry.RootID, SHA256: entry.SHA256, Size: entry.Size, ModTime: entry.ModTime,
	}
	_ = updateIndex(func(fresh *Index) error {
		fresh.remove(entry.Path)
		fresh.put(trashed)
		return nil
	})
	idx.remove(entry.Path)
	idx.put(trashed)
	recordDeleteInSyncState(entry.Path)
	return nil
}

// purgeOne permanently deletes one file and its earlier versions.
func purgeOne(ctx context.Context, rt *Remote, idx *Index, entry *IndexEntry, earlier []PathInfo) error {
	if err := rt.DeleteObject(ctx, entry.ObjectID); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	for _, v := range earlier {
		if err := rt.DeleteObject(ctx, v.ObjectID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not remove an earlier version of %s: %v\n", entry.Path, err)
		}
	}
	if err := deleteVersions(ctx, rt, entry); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove some versions: %v\n", err)
	}
	_ = updateIndex(func(fresh *Index) error {
		fresh.remove(entry.Path)
		return nil
	})
	idx.remove(entry.Path)
	recordDeleteInSyncState(entry.Path)
	return nil
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
		fatal("download: %v", explainTransferError(cfg, err, false))
	}
	defer rc.Close()

	out, err := os.Create(outName)
	if err != nil {
		fatal("create %s: %v", outName, err)
	}
	defer out.Close()

	hasher := sha256.New()
	tee := io.TeeReader(rc, hasher)
	transferProgress.Start("↓ "+filepath.Base(outName), 0, "downloading")
	n, err := io.Copy(out, &countingReader{r: tee, fn: transferProgress.Add})
	transferProgress.Finish()
	if err != nil {
		fatal("read: %v", err)
	}

	elapsed := time.Since(start)
	speed := float64(n) / elapsed.Seconds() / (1 << 20)
	fmt.Printf("Downloaded %s in %.1fs (%.2f MiB/s)\n", formatBytes(uint64(n)), elapsed.Seconds(), speed)
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
	matches := []*IndexEntry{}
	for _, e := range idx.SortedPaths() {
		if isInternalPath(e.Path) {
			continue
		}
		if strings.Contains(strings.ToLower(e.Path), lower) {
			matches = append(matches, e)
		}
	}

	if byContent && len(matches) == 0 {
		// Content search downloads candidates of plausible text type and size.
		fmt.Fprintf(os.Stderr, "Searching content across %d file(s) (this downloads data)...\n", len(idx.Paths))
		for _, e := range idx.SortedPaths() {
			if e.Size > 4<<20 || isInternalPath(e.Path) {
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
	fmt.Printf("%-52s  %10s  %-19s  %s\n", "PATH", "SIZE", "UPLOADED", "MODIFIED")
	fmt.Println(strings.Repeat("-", 108))
	for _, e := range matches {
		fmt.Printf("%-52s  %10s  %-19s  %s\n", truncate(e.Path, 52), formatBytes(uint64(e.Size)), uploadedTime(e), modifiedTime(e))
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

// cmdVersions lists or restores earlier versions of a path. Uploading to a
// path that already holds a file keeps the previous copy as an earlier
// version (it stays stored and counts toward storage until deleted). Copies
// kept by the older folder-sync version store (.tessera-versions) are listed
// too.
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
	asJSON := hasFlag(args, "--json")

	restoreAt, hasRestore := flagValue(args, "--restore")
	restoreN := 0
	if hasRestore {
		n, err := strconv.Atoi(strings.TrimSpace(restoreAt))
		if err != nil || n < 1 {
			fatal("--restore expects a version number from 'tessera versions %s' (1, 2, ...)", target)
		}
		restoreN = n
	}

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

	all, err := rt.LiveObjects(ctx)
	if err != nil {
		fatal("versions: %v", explainOffline(err))
	}
	versions := earlierCopies(all, entry)
	// Copies kept by the folder-sync version store, beside the file:
	// "<dir>/.tessera-versions/<name>/<stamp>-<sha>", or under a sync root.
	relPath := entry.Path
	if entry.RootID != "" {
		if rel, ok := relativeToPrefix(entry.Root, entry.Path); ok {
			relPath = rel
		}
	}
	suffix := VersionsPrefix + "/" + relPath
	besides := path.Join(path.Dir(entry.Path), VersionsPrefix, path.Base(entry.Path))
	for _, info := range all {
		p := info.Path
		base := p
		if i := strings.LastIndex(p, "/"+VersionsPrefix+"/"); i >= 0 {
			base = p[i+1:]
		}
		if strings.HasPrefix(p, besides+"/") || base == suffix || strings.HasPrefix(base, suffix+"/") {
			versions = append(versions, info)
		}
	}
	sort.SliceStable(versions, func(i, j int) bool { return versions[i].Updated.Before(versions[j].Updated) })

	if hasRestore {
		if len(versions) == 0 {
			fatal("%s has no earlier versions to restore (only the current copy is stored)", entry.Path)
		}
		if restoreN > len(versions) {
			fatal("--restore expects a number from 1 to %d for %s (see 'tessera versions %s')", len(versions), entry.Path, entry.Path)
		}
		chosen := versions[restoreN-1]
		sameDir := chosen.Path == entry.Path
		restored, err := rt.Reattach(ctx, chosen, entry.Path, chosen.Root)
		if err != nil {
			if isNotFound(err) {
				fatal("version %d of %s is no longer stored (it may have been deleted); run 'tessera versions %s' again", restoreN, entry.Path, entry.Path)
			}
			fatal("restore: %v", err)
		}
		if !sameDir {
			// A folder-sync version: the copy that was current is moved
			// into the version store, so the restore can be undone.
			restoreNamespace := path.Join(path.Dir(entry.Path), VersionsPrefix, path.Base(entry.Path))
			_, _ = rt.Reattach(ctx, pathInfoFromIndex(entry),
				path.Join(restoreNamespace, fmt.Sprintf("%s-restored-over-%s", time.Now().UTC().Format("20060102-150405"), shortHash(entry.SHA256))), entry.RootID)
		}
		_ = updateIndex(func(fresh *Index) error {
			fresh.put(restored)
			return nil
		})
		idx.put(restored)
		if asJSON {
			emitJSON(map[string]interface{}{
				"path":            entry.Path,
				"restored":        restoreN,
				"object_id":       restored.ObjectID,
				"sha256":          restored.SHA256,
				"size":            restored.Size,
				"previous_kept":   true,
				"previous_sha256": entry.SHA256,
			})
			return
		}
		fmt.Printf("Restored %s to version %d (%s, SHA-256 %s).\n", entry.Path, restoreN, formatBytes(uint64(restored.Size)), shortHash(restored.SHA256))
		fmt.Printf("The copy that was current (%s, SHA-256 %s) is kept as an earlier version; see 'tessera versions %s'.\n",
			formatBytes(uint64(entry.Size)), shortHash(entry.SHA256), entry.Path)
		return
	}

	if asJSON {
		if versions == nil {
			versions = []PathInfo{}
		}
		emitJSON(versions)
		return
	}

	if len(versions) == 0 {
		fmt.Printf("No earlier versions of %s: only the current copy is stored.\n", entry.Path)
		fmt.Println("Uploading to the same path again keeps the current copy as an earlier version.")
		return
	}
	fmt.Printf("Earlier versions of %s (oldest first; the current copy is not listed):\n\n", entry.Path)
	fmt.Printf("  %-8s  %-23s  %10s  %s\n", "N", "DATE", "SIZE", "SHA-256")
	for i, v := range versions {
		fmt.Printf("  %-8s  %-23s  %10s  %s\n", fmt.Sprintf("[%d]", i+1), localStamp(v.Updated, true),
			formatBytes(uint64(v.Size)), shortHash(v.SHA256))
	}
	fmt.Printf("  %-8s  %-23s  %10s  %s\n\n", "current", localStamp(entry.Updated, true), formatBytes(uint64(entry.Size)), shortHash(entry.SHA256))
	fmt.Println("Earlier versions stay stored and count toward storage until removed with")
	fmt.Printf("'tessera delete %s --purge' (deletes the file and all of its versions).\n", entry.Path)
	fmt.Printf("Restore one with: tessera versions %s --restore <N>\n", entry.Path)
}

// uploadedTime is when the object was stored on Tessera (index Updated).
func uploadedTime(e *IndexEntry) string {
	if e == nil || e.Updated.IsZero() {
		return "unknown"
	}
	return localStamp(e.Updated, false)
}

// modifiedTime is the local file mtime when known; "—" when absent so it is
// never confused with the upload time (TT run 5 #8).
func modifiedTime(e *IndexEntry) string {
	if e == nil || e.ModTime.IsZero() {
		return "—"
	}
	return localStamp(e.ModTime, false)
}

// displayTime prefers upload time (Updated), falling back to local mtime.
// Kept for callers that want a single column.
func displayTime(e *IndexEntry) string {
	if e == nil {
		return "unknown"
	}
	t := e.Updated
	if t.IsZero() {
		t = e.ModTime
	}
	if t.IsZero() {
		return "unknown"
	}
	return localStamp(t, false)
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
