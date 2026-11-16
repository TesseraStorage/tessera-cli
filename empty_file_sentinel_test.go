package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestUploadBodyWithEmptyFileSentinel is the core substitution the packet
// asks for: a zero-length input becomes a one-byte reader containing the
// ASCII character "0" (0x30); any non-empty input passes through unchanged.
func TestUploadBodyWithEmptyFileSentinel(t *testing.T) {
	t.Run("zero-byte input becomes the one-byte sentinel", func(t *testing.T) {
		body := uploadBodyWithEmptyFileSentinel(strings.NewReader(""), 0)
		got, err := io.ReadAll(body)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d bytes, want exactly 1", len(got))
		}
		if got[0] != '0' {
			t.Fatalf("got byte %q (0x%02x), want ASCII \"0\" (0x30)", got, got[0])
		}
	})

	t.Run("non-empty input is unchanged", func(t *testing.T) {
		const content = "hello tessera"
		body := uploadBodyWithEmptyFileSentinel(strings.NewReader(content), int64(len(content)))
		got, err := io.ReadAll(body)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != content {
			t.Fatalf("got %q, want %q (non-empty input must pass through unchanged)", got, content)
		}
	})

	t.Run("a non-empty file that happens to start with the sentinel byte is unchanged", func(t *testing.T) {
		const content = "0 is not special when the file really has content"
		body := uploadBodyWithEmptyFileSentinel(strings.NewReader(content), int64(len(content)))
		got, err := io.ReadAll(body)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != content {
			t.Fatalf("got %q, want %q", got, content)
		}
	})
}

// TestHashForUploadMatchesSentinel is the integrity-round-trip half of the
// fix: a zero-byte file's recorded hash must be the sentinel's hash (what
// indexd actually stores), not the real empty file's hash -- otherwise a
// later `tessera download` of the same file always fails its own
// SHA-256 check (see writeVerified/expectedHash in remote.go).
func TestHashForUploadMatchesSentinel(t *testing.T) {
	dir := t.TempDir()
	emptyPath := filepath.Join(dir, "empty.bin")
	if err := os.WriteFile(emptyPath, nil, 0644); err != nil {
		t.Fatalf("write empty file: %v", err)
	}

	hash, err := hashForUpload(emptyPath, 0)
	if err != nil {
		t.Fatalf("hashForUpload: %v", err)
	}
	if hash != emptyFileSentinelHash {
		t.Fatalf("hashForUpload(empty) = %q, want emptyFileSentinelHash %q", hash, emptyFileSentinelHash)
	}

	// The hash must equal SHA-256 of the literal byte "0": write a real
	// one-byte file containing it and confirm hashForUpload agrees,
	// independent of how emptyFileSentinelHash itself is computed, so a
	// regression that changes the sentinel constant without updating its
	// hash is caught.
	sentinelPath := filepath.Join(dir, "sentinel.bin")
	if err := os.WriteFile(sentinelPath, []byte(emptyFileSentinel), 0644); err != nil {
		t.Fatalf("write sentinel file: %v", err)
	}
	directHash, err := hashForUpload(sentinelPath, 1)
	if err != nil {
		t.Fatalf("hashForUpload(sentinel): %v", err)
	}
	if directHash != hash {
		t.Fatalf("SHA-256 of a real one-byte %q file (%q) does not match emptyFileSentinelHash (%q) -- download integrity would break", emptyFileSentinel, directHash, hash)
	}

	// A non-empty file's recorded hash is untouched -- the real content hash.
	nonEmptyPath := filepath.Join(dir, "content.bin")
	if err := os.WriteFile(nonEmptyPath, []byte("real content"), 0644); err != nil {
		t.Fatalf("write content file: %v", err)
	}
	gotHash, err := hashForUpload(nonEmptyPath, int64(len("real content")))
	if err != nil {
		t.Fatalf("hashForUpload(nonEmpty): %v", err)
	}
	wantContentHash, err := hashFile(nonEmptyPath)
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	if gotHash != wantContentHash {
		t.Fatalf("hashForUpload(nonEmpty) = %q, want the real content hash %q", gotHash, wantContentHash)
	}
}

// TestEmptyFileSentinelLiveIntegration is the packet's third required test:
// a zero-byte file pins successfully against a live indexd that still
// enforces ErrObjectMinimumSlabs, and the stored object is exactly the
// one-byte sentinel "0" -- downloadable with no integrity-check failure.
// It requires a live demo account (see livePackedSDK) and is skipped in CI,
// same convention as TestPackedSyncIntegration in packed_integration_test.go.
func TestEmptyFileSentinelLiveIntegration(t *testing.T) {
	sdk := livePackedSDK(t)
	rt := NewRemote(sdk)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dir := t.TempDir()
	emptyPath := filepath.Join(dir, "empty.bin")
	if err := os.WriteFile(emptyPath, nil, 0644); err != nil {
		t.Fatalf("write empty file: %v", err)
	}

	req, _, err := prepareUpload(ctx, rt, nil, emptyPath, fmt.Sprintf("tessera/empty-sentinel-test-%d.bin", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("prepareUpload: %v", err)
	}
	if req.Size != 0 {
		t.Fatalf("req.Size = %d, want 0 (the true local file size)", req.Size)
	}

	info, err := rt.Upload(ctx, req, nil)
	if err != nil {
		t.Fatalf("Upload of a zero-byte file must succeed against live indexd (ErrObjectMinimumSlabs must never reach the caller): %v", err)
	}
	t.Cleanup(func() {
		_ = rt.DeleteObject(context.Background(), info.ObjectID)
	})

	destPath := filepath.Join(dir, "downloaded.bin")
	if _, err := rt.Download(ctx, info, destPath, 0644); err != nil {
		t.Fatalf("Download: %v (an integrity-check failure here means the recorded hash does not match the stored sentinel)", err)
	}
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if len(got) != 1 || got[0] != '0' {
		t.Fatalf("downloaded content = %q, want exactly one byte \"0\" (0x30)", got)
	}
}
