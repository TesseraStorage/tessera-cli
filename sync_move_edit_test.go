package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// setupMovedWhileOffline models machine B: it last synced notes.txt (object
// obj-1, content "original"), another machine then moved that object to
// old/notes.txt, and B's local notes.txt now holds an edit made at editAt.
func setupMovedWhileOffline(t *testing.T, editAt, movedAt time.Time) (*SyncEngine, string) {
	t.Helper()
	synced := movedAt.Add(-time.Hour)
	e, dir := newTestEngine(t, conflictNewest)
	writeLocal(t, dir, "notes.txt", "B offline edit", editAt)
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	base := sha("original")
	e.state.Entries["notes.txt"] = &EntryState{BaseSHA256: base, ObjectID: "obj-1", Size: 8, ModTime: synced}
	e.tombstoneRemotePath("notes.txt", movedAt)
	e.state.Remote["notes.txt"].SHA256 = base
	e.state.Remote["old/notes.txt"] = &RemoteEntry{ObjectID: "obj-1", SHA256: base, Size: 8, ModTime: synced, Updated: movedAt}
	return e, dir
}

func checkEditFollowsMove(t *testing.T, e *SyncEngine, dir string) {
	t.Helper()
	ops := e.Plan()
	if !hasMove(ops, "notes.txt", "old/notes.txt") {
		t.Fatalf("want local rename to the new path, got %s", describeOps(ops))
	}
	if hasOp(ops, OpUpload, "notes.txt") {
		t.Fatalf("must not upload at the old path (undoes the move), got %s", describeOps(ops))
	}
	if !hasOp(ops, OpUpload, "old/notes.txt") {
		t.Fatalf("the offline edit must be uploaded at the new path, got %s", describeOps(ops))
	}
	var moveAt, upAt int = -1, -1
	for i, op := range ops {
		if op.Kind == OpMove {
			moveAt = i
		}
		if op.Kind == OpUpload && op.Path == "old/notes.txt" {
			upAt = i
		}
	}
	if upAt < moveAt {
		t.Fatalf("upload must come after the rename: %s", describeOps(ops))
	}

	// Apply the rename alone (no network here): the ledger must still see the
	// edit as unsynced, so the upload happens even if this run stops early.
	var moveOnly []Op
	for _, op := range ops {
		if op.Kind == OpMove {
			moveOnly = append(moveOnly, op)
		}
	}
	if _, err := e.Apply(context.Background(), moveOnly); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "old", "notes.txt"))
	if err != nil || string(got) != "B offline edit" {
		t.Fatalf("renamed file: %q %v", got, err)
	}
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	ops = e.Plan()
	if !hasOp(ops, OpUpload, "old/notes.txt") || hasOp(ops, OpDownload, "old/notes.txt") {
		t.Fatalf("after the rename the edit must still upload, got %s", describeOps(ops))
	}
	if hasOp(ops, OpUpload, "notes.txt") {
		t.Fatalf("old path must stay gone, got %s", describeOps(ops))
	}
}

// Bug A: B edited notes.txt offline, then A moved it. B's edit used to be
// renamed locally and recorded as in sync without ever being uploaded.
func TestOfflineEditBeforeRemoteMoveIsUploadedAtNewPath(t *testing.T) {
	movedAt := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	e, dir := setupMovedWhileOffline(t, movedAt.Add(-10*time.Minute), movedAt)
	checkEditFollowsMove(t, e, dir)
}

// Bug B: A moved notes.txt, then B (still offline) edited it. B's sync used
// to upload at the old path, which undid the move for every machine.
func TestOfflineEditAfterRemoteMoveFollowsTheMove(t *testing.T) {
	movedAt := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	e, dir := setupMovedWhileOffline(t, movedAt.Add(10*time.Minute), movedAt)
	checkEditFollowsMove(t, e, dir)
}

// When the moved file was also changed remotely, the rename happens but no
// blind upload is scheduled; the next run treats it as a two-sided edit.
func TestOfflineEditWithRemoteEditAfterMoveIsNotOverwritten(t *testing.T) {
	movedAt := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	e, dir := setupMovedWhileOffline(t, movedAt.Add(-10*time.Minute), movedAt)
	e.state.Remote["old/notes.txt"].SHA256 = sha("A edited after moving")
	ops := e.Plan()
	if hasOp(ops, OpUpload, "old/notes.txt") || hasOp(ops, OpUpload, "notes.txt") {
		t.Fatalf("must not overwrite a remote edit blindly, got %s", describeOps(ops))
	}
	if !hasMove(ops, "notes.txt", "old/notes.txt") {
		t.Fatalf("want rename, got %s", describeOps(ops))
	}
	if _, err := e.Apply(context.Background(), ops); err != nil {
		t.Fatal(err)
	}
	if err := e.ScanLocal(); err != nil {
		t.Fatal(err)
	}
	if !e.changedLocal("old/notes.txt", e.local["old/notes.txt"]) || !e.changedRemote("old/notes.txt") {
		t.Fatalf("both sides must count as changed so conflict handling applies")
	}
	_ = dir
}
