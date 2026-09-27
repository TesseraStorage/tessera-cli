package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.sia.tech/indexd/slabs"
)

// Conflict policies.
const (
	// conflictNewest keeps the file with the later mtime and discards the
	// other. It is the default because it is the least surprising for a
	// personal folder: you edited it last, so that is the copy you get.
	conflictNewest = "newest"
	// conflictKeepBoth preserves both sides, renaming the loser with the
	// machine name and timestamp so nothing is ever lost.
	conflictKeepBoth = "keep-both"
)

// OpKind is a single unit of sync work.
type OpKind string

const (
	OpUpload         OpKind = "upload"
	OpDownload       OpKind = "download"
	OpDeleteLocal    OpKind = "delete-local"
	OpDeleteRemote   OpKind = "delete-remote"
	OpConflictLocal  OpKind = "conflict-local"  // remote copy saved under a new name
	OpConflictRemote OpKind = "conflict-remote" // local copy saved remotely under a new name
	// OpAdopt records a path whose local bytes already match the network copy.
	// It transfers nothing; it exists so planning stays free of side effects.
	OpAdopt OpKind = "adopt"
)

// Op is one planned action.
type Op struct {
	Kind OpKind `json:"kind"`
	Path string `json:"path"`
	// Extra carries the conflict copy's path when relevant.
	Extra string `json:"extra,omitempty"`
	// Reason is a human-readable explanation used by --dry-run.
	Reason string `json:"reason,omitempty"`
	// Size is the transfer size where applicable.
	Size int64 `json:"size,omitempty"`
}

func (o Op) String() string {
	switch o.Kind {
	case OpUpload:
		return fmt.Sprintf("↑ upload        %s", o.Path)
	case OpDownload:
		return fmt.Sprintf("↓ download      %s", o.Path)
	case OpDeleteLocal:
		return fmt.Sprintf("✗ delete local  %s", o.Path)
	case OpDeleteRemote:
		return fmt.Sprintf("✗ delete remote %s", o.Path)
	case OpConflictLocal:
		return fmt.Sprintf("⚑ conflict      %s (kept as %s)", o.Path, o.Extra)
	case OpConflictRemote:
		return fmt.Sprintf("⚑ conflict      %s (kept remote as %s)", o.Path, o.Extra)
	case OpAdopt:
		return fmt.Sprintf("= already synced %s", o.Path)
	}
	return string(o.Kind) + " " + o.Path
}

// localFile is one file found on disk.
type localFile struct {
	RelPath string
	AbsPath string
	Size    int64
	ModTime time.Time
	Mode    os.FileMode
	// Hash is filled lazily: unchanged files reuse the hash from state.
	Hash string
}

// Result summarises a sync run.
type Result struct {
	RootID     string    `json:"root_id"`
	LocalPath  string    `json:"local_path"`
	Remote     string    `json:"remote_prefix"`
	Scanned    int       `json:"scanned"`
	Adopted    int       `json:"adopted"`
	Uploaded   int       `json:"uploaded"`
	Downloaded int       `json:"downloaded"`
	Deleted    int       `json:"deleted"`
	Conflicts  int       `json:"conflicts"`
	Skipped    int       `json:"skipped"`
	BytesUp    int64     `json:"bytes_uploaded"`
	BytesDown  int64     `json:"bytes_downloaded"`
	Errors     []string  `json:"errors,omitempty"`
	DryRun     bool      `json:"dry_run,omitempty"`
	Started    time.Time `json:"started"`
	Elapsed    string    `json:"elapsed"`
}

// SyncOptions tunes a run.
type SyncOptions struct {
	DryRun       bool
	Verbose      bool
	Jobs         int
	MaxRate      int64 // bytes/sec, 0 = unlimited
	Adopt        string
	ConflictMode string
	OnEvent      func(string)
}

// SyncEngine reconciles one sync root.
type SyncEngine struct {
	cfg   *SyncConfig
	state *SyncState
	rt    *Remote
	ig    *Ignore
	opts  SyncOptions

	local  map[string]localFile
	remote map[string]*RemoteEntry

	machine string
}

// loadEngine prepares an engine for one registered root.
func loadEngine(cfg *SyncConfig, rt *Remote, opts SyncOptions) (*SyncEngine, error) {
	st, err := loadSyncState(cfg.ID)
	if err != nil {
		return nil, err
	}
	ig, err := newIgnore(cfg.LocalPath, cfg.Ignore)
	if err != nil {
		return nil, err
	}
	if opts.ConflictMode == "" {
		opts.ConflictMode = cfg.ConflictPolicy
	}
	if opts.ConflictMode == "" {
		opts.ConflictMode = conflictNewest
	}
	if opts.ConflictMode != conflictNewest && opts.ConflictMode != conflictKeepBoth {
		return nil, fmt.Errorf("unknown conflict policy %q (use %q or %q)", opts.ConflictMode, conflictNewest, conflictKeepBoth)
	}
	// A brand new root adopts whatever the local folder already contains when
	// the user records an explicit choice.
	if st.Adopt == "" && opts.Adopt != "" {
		st.Adopt = opts.Adopt
		_ = st.save()
	}
	return &SyncEngine{
		cfg:     cfg,
		state:   st,
		rt:      rt,
		ig:      ig,
		opts:    opts,
		machine: machineName(),
	}, nil
}

// log reports progress. When no sink is supplied the message goes to stdout, so
// CLI callers get --verbose and --dry-run output without having to wire a
// callback (and without the silent no-op that hid it before).
func (e *SyncEngine) log(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if e.opts.OnEvent != nil {
		e.opts.OnEvent(msg)
		return
	}
	fmt.Fprintln(os.Stdout, msg)
}

func (e *SyncEngine) verbose(format string, args ...interface{}) {
	if e.opts.Verbose || e.opts.DryRun {
		e.log(format, args...)
	}
}

// RefreshRemote brings the cached remote view up to date. On the first run
// every live object is enumerated; afterwards only events past the cursor are
// applied, which keeps repeated syncs cheap.
func (e *SyncEngine) RefreshRemote(ctx context.Context) error {
	bootstrap := e.state.Cursor.IsZero()

	if bootstrap {
		live, err := e.rt.LivePaths(ctx)
		if err != nil {
			return err
		}
		var mine int
		for p, info := range live {
			rel, ours := e.localPathForRemote(p)
			if !ours {
				continue
			}
			info.Path = rel
			e.state.applyRemoteEvent(rel, info)
			mine++
		}
		// Record the newest observed event across the whole account so later
		// refreshes are incremental.
		var newest time.Time
		var newestKey string
		for _, info := range live {
			if info.Updated.After(newest) {
				newest = info.Updated
				newestKey = info.ObjectID
			}
		}
		e.state.Cursor = Cursor{After: newest, Key: newestKey}
		e.log("remote: %d object(s) under %s/", mine, e.cfg.RemotePrefix)
		return e.state.save()
	}

	cursor := slabs.Cursor{}
	e.state.Cursor.applyTo(&cursor)

	applied := 0
	var lastKey string
	var lastAt time.Time
	for {
		evs, err := e.rt.sdk.ObjectEvents(ctx, cursor, 100)
		if err != nil {
			return fmt.Errorf("refresh remote: %w", err)
		}
		if len(evs) == 0 {
			break
		}
		for _, ev := range evs {
			applied++
			lastKey = ev.Key.String()
			lastAt = ev.UpdatedAt
			// Paths are only readable from live objects; a deletion carries no
			// metadata, so it is resolved through the object id we cached.
			// The cache is keyed by root-relative paths.
			evPath := e.pathForObjectID(lastKey)
			if ev.Deleted {
				if evPath != "" {
					e.verbose("  remote delete → %s", evPath)
					e.state.Remote[evPath] = &RemoteEntry{Deleted: true, Updated: ev.UpdatedAt}
				} else {
					e.verbose("  remote delete for unknown object %s (not tracked by this folder)", shortHash(lastKey))
				}
				continue
			}
			info, ok := pathFromEvent(ev)
			if !ok {
				continue
			}
			// Objects outside this root's remote prefix are not ours to sync.
			rel, ours := e.localPathForRemote(info.Path)
			if !ours {
				continue
			}
			info.Path = rel
			// An object that moved to a new path leaves the old one behind.
			if evPath != "" && evPath != rel {
				e.verbose("  remote move → %s (was %s)", rel, evPath)
				delete(e.state.Remote, evPath)
			}
			e.state.applyRemoteEvent(rel, info)
		}
		last := evs[len(evs)-1]
		next := slabs.Cursor{Key: last.Key, After: last.UpdatedAt}
		if sameCursor(cursor, next) {
			break
		}
		cursor = next
	}

	e.state.Cursor = Cursor{After: lastAt, Key: lastKey}
	if applied > 0 {
		e.log("remote: %d events applied", applied)
	}
	return e.state.save()
}

// applyTo converts a stored cursor into the SDK's cursor type.
func (c Cursor) applyTo(out *slabs.Cursor) {
	out.After = c.After
	if c.Key != "" {
		_ = out.Key.UnmarshalText([]byte(c.Key))
	}
}

// ScanLocal walks the root and produces the local view, reusing hashes from
// state whenever size and mtime are unchanged.
func (e *SyncEngine) ScanLocal() error {
	e.local = make(map[string]localFile)
	root := e.cfg.LocalPath

	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			// A file that vanished mid-walk is not an error worth failing on.
			return nil
		}
		if p == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		if info.IsDir() {
			if e.ig.Match(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			// Symlinks, sockets and devices are not synced.
			if info.Mode()&os.ModeSymlink != 0 {
				e.verbose("skip symlink %s", rel)
			}
			return nil
		}
		if e.ig.Match(rel, false) {
			e.verbose("ignored %s", rel)
			return nil
		}
		if strings.HasPrefix(filepath.Base(rel), ".tessera-tmp-") {
			return nil
		}

		lf := localFile{
			RelPath: rel,
			AbsPath: p,
			Size:    info.Size(),
			ModTime: info.ModTime(),
			Mode:    info.Mode(),
		}
		if st, ok := e.state.Entries[rel]; ok && st.Size == lf.Size && sameMTime(st.ModTime, lf.ModTime) && st.BaseSHA256 != "" {
			lf.Hash = st.BaseSHA256
		} else {
			h, herr := hashFile(p)
			if herr != nil {
				return fmt.Errorf("hash %s: %w", rel, herr)
			}
			lf.Hash = h
		}
		e.local[rel] = lf
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

// markMissingLocal flags tracked paths whose file is no longer on disk.
//
// This is what makes an ordinary deletion visible to the planner: the remote
// object is unchanged and the local file is simply gone, so neither
// "has the content changed" test fires. Without this flag such a path looks
// unchanged and the deletion never propagates, which resurrects the file on
// every other machine.
func (e *SyncEngine) markMissingLocal() int {
	marked := 0
	for p, st := range e.state.Entries {
		if st.BaseSHA256 == "" || st.DeletedLocal {
			continue
		}
		if _, present := e.local[p]; present {
			continue
		}
		if _, shared := e.state.Remote[p]; !shared {
			// Nothing remote either; the deletion already completed.
			continue
		}
		st.DeletedLocal = true
		marked++
	}
	return marked
}

// sameMTime compares timestamps at second precision, matching both the
// indexer's event granularity and FAT/NTFS friendliness.
func sameMTime(a, b time.Time) bool {
	return a.Truncate(time.Second).Equal(b.Truncate(time.Second))
}

// changedLocal reports whether the on-disk file differs from what we last
// synced for this path.
func (e *SyncEngine) changedLocal(p string, lf localFile) bool {
	st, ok := e.state.Entries[p]
	if !ok {
		return true
	}
	if st.Untracked {
		// Declared a duplicate during a first-run adopt: it is not a change.
		return false
	}
	if st.BaseSHA256 == "" {
		return true
	}
	return st.BaseSHA256 != lf.Hash
}

// changedRemote reports whether the remote entry is newer than our base.
func (e *SyncEngine) changedRemote(p string) bool {
	re := e.state.Remote[p]
	if re == nil {
		return false
	}
	st, ok := e.state.Entries[p]
	if !ok {
		return !re.Deleted
	}
	if st.BaseSHA256 == "" {
		return true
	}
	// A tombstone we have not yet acted on is always a change. This must not
	// depend on current liveness, because a competing upload can make the path
	// live again before we react — which would silently resurrect a deleted
	// file. Note that st.DeletedLocal describes a deletion *we* made and is
	// handled by the delete-remote branch, so it is not excluded here.
	if re.Deleted {
		return !st.DeletedRemote
	}
	if st.ObjectID != "" && st.ObjectID == re.ObjectID {
		return false
	}
	// The object id changed: either the content changed, or the same bytes
	// were re-attached under a new object (deduplication, or an upload from
	// another machine). Both need bookkeeping, so report a change; the plan
	// turns a content match into an adopt rather than a transfer.
	return true
}

// describePath renders the inputs of one planning decision, for --verbose.
func (e *SyncEngine) describePath(p string, lf localFile, hasLocal bool, st *EntryState, re *RemoteEntry) string {
	localHash := "-"
	if hasLocal {
		localHash = shortHash(lf.Hash)
	}
	base := "-"
	objectID := "-"
	if st != nil {
		base = shortHash(st.BaseSHA256)
		objectID = shortHash(st.ObjectID)
	}
	remote := "-"
	state := "absent"
	if re != nil {
		remote = shortHash(re.SHA256)
		state = "live"
		if re.Deleted {
			state = "tombstone"
		}
	}
	return fmt.Sprintf("%s  local=%s base=%s remote=%s(%s) entry=%s",
		p, localHash, base, remote, state, objectID)
}

// describeOp renders one operation with the reason it was chosen.
func describeOp(op Op) string {
	line := op.String()
	if op.Reason != "" {
		line += "  (" + op.Reason + ")"
	}
	return line
}

// DescribePlan renders a plan with reasons, for --dry-run and --verbose.
func (e *SyncEngine) DescribePlan(ops []Op) []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, describeOp(op))
	}
	return out
}

// Plan computes the operations needed to converge both sides.
func (e *SyncEngine) Plan() []Op {
	ops := e.plan()
	if e.opts.Verbose || e.opts.DryRun {
		for _, op := range ops {
			e.verbose("  %s", describeOp(op))
		}
	}
	return ops
}

// plan is the side-effect-free planner; every branch records a reason so a
// decision can always be explained, including from --dry-run output.
func (e *SyncEngine) plan() []Op {
	var ops []Op

	paths := make(map[string]struct{}, len(e.local)+len(e.state.Entries)+len(e.state.Remote))
	for p := range e.local {
		paths[p] = struct{}{}
	}
	for p, st := range e.state.Entries {
		if st.DeletedLocal || st.BaseSHA256 != "" {
			paths[p] = struct{}{}
		}
	}
	for p, re := range e.state.Remote {
		if !re.Deleted {
			paths[p] = struct{}{}
		}
	}

	ordered := make([]string, 0, len(paths))
	for p := range paths {
		ordered = append(ordered, p)
	}
	sort.Strings(ordered)

	for _, p := range ordered {
		// Retained versions are internal bookkeeping, never synced as files.
		if isVersionPath(e.cfg.RemotePrefix, p) || strings.Contains(p, VersionsPrefix+"/") {
			continue
		}
		// A conflict copy created by another machine is recorded here too, so
		// every peer reports the same conflict — including the one that only
		// downloaded the copy and had nothing else to do.
		e.recordConflictIfCopy(p)
		lf, hasLocal := e.local[p]
		// Ignore rules protect the local folder from remote junk as well, so a
		// peer's .DS_Store is never materialised here.
		if !hasLocal && e.ig.Match(p, false) {
			continue
		}
		st := e.state.Entries[p]
		re := e.state.Remote[p]
		remoteLive := re != nil && !re.Deleted

		localExists := hasLocal
		localChanged := false
		if hasLocal {
			localChanged = e.changedLocal(p, lf)
		}
		remoteChanged := e.changedRemote(p)
		if e.opts.Verbose || e.opts.DryRun {
			e.verbose("  ? %s localChanged=%v remoteChanged=%v", e.describePath(p, lf, hasLocal, st, re), localChanged, remoteChanged)
		}

		// Content shortcut: when the bytes on disk already match what is
		// stored, record the new object id without transferring anything. This
		// matters because the object id changes whenever content is
		// deduplicated to an existing object or re-uploaded, so an id
		// comparison alone would re-download identical data. It applies even
		// under --adopt remote, since the file is byte-identical.
		if hasLocal && remoteLive && re.SHA256 != "" && re.SHA256 == lf.Hash && remoteChanged {
			ops = append(ops, Op{Kind: OpAdopt, Path: p, Size: lf.Size, Reason: "content already matches the stored copy"})
			continue
		}

		switch {
		case !localExists && !remoteLive:
			// Nothing anywhere. Drop bookkeeping.
			if st != nil && st.BaseSHA256 != "" {
				// A deletion that already landed remotely.
				e.state.removeEntry(p)
			}
			continue

		case localExists && !remoteLive:
			// A remote tombstone takes precedence over every other
			// interpretation, including "the local file is newer". A delete is
			// a deliberate act and resurrecting the file is the surprising
			// outcome; only a strictly newer local edit is allowed to win.
			if re != nil && re.Deleted {
				if localChanged && e.strictlyNewerLocal(lf, re) {
					ops = append(ops, Op{Kind: OpUpload, Path: p, Size: lf.Size, Reason: "local edit is newer than the remote delete"})
				} else {
					ops = append(ops, Op{Kind: OpDeleteLocal, Path: p, Reason: "deleted on another machine"})
				}
				continue
			}
			if e.state.Adopt == adoptRemote {
				// The folder mirrored a different history first; drop ours.
				ops = append(ops, Op{Kind: OpDeleteLocal, Path: p, Reason: "not present remotely (adopting remote)"})
				continue
			}
			if st == nil || st.BaseSHA256 == "" {
				// Never tracked here and nothing remote to compare with: it is
				// a genuinely new local file.
				ops = append(ops, Op{Kind: OpUpload, Path: p, Size: lf.Size, Reason: "new local file"})
				continue
			}
			// The remote object disappeared without a tombstone (unpinned
			// elsewhere): the local copy is the only one left.
			ops = append(ops, Op{Kind: OpUpload, Path: p, Size: lf.Size, Reason: "missing remotely"})
			continue

		case !localExists && remoteLive:
			if st != nil && st.BaseSHA256 != "" {
				switch {
				case st.DeletedLocal:
					// The file is gone locally and the deletion has not been
					// pushed yet.
					ops = append(ops, Op{Kind: OpDeleteRemote, Path: p, Reason: "deleted locally"})
				case st.BaseSHA256 != re.SHA256 && re.SHA256 != "":
					// The local copy is gone and the remote moved on: take the
					// remote version.
					ops = append(ops, Op{Kind: OpDownload, Path: p, Size: remoteSize(re), Reason: "remote update after local removal"})
				default:
					// The remote object still matches what we last agreed on
					// and is simply not materialised on this machine (for
					// example a conflict copy this machine never downloaded).
					// Deleting it here would destroy a file that exists
					// elsewhere, so restore it locally instead.
					ops = append(ops, Op{Kind: OpDownload, Path: p, Size: remoteSize(re), Reason: "present remotely but missing locally"})
				}
				continue
			}
			if st == nil && (remoteChanged || e.state.Adopt == adoptRemote) {
				// First time this root sees the path and the local folder
				// already has differing content. "merge" (the default) lets
				// the newer copy win per file instead of silently discarding
				// one side; --adopt local/remote overrides it.
				switch e.state.Adopt {
				case adoptLocal:
					ops = append(ops, Op{Kind: OpUpload, Path: p, Size: lf.Size, Reason: "adopting local content"})
				case adoptRemote:
					ops = append(ops, Op{Kind: OpDownload, Path: p, Size: remoteSize(re), Reason: "adopting remote content"})
				default:
					if lf.Hash == "" {
						ops = append(ops, Op{Kind: OpDownload, Path: p, Size: remoteSize(re), Reason: "content differs; remote taken"})
					} else if e.newerLocal(lf, re) && !lf.ModTime.IsZero() {
						ops = append(ops, Op{Kind: OpUpload, Path: p, Size: lf.Size, Reason: "first sync: local copy is newer"})
					} else {
						ops = append(ops, Op{Kind: OpDownload, Path: p, Size: remoteSize(re), Reason: "first sync: remote copy is newer"})
					}
				}
				continue
			}
			// Never seen here, and nothing changed remotely: record it.
			ops = append(ops, Op{Kind: OpDownload, Path: p, Size: remoteSize(re), Reason: "new remote file"})
			continue

		case localExists && remoteLive:
			// A first-run --adopt remote choice means the network's history
			// wins outright for paths this machine never tracked.
			if e.state.Adopt == adoptRemote && st == nil {
				ops = append(ops, Op{Kind: OpDownload, Path: p, Size: remoteSize(re), Reason: "adopting remote content"})
				continue
			}
			if !localChanged && !remoteChanged {
				continue
			}
			if localChanged && !remoteChanged {
				ops = append(ops, Op{Kind: OpUpload, Path: p, Size: lf.Size, Reason: "local change"})
				continue
			}
			if !localChanged && remoteChanged {
				ops = append(ops, Op{Kind: OpDownload, Path: p, Size: remoteSize(re), Reason: "remote change"})
				continue
			}
			// Both sides changed, but the bytes agree: the same edit arrived
			// twice (often via deduplication or a re-upload). Record the new
			// object id without transferring anything.
			if lf.Hash == re.SHA256 {
				ops = append(ops, Op{Kind: OpAdopt, Path: p, Size: lf.Size, Reason: "both sides hold identical content"})
				continue
			}
			newerLocal := e.newerLocal(lf, re)
			if e.opts.ConflictMode == conflictKeepBoth {
				conflictPath := e.uniqueConflictPath(p, conflictName(p, re.Machine, re.ModTime))
				e.state.noteConflict(p, conflictPath)
				if newerLocal {
					ops = append(ops, Op{
						Kind: OpConflictRemote, Path: p, Extra: conflictPath,
						Reason: "both sides changed; keeping both",
					})
					ops = append(ops, Op{Kind: OpUpload, Path: p, Size: lf.Size, Reason: "local edit is newer"})
				} else {
					ops = append(ops, Op{
						Kind: OpConflictLocal, Path: p, Extra: conflictPath,
						Reason: "both sides changed; keeping both",
					})
				}
			} else if newerLocal {
				ops = append(ops, Op{Kind: OpUpload, Path: p, Size: lf.Size, Reason: "both changed; local is newer"})
			} else {
				ops = append(ops, Op{Kind: OpDownload, Path: p, Size: remoteSize(re), Reason: "both changed; remote is newer"})
			}
			continue
		}
	}
	return ops
}

func remoteSize(re *RemoteEntry) int64 {
	if re == nil {
		return 0
	}
	return re.Size
}

// newerLocal reports whether the local edit is at least as new as the remote
// edit. Timestamps are compared at second precision, matching indexer
// granularity.
func (e *SyncEngine) newerLocal(lf localFile, re *RemoteEntry) bool {
	if re == nil || re.ModTime.IsZero() {
		return true
	}
	return !lf.ModTime.Truncate(time.Second).Before(re.ModTime.Truncate(time.Second))
}

// strictlyNewerLocal is the conservative variant used when the alternative is
// deleting the local file: only a clearly newer edit wins, so an unknown or
// equal timestamp never resurrects a deleted file.
func (e *SyncEngine) strictlyNewerLocal(lf localFile, re *RemoteEntry) bool {
	if re == nil || re.ModTime.IsZero() {
		// Unknown remote time: fall back to the upload time, so an edit made
		// after the deletion still wins.
		if re != nil && !re.Updated.IsZero() {
			return lf.ModTime.Truncate(time.Second).After(re.Updated.Truncate(time.Second))
		}
		return true
	}
	return lf.ModTime.Truncate(time.Second).After(re.ModTime.Truncate(time.Second))
}

// Apply executes the planned operations.
func (e *SyncEngine) Apply(ctx context.Context, ops []Op) (*Result, error) {
	res := &Result{
		RootID:    e.cfg.ID,
		LocalPath: e.cfg.LocalPath,
		Remote:    e.cfg.RemotePrefix,
		Scanned:   len(e.local),
		Started:   time.Now(),
		DryRun:    e.opts.DryRun,
	}

	for _, op := range ops {
		if err := ctx.Err(); err != nil {
			res.Errors = append(res.Errors, err.Error())
			break
		}
		if e.opts.DryRun {
			e.log("  %s", op)
			switch op.Kind {
			case OpUpload, OpConflictRemote:
				res.Uploaded++
			case OpDownload, OpConflictLocal:
				res.Downloaded++
			case OpDeleteLocal, OpDeleteRemote:
				res.Deleted++
			case OpAdopt:
				res.Adopted++
			}
			continue
		}
		if err := e.applyOp(ctx, op, res); err != nil {
			msg := fmt.Sprintf("%s: %v", op.Path, err)
			res.Errors = append(res.Errors, msg)
			e.log("  error: %s", msg)
		}
	}
	res.Elapsed = time.Since(res.Started).Round(time.Millisecond).String()
	if !e.opts.DryRun {
		e.cfg.LastSync = time.Now()
		if err := saveSyncConfig(e.cfg); err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
		if err := e.state.save(); err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
	}
	return res, nil
}

func (e *SyncEngine) applyOp(ctx context.Context, op Op, res *Result) error {
	switch op.Kind {
	case OpUpload:
		return e.doUpload(ctx, op.Path, res)
	case OpDownload:
		return e.doDownload(ctx, op.Path, res)
	case OpDeleteLocal:
		return e.doDeleteLocal(op.Path, res)
	case OpDeleteRemote:
		return e.doDeleteRemote(ctx, op.Path, res)
	case OpConflictLocal:
		return e.doConflictLocal(ctx, op.Path, op.Extra, res)
	case OpConflictRemote:
		return e.doConflictRemote(ctx, op.Path, op.Extra, res)
	case OpAdopt:
		return e.doAdopt(op.Path, res)
	}
	return fmt.Errorf("unknown operation %q", op.Kind)
}

// doUpload pushes a local file and retires the object it replaced. The
// superseded object is retained (as a version) or moved to trash, never
// silently destroyed.
func (e *SyncEngine) doUpload(ctx context.Context, p string, res *Result) error {
	lf, ok := e.local[p]
	if !ok {
		return fmt.Errorf("local file disappeared")
	}
	st := e.state.Entries[p]
	oldID := ""
	if st != nil {
		oldID = st.ObjectID
	}

	info, err := e.rt.Upload(ctx, uploadRequest{
		LocalPath: lf.AbsPath,
		RelPath:   e.remotePath(p),
		Name:      path.Base(p),
		Hash:      lf.Hash,
		Size:      lf.Size,
		Mode:      lf.Mode,
		ModTime:   lf.ModTime,
		Root:      e.cfg.ID,
	}, nil)
	if err != nil {
		return err
	}
	// Retire the object this upload replaced: retained as a version, or moved
	// to trash when version retention is switched off.
	if oldID != "" && oldID != info.ObjectID && st != nil && st.BaseSHA256 != "" {
		if err := e.keepVersionRef(ctx, p, st); err != nil {
			e.verbose("  version bookkeeping skipped for %s: %v", p, err)
		}
	}

	ent := e.state.entry(p)
	ent.BaseSHA256 = lf.Hash
	ent.ObjectID = info.ObjectID
	ent.RemotePath = info.Path
	ent.Size = lf.Size
	ent.ModTime = lf.ModTime
	ent.DeletedLocal = false
	ent.DeletedRemote = false
	// ent.Conflict is deliberately preserved: the conflict copy still exists
	// until the user resolves it, and overwriting the file locally does not.
	e.state.applyRemoteEvent(p, info)
	res.Uploaded++
	res.BytesUp += lf.Size
	e.log("  ↑ %s (%s)", p, formatBytes(uint64(lf.Size)))
	return e.state.save()
}

// clearConflictFor drops every conflict record that referenced the given
// conflict copy, so deleting the copy also resolves the original path.
func (e *SyncEngine) clearConflictFor(conflictPath string) {
	e.state.resolveConflictCopy(conflictPath)
}

// doAdopt records that a path is already in sync, without transferring data.
// This happens when the object id changed (a deduplicated re-attach, or an
// upload from another machine) but the bytes on disk are identical.
func (e *SyncEngine) doAdopt(p string, res *Result) error {
	lf, ok := e.local[p]
	if !ok {
		return fmt.Errorf("local file disappeared")
	}
	re := e.state.Remote[p]
	ent := e.state.entry(p)
	ent.BaseSHA256 = lf.Hash
	ent.Size = lf.Size
	ent.ModTime = lf.ModTime
	if re != nil {
		ent.ObjectID = re.ObjectID
	}
	ent.RemotePath = e.remotePath(p)
	ent.DeletedLocal = false
	ent.DeletedRemote = false
	res.Adopted++
	e.verbose("  = %s already matches (%s)", p, shortHash(lf.Hash))
	return e.state.save()
}

// doDownload fetches a remote object and installs it atomically.
func (e *SyncEngine) doDownload(ctx context.Context, p string, res *Result) error {
	re := e.state.Remote[p]
	if re == nil || re.ObjectID == "" {
		return fmt.Errorf("no remote object for %s", p)
	}
	info := PathInfo{
		ObjectID: re.ObjectID,
		Path:     e.remotePath(p),
		Name:     path.Base(p),
		Root:     e.cfg.ID,
		SHA256:   re.SHA256,
		Size:     re.Size,
		ModTime:  re.ModTime,
		Machine:  re.Machine,
	}
	mode := os.FileMode(0644)
	dest := filepath.Join(e.cfg.LocalPath, filepath.FromSlash(p))
	n, err := e.rt.Download(ctx, info, dest, mode)
	if err != nil {
		return err
	}
	ent := e.state.entry(p)
	ent.BaseSHA256 = info.SHA256
	ent.ObjectID = re.ObjectID
	ent.RemotePath = e.remotePath(p)
	ent.Size = info.Size
	ent.ModTime = info.ModTime
	ent.DeletedLocal = false
	ent.DeletedRemote = false
	// See doUpload: an unresolved conflict copy stays recorded. Re-record here
	// as well so the note cannot be lost by a later no-op pass.
	e.recordConflictIfCopy(p)
	res.Downloaded++
	res.BytesDown += n
	e.log("  ↓ %s (%s)", p, formatBytes(uint64(n)))
	return e.state.save()
}

// doDeleteLocal removes a file that was deleted on another machine.
//
// The path keeps a tombstone instead of being forgotten entirely. That matters
// when the peer did not delete the file but moved it: the new path holds the
// same content, so without a tombstone the old path would look like a new
// local file and be uploaded again, resurrecting it at its old name.
func (e *SyncEngine) doDeleteLocal(p string, res *Result) error {
	e.clearConflictFor(p)
	abs := filepath.Join(e.cfg.LocalPath, filepath.FromSlash(p))
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		return err
	}
	e.pruneEmptyDirs(filepath.Dir(abs))

	st := e.state.entry(p)
	st.BaseSHA256 = ""
	st.ObjectID = ""
	st.RemotePath = ""
	st.DeletedLocal = true
	st.DeletedRemote = true
	if re := e.state.Remote[p]; re != nil {
		re.Deleted = true
		re.ObjectID = ""
		re.SHA256 = ""
	} else {
		e.state.Remote[p] = &RemoteEntry{Deleted: true, Updated: time.Now()}
	}
	res.Deleted++
	e.log("  ✗ local %s", p)
	return e.state.save()
}

// doDeleteRemote pushes a local deletion as an object deletion, which the
// indexer records as a tombstone so every machine sees it.
func (e *SyncEngine) doDeleteRemote(ctx context.Context, p string, res *Result) error {
	e.clearConflictFor(p)
	st := e.state.Entries[p]
	re := e.state.Remote[p]
	if re != nil && re.ObjectID != "" {
		if err := e.rt.DeleteObject(ctx, re.ObjectID); err != nil {
			return err
		}
	}
	if st != nil && st.ObjectID != "" && (re == nil || st.ObjectID != re.ObjectID) {
		if err := e.rt.DeleteObject(ctx, st.ObjectID); err != nil {
			return err
		}
	}
	st = e.state.entry(p)
	st.BaseSHA256 = ""
	st.ObjectID = ""
	st.RemotePath = ""
	st.DeletedLocal = true
	st.DeletedRemote = true
	st.TombstonedAt = time.Now()
	if re != nil {
		re.Deleted = true
		re.ObjectID = ""
		re.SHA256 = ""
	} else {
		e.state.Remote[p] = &RemoteEntry{Deleted: true, Updated: time.Now()}
	}
	res.Deleted++
	e.log("  ✗ remote %s", p)
	return e.state.save()
}

// doConflictLocal keeps the remote version under a conflict name while leaving
// the local edit untouched.
func (e *SyncEngine) doConflictLocal(ctx context.Context, p, conflictPath string, res *Result) error {
	re := e.state.Remote[p]
	if re == nil || re.ObjectID == "" {
		return fmt.Errorf("no remote object for %s", p)
	}
	dest := filepath.Join(e.cfg.LocalPath, filepath.FromSlash(conflictPath))
	n, err := e.rt.Download(ctx, PathInfo{
		ObjectID: re.ObjectID,
		Path:     e.remotePath(conflictPath),
		ModTime:  re.ModTime,
	}, dest, 0644)
	if err != nil {
		return err
	}
	// Re-upload the remote copy under its conflict path so every machine sees
	// both sides, then leave the local file to be uploaded by its own op.
	h, err := hashFile(dest)
	if err != nil {
		return err
	}
	fi, err := os.Stat(dest)
	if err != nil {
		return err
	}
	info, err := e.rt.Upload(ctx, uploadRequest{
		LocalPath: dest,
		RelPath:   e.remotePath(conflictPath),
		Name:      path.Base(conflictPath),
		Hash:      h,
		Size:      fi.Size(),
		Mode:      fi.Mode(),
		ModTime:   fi.ModTime(),
		Root:      e.cfg.ID,
	}, nil)
	if err != nil {
		return err
	}
	ce := e.state.entry(conflictPath)
	ce.BaseSHA256 = h
	ce.ObjectID = info.ObjectID
	ce.Size = fi.Size()
	ce.ModTime = fi.ModTime()
	e.state.applyRemoteEvent(conflictPath, info)
	e.state.noteConflict(p, conflictPath)
	if st, ok := e.state.Entries[p]; ok {
		st.DeletedLocal = false
		st.DeletedRemote = false
	}
	res.Conflicts++
	res.Downloaded++
	res.BytesDown += n
	e.log("  ⚑ %s kept as %s", p, conflictPath)
	return e.state.save()
}

// doConflictRemote keeps the remote version at its path and uploads the local
// version under a conflict name.
func (e *SyncEngine) doConflictRemote(ctx context.Context, p, conflictPath string, res *Result) error {
	lf, ok := e.local[p]
	if !ok {
		return fmt.Errorf("local file disappeared")
	}
	info, err := e.rt.Upload(ctx, uploadRequest{
		LocalPath: lf.AbsPath,
		RelPath:   e.remotePath(conflictPath),
		Name:      path.Base(conflictPath),
		Hash:      lf.Hash,
		Size:      lf.Size,
		Mode:      lf.Mode,
		ModTime:   lf.ModTime,
		Root:      e.cfg.ID,
	}, nil)
	if err != nil {
		return err
	}
	// The conflict copy must exist on this machine's disk as well as remotely.
	// Without the local copy the next scan sees a tracked path with no file,
	// treats it as a deletion, and unpins the copy it just created.
	localCopy := filepath.Join(e.cfg.LocalPath, filepath.FromSlash(conflictPath))
	src, err := os.Open(lf.AbsPath)
	if err != nil {
		return fmt.Errorf("materialise conflict copy %s: %w", conflictPath, err)
	}
	_, copyErr := copyFileAtomic(localCopy, src, lf.Mode.Perm())
	src.Close()
	if copyErr != nil {
		return fmt.Errorf("materialise conflict copy %s: %w", conflictPath, copyErr)
	}
	if !lf.ModTime.IsZero() {
		_ = os.Chtimes(localCopy, lf.ModTime, lf.ModTime)
	}

	ce := e.state.entry(conflictPath)
	ce.BaseSHA256 = lf.Hash
	ce.ObjectID = info.ObjectID
	ce.Size = lf.Size
	ce.ModTime = lf.ModTime
	e.state.applyRemoteEvent(conflictPath, info)
	e.state.noteConflict(p, conflictPath)
	if st, ok := e.state.Entries[p]; ok {
		st.DeletedLocal = false
		st.DeletedRemote = false
	}
	res.Conflicts++
	res.Uploaded++
	res.BytesUp += lf.Size
	e.log("  ⚑ %s kept as %s", p, conflictPath)
	return e.state.save()
}

// pruneEmptyDirs removes now-empty parent directories after a deletion, up to
// (but not including) the sync root.
func (e *SyncEngine) pruneEmptyDirs(dir string) {
	root := filepath.Clean(e.cfg.LocalPath)
	for {
		dir = filepath.Clean(dir)
		if dir == root || !strings.HasPrefix(dir, root+string(os.PathSeparator)) {
			return
		}
		ents, err := os.ReadDir(dir)
		if err != nil || len(ents) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// remotePath prefixes a root-relative path with the root's remote prefix.
func (e *SyncEngine) remotePath(rel string) string {
	return path.Join(e.cfg.RemotePrefix, rel)
}

// localPathForRemote maps a remote-absolute path back to a root-relative one.
// It reports false for objects that belong to a different sync root or to the
// internal version/trash namespaces.
func (e *SyncEngine) localPathForRemote(remote string) (string, bool) {
	prefix := strings.Trim(e.cfg.RemotePrefix, "/")
	remote = strings.Trim(remote, "/")
	if remote == prefix {
		return "", false
	}
	if !strings.HasPrefix(remote, prefix+"/") {
		return "", false
	}
	rel := strings.TrimPrefix(remote, prefix+"/")
	if rel == "" {
		return "", false
	}
	for _, reserved := range []string{VersionsPrefix, TrashPrefix} {
		if rel == reserved || strings.HasPrefix(rel, reserved+"/") {
			return "", false
		}
	}
	return rel, true
}

// pathForObjectID finds the cached root-relative path for an object id.
func (e *SyncEngine) pathForObjectID(id string) string {
	for p, re := range e.state.Remote {
		if re.ObjectID == id {
			return p
		}
	}
	return ""
}

// uniqueConflictPath avoids clobbering a conflict copy that already exists.
// Repeated conflicts on the same file are not rare, and each one should remain
// individually recoverable.
func (e *SyncEngine) uniqueConflictPath(original, candidate string) string {
	if _, taken := e.state.Remote[candidate]; !taken {
		if _, onDisk := e.local[candidate]; !onDisk {
			return candidate
		}
	}
	ext := path.Ext(candidate)
	base := strings.TrimSuffix(candidate, ext)
	return fmt.Sprintf("%s-%d%s", base, time.Now().Unix(), ext)
}

// conflictMarker is the phrase every conflict copy filename contains. It makes
// a conflict copy recognisable on machines that only received it, which is what
// lets every peer report the same conflict.
const conflictMarker = " (conflicted copy, "

// recordConflictIfCopy records p as a conflict copy of its original path when
// the original is known to the remote view. Recording is idempotent, so every
// peer converges on the same conflict ledger.
func (e *SyncEngine) recordConflictIfCopy(p string) {
	original, ok := isConflictCopy(p)
	if !ok {
		return
	}
	if _, exists := e.state.Remote[original]; exists {
		e.state.noteConflict(original, p)
	}
}

// isConflictCopy reports whether a path names a conflict copy, and returns the
// original path it belongs to.
func isConflictCopy(p string) (string, bool) {
	i := strings.Index(p, conflictMarker)
	if i <= 0 {
		return "", false
	}
	rest := p[i+len(conflictMarker):]
	j := strings.LastIndex(rest, ")")
	if j < 0 {
		return "", false
	}
	original := p[:i] + path.Ext(p)
	return original, true
}

// conflictName builds a conflict copy path that includes the machine that
// produced the losing copy, so the two versions are self-describing.
func conflictName(p, machine string, at time.Time) string {
	if machine == "" {
		machine = "other"
	}
	if at.IsZero() {
		at = time.Now()
	}
	stamp := at.UTC().Format("2006-01-02-150405")
	ext := path.Ext(p)
	base := strings.TrimSuffix(p, ext)
	return fmt.Sprintf("%s (conflicted copy, %s %s)%s", base, machine, stamp, ext)
}

// SyncOnce runs a full reconcile for one root.
func (e *SyncEngine) SyncOnce(ctx context.Context) (*Result, error) {
	if err := e.RefreshRemote(ctx); err != nil {
		return nil, err
	}
	if err := e.ScanLocal(); err != nil {
		return nil, err
	}
	if n := e.markMissingLocal(); n > 0 {
		e.verbose("  %d path(s) are no longer present locally", n)
	}
	if pruned := e.state.pruneTombstones(time.Now()); pruned > 0 {
		e.verbose("  pruned %d expired deletion record(s)", pruned)
	}

	ops := e.Plan()
	if len(ops) == 0 {
		if e.opts.DryRun {
			e.log("in sync — nothing to do (%d files)", len(e.local))
		} else {
			e.log("already in sync (%d files)", len(e.local))
		}
		res := &Result{
			RootID: e.cfg.ID, LocalPath: e.cfg.LocalPath, Remote: e.cfg.RemotePrefix,
			Scanned: len(e.local), Started: time.Now(),
		}
		// Planning can record things worth keeping (a conflict copy observed on
		// another machine, for example), so persist even when nothing moved.
		if !e.opts.DryRun {
			if err := e.state.save(); err != nil {
				res.Errors = append(res.Errors, err.Error())
			}
		}
		res.Elapsed = time.Since(res.Started).Round(time.Millisecond).String()
		return res, nil
	}
	if e.opts.DryRun {
		e.log("would apply %d change(s):", len(ops))
	}
	res, err := e.Apply(ctx, ops)
	if err != nil {
		return res, err
	}
	return res, nil
}

// Paths is a convenience wrapper for callers that only need the local scan.
func (e *SyncEngine) LocalPaths() []string {
	out := make([]string, 0, len(e.local))
	for p := range e.local {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// jsonResult renders a result for --json output.
func (r *Result) JSON() string {
	b, _ := json.MarshalIndent(r, "", "  ")
	return string(b)
}

// DefaultVersionRetention keeps one previous copy of a replaced file, so an
// accidental overwrite is recoverable without any configuration.
const DefaultVersionRetention = 1

// versionRetention reads the retention count from config. Retention is on by
// default for safety; 0 disables it entirely.
func versionRetention() int {
	cfg, err := loadConfig()
	if err != nil {
		return DefaultVersionRetention
	}
	if cfg.VersionRetention == 0 {
		return DefaultVersionRetention
	}
	if cfg.VersionRetention < 0 {
		return 0
	}
	return cfg.VersionRetention
}
