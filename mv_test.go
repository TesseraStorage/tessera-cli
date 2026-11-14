package main

import (
	"strings"
	"testing"
)

func testIndex(paths ...string) *Index {
	idx := &Index{Schema: indexSchema, Paths: map[string]*IndexEntry{}}
	for _, p := range paths {
		idx.Paths[p] = &IndexEntry{Path: p, Name: baseName(p), ObjectID: "obj-" + p, SHA256: "sha-" + p, Size: 10}
	}
	return idx
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func TestResolveMvFileToRootAndBack(t *testing.T) {
	idx := testIndex("tessera/test/textfile.txt")
	moves, err := resolveMvTargets(idx, "tessera/test/textfile.txt", "textfile.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(moves) != 1 || moves[0].src != "tessera/test/textfile.txt" || moves[0].dst != "textfile.txt" {
		t.Fatalf("got %#v", moves)
	}

	idx = testIndex("textfile.txt")
	moves, err = resolveMvTargets(idx, "textfile.txt", "tessera/test/")
	if err != nil {
		t.Fatal(err)
	}
	if len(moves) != 1 || moves[0].dst != "tessera/test/textfile.txt" {
		t.Fatalf("into folder: %#v", moves)
	}
}

func TestResolveMvIntoExistingFolderWithoutSlash(t *testing.T) {
	idx := testIndex("notes/a.txt", "inbox/hello.txt")
	moves, err := resolveMvTargets(idx, "inbox/hello.txt", "notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(moves) != 1 || moves[0].dst != "notes/hello.txt" {
		t.Fatalf("existing folder dest: %#v", moves)
	}
}

func TestResolveMvFolderPrefix(t *testing.T) {
	idx := testIndex("photos/2024/a.jpg", "photos/2024/b.jpg", "other.txt")
	moves, err := resolveMvTargets(idx, "photos/", "archive/")
	if err != nil {
		t.Fatal(err)
	}
	if len(moves) != 2 {
		t.Fatalf("folder move count = %d, want 2: %#v", len(moves), moves)
	}
	got := map[string]string{}
	for _, m := range moves {
		got[m.src] = m.dst
	}
	if got["photos/2024/a.jpg"] != "archive/photos/2024/a.jpg" || got["photos/2024/b.jpg"] != "archive/photos/2024/b.jpg" {
		t.Fatalf("folder mapping: %#v", got)
	}
}

func TestResolveMvRenameFolder(t *testing.T) {
	idx := testIndex("old/a.txt", "old/b.txt")
	moves, err := resolveMvTargets(idx, "old", "new")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range moves {
		got[m.src] = m.dst
	}
	if got["old/a.txt"] != "new/a.txt" || got["old/b.txt"] != "new/b.txt" {
		t.Fatalf("rename folder: %#v", got)
	}
}

func TestResolveMvRefusesSelf(t *testing.T) {
	idx := testIndex("photos/a.jpg")
	if _, err := resolveMvTargets(idx, "photos", "photos/nested"); err == nil {
		t.Fatal("expected error moving a folder into itself")
	}
}

func TestResolveMvMissing(t *testing.T) {
	idx := testIndex("a.txt")
	if _, err := resolveMvTargets(idx, "missing.txt", "dest.txt"); err == nil {
		t.Fatal("expected not-found error")
	}
}

// Re-running a folder move that was interrupted must continue into the same
// destination folder, not nest the rest one level deeper.
func TestResolveMvResumesInterruptedFolderMove(t *testing.T) {
	t.Setenv("TESSERA_HOME", t.TempDir())
	idx := testIndex("archive/keep.txt", "photos/a.jpg", "photos/b.jpg", "photos/c.jpg")
	first, err := resolveMv(idx, "photos", "archive/photos", pendingMvPrefix("acct", "photos", "archive/photos"))
	if err != nil || first.dstPrefix != "archive/photos" || !first.isDir {
		t.Fatalf("first run: %#v %v", first, err)
	}
	savePendingMv("acct", "photos", "archive/photos", first.dstPrefix)

	// Interrupted after a.jpg moved.
	idx = testIndex("archive/keep.txt", "archive/photos/a.jpg", "photos/b.jpg", "photos/c.jpg")
	before, _ := resolveMvTargets(idx, "photos", "archive/photos")
	if len(before) == 0 || before[0].dst != "archive/photos/photos/b.jpg" {
		t.Fatalf("expected the old nesting without the resume record: %#v", before)
	}
	again, err := resolveMv(idx, "photos", "archive/photos/", pendingMvPrefix("acct", "photos", "archive/photos/"))
	if err != nil {
		t.Fatal(err)
	}
	if len(again.moves) != 2 || again.moves[0].dst != "archive/photos/b.jpg" || again.moves[1].dst != "archive/photos/c.jpg" {
		t.Fatalf("re-run must land in archive/photos/: %#v", again.moves)
	}
	if pendingMvPrefix("other-account", "photos", "archive/photos") != "" {
		t.Fatal("resume record must be per account")
	}
	clearPendingMv("acct", "photos", "archive/photos")
	if pendingMvPrefix("acct", "photos", "archive/photos") != "" {
		t.Fatal("finished move must clear the resume record")
	}
	// Without a record, an existing folder destination still nests (mv semantics).
	idx = testIndex("archive/keep.txt", "photos/a.jpg")
	res, _ := resolveMv(idx, "photos", "archive", "")
	if res.moves[0].dst != "archive/photos/a.jpg" {
		t.Fatalf("into existing folder: %#v", res.moves)
	}
}
