package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPlanMoveWithTombstoneRenamesLocalFile(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	e, dir := newTestEngine(t, conflictNewest)
	writeLocal(t, dir, "old.txt", "unique-move-payload", now)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	hash := e.local["old.txt"].Hash
	e.state.Entries["old.txt"] = &EntryState{BaseSHA256: hash, ObjectID: "obj-moved", Size: e.local["old.txt"].Size, ModTime: now}
	e.tombstoneRemotePath("old.txt", now)
	e.state.Remote["new.txt"] = &RemoteEntry{ObjectID: "obj-moved", SHA256: hash, Size: e.local["old.txt"].Size, ModTime: now}

	ops := e.Plan()
	if !hasMove(ops, "old.txt", "new.txt") {
		t.Fatalf("want move old.txt → new.txt, got %s", describeOps(ops))
	}
	if hasOp(ops, OpUpload, "old.txt") || hasOp(ops, OpDownload, "new.txt") {
		t.Fatalf("move must not upload or download, got %s", describeOps(ops))
	}

	res, err := e.Apply(context.Background(), ops)
	if err != nil {
		t.Fatal(err)
	}
	if res.Moved != 1 || res.Uploaded != 0 || res.Downloaded != 0 {
		t.Fatalf("apply counts: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old path still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err != nil {
		t.Fatalf("new path missing: %v", err)
	}
}

func TestPlanMoveByHashWithoutTombstone(t *testing.T) {
	// A second sync folder (or a machine that never saw the incremental
	// event) still has the file at the old path. The same bytes now live
	// remotely under a new name: treat that as a move, not an upload.
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	e, dir := newTestEngine(t, conflictNewest)
	writeLocal(t, dir, "textfile.txt", "payload-from-the-root", now)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	hash := e.local["textfile.txt"].Hash
	e.state.Remote["tessera/test/textfile.txt"] = &RemoteEntry{
		ObjectID: "obj-1", SHA256: hash, Size: e.local["textfile.txt"].Size, ModTime: now,
	}

	ops := e.Plan()
	if hasOp(ops, OpUpload, "textfile.txt") {
		t.Fatalf("must not re-upload the old path, got %s", describeOps(ops))
	}
	if !hasMove(ops, "textfile.txt", "tessera/test/textfile.txt") {
		t.Fatalf("want move to the new remote path, got %s", describeOps(ops))
	}
}

func TestPlanAdoptWhenObjectIDChangesSamePath(t *testing.T) {
	// tessera optimize re-uploads at the same path with a new object id.
	// The bytes are unchanged, so sync must adopt rather than download.
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	e, dir := newTestEngine(t, conflictNewest)
	writeLocal(t, dir, "packed.txt", "same-bytes-after-optimize", now)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	hash := e.local["packed.txt"].Hash
	size := e.local["packed.txt"].Size
	e.state.Entries["packed.txt"] = &EntryState{BaseSHA256: hash, ObjectID: "old-id", Size: size, ModTime: now}
	e.state.Remote["packed.txt"] = &RemoteEntry{ObjectID: "new-id", SHA256: hash, Size: size, ModTime: now}

	ops := e.Plan()
	if !hasOp(ops, OpAdopt, "packed.txt") {
		t.Fatalf("want adopt after object-id change, got %s", describeOps(ops))
	}
	if hasOp(ops, OpDownload, "packed.txt") || hasOp(ops, OpUpload, "packed.txt") {
		t.Fatalf("object-id change with matching hash must not transfer: %s", describeOps(ops))
	}
}

func TestPlanAdoptFromLocalCopy(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	e, dir := newTestEngine(t, conflictNewest)
	// The old name is still a live remote path (a second copy), so we must
	// not steal it via rename; we copy/adopt the matching bytes instead.
	writeLocal(t, dir, "kept.txt", "shared-bytes", now)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	hash := e.local["kept.txt"].Hash
	size := e.local["kept.txt"].Size
	e.state.Entries["kept.txt"] = &EntryState{BaseSHA256: hash, ObjectID: "obj-keep", Size: size, ModTime: now}
	e.state.Remote["kept.txt"] = &RemoteEntry{ObjectID: "obj-keep", SHA256: hash, Size: size, ModTime: now}
	e.state.Remote["copy.txt"] = &RemoteEntry{ObjectID: "obj-copy", SHA256: hash, Size: size, ModTime: now}

	ops := e.Plan()
	if !hasOp(ops, OpAdopt, "copy.txt") {
		t.Fatalf("want adopt of copy.txt from local bytes, got %s", describeOps(ops))
	}
	var adopt Op
	for _, op := range ops {
		if op.Kind == OpAdopt && op.Path == "copy.txt" {
			adopt = op
		}
	}
	if adopt.Extra != "kept.txt" {
		t.Fatalf("adopt extra = %q, want kept.txt", adopt.Extra)
	}
	res, err := e.Apply(context.Background(), ops)
	if err != nil {
		t.Fatal(err)
	}
	if res.Adopted != 1 || res.Downloaded != 0 {
		t.Fatalf("adopt must not download: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "kept.txt")); err != nil {
		t.Fatalf("source copy should remain: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "copy.txt"))
	if err != nil || string(got) != "shared-bytes" {
		t.Fatalf("copied dest contents = %q err=%v", got, err)
	}
}

func TestTombstoneRemotePathKeepsHash(t *testing.T) {
	e, _ := newTestEngine(t, conflictNewest)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	e.state.Remote["old.txt"] = &RemoteEntry{ObjectID: "obj", SHA256: "abc", Size: 4, Updated: now}
	e.tombstoneRemotePath("old.txt", now.Add(time.Minute))
	re := e.state.Remote["old.txt"]
	if re == nil || !re.Deleted || re.ObjectID != "" {
		t.Fatalf("tombstone = %#v", re)
	}
	if re.SHA256 != "abc" || re.Size != 4 {
		t.Fatalf("tombstone should keep hash and size: %#v", re)
	}
}

func hasMove(ops []Op, src, dst string) bool {
	for _, op := range ops {
		if op.Kind == OpMove && op.Path == src && op.Extra == dst {
			return true
		}
	}
	return false
}
