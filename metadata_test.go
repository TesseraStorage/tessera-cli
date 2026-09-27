package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeRelPath(t *testing.T) {
	good := map[string]string{
		"a.txt":         "a.txt",
		"./a.txt":       "a.txt",
		"dir/a.txt":     "dir/a.txt",
		"dir//a.txt":    "dir/a.txt",
		`dir\a.txt`:     "dir/a.txt",
		"dir/sub/a.txt": "dir/sub/a.txt",
		"a b/c d.txt":   "a b/c d.txt",
		"ünïcode/ß.txt": "ünïcode/ß.txt",
	}
	for in, want := range good {
		got, err := NormalizeRelPath(in)
		if err != nil {
			t.Errorf("NormalizeRelPath(%q) returned error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeRelPath(%q) = %q, want %q", in, got, want)
		}
	}

	bad := []string{"", ".", "/", "/etc/passwd", "..", "../secret", "dir/../../secret"}
	for _, in := range bad {
		if got, err := NormalizeRelPath(in); err == nil {
			t.Errorf("NormalizeRelPath(%q) = %q, want an error", in, got)
		}
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	m := &tsMeta{
		Root:    "abcd1234",
		Path:    "notes/2026/report.pdf",
		SHA256:  "deadbeef",
		Size:    18234,
		Mode:    "0644",
		ModTime: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC),
		Machine: "macbook",
	}
	raw, err := marshalMetadata("report.pdf", m)
	if err != nil {
		t.Fatalf("marshalMetadata: %v", err)
	}
	if len(raw) > 1024 {
		t.Fatalf("metadata is %d bytes, over the 1024 byte indexer limit", len(raw))
	}

	env, err := decodeMetadata(raw)
	if err != nil {
		t.Fatalf("decodeMetadata: %v", err)
	}
	if env.Name != "report.pdf" {
		t.Errorf("Name = %q, want report.pdf", env.Name)
	}
	if env.TS == nil {
		t.Fatal("TS is nil after round-trip")
	}
	if env.TS.Path != m.Path || env.TS.SHA256 != m.SHA256 || env.TS.Root != m.Root {
		t.Errorf("round-trip mismatch: got %+v want %+v", env.TS, m)
	}
	if env.logicalPath() != m.Path {
		t.Errorf("logicalPath = %q, want %q", env.logicalPath(), m.Path)
	}
}

// TestMetadataLegacy covers objects uploaded by the pre-sync CLI, whose
// metadata was a bare {"name": "..."} map.
func TestMetadataLegacy(t *testing.T) {
	raw := json.RawMessage(`{"name":"photo.png"}`)
	env, err := decodeMetadata(raw)
	if err != nil {
		t.Fatalf("decodeMetadata(legacy): %v", err)
	}
	if env.Name != "photo.png" {
		t.Errorf("Name = %q, want photo.png", env.Name)
	}
	if env.TS != nil {
		t.Errorf("TS = %+v, want nil for a legacy object", env.TS)
	}
	if env.logicalPath() != "photo.png" {
		t.Errorf("logicalPath = %q, want the legacy name", env.logicalPath())
	}
}

func TestNormalizeRemotePrefix(t *testing.T) {
	cases := map[string]string{
		"tessera/macos/Documents":  "tessera/macos/Documents",
		"/tessera/macos/Documents": "tessera/macos/Documents",
		"tessera//x/":              "tessera/x",
		`tessera\windows\Docs`:     "tessera/windows/Docs",
	}
	for in, want := range cases {
		got, err := normalizeRemotePrefix(in)
		if err != nil {
			t.Errorf("normalizeRemotePrefix(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("normalizeRemotePrefix(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := normalizeRemotePrefix("  "); err == nil {
		t.Error("an empty prefix should be rejected")
	}
}

func TestConflictName(t *testing.T) {
	at := time.Date(2026, 9, 25, 14, 30, 5, 0, time.UTC)
	got := conflictName("notes/report.pdf", "WINDOWS-PC", at)
	want := "notes/report (conflicted copy, WINDOWS-PC 2026-09-25-143005).pdf"
	if got != want {
		t.Errorf("conflictName = %q, want %q", got, want)
	}

	// A file with no extension keeps its name and gains a suffix.
	got = conflictName("Makefile", "", at)
	want = "Makefile (conflicted copy, other 2026-09-25-143005)"
	if got != want {
		t.Errorf("conflictName = %q, want %q", got, want)
	}
}

func TestIgnoreRules(t *testing.T) {
	dir := t.TempDir()
	ig, err := newIgnore(dir, []string{"*.log", "build/", "!keep.log"})
	if err != nil {
		t.Fatalf("newIgnore: %v", err)
	}

	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"notes.txt", false, false},
		{"debug.log", false, true},
		{"sub/debug.log", false, true},
		{"keep.log", false, false},
		{"build", true, true},
		{"build/output.bin", false, true},
		{".DS_Store", false, true},
		{"sub/.DS_Store", false, true},
		{"Thumbs.db", false, true},
		{"desktop.ini", false, true},
		{"file.txt~", false, true},
		{".git", true, true},
		{".git/config", false, true},
		{"temporary.swp", false, true},
	}
	for _, c := range cases {
		if got := ig.Match(c.path, c.isDir); got != c.want {
			t.Errorf("Match(%q, isDir=%v) = %v, want %v", c.path, c.isDir, got, c.want)
		}
	}
}

// TestIgnoreFile verifies that a .tesseraignore at the root is honoured.
func TestIgnoreFile(t *testing.T) {
	dir := t.TempDir()
	if err := writeFileAtomic(filepath.Join(dir, ".tesseraignore"), []byte("# comment\n*.tmp\nsecrets/\n"), 0644); err != nil {
		t.Fatalf("write ignore file: %v", err)
	}
	ig, err := newIgnore(dir, nil)
	if err != nil {
		t.Fatalf("newIgnore: %v", err)
	}
	if !ig.Match("scratch.tmp", false) {
		t.Error(".tesseraignore pattern *.tmp was not applied")
	}
	if !ig.Match("secrets", true) {
		t.Error(".tesseraignore directory pattern was not applied")
	}
	if ig.Match("notes.txt", false) {
		t.Error("notes.txt should not be ignored")
	}
}

func TestParseByteSize(t *testing.T) {
	cases := map[string]int64{
		"1024":  1024,
		"1KB":   1 << 10,
		"1k":    1 << 10,
		"20MB":  20 << 20,
		"1.5GB": int64(1.5 * float64(1<<30)),
		"2TB":   2 << 40,
		"512B":  512,
	}
	for in, want := range cases {
		got, err := parseByteSize(in)
		if err != nil {
			t.Errorf("parseByteSize(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseByteSize(%q) = %d, want %d", in, got, want)
		}
	}
	if _, err := parseByteSize("abc"); err == nil {
		t.Error("parseByteSize(\"abc\") should fail")
	}
}

func TestFlagParsing(t *testing.T) {
	args := []string{"add", "/tmp/docs", "--as", "tessera/macos/Documents", "--json"}
	pos := positional(args)
	want := []string{"add", "/tmp/docs"}
	if len(pos) != len(want) {
		t.Fatalf("positional = %v, want %v", pos, want)
	}
	for i := range want {
		if pos[i] != want[i] {
			t.Fatalf("positional = %v, want %v", pos, want)
		}
	}
	if v, ok := flagValue(args, "--as"); !ok || v != "tessera/macos/Documents" {
		t.Errorf("flagValue(--as) = %q, %v", v, ok)
	}
	if !hasFlag(args, "--json") {
		t.Error("hasFlag(--json) = false, want true")
	}

	// --name=value form
	if v, ok := flagValue([]string{"--interval=15s"}, "--interval"); !ok || v != "15s" {
		t.Errorf("flagValue(--interval=15s) = %q, %v", v, ok)
	}
}

func TestSameMTime(t *testing.T) {
	a := time.Date(2026, 9, 25, 10, 0, 0, 400_000_000, time.UTC)
	b := time.Date(2026, 9, 25, 10, 0, 0, 900_000_000, time.UTC)
	if !sameMTime(a, b) {
		t.Error("sub-second differences must compare equal (indexer has second granularity)")
	}
	c := a.Add(2 * time.Second)
	if sameMTime(a, c) {
		t.Error("a two second difference must compare unequal")
	}
}
