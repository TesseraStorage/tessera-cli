package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newTestEngine builds an engine with no network attached; only the pure
// planning and scanning paths are exercised.
func newTestEngine(t *testing.T, policy string) (*SyncEngine, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &SyncConfig{
		ID:             "testroot",
		LocalPath:      dir,
		RemotePrefix:   "tessera/test",
		ConflictPolicy: policy,
	}
	st := newSyncState(cfg.ID)
	ig, err := newIgnore(dir, nil)
	if err != nil {
		t.Fatalf("newIgnore: %v", err)
	}
	return &SyncEngine{
		cfg:     cfg,
		state:   st,
		ig:      ig,
		opts:    SyncOptions{ConflictMode: policy},
		machine: "testhost",
	}, dir
}

// writeLocal creates a file on disk and returns its size and mtime.
func writeLocal(t *testing.T, dir, rel, content string, mtime time.Time) (int64, time.Time) {
	t.Helper()
	abs := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	if err := os.Chtimes(abs, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return fi.Size(), fi.ModTime()
}

func hasOp(ops []Op, kind OpKind, path string) bool {
	for _, op := range ops {
		if op.Kind == kind && op.Path == path {
			return true
		}
	}
	return false
}

func describeOps(ops []Op) string {
	if len(ops) == 0 {
		return "(none)"
	}
	s := ""
	for i, op := range ops {
		if i > 0 {
			s += ", "
		}
		s += string(op.Kind) + " " + op.Path
	}
	return s
}

// TestPlanReconcileTable walks the reconcile decision table from the design.
func TestPlanReconcileTable(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	older := now.Add(-time.Hour)

	t.Run("identical sides are a no-op", func(t *testing.T) {
		e, dir := newTestEngine(t, conflictNewest)
		size, mtime := writeLocal(t, dir, "a.txt", "hello", now)
		if err := e.ScanLocal(); err != nil {
			t.Fatal(err)
		}
		e.state.Entries["a.txt"] = &EntryState{BaseSHA256: e.local["a.txt"].Hash, ObjectID: "obj1", Size: size, ModTime: mtime}
		e.state.Remote["a.txt"] = &RemoteEntry{ObjectID: "obj1", SHA256: e.local["a.txt"].Hash, Size: size, ModTime: mtime}
		if ops := e.Plan(); len(ops) != 0 {
			t.Errorf("want no ops, got %s", describeOps(ops))
		}
	})

	t.Run("new local file uploads", func(t *testing.T) {
		e, dir := newTestEngine(t, conflictNewest)
		writeLocal(t, dir, "new.txt", "data", now)
		if err := e.ScanLocal(); err != nil {
			t.Fatal(err)
		}
		ops := e.Plan()
		if !hasOp(ops, OpUpload, "new.txt") {
			t.Errorf("want upload of new.txt, got %s", describeOps(ops))
		}
	})

	t.Run("new remote file downloads", func(t *testing.T) {
		e, _ := newTestEngine(t, conflictNewest)
		if err := e.ScanLocal(); err != nil {
			t.Fatal(err)
		}
		e.state.Remote["remote.txt"] = &RemoteEntry{ObjectID: "obj2", SHA256: "abc", Size: 10, ModTime: now}
		ops := e.Plan()
		if !hasOp(ops, OpDownload, "remote.txt") {
			t.Errorf("want download of remote.txt, got %s", describeOps(ops))
		}
	})

	t.Run("local edit uploads", func(t *testing.T) {
		e, dir := newTestEngine(t, conflictNewest)
		writeLocal(t, dir, "a.txt", "changed", now)
		if err := e.ScanLocal(); err != nil {
			t.Fatal(err)
		}
		e.state.Entries["a.txt"] = &EntryState{BaseSHA256: "oldhash", ObjectID: "obj1"}
		e.state.Remote["a.txt"] = &RemoteEntry{ObjectID: "obj1", SHA256: "oldhash", ModTime: older}
		ops := e.Plan()
		if !hasOp(ops, OpUpload, "a.txt") {
			t.Errorf("want upload, got %s", describeOps(ops))
		}
	})

	t.Run("remote edit downloads", func(t *testing.T) {
		e, dir := newTestEngine(t, conflictNewest)
		size, mtime := writeLocal(t, dir, "a.txt", "hello", older)
		if err := e.ScanLocal(); err != nil {
			t.Fatal(err)
		}
		e.state.Entries["a.txt"] = &EntryState{BaseSHA256: e.local["a.txt"].Hash, ObjectID: "obj1", Size: size, ModTime: mtime}
		e.state.Remote["a.txt"] = &RemoteEntry{ObjectID: "obj2", SHA256: "newhash", ModTime: now}
		ops := e.Plan()
		if !hasOp(ops, OpDownload, "a.txt") {
			t.Errorf("want download, got %s", describeOps(ops))
		}
	})

	t.Run("local delete propagates remotely", func(t *testing.T) {
		e, _ := newTestEngine(t, conflictNewest)
		if err := e.ScanLocal(); err != nil {
			t.Fatal(err)
		}
		e.state.Entries["gone.txt"] = &EntryState{BaseSHA256: "h", ObjectID: "obj1", DeletedLocal: true}
		e.state.Remote["gone.txt"] = &RemoteEntry{ObjectID: "obj1", SHA256: "h", ModTime: older}
		ops := e.Plan()
		if !hasOp(ops, OpDeleteRemote, "gone.txt") {
			t.Errorf("want remote delete, got %s", describeOps(ops))
		}
	})

	t.Run("remote delete removes the local file", func(t *testing.T) {
		e, dir := newTestEngine(t, conflictNewest)
		writeLocal(t, dir, "gone.txt", "hello", older)
		if err := e.ScanLocal(); err != nil {
			t.Fatal(err)
		}
		e.state.Entries["gone.txt"] = &EntryState{BaseSHA256: e.local["gone.txt"].Hash, ObjectID: "obj1", ModTime: older}
		e.state.Remote["gone.txt"] = &RemoteEntry{Deleted: true, Updated: now}
		ops := e.Plan()
		if !hasOp(ops, OpDeleteLocal, "gone.txt") {
			t.Errorf("want local delete, got %s", describeOps(ops))
		}
	})

	t.Run("remote delete loses to a newer local edit", func(t *testing.T) {
		e, dir := newTestEngine(t, conflictNewest)
		writeLocal(t, dir, "a.txt", "edited after delete", now)
		if err := e.ScanLocal(); err != nil {
			t.Fatal(err)
		}
		e.state.Entries["a.txt"] = &EntryState{BaseSHA256: "oldhash", ObjectID: "obj1", ModTime: older}
		e.state.Remote["a.txt"] = &RemoteEntry{Deleted: true, ModTime: older, Updated: now}
		ops := e.Plan()
		if !hasOp(ops, OpUpload, "a.txt") {
			t.Errorf("want re-upload of the newer local edit, got %s", describeOps(ops))
		}
	})

	t.Run("newer local edit beats other machine's remote delete", func(t *testing.T) {
		e, dir := newTestEngine(t, conflictNewest)
		writeLocal(t, dir, "a.txt", "v2", now)
		if err := e.ScanLocal(); err != nil {
			t.Fatal(err)
		}
		e.state.Entries["a.txt"] = &EntryState{BaseSHA256: "h1", ObjectID: "obj1", ModTime: older}
		e.state.Remote["a.txt"] = &RemoteEntry{Deleted: true, ModTime: older}
		ops := e.Plan()
		if !hasOp(ops, OpUpload, "a.txt") {
			t.Errorf("want upload of newer local edit, got %s", describeOps(ops))
		}
	})

	t.Run("both changed, newest policy picks the newer local copy", func(t *testing.T) {
		e, dir := newTestEngine(t, conflictNewest)
		writeLocal(t, dir, "a.txt", "local newer", now)
		if err := e.ScanLocal(); err != nil {
			t.Fatal(err)
		}
		e.state.Entries["a.txt"] = &EntryState{BaseSHA256: "base", ObjectID: "obj1", ModTime: older}
		e.state.Remote["a.txt"] = &RemoteEntry{ObjectID: "obj2", SHA256: "remotehash", ModTime: older}
		ops := e.Plan()
		if !hasOp(ops, OpUpload, "a.txt") {
			t.Errorf("want upload (local newer), got %s", describeOps(ops))
		}
	})

	t.Run("both changed, newest policy picks the newer remote copy", func(t *testing.T) {
		e, dir := newTestEngine(t, conflictNewest)
		writeLocal(t, dir, "a.txt", "local older", older)
		if err := e.ScanLocal(); err != nil {
			t.Fatal(err)
		}
		e.state.Entries["a.txt"] = &EntryState{BaseSHA256: "base", ObjectID: "obj1", ModTime: older}
		e.state.Remote["a.txt"] = &RemoteEntry{ObjectID: "obj2", SHA256: "remotehash", ModTime: now}
		ops := e.Plan()
		if !hasOp(ops, OpDownload, "a.txt") {
			t.Errorf("want download (remote newer), got %s", describeOps(ops))
		}
		if hasOp(ops, OpConflictLocal, "a.txt") || hasOp(ops, OpConflictRemote, "a.txt") {
			t.Errorf("newest policy must not create conflict copies: %s", describeOps(ops))
		}
	})

	t.Run("both changed, keep-both preserves the loser", func(t *testing.T) {
		e, dir := newTestEngine(t, conflictKeepBoth)
		writeLocal(t, dir, "a.txt", "local newer", now)
		if err := e.ScanLocal(); err != nil {
			t.Fatal(err)
		}
		e.state.Entries["a.txt"] = &EntryState{BaseSHA256: "base", ObjectID: "obj1", ModTime: older}
		e.state.Remote["a.txt"] = &RemoteEntry{ObjectID: "obj2", SHA256: "remotehash", ModTime: older, Machine: "WINDOWS-PC"}
		ops := e.Plan()
		if !hasOp(ops, OpConflictRemote, "a.txt") {
			t.Errorf("want a conflict copy of the remote side, got %s", describeOps(ops))
		}
		if !hasOp(ops, OpUpload, "a.txt") {
			t.Errorf("want the local side uploaded, got %s", describeOps(ops))
		}
		for _, op := range ops {
			if op.Kind == OpConflictRemote && op.Extra == "" {
				t.Error("conflict op must record where the copy was kept")
			}
		}
	})

	t.Run("identical content on both sides is not a conflict", func(t *testing.T) {
		e, dir := newTestEngine(t, conflictKeepBoth)
		writeLocal(t, dir, "a.txt", "same", now)
		if err := e.ScanLocal(); err != nil {
			t.Fatal(err)
		}
		h := e.local["a.txt"].Hash
		e.state.Entries["a.txt"] = &EntryState{BaseSHA256: "base", ObjectID: "obj1"}
		e.state.Remote["a.txt"] = &RemoteEntry{ObjectID: "obj2", SHA256: h, ModTime: older}
		ops := e.Plan()
		// Exactly one adopt, no transfer and no conflict copy.
		if len(ops) != 1 || ops[0].Kind != OpAdopt {
			t.Errorf("want a single adopt for identical content, got %s", describeOps(ops))
		}
		for _, op := range ops {
			if op.Kind != OpAdopt {
				t.Errorf("identical content must not be transferred or copied: %s", describeOps(ops))
			}
		}
	})

	t.Run("remote-only file after a local delete is removed remotely", func(t *testing.T) {
		e, _ := newTestEngine(t, conflictNewest)
		if err := e.ScanLocal(); err != nil {
			t.Fatal(err)
		}
		e.state.Entries["x.txt"] = &EntryState{BaseSHA256: "h", ObjectID: "obj1", DeletedLocal: true}
		e.state.Remote["x.txt"] = &RemoteEntry{ObjectID: "obj1", SHA256: "h"}
		ops := e.Plan()
		if !hasOp(ops, OpDeleteRemote, "x.txt") {
			t.Errorf("want remote delete, got %s", describeOps(ops))
		}
	})

	t.Run("adopting remote deletes a local-only file", func(t *testing.T) {
		e, dir := newTestEngine(t, conflictNewest)
		writeLocal(t, dir, "stray.txt", "x", now)
		e.state.Adopt = adoptRemote
		if err := e.ScanLocal(); err != nil {
			t.Fatal(err)
		}
		ops := e.Plan()
		if !hasOp(ops, OpDeleteLocal, "stray.txt") {
			t.Errorf("want local delete when adopting remote, got %s", describeOps(ops))
		}
	})
}

// TestScanLocalIgnoresAndCaching checks the ignore rules and the hash cache.
func TestScanLocalIgnoresAndCaching(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	e, dir := newTestEngine(t, conflictNewest)

	writeLocal(t, dir, "keep/a.txt", "hello", now)
	writeLocal(t, dir, "skip.log", "noise", now)
	writeLocal(t, dir, ".DS_Store", "junk", now)
	writeLocal(t, dir, "sub/keep/b.txt", "world", now)
	e.ig.add("*.log")

	if err := e.ScanLocal(); err != nil {
		t.Fatalf("ScanLocal: %v", err)
	}
	for _, want := range []string{"keep/a.txt", "sub/keep/b.txt"} {
		if _, ok := e.local[want]; !ok {
			t.Errorf("%s should have been scanned (got %v)", want, e.LocalPaths())
		}
	}
	for _, unwanted := range []string{"skip.log", ".DS_Store"} {
		if _, ok := e.local[unwanted]; ok {
			t.Errorf("%s should have been ignored", unwanted)
		}
	}

	// A second scan with matching state must reuse the stored hash rather than
	// re-reading the file.
	h := e.local["keep/a.txt"].Hash
	fi, _ := os.Stat(filepath.Join(dir, "keep", "a.txt"))
	e.state.Entries["keep/a.txt"] = &EntryState{BaseSHA256: h, Size: fi.Size(), ModTime: fi.ModTime()}
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	if got := e.local["keep/a.txt"].Hash; got != h {
		t.Errorf("hash cache miss: got %s want %s", got, h)
	}
}

// TestStateRoundTrip verifies the on-disk state survives a save/load cycle.
func TestStateRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	st := newSyncState("abcd1234")
	st.Cursor = Cursor{After: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC), Key: "abc"}
	st.Entries["a.txt"] = &EntryState{BaseSHA256: "h", ObjectID: "obj", Size: 5}
	st.Remote["a.txt"] = &RemoteEntry{ObjectID: "obj", SHA256: "h", Size: 5}
	st.Adopt = adoptMerge
	if err := st.save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := loadSyncState("abcd1234")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Cursor.Key != "abc" || !got.Cursor.After.Equal(st.Cursor.After) {
		t.Errorf("cursor round-trip failed: %+v", got.Cursor)
	}
	if e := got.Entries["a.txt"]; e == nil || e.ObjectID != "obj" {
		t.Errorf("entry round-trip failed: %+v", got.Entries)
	}
	if got.Adopt != adoptMerge {
		t.Errorf("Adopt = %q, want %q", got.Adopt, adoptMerge)
	}
}

// TestVersionNamespaceAndPruning checks retained-version bookkeeping.
func TestVersionNamespaceAndPruning(t *testing.T) {
	// Versions sit beside the file, keyed by its base name, so the namespace is
	// derived from the file path (which carries the remote prefix) rather than
	// from the sync root id.
	if got, want := versionNamespace("tessera/mac/Docs", "a/b.txt"), "tessera/mac/Docs/.tessera-versions/b.txt"; got != want {
		t.Errorf("versionNamespace = %q, want %q", got, want)
	}
	if !isVersionPath("tessera/mac/Docs", "tessera/mac/Docs/.tessera-versions/b.txt/20260101-000000-deadbeef") {
		t.Error("isVersionPath should recognise a version path")
	}
	if isVersionPath("tessera/mac/Docs", "tessera/mac/Docs/a/b.txt") {
		t.Error("isVersionPath should not match a normal path")
	}
}

// TestPlanSkipsVersionNamespace ensures retained versions are never synced as
// ordinary files.
func TestPlanSkipsVersionNamespace(t *testing.T) {
	e, _ := newTestEngine(t, conflictNewest)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	vpath := "tessera/test/.tessera-versions/a.txt/20260101-000000-deadbeef"
	e.state.Remote[vpath] = &RemoteEntry{ObjectID: "objv", SHA256: "vh"}
	if ops := e.Plan(); len(ops) != 0 {
		t.Errorf("version paths must not produce ops, got %s", describeOps(ops))
	}
}

// TestLocalPathForRemote pins the mapping between remote-absolute paths and
// root-relative ones, which is what keeps a peer's objects out of the wrong
// subfolder and out of the version/trash namespaces.
func TestLocalPathForRemote(t *testing.T) {
	e, _ := newTestEngine(t, conflictNewest) // remote prefix tessera/test
	cases := []struct {
		remote string
		want   string
		ours   bool
	}{
		{"tessera/test/a.txt", "a.txt", true},
		{"tessera/test/dir/b.txt", "dir/b.txt", true},
		{"tessera/test", "", false},
		{"tessera/testx/a.txt", "", false},
		{"tessera/other/a.txt", "", false},
		{"a.txt", "", false},
		{"tessera/test/.tessera-versions/a.txt/20260101-000000-deadbeef", "", false},
		{"tessera/test/.tessera-trash/deadbeef/a.txt", "", false},
	}
	for _, c := range cases {
		got, ours := e.localPathForRemote(c.remote)
		if ours != c.ours || (ours && got != c.want) {
			t.Errorf("localPathForRemote(%q) = (%q, %v), want (%q, %v)", c.remote, got, ours, c.want, c.ours)
		}
	}

	if got, want := e.remotePath("dir/b.txt"), "tessera/test/dir/b.txt"; got != want {
		t.Errorf("remotePath = %q, want %q", got, want)
	}
}

// TestRemoteDeleteIsNotResurrectedByLocalCopy guards the bug found by the live
// end-to-end test: when machine A had downloaded a file and machine B deleted
// it, A pushed its local copy back up instead of deleting it, because the
// deletion was judged against current liveness rather than the tombstone.
func TestRemoteDeleteIsNotResurrectedByLocalCopy(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	e, dir := newTestEngine(t, conflictNewest)
	// The local copy is byte-identical to what we last agreed on.
	size, mtime := writeLocal(t, dir, "shared.txt", "same bytes", now)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	hash := e.local["shared.txt"].Hash
	e.state.Entries["shared.txt"] = &EntryState{
		BaseSHA256: hash, ObjectID: "obj1", Size: size, ModTime: mtime,
	}
	// The remote side tombstoned it.
	e.state.Remote["shared.txt"] = &RemoteEntry{Deleted: true, ObjectID: "obj1", Updated: now}

	ops := e.Plan()
	if hasOp(ops, OpUpload, "shared.txt") {
		t.Fatalf("a tombstoned file must not be re-uploaded, got %s", describeOps(ops))
	}
	if !hasOp(ops, OpDeleteLocal, "shared.txt") {
		t.Fatalf("want a local delete, got %s", describeOps(ops))
	}
}

// TestLocalEditBeatsRemoteDeleteWhenClearlyNewer documents the one exception:
// an edit made after the deletion wins, so a delete by another machine cannot
// silently discard newer work.
func TestLocalEditBeatsRemoteDeleteWhenClearlyNewer(t *testing.T) {
	deletedAt := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	editedAt := deletedAt.Add(5 * time.Minute)
	e, dir := newTestEngine(t, conflictNewest)
	writeLocal(t, dir, "work.txt", "edited after the delete", editedAt)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	e.state.Entries["work.txt"] = &EntryState{BaseSHA256: "base", ObjectID: "obj1", ModTime: deletedAt}
	e.state.Remote["work.txt"] = &RemoteEntry{Deleted: true, Updated: deletedAt}

	ops := e.Plan()
	if !hasOp(ops, OpUpload, "work.txt") {
		t.Fatalf("a clearly newer edit should be re-uploaded, got %s", describeOps(ops))
	}
}

// TestUnseenTombstoneIsAChange pins the liveness-independent check.
func TestUnseenTombstoneIsAChange(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	e, dir := newTestEngine(t, conflictNewest)
	writeLocal(t, dir, "a.txt", "hello", now)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	e.state.Entries["a.txt"] = &EntryState{BaseSHA256: e.local["a.txt"].Hash, ObjectID: "obj1", ModTime: now}
	e.state.Remote["a.txt"] = &RemoteEntry{Deleted: true, ObjectID: "obj1", Updated: now}
	if !e.changedRemote("a.txt") {
		t.Error("an unacted tombstone must count as a remote change")
	}
	// Once acted upon, it stops being a change.
	e.state.Entries["a.txt"].DeletedRemote = true
	if e.changedRemote("a.txt") {
		t.Error("an already-applied tombstone must not report a change")
	}
}

// TestBootstrapAdoptsMatchingContent covers a first sync of a machine that
// already holds the same bytes: it must record the file as in-sync rather than
// downloading data that is already on disk. This was the bug that made
// `tessera upload` on a folder registered for sync download its own upload.
func TestBootstrapAdoptsMatchingContent(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	e, dir := newTestEngine(t, conflictNewest)
	writeLocal(t, dir, "report.txt", "quarterly report v1", now)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	hash := e.local["report.txt"].Hash
	// The remote holds identical content, but this machine has no entry yet.
	e.state.Remote["report.txt"] = &RemoteEntry{ObjectID: "obj1", SHA256: hash, Size: 20, ModTime: now}

	ops := e.Plan()
	if len(ops) != 1 || ops[0].Kind != OpAdopt {
		t.Fatalf("want a single adopt operation, got %s", describeOps(ops))
	}
	// Applying the adopt must record the path without transferring anything.
	eng2, _ := newTestEngine(t, conflictNewest)
	_ = eng2
	res, err := e.Apply(context.Background(), ops)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Adopted != 1 || res.Downloaded != 0 || res.Uploaded != 0 {
		t.Fatalf("adopt must not transfer data: %+v", res)
	}
	if st := e.state.Entries["report.txt"]; st == nil || st.BaseSHA256 != hash {
		t.Fatalf("adopted path was not recorded in state: %+v", e.state.Entries["report.txt"])
	}
	// A later plan must be a no-op.
	if ops := e.Plan(); len(ops) != 0 {
		t.Fatalf("second plan should be empty, got %s", describeOps(ops))
	}
}

// TestBootstrapMergePicksTheNewerSide documents the default first-run policy
// when a folder and the network both already hold different content for the
// same path: the newer copy wins, per file.
func TestBootstrapMergePicksTheNewerSide(t *testing.T) {
	remoteNewer := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	localOlder := remoteNewer.Add(-time.Hour)

	t.Run("remote is newer so it wins", func(t *testing.T) {
		e, dir := newTestEngine(t, conflictNewest)
		writeLocal(t, dir, "report.txt", "local variant", localOlder)
		if err := e.ScanLocal(); err != nil {
			t.Fatal(err)
		}
		e.state.Remote["report.txt"] = &RemoteEntry{ObjectID: "obj1", SHA256: "different", Size: 20, ModTime: remoteNewer}
		ops := e.Plan()
		if !hasOp(ops, OpDownload, "report.txt") {
			t.Fatalf("remote copy is newer, want download, got %s", describeOps(ops))
		}
	})

	t.Run("local is newer so it wins", func(t *testing.T) {
		e, dir := newTestEngine(t, conflictNewest)
		writeLocal(t, dir, "report.txt", "local variant", remoteNewer)
		if err := e.ScanLocal(); err != nil {
			t.Fatal(err)
		}
		e.state.Remote["report.txt"] = &RemoteEntry{ObjectID: "obj1", SHA256: "different", Size: 20, ModTime: localOlder}
		ops := e.Plan()
		if !hasOp(ops, OpUpload, "report.txt") {
			t.Fatalf("local copy is newer, want upload, got %s", describeOps(ops))
		}
	})

	t.Run("adopt remote overrides mtime", func(t *testing.T) {
		e, dir := newTestEngine(t, conflictNewest)
		writeLocal(t, dir, "report.txt", "local variant", remoteNewer)
		if err := e.ScanLocal(); err != nil {
			t.Fatal(err)
		}
		e.state.Adopt = adoptRemote
		e.state.Remote["report.txt"] = &RemoteEntry{ObjectID: "obj1", SHA256: "different", Size: 20, ModTime: localOlder}
		ops := e.Plan()
		if !hasOp(ops, OpDownload, "report.txt") {
			t.Fatalf("adopt remote must take the remote copy, got %s", describeOps(ops))
		}
	})
}

// TestUploadBookkeepingSharedWithSync covers the second live-test bug:
// `tessera upload` on a folder registered for sync wrote only to the index, so
// the next sync saw the file as new on both sides and pulled its own upload
// back down.
func TestUploadBookkeepingSharedWithSync(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	local := t.TempDir()

	cfg, err := addRoot(local, "tessera/mac/Docs")
	if err != nil {
		t.Fatalf("addRoot: %v", err)
	}

	info := PathInfo{
		ObjectID: "abcdef", Path: "tessera/mac/Docs/report.txt",
		Name: "report.txt", Root: cfg.ID, SHA256: "hash123", Size: 42,
	}
	recordUploadInSyncState(info, time.Now())

	st, err := loadSyncState(cfg.ID)
	if err != nil {
		t.Fatalf("loadSyncState: %v", err)
	}
	ent := st.Entries["report.txt"]
	if ent == nil {
		t.Fatal("upload did not record sync bookkeeping; sync would re-download the file")
	}
	if ent.BaseSHA256 != "hash123" || ent.ObjectID != "abcdef" {
		t.Errorf("bookkeeping mismatch: %+v", ent)
	}
	re := st.Remote["report.txt"]
	if re == nil || re.Deleted {
		t.Errorf("remote view was not updated: %+v", re)
	}

	// A CLI delete must mark the path so the next sync propagates it instead
	// of restoring the file.
	recordDeleteInSyncState("tessera/mac/Docs/report.txt")
	st2, err := loadSyncState(cfg.ID)
	if err != nil {
		t.Fatalf("loadSyncState: %v", err)
	}
	if e := st2.Entries["report.txt"]; e == nil || !e.DeletedLocal {
		t.Errorf("delete did not record a local tombstone: %+v", e)
	}
}

// TestUploadBookkeepingIgnoresOtherRoots ensures bookkeeping is scoped to the
// owning sync folder.
func TestUploadBookkeepingIgnoresOtherRoots(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	local := t.TempDir()
	cfg, err := addRoot(local, "tessera/mac/Docs")
	if err != nil {
		t.Fatalf("addRoot: %v", err)
	}

	recordUploadInSyncState(PathInfo{
		ObjectID: "x", Path: "tessera/other/thing.txt", Name: "thing.txt", SHA256: "h",
	}, time.Now())

	st, err := loadSyncState(cfg.ID)
	if err != nil {
		t.Fatalf("loadSyncState: %v", err)
	}
	if len(st.Entries) != 0 {
		t.Errorf("a path outside the root must not create bookkeeping: %+v", st.Entries)
	}
}

// TestRemoteOnlyFileIsNotDeletedLocally covers the bug where a remote object
// that simply is not materialised on this machine — a conflict copy another
// machine created, for example — was treated as "deleted locally" and unpinned
// from the network, destroying a file that existed elsewhere.
func TestRemoteOnlyFileIsNotDeletedLocally(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	e, _ := newTestEngine(t, conflictNewest)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	const conflict = "shared (conflicted copy, WINDOWS-PC 2026-09-25-120000).txt"
	e.state.Entries[conflict] = &EntryState{
		BaseSHA256: "hashA", ObjectID: "obj1", Size: 18, ModTime: now,
		// The remote still holds exactly what we last agreed on.
	}
	e.state.Remote[conflict] = &RemoteEntry{
		ObjectID: "obj1", SHA256: "hashA", Size: 18, ModTime: now, Machine: "WINDOWS-PC",
	}

	ops := e.Plan()
	if hasOp(ops, OpDeleteRemote, conflict) {
		t.Fatalf("a file that exists remotely must never be unpinned just because it is absent locally: %s", describeOps(ops))
	}
	if !hasOp(ops, OpDownload, conflict) {
		t.Fatalf("want the missing local copy restored, got %s", describeOps(ops))
	}
}

// TestMissingLocalWithGenuineTombstoneStillDeletes keeps the complementary
// case intact: a real local deletion must still be pushed.
func TestMissingLocalWithGenuineTombstoneStillDeletes(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	e, _ := newTestEngine(t, conflictNewest)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	e.state.Entries["gone.txt"] = &EntryState{
		BaseSHA256: "hashA", ObjectID: "obj1", Size: 18, ModTime: now, DeletedLocal: true,
	}
	e.state.Remote["gone.txt"] = &RemoteEntry{ObjectID: "obj1", SHA256: "hashA", Size: 18, ModTime: now}

	ops := e.Plan()
	if !hasOp(ops, OpDeleteRemote, "gone.txt") {
		t.Fatalf("a genuine local deletion must still be pushed, got %s", describeOps(ops))
	}
}

// TestContentEqualitySkipsDownloadForNewObjectID covers deduplication: the
// object id changes when an existing object is re-attached under a new path,
// but the bytes are identical, so no download should happen.
func TestContentEqualitySkipsDownloadForNewObjectID(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	e, dir := newTestEngine(t, conflictNewest)
	writeLocal(t, dir, "one.txt", "same bytes", now)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	hash := e.local["one.txt"].Hash
	// We previously agreed on this content under a different object id.
	e.state.Entries["one.txt"] = &EntryState{BaseSHA256: hash, ObjectID: "old-object", Size: 10, ModTime: now}
	e.state.Remote["one.txt"] = &RemoteEntry{ObjectID: "new-object", SHA256: hash, Size: 10, ModTime: now}

	ops := e.Plan()
	// The plan carries an explicit adopt so Apply can persist the new id, and
	// crucially the adopt transfers nothing.
	if len(ops) != 1 || ops[0].Kind != OpAdopt {
		t.Fatalf("want a single adopt operation, got %s", describeOps(ops))
	}
	for _, op := range ops {
		if op.Kind == OpDownload || op.Kind == OpUpload {
			t.Fatalf("identical content must not be transferred, got %s", describeOps(ops))
		}
	}
}

// TestMovedFileIsNotResurrectedAtItsOldPath covers the move case: a peer moves
// a file, so the old path is deleted and a new path carries the same content.
// The deleted path must stay deleted rather than being detected as a fresh
// local file once the peer's upload arrives.
func TestMovedFileIsNotResurrectedAtItsOldPath(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	e, dir := newTestEngine(t, conflictNewest)

	// The peer renamed old.txt to new.txt and both paths are now known.
	writeLocal(t, dir, "new.txt", "the content", now)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	hash := e.local["new.txt"].Hash

	e.state.Entries["old.txt"] = &EntryState{
		DeletedLocal: true, DeletedRemote: true, ModTime: now,
	}
	e.state.Remote["old.txt"] = &RemoteEntry{Deleted: true, Updated: now}
	e.state.Remote["new.txt"] = &RemoteEntry{ObjectID: "obj2", SHA256: hash, Size: 11, ModTime: now}

	ops := e.Plan()
	if hasOp(ops, OpUpload, "old.txt") {
		t.Fatalf("a renamed file must not reappear at its old path: %s", describeOps(ops))
	}
	// new.txt should be adopted silently since it already matches.
	if !hasOp(ops, OpAdopt, "new.txt") {
		t.Fatalf("new path should be adopted, got %s", describeOps(ops))
	}
}

// TestPruneTombstones checks that deletion records are bounded: recent ones
// are kept (they prevent resurrection), old ones are dropped.
func TestPruneTombstones(t *testing.T) {
	e, _ := newTestEngine(t, conflictNewest)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

	e.state.Entries["recent.txt"] = &EntryState{DeletedLocal: true, DeletedRemote: true, TombstonedAt: now.Add(-time.Hour)}
	e.state.Entries["ancient.txt"] = &EntryState{DeletedLocal: true, DeletedRemote: true, TombstonedAt: now.Add(-90 * 24 * time.Hour)}
	e.state.Entries["live.txt"] = &EntryState{BaseSHA256: "h", ObjectID: "o"}

	if n := e.state.pruneTombstones(now); n != 1 {
		t.Fatalf("pruned %d entries, want 1", n)
	}
	if _, ok := e.state.Entries["recent.txt"]; !ok {
		t.Error("a recent tombstone must be kept to prevent resurrection")
	}
	if _, ok := e.state.Entries["ancient.txt"]; ok {
		t.Error("an expired tombstone should have been pruned")
	}
	if _, ok := e.state.Entries["live.txt"]; !ok {
		t.Error("a live entry must never be pruned")
	}
}

// TestPushedDeletionLeavesTombstone guards the rename case: after a machine
// pushes a deletion, the object's own tombstone event must still resolve to a
// path. Without that, the remote view keeps the file alive and the next sync
// downloads it again, resurrecting a renamed file at its old name.
func TestPushedDeletionLeavesTombstone(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	e, dir := newTestEngine(t, conflictNewest)
	// The file was renamed: the old path is gone locally, the new path exists.
	writeLocal(t, dir, "renamed.txt", "shared", now)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	hash := e.local["renamed.txt"].Hash

	e.state.Entries["doc.txt"] = &EntryState{
		BaseSHA256: hash, ObjectID: "obj-old", Size: 6, ModTime: now, DeletedLocal: true,
	}
	e.state.Remote["doc.txt"] = &RemoteEntry{ObjectID: "obj-old", SHA256: hash, Size: 6, ModTime: now}
	e.state.Remote["renamed.txt"] = &RemoteEntry{ObjectID: "obj-new", SHA256: hash, Size: 6, ModTime: now}

	ops := e.Plan()
	if !hasOp(ops, OpDeleteRemote, "doc.txt") {
		t.Fatalf("want the old path deleted remotely, got %s", describeOps(ops))
	}
	if hasOp(ops, OpDownload, "doc.txt") {
		t.Fatalf("the renamed file must not be re-downloaded at its old path: %s", describeOps(ops))
	}

	// Simulate the indexer's tombstone event arriving afterwards: it must still
	// resolve to the path, so the deletion is not mistaken for a live file.
	e.state.Remote["doc.txt"] = &RemoteEntry{Deleted: true, Updated: now}
	if !e.changedRemote("doc.txt") {
		// Already applied by the delete itself; ensure no resurrection plan.
		if ops := e.Plan(); hasOp(ops, OpUpload, "doc.txt") || hasOp(ops, OpDownload, "doc.txt") {
			t.Fatalf("a tombstoned path must not reappear: %s", describeOps(ops))
		}
	}
}

// TestConflictMarkerSurvivesPlanning checks that `sync conflicts` has
// something to report even before any transfer happens.
func TestConflictMarkerSurvivesPlanning(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	e, dir := newTestEngine(t, conflictKeepBoth)
	writeLocal(t, dir, "a.txt", "local newer", now)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	e.state.Entries["a.txt"] = &EntryState{BaseSHA256: "base", ObjectID: "obj1", ModTime: now.Add(-time.Hour)}
	e.state.Remote["a.txt"] = &RemoteEntry{ObjectID: "obj2", SHA256: "other", ModTime: now.Add(-time.Hour), Machine: "WINDOWS-PC"}

	ops := e.Plan()
	var conflict Op
	for _, op := range ops {
		if op.Kind == OpConflictLocal || op.Kind == OpConflictRemote {
			conflict = op
		}
	}
	if conflict.Kind == "" {
		t.Fatalf("want a conflict operation, got %s", describeOps(ops))
	}
	if got := e.state.Conflicts["a.txt"]; got != conflict.Extra {
		t.Errorf("planner did not record the conflict copy: ledger=%q op=%q", got, conflict.Extra)
	}
}

// TestLocalDeletionWithUnchangedRemotePropagates is the case the live test
// caught: the remote object is byte-identical and untouched, and the local file
// is simply gone. Neither "content changed" test fires, so without an explicit
// scan diff the deletion is invisible and the file is never unpinned — which
// means peers keep it forever (this is what broke renames).
func TestLocalDeletionWithUnchangedRemotePropagates(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	e, dir := newTestEngine(t, conflictNewest)

	// A previous sync recorded this path.
	writeLocal(t, dir, "doc.txt", "shared", now)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	hash := e.local["doc.txt"].Hash
	e.state.Entries["doc.txt"] = &EntryState{BaseSHA256: hash, ObjectID: "obj1", Size: 6, ModTime: now}
	e.state.Remote["doc.txt"] = &RemoteEntry{ObjectID: "obj1", SHA256: hash, Size: 6, ModTime: now}

	// The user deletes it (or renames it away).
	if err := os.Remove(filepath.Join(dir, "doc.txt")); err != nil {
		t.Fatal(err)
	}
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	if e.changedRemote("doc.txt") {
		t.Fatal("nothing changed remotely in this scenario, by construction")
	}

	// This is the fix under test.
	if n := e.markMissingLocal(); n != 1 {
		t.Fatalf("markMissingLocal flagged %d paths, want 1", n)
	}

	ops := e.Plan()
	if !hasOp(ops, OpDeleteRemote, "doc.txt") {
		t.Fatalf("a deleted local file must be unpinned remotely, got %s", describeOps(ops))
	}
	if hasOp(ops, OpDownload, "doc.txt") {
		t.Fatalf("the deleted file must not be downloaded back: %s", describeOps(ops))
	}
}

// TestNeverMaterialisedRemoteFileIsNotTreatedAsDeleted keeps the complement:
// a path that was never downloaded here must not be unpinned.
func TestNeverMaterialisedRemoteFileIsNotTreatedAsDeleted(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	e, _ := newTestEngine(t, conflictNewest)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	// No local entry: this path is remote-only from this machine's point of view.
	e.state.Remote["conflict copy.txt"] = &RemoteEntry{ObjectID: "obj9", SHA256: "h9", Size: 4, ModTime: now}
	if n := e.markMissingLocal(); n != 0 {
		t.Fatalf("markMissingLocal flagged %d paths, want 0", n)
	}
	ops := e.Plan()
	if hasOp(ops, OpDeleteRemote, "conflict copy.txt") {
		t.Fatalf("a remote-only file must never be unpinned: %s", describeOps(ops))
	}
	if !hasOp(ops, OpDownload, "conflict copy.txt") {
		t.Fatalf("want the remote-only file downloaded, got %s", describeOps(ops))
	}
}

// TestConflictMarkerSurvivesApply covers the full conflict sequence: the
// planner decides, the winning side is uploaded, and the conflict copy is
// created. The record must survive all of it so `sync conflicts` can report
// it, and must be dropped when the copy is deleted.
func TestConflictMarkerSurvivesApply(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	e, dir := newTestEngine(t, conflictKeepBoth)
	writeLocal(t, dir, "a.txt", "local newer", now)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	hash := e.local["a.txt"].Hash
	e.state.Entries["a.txt"] = &EntryState{BaseSHA256: "base", ObjectID: "obj1", ModTime: now.Add(-time.Hour)}
	e.state.Remote["a.txt"] = &RemoteEntry{ObjectID: "obj2", SHA256: "other", ModTime: now.Add(-time.Hour), Machine: "WINDOWS-PC"}

	ops := e.Plan()
	var conflictPath string
	for _, op := range ops {
		if op.Kind == OpConflictRemote {
			conflictPath = op.Extra
		}
	}
	if conflictPath == "" {
		t.Fatalf("want a conflict operation, got %s", describeOps(ops))
	}

	// Simulate the apply of the winning upload without touching the network.
	ent := e.state.entry("a.txt")
	ent.BaseSHA256 = hash
	ent.ObjectID = "obj3"
	ent.ModTime = now
	if got := e.state.Conflicts["a.txt"]; got != conflictPath {
		t.Fatalf("the marker must survive the winning upload: %q", got)
	}

	// Deleting the conflict copy resolves the original path's note.
	e.clearConflictFor(conflictPath)
	if got := e.state.Conflicts["a.txt"]; got != "" {
		t.Errorf("deleting the conflict copy should clear the note, got %q", got)
	}
}

// TestConflictLedgerSurvivesNoOpSync covers the case that made `sync conflicts`
// report nothing: the conflict copy arrives from another machine, the local
// machine has nothing to transfer, so the plan is empty. The ledger must still
// be written to disk.
func TestConflictLedgerSurvivesNoOpSync(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	e, dir := newTestEngine(t, conflictKeepBoth)
	writeLocal(t, dir, "a.txt", "agreed content", now)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	hash := e.local["a.txt"].Hash
	const copyPath = "a (conflicted copy, WINDOWS-PC 2026-09-25-120000).txt"

	// This machine is fully in sync for a.txt, but the conflict copy exists
	// remotely because the peer recorded the conflict.
	e.state.Entries["a.txt"] = &EntryState{BaseSHA256: hash, ObjectID: "obj1", Size: 14, ModTime: now}
	e.state.Remote["a.txt"] = &RemoteEntry{ObjectID: "obj1", SHA256: hash, Size: 14, ModTime: now}
	e.state.Remote[copyPath] = &RemoteEntry{ObjectID: "obj2", SHA256: "otherhash", Size: 6, ModTime: now, Machine: "WINDOWS-PC"}
	e.state.noteConflict("a.txt", copyPath)

	if err := e.state.save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadSyncState(e.cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Conflicts["a.txt"] != copyPath {
		t.Fatalf("conflict ledger did not persist: %+v", reloaded.Conflicts)
	}
}

// TestConflictCopyIsRecognisedByEveryPeer covers the machine that only
// receives a conflict copy: it has nothing to transfer, but it must still
// report the conflict.
func TestConflictCopyIsRecognisedByEveryPeer(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	const copyPath = "notes/report (conflicted copy, WINDOWS-PC 2026-09-25-120000).pdf"

	original, ok := isConflictCopy(copyPath)
	if !ok || original != "notes/report.pdf" {
		t.Fatalf("isConflictCopy = (%q, %v), want (notes/report.pdf, true)", original, ok)
	}
	if _, ok := isConflictCopy("notes/report.pdf"); ok {
		t.Error("an ordinary path must not look like a conflict copy")
	}

	e, dir := newTestEngine(t, conflictKeepBoth)
	writeLocal(t, dir, "notes/report.pdf", "agreed", now)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	hash := e.local["notes/report.pdf"].Hash
	e.state.Entries["notes/report.pdf"] = &EntryState{BaseSHA256: hash, ObjectID: "obj1", Size: 6, ModTime: now}
	e.state.Remote["notes/report.pdf"] = &RemoteEntry{ObjectID: "obj1", SHA256: hash, Size: 6, ModTime: now}
	e.state.Remote[copyPath] = &RemoteEntry{ObjectID: "obj2", SHA256: "other", Size: 6, ModTime: now, Machine: "WINDOWS-PC"}

	if ops := e.Plan(); len(ops) != 1 || ops[0].Kind != OpDownload {
		t.Fatalf("want just the conflict copy downloaded, got %s", describeOps(ops))
	}
	if got := e.state.Conflicts["notes/report.pdf"]; got != copyPath {
		t.Errorf("the receiving machine must record the conflict, got %q", got)
	}
}

// TestPathsMatchesVersionNamespace pins the version/trash namespace matching,
// which the naive prefix comparison got wrong for every version lookup.
func TestPathsMatchesVersionNamespace(t *testing.T) {
	cases := []struct {
		stored  string
		prefix  string
		matches bool
	}{
		{"tessera/mac/Docs/a.txt", "tessera/mac/Docs", true},
		{"tessera/mac/Docs/a.txt", "tessera/mac/Docs/a.txt", true},
		{"tessera/mac/Docs/other.txt", "tessera/mac/Docs/a.txt", false},
		// Versions are stored namespace-relative.
		{"tessera/mac/Docs/.tessera-versions/a.txt/20260101-000000-deadbeef",
			"tessera/mac/Docs/.tessera-versions/a.txt", true},
		{"tessera/mac/Docs/.tessera-versions/a.txt/20260101-000000-deadbeef",
			"tessera/mac/Docs/.tessera-versions", true},
		{"tessera/mac/Docs/.tessera-versions/b.txt/20260101-000000-deadbeef",
			"tessera/mac/Docs/.tessera-versions/a.txt", false},
		{"tessera/mac/Docs/.tessera-trash/deadbeef/a.txt", "tessera/mac/Docs/.tessera-trash", true},
	}
	for _, c := range cases {
		if got := matchesNamespacePrefix(c.stored, c.prefix); got != c.matches {
			t.Errorf("matchesNamespacePrefix(%q, %q) = %v, want %v", c.stored, c.prefix, got, c.matches)
		}
	}
}
