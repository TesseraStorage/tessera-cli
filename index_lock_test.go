package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestUpdateIndexConcurrentDoesNotDropWrites reproduces the exact race the
// lock in index.go ("exclusive lock around shared index updates") fixes:
// many goroutines each adding a different path to the index, started close
// enough together that a naive load-mutate-save (no lock) would have one
// save silently clobber another's addition. With the lock, updateIndex
// serializes the mutations so every one of them lands.
func TestUpdateIndexConcurrentDoesNotDropWrites(t *testing.T) {
	t.Setenv("TESSERA_HOME", t.TempDir())

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("/file-%02d", i)
			// Retry on lock contention: acquireIndexLock fails fast rather
			// than blocking (see TestAcquireIndexLockFailsFastWhenHeld), so
			// a caller that wants its write to land under real concurrency
			// must retry. This mirrors what a long-lived process (a sync
			// watcher) would do between its own periodic update attempts.
			var err error
			for attempt := 0; attempt < 200; attempt++ {
				err = updateIndex(func(idx *Index) error {
					idx.put(PathInfo{Path: path, Name: path, ObjectID: "obj-" + path, Size: 1})
					return nil
				})
				if err == nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d never acquired the lock: %v", i, err)
		}
	}

	idx, err := loadIndex()
	if err != nil {
		t.Fatalf("loadIndex: %v", err)
	}
	if len(idx.Paths) != n {
		t.Fatalf("expected %d surviving entries, got %d -- a concurrent write was dropped (the bug updateIndex fixes)", n, len(idx.Paths))
	}
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("/file-%02d", i)
		if _, ok := idx.Paths[path]; !ok {
			t.Errorf("entry %s is missing -- dropped by a concurrent save", path)
		}
	}
}

// TestAcquireIndexLockFailsFastWhenHeld confirms the lock is advisory and
// exclusive: a second holder gets the documented clear error immediately
// (no blocking/hanging), and releasing the first lets a retry succeed.
func TestAcquireIndexLockFailsFastWhenHeld(t *testing.T) {
	t.Setenv("TESSERA_HOME", t.TempDir())

	release, err := acquireIndexLock()
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	if _, err := acquireIndexLock(); err == nil {
		t.Fatal("expected second acquire to fail while the first lock is held")
	} else if !strings.Contains(err.Error(), "another tessera command is updating the local index") {
		t.Fatalf("unexpected error message: %v", err)
	}

	release()

	release2, err := acquireIndexLock()
	if err != nil {
		t.Fatalf("acquire after release should succeed: %v", err)
	}
	release2()
}

// TestAcquireIndexLockReclaimsStaleLock confirms a lock file left behind by
// a dead process (a PID that cannot be alive on this machine) is reclaimed
// rather than blocking forever.
func TestAcquireIndexLockReclaimsStaleLock(t *testing.T) {
	t.Setenv("TESSERA_HOME", t.TempDir())

	p, err := indexLockPath()
	if err != nil {
		t.Fatalf("indexLockPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stale := []byte(`{"pid":999999999,"started":"2020-01-01T00:00:00Z","host":"` + machineName() + `"}`)
	if err := os.WriteFile(p, stale, 0600); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}

	release, err := acquireIndexLock()
	if err != nil {
		t.Fatalf("expected the stale lock to be reclaimed, got: %v", err)
	}
	release()
}
