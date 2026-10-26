package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"go.sia.tech/core/types"
	siastorage "go.sia.tech/siastorage"
)

// livePackedSDK returns an SDK backed by a real indexd, or skips when the
// integration environment is not configured. The live test is what proves the
// packed path against the real backend; CI without credentials skips it.
func livePackedSDK(t *testing.T) *siastorage.SDK {
	t.Helper()
	indexer := os.Getenv("TESSERA_TEST_INDEXER_URL")
	appIDHex := os.Getenv("TESSERA_TEST_APP_ID")
	appKeyHex := os.Getenv("TESSERA_TEST_APP_KEY")
	if indexer == "" || appIDHex == "" || appKeyHex == "" {
		t.Skip("set TESSERA_TEST_INDEXER_URL, TESSERA_TEST_APP_ID and TESSERA_TEST_APP_KEY to run the live packed-upload test")
	}

	var appID types.Hash256
	if err := appID.UnmarshalText([]byte(appIDHex)); err != nil {
		t.Fatalf("TESSERA_TEST_APP_ID: %v", err)
	}
	keyBytes, err := hex.DecodeString(appKeyHex)
	if err != nil || len(keyBytes) != 64 {
		t.Fatalf("TESSERA_TEST_APP_KEY must be a 64-byte hex-encoded ed25519 private key")
	}
	builder := siastorage.NewBuilder(indexer, siastorage.AppMetadata{
		ID:          appID,
		Name:        "tessera-cli packed test",
		Description: "live packed-upload integration test",
		ServiceURL:  indexer,
	})
	sdk, err := builder.SDK(types.PrivateKey(keyBytes))
	if err != nil {
		t.Fatalf("connect SDK: %v", err)
	}
	t.Cleanup(func() { sdk.Close() })
	return sdk
}

func writePackedTestFile(t *testing.T, dir, name string, size int, seed byte) string {
	t.Helper()
	content := bytes.Repeat([]byte{seed}, size)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, content, 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func packedSlabSet(t *testing.T, sdk *siastorage.SDK, ids []string) map[string]struct{} {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	set := make(map[string]struct{})
	for _, idHex := range ids {
		var id types.Hash256
		if err := id.UnmarshalText([]byte(idHex)); err != nil {
			t.Fatalf("object id %q: %v", idHex, err)
		}
		obj, err := sdk.Object(ctx, id)
		if err != nil {
			t.Fatalf("query object %s: %v", idHex, err)
		}
		for _, slab := range obj.Slabs() {
			set[slab.Digest().String()] = struct{}{}
		}
	}
	return set
}

// TestPackedSyncIntegration is the Task 0.1 acceptance test: sync a folder of
// 50 x 10KB files and prove through the real indexd that they share one slab,
// not fifty. It requires a live demo account (see the environment variables in
// livePackedSDK) and is skipped in CI.
func TestPackedSyncIntegration(t *testing.T) {
	sdk := livePackedSDK(t)
	t.Setenv("TESSERA_HOME", t.TempDir())

	dir := t.TempDir()
	for i := 0; i < 50; i++ {
		writePackedTestFile(t, dir, fmt.Sprintf("small-%02d.bin", i), 10*1024, byte(i+1))
	}

	stamp := time.Now().UnixNano()
	cfg := &SyncConfig{
		ID:             fmt.Sprintf("packed-%d", stamp),
		LocalPath:      dir,
		RemotePrefix:   fmt.Sprintf("tessera/packed-test-%d", stamp),
		ConflictPolicy: conflictNewest,
	}
	ig, err := newIgnore(dir, nil)
	if err != nil {
		t.Fatalf("newIgnore: %v", err)
	}
	e := &SyncEngine{
		cfg:     cfg,
		state:   newSyncState(cfg.ID),
		rt:      NewRemote(sdk),
		ig:      ig,
		opts:    SyncOptions{ConflictMode: conflictNewest},
		machine: "packed-test",
	}
	if err := e.ScanLocal(); err != nil {
		t.Fatalf("ScanLocal: %v", err)
	}
	ops := e.Plan()
	if len(ops) != 50 {
		t.Fatalf("planned %d ops, want 50", len(ops))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := e.Apply(ctx, ops)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Uploaded != 50 {
		t.Fatalf("uploaded %d files, want 50; errors: %v", res.Uploaded, res.Errors)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("sync errors: %v", res.Errors)
	}

	var ids []string
	for _, ent := range e.state.Entries {
		if ent.ObjectID != "" {
			ids = append(ids, ent.ObjectID)
		}
	}
	if len(ids) != 50 {
		t.Fatalf("recorded %d object ids, want 50", len(ids))
	}
	slabs := packedSlabSet(t, sdk, ids)
	if len(slabs) != 1 {
		t.Fatalf("packed upload used %d slabs, want 1", len(slabs))
	}

	// Leave the demo account tidy for repeated runs.
	for _, idHex := range ids {
		var id types.Hash256
		_ = id.UnmarshalText([]byte(idHex))
		_ = sdk.DeleteObject(ctx, id)
	}
}

// TestPackedDirUploadIntegration is the directory-upload acceptance test: a
// whole directory (including a nested file) goes through uploadDir, and the
// real indexd shows the files share one slab. It requires a live demo account
// and is skipped in CI without credentials.
func TestPackedDirUploadIntegration(t *testing.T) {
	sdk := livePackedSDK(t)
	t.Setenv("TESSERA_HOME", t.TempDir())

	dir := t.TempDir()
	for i := 0; i < 20; i++ {
		writePackedTestFile(t, dir, fmt.Sprintf("dir-small-%02d.bin", i), 10*1024, byte(i+1))
	}
	sub := filepath.Join(dir, "nested")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	writePackedTestFile(t, sub, "nested.bin", 10*1024, 0x7f)

	idx, err := loadIndex()
	if err != nil {
		t.Fatalf("loadIndex: %v", err)
	}
	rt := NewRemote(sdk)
	stamp := time.Now().UnixNano()
	prefix := fmt.Sprintf("tessera/packed-dir-test-%d", stamp)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := uploadDir(ctx, rt, idx, dir, prefix); err != nil {
		t.Fatalf("uploadDir: %v", err)
	}

	var ids []string
	for p, ent := range idx.Paths {
		if ent.ObjectID == "" {
			t.Fatalf("index entry %s has no object id", p)
		}
		ids = append(ids, ent.ObjectID)
	}
	if len(ids) != 21 {
		t.Fatalf("index has %d objects, want 21", len(ids))
	}
	for _, p := range []string{
		path.Join(prefix, "dir-small-00.bin"),
		path.Join(prefix, "nested", "nested.bin"),
	} {
		if _, ok := idx.Paths[p]; !ok {
			t.Fatalf("missing index path %s", p)
		}
	}
	slabs := packedSlabSet(t, sdk, ids)
	if len(slabs) != 1 {
		t.Fatalf("directory upload used %d slabs, want 1", len(slabs))
	}

	for _, idHex := range ids {
		var id types.Hash256
		_ = id.UnmarshalText([]byte(idHex))
		_ = sdk.DeleteObject(ctx, id)
	}
}

// TestPackedUploadBatchPerFileFailure is the Task 0.1 failure-path test: one
// member of the batch errors while being read (a directory is opened as a
// file, so Add fails mid-stream), and the remaining files must still upload
// and pin successfully.
func TestPackedUploadBatchPerFileFailure(t *testing.T) {
	sdk := livePackedSDK(t)
	rt := NewRemote(sdk)

	dir := t.TempDir()
	goodA := writePackedTestFile(t, dir, "good-a.bin", 10*1024, 0x11)
	bad := filepath.Join(dir, "not-a-file")
	if err := os.Mkdir(bad, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	goodB := writePackedTestFile(t, dir, "good-b.bin", 10*1024, 0x22)

	now := time.Now()
	reqs := []uploadRequest{
		{LocalPath: goodA, RelPath: "test/good-a.bin", Name: "good-a.bin", Hash: "a", Size: 10 * 1024, Mode: 0644, ModTime: now, Root: "test"},
		{LocalPath: bad, RelPath: "test/not-a-file", Name: "not-a-file", Hash: "b", Size: 4096, Mode: 0755, ModTime: now, Root: "test"},
		{LocalPath: goodB, RelPath: "test/good-b.bin", Name: "good-b.bin", Hash: "c", Size: 10 * 1024, Mode: 0644, ModTime: now, Root: "test"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	outcomes := rt.UploadBatch(ctx, reqs, nil)
	if len(outcomes) != 3 {
		t.Fatalf("got %d outcomes, want 3", len(outcomes))
	}
	if outcomes[1].Err == nil {
		t.Fatal("the unreadable batch member must report its own error")
	}
	for _, i := range []int{0, 2} {
		if outcomes[i].Err != nil {
			t.Fatalf("file %d failed even though the batch should continue: %v", i, outcomes[i].Err)
		}
		if outcomes[i].Info.ObjectID == "" {
			t.Fatalf("file %d did not return an object id", i)
		}
	}

	slabs := packedSlabSet(t, sdk, []string{outcomes[0].Info.ObjectID, outcomes[2].Info.ObjectID})
	if len(slabs) != 1 {
		t.Fatalf("surviving files used %d slabs, want 1", len(slabs))
	}
	for _, i := range []int{0, 2} {
		var id types.Hash256
		_ = id.UnmarshalText([]byte(outcomes[i].Info.ObjectID))
		_ = sdk.DeleteObject(ctx, id)
	}
}

// TestPackedMixedBatchIntegration is the Task 0.4 CLI acceptance experiment:
// the report's exact 38MB + 1MB + 100KB mixed batch uploaded as one folder
// action must land in a single shared slab instead of three unpacked slabs.
// It also checks that each object kept its own metadata path, since packed
// objects are only addressable if the per-file descriptor survives Finalize.
func TestPackedMixedBatchIntegration(t *testing.T) {
	sdk := livePackedSDK(t)
	t.Setenv("TESSERA_HOME", t.TempDir())

	dir := t.TempDir()
	// Exact sizes from REPORT-network-vs-logical-storage-2026-10-25.md §3.
	writePackedTestFile(t, dir, "big.bin", 38*1024*1024, 0x11)
	writePackedTestFile(t, dir, "medium.bin", 1*1024*1024, 0x22)
	writePackedTestFile(t, dir, "small.bin", 100*1024, 0x33)

	idx, err := loadIndex()
	if err != nil {
		t.Fatalf("loadIndex: %v", err)
	}
	rt := NewRemote(sdk)
	stamp := time.Now().UnixNano()
	prefix := fmt.Sprintf("tessera/packed-mixed-test-%d", stamp)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := uploadDir(ctx, rt, idx, dir, prefix); err != nil {
		t.Fatalf("uploadDir: %v", err)
	}

	var ids []string
	byPath := make(map[string]string)
	for p, ent := range idx.Paths {
		if ent.ObjectID == "" {
			t.Fatalf("index entry %s has no object id", p)
		}
		ids = append(ids, ent.ObjectID)
		byPath[p] = ent.ObjectID
	}
	if len(ids) != 3 {
		t.Fatalf("index has %d objects, want 3", len(ids))
	}

	slabs := packedSlabSet(t, sdk, ids)
	if len(slabs) != 1 {
		t.Fatalf("mixed batch used %d slabs, want 1 (unpacked baseline is 3)", len(slabs))
	}

	// The per-file metadata must survive the packed Finalize/PinObject path:
	// each object's tessera.path must match the remote path it was uploaded
	// to. Otherwise a fresh `tessera list` would collapse every file onto one
	// name even though the bytes are correct.
	seenPaths := make(map[string]struct{})
	for remotePath, idHex := range byPath {
		var id types.Hash256
		if err := id.UnmarshalText([]byte(idHex)); err != nil {
			t.Fatalf("object id %q: %v", idHex, err)
		}
		obj, err := sdk.Object(ctx, id)
		if err != nil {
			t.Fatalf("query object %s: %v", remotePath, err)
		}
		var env metaEnvelope
		if err := json.Unmarshal(obj.Metadata(), &env); err != nil {
			t.Fatalf("object %s metadata: %v", remotePath, err)
		}
		if env.TS == nil {
			t.Fatalf("object %s has no tessera metadata", remotePath)
		}
		if got := string(env.TS.Path); got != remotePath {
			t.Fatalf("object metadata path = %q, want %q", got, remotePath)
		}
		seenPaths[env.TS.Path] = struct{}{}
	}
	if len(seenPaths) != 3 {
		t.Fatalf("metadata collapsed to %d distinct paths, want 3", len(seenPaths))
	}

	for _, idHex := range ids {
		var id types.Hash256
		_ = id.UnmarshalText([]byte(idHex))
		_ = sdk.DeleteObject(ctx, id)
	}
}
