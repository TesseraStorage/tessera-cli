package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A stale-looking partial file that a live download still holds (locked)
// must survive removeStaleTemps; an unlocked one of the same age must go.
func TestRemoveStaleTempsSkipsLockedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock-based test")
	}
	dir := t.TempDir()
	old := time.Now().Add(-2 * staleTempAge)

	live, err := os.CreateTemp(dir, tempFilePrefix+"*")
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	lockTempFile(live)
	if err := os.Chtimes(live.Name(), old, old); err != nil {
		t.Fatal(err)
	}

	orphan := filepath.Join(dir, tempFilePrefix+"orphan")
	if err := os.WriteFile(orphan, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	if n := removeStaleTemps(dir); n != 1 {
		t.Fatalf("removed %d, want 1 (only the orphan)", n)
	}
	if _, err := os.Stat(live.Name()); err != nil {
		t.Fatalf("locked partial file was removed: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan not removed: %v", err)
	}
	// Once the writer closes (lock released), it is cleaned like any orphan.
	live.Close()
	if n := removeStaleTemps(dir); n != 1 {
		t.Fatalf("after close removed %d, want 1", n)
	}
}

func TestTempGoneErr(t *testing.T) {
	dir := t.TempDir()
	gone := filepath.Join(dir, tempFilePrefix+"gone")
	err := tempGoneErr(&os.PathError{Op: "rename", Path: gone, Err: os.ErrNotExist}, gone, "dst.bin")
	if err == nil || !strings.Contains(err.Error(), "removed by another process") || !strings.Contains(err.Error(), "dst.bin was not changed") {
		t.Fatalf("got %v", err)
	}
	other := errors.New("disk full")
	if tempGoneErr(other, gone, "dst.bin") != other {
		t.Fatal("other errors must pass through")
	}
	if tempGoneErr(nil, gone, "dst.bin") != nil {
		t.Fatal("nil must stay nil")
	}
}

func TestCopyFileAtomicVerifyRenameAfterTempRemoved(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(dst, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := copyFileAtomicVerify(dst, strings.NewReader("new"), 0644, func() error {
		// Simulate another process deleting the partial file.
		m, _ := filepath.Glob(filepath.Join(dir, tempFilePrefix+"*"))
		for _, f := range m {
			os.Remove(f)
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "Run the download again") {
		t.Fatalf("got %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "old" {
		t.Fatalf("dst changed: %q", b)
	}
}

func TestRemoveInstallerPathLines(t *testing.T) {
	dir := "/home/u/.local/bin"
	in := "alias ll='ls -l'\n\nexport PATH=\"/home/u/.local/bin:$PATH\" # added by the Tessera installer\n" +
		"export PATH=\"/home/u/.local/bin:$PATH\"\n" + // user's own line, no marker: kept
		"\nexport PATH=\"/opt/other:$PATH\" # added by the Tessera installer\n" // other dir: kept
	out, removed := removeInstallerPathLines([]byte(in), dir)
	want := "alias ll='ls -l'\nexport PATH=\"/home/u/.local/bin:$PATH\"\n\nexport PATH=\"/opt/other:$PATH\" # added by the Tessera installer\n"
	if string(out) != want {
		t.Fatalf("got\n%q\nwant\n%q", out, want)
	}
	if len(removed) != 1 {
		t.Fatalf("removed %v", removed)
	}
	fish := "set x 1\n\nfish_add_path \"/home/u/.local/bin\" # added by the Tessera installer\n"
	out, removed = removeInstallerPathLines([]byte(fish), dir)
	if string(out) != "set x 1\n" || len(removed) != 1 {
		t.Fatalf("fish: %q %v", out, removed)
	}
}

func TestPlanPathLineEditsFindsStartupFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("startup files are Unix only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZDOTDIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	bin := filepath.Join(home, ".local", "bin")
	os.MkdirAll(bin, 0755)
	exe := filepath.Join(bin, "tessera")
	os.WriteFile(exe, []byte("x"), 0755)
	line := "export PATH=\"" + bin + ":$PATH\" " + installerPathMarker + "\n"
	os.WriteFile(filepath.Join(home, ".bashrc"), []byte("# mine\n\n"+line), 0644)
	os.MkdirAll(filepath.Join(home, ".config", "fish"), 0755)
	os.WriteFile(filepath.Join(home, ".config", "fish", "config.fish"),
		[]byte("\nfish_add_path \""+bin+"\" "+installerPathMarker+"\n"), 0644)
	edits := planPathLineEdits(exe)
	if len(edits) != 2 {
		t.Fatalf("edits: %+v", edits)
	}
	for _, e := range edits {
		if strings.Contains(string(e.NewData), installerPathMarker) {
			t.Fatalf("%s still has the marker", e.File)
		}
	}
}

func TestDescribeTrashNoExpiry(t *testing.T) {
	for _, n := range []int{0, 14, 30} {
		if s := describeTrash(n); !strings.Contains(s, "nothing expires automatically") || strings.Contains(s, "days") {
			t.Errorf("describeTrash(%d) = %q", n, s)
		}
	}
	if s := describeTrash(-1); !strings.HasPrefix(s, "off") {
		t.Errorf("describeTrash(-1) = %q", s)
	}
}
