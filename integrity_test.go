package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sumHex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// tempLeftovers lists .tessera-tmp-* files left in dir.
func tempLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, ".tessera-tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestHashStream(t *testing.T) {
	good := "hello tessera"
	n, got, err := hashStream(strings.NewReader(good), "a.txt", sumHex(good))
	if err != nil || n != int64(len(good)) || got != sumHex(good) {
		t.Fatalf("match: n=%d got=%s err=%v", n, got, err)
	}
	_, _, err = hashStream(strings.NewReader("corrupted"), "a.txt", sumHex(good))
	var ie *ErrIntegrity
	if !errors.As(err, &ie) || ie.Expected != sumHex(good) || ie.Got != sumHex("corrupted") {
		t.Fatalf("mismatch: err=%v", err)
	}
	if _, got, err := hashStream(strings.NewReader(good), "a.txt", ""); err != nil || got != sumHex(good) {
		t.Fatalf("no hash: got=%s err=%v", got, err)
	}
}

// fakeVerifier serves object bytes from memory, like the SDK reader would.
type fakeVerifier struct {
	data map[string]string
	fail map[string]error
}

func (f fakeVerifier) VerifyHash(_ context.Context, info PathInfo) (int64, error) {
	if err := f.fail[info.Path]; err != nil {
		return 0, err
	}
	n, _, err := hashStream(strings.NewReader(f.data[info.Path]), info.Path, info.SHA256)
	return n, err
}

func TestAuditVerifyCounts(t *testing.T) {
	fv := fakeVerifier{
		data: map[string]string{"ok": "good bytes", "bad": "garbled", "nohash": "anything"},
		fail: map[string]error{"gone": errors.New("not enough hosts")},
	}
	var rep AuditReport
	for _, info := range []PathInfo{
		{Path: "ok", SHA256: sumHex("good bytes")},
		{Path: "bad", SHA256: sumHex("good bytes")},
		{Path: "nohash"},
		{Path: "gone", SHA256: sumHex("x")},
	} {
		auditVerify(context.Background(), fv, info, &rep)
	}
	if rep.Verified != 1 || rep.Mismatches != 1 || rep.Unverifiable != 1 || len(rep.AtRisk) != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if !strings.HasPrefix(rep.AtRisk[0].Reason, "hash mismatch: expected ") || rep.AtRisk[0].Path != "bad" {
		t.Fatalf("mismatch finding = %+v", rep.AtRisk[0])
	}
	if !strings.HasPrefix(rep.AtRisk[1].Reason, "download failed: ") {
		t.Fatalf("failure finding = %+v", rep.AtRisk[1])
	}
}

func TestWriteVerifiedMatchReplaces(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(dest, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	n, got, err := writeVerified(dest, strings.NewReader("new content"), 0644, "f.txt", sumHex("new content"))
	if err != nil || n != 11 || got != sumHex("new content") {
		t.Fatalf("n=%d got=%s err=%v", n, got, err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "new content" {
		t.Fatalf("dest = %q", b)
	}
	if l := tempLeftovers(t, dir); len(l) != 0 {
		t.Fatalf("temp files left: %v", l)
	}
}

func TestWriteVerifiedMismatchKeepsExistingTarget(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(dest, []byte("known good"), 0644); err != nil {
		t.Fatal(err)
	}
	_, _, err := writeVerified(dest, strings.NewReader("corrupted"), 0644, "docs/f.txt", sumHex("known good"))
	var ie *ErrIntegrity
	if !errors.As(err, &ie) || ie.Path != "docs/f.txt" {
		t.Fatalf("err = %v, want ErrIntegrity", err)
	}
	if !strings.HasPrefix(err.Error(), "integrity check failed for docs/f.txt: expected ") {
		t.Fatalf("message = %q", err.Error())
	}
	if b, _ := os.ReadFile(dest); string(b) != "known good" {
		t.Fatalf("existing target changed to %q", b)
	}
	if l := tempLeftovers(t, dir); len(l) != 0 {
		t.Fatalf("temp files left: %v", l)
	}
}

func TestWriteVerifiedMismatchCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "new.txt")
	if _, _, err := writeVerified(dest, strings.NewReader("corrupted"), 0644, "new.txt", sumHex("good")); err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("dest exists after mismatch: %v", err)
	}
	if l := tempLeftovers(t, dir); len(l) != 0 {
		t.Fatalf("temp files left: %v", l)
	}
}

func TestWriteVerifiedNoStoredHash(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "f.txt")
	_, got, err := writeVerified(dest, strings.NewReader("anything"), 0644, "f.txt", "")
	if err != nil || got != sumHex("anything") {
		t.Fatalf("got=%s err=%v", got, err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "anything" {
		t.Fatalf("dest = %q", b)
	}
}

func TestExpectedHashRangeUnchecked(t *testing.T) {
	info := PathInfo{SHA256: sumHex("whole file")}
	if got := expectedHash(info, byteRange{}); got != info.SHA256 {
		t.Fatalf("full download: %q", got)
	}
	if got := expectedHash(info, byteRange{Set: true, Offset: 5, Length: 3}); got != "" {
		t.Fatalf("range download must not be checked, got %q", got)
	}
	// A range write with no expected hash succeeds whatever the bytes are.
	dest := filepath.Join(t.TempDir(), "part")
	if _, _, err := writeVerified(dest, strings.NewReader("abc"), 0644, "f", expectedHash(info, byteRange{Set: true, Length: 3})); err != nil {
		t.Fatal(err)
	}
}

func TestCopyFileAtomicRefusesDirectory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sub")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := copyFileAtomic(target, strings.NewReader("x"), 0644); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v", err)
	}
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		t.Fatalf("directory changed: %v", err)
	}
	if l := tempLeftovers(t, dir); len(l) != 0 {
		t.Fatalf("temp files left: %v", l)
	}
}
