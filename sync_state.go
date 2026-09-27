package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// SyncConfig is persisted per sync root.
type SyncConfig struct {
	ID           string `json:"id"`
	LocalPath    string `json:"local_path"`
	RemotePrefix string `json:"remote_prefix"`
	// ConflictPolicy is "newest" (default) or "keep-both".
	ConflictPolicy string `json:"conflict_policy,omitempty"`
	// Ignore holds extra ignore patterns on top of .tesseraignore.
	Ignore []string `json:"ignore,omitempty"`
	// CreatedAt / LastSync are informational.
	CreatedAt time.Time `json:"created_at"`
	LastSync  time.Time `json:"last_sync,omitempty"`
}

// EntryState tracks one path's last-synced identity on this machine.
type EntryState struct {
	// SHA256 of the content this machine last agreed on with the remote.
	BaseSHA256 string `json:"base_sha256,omitempty"`
	// ObjectID is the remote object that currently holds this path.
	ObjectID string `json:"object_id,omitempty"`
	// RemotePath is the remote-absolute path this entry maps to, recorded so
	// other subsystems can resolve it without recomputing the prefix.
	RemotePath string `json:"remote_path,omitempty"`
	// Size and ModTime are used to skip re-hashing unchanged files.
	Size    int64     `json:"size,omitempty"`
	ModTime time.Time `json:"mod_time,omitempty"`
	// DeletedLocal marks a local deletion that still needs to be pushed.
	DeletedLocal bool `json:"deleted_local,omitempty"`
	// DeletedRemote marks a remote deletion that still needs to be applied.
	DeletedRemote bool `json:"deleted_remote,omitempty"`
	// Untracked marks a local file that was deliberately overwritten during a
	// first-run adopt, so it is not re-reported as a local change.
	Untracked bool `json:"untracked,omitempty"`
	// TombstonedAt records when a path was deleted on both sides. Such entries
	// are kept (they stop a moved file from reappearing at its old name) but
	// are pruned once they are old enough to be irrelevant.
	TombstonedAt time.Time `json:"tombstoned_at,omitempty"`
	// Versions lists retained previous copies, oldest first.
	Versions []VersionRef `json:"versions,omitempty"`
}

// RemoteEntry is the materialised remote view for one path.
type RemoteEntry struct {
	ObjectID string    `json:"object_id,omitempty"`
	SHA256   string    `json:"sha256,omitempty"`
	Size     int64     `json:"size,omitempty"`
	ModTime  time.Time `json:"mod_time,omitempty"`
	Machine  string    `json:"machine,omitempty"`
	Updated  time.Time `json:"updated,omitempty"`
	// Deleted is true when the newest event for this path was a deletion.
	Deleted bool `json:"deleted,omitempty"`
	// Legacy marks objects uploaded before sync metadata existed.
	Legacy bool `json:"legacy,omitempty"`
}

// Cursor is a pagination high-water mark. Indexer timestamps have second
// granularity, so the object key is required to make progress deterministic.
type Cursor struct {
	After time.Time `json:"after"`
	Key   string    `json:"key"`
}

// IsZero reports whether the cursor has never advanced.
func (c Cursor) IsZero() bool { return c.After.IsZero() && c.Key == "" }

// SyncState is the on-disk state for one sync root.
type SyncState struct {
	Schema  int                    `json:"schema"`
	RootID  string                 `json:"root_id"`
	Cursor  Cursor                 `json:"cursor"`
	Entries map[string]*EntryState `json:"entries"`
	// Remote is the cached remote view, refreshed incrementally via Cursor.
	Remote map[string]*RemoteEntry `json:"remote,omitempty"`
	// Adopt records how the first sync resolved a pre-existing folder:
	// "merge" (default), "local" or "remote".
	Adopt string `json:"adopt,omitempty"`
	// Conflicts maps an original path to the conflict copy that preserves the
	// losing side. It is a dedicated ledger rather than a field on EntryState
	// because it must be persisted even when a sync decides to do nothing —
	// the machine that first notices the conflict, and the machine that only
	// receives the conflict copy, both need to report it.
	Conflicts map[string]string `json:"conflicts,omitempty"`
}

// SchemaVersion is the current state file schema.
const SchemaVersion = 1

// SyncDirName is where per-root state lives under ~/.tessera.
const SyncDirName = "sync"

func newSyncState(rootID string) *SyncState {
	return &SyncState{
		Schema:    SchemaVersion,
		RootID:    rootID,
		Entries:   make(map[string]*EntryState),
		Remote:    make(map[string]*RemoteEntry),
		Conflicts: make(map[string]string),
	}
}

// syncRootDir returns ~/.tessera/sync/<rootID>.
func syncRootDir(rootID string) (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, SyncDirName, rootID), nil
}

func stateFilePath(rootID string) (string, error) {
	dir, err := syncRootDir(rootID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "state.json"), nil
}

func lockFilePath(rootID string) (string, error) {
	dir, err := syncRootDir(rootID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "lock"), nil
}

// loadSyncState reads a root's state, returning a fresh one when absent.
func loadSyncState(rootID string) (*SyncState, error) {
	p, err := stateFilePath(rootID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return newSyncState(rootID), nil
		}
		return nil, err
	}
	st := newSyncState(rootID)
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("corrupt sync state at %s: %w", p, err)
	}
	if st.Schema != SchemaVersion {
		return nil, fmt.Errorf("sync state %s has schema %d, this build understands %d", p, st.Schema, SchemaVersion)
	}
	if st.Entries == nil {
		st.Entries = make(map[string]*EntryState)
	}
	if st.Remote == nil {
		st.Remote = make(map[string]*RemoteEntry)
	}
	if st.Conflicts == nil {
		st.Conflicts = make(map[string]string)
	}
	if st.RootID == "" {
		st.RootID = rootID
	}
	return st, nil
}

// save writes the state atomically. A crash can therefore never lose the
// previous consistent view.
func (s *SyncState) save() error {
	p, err := stateFilePath(s.RootID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	s.Schema = SchemaVersion
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeFileAtomic(p, data, 0600)
}

func (s *SyncState) entry(p string) *EntryState {
	if s.Entries == nil {
		s.Entries = make(map[string]*EntryState)
	}
	e, ok := s.Entries[p]
	if !ok {
		e = &EntryState{}
		s.Entries[p] = e
	}
	return e
}

func (s *SyncState) removeEntry(p string) { delete(s.Entries, p) }

// TombstoneRetention is how long a fully-deleted path is remembered. It only
// has to outlive the window in which a peer might still be moving the file.
const TombstoneRetention = 30 * 24 * time.Hour

// pruneTombstones drops deletion records that are old enough to be irrelevant,
// keeping the state file from growing without bound.
func (s *SyncState) pruneTombstones(now time.Time) int {
	cutoff := now.Add(-TombstoneRetention)
	var pruned int
	for p, e := range s.Entries {
		if e.DeletedLocal && e.DeletedRemote && !e.TombstonedAt.IsZero() && e.TombstonedAt.Before(cutoff) {
			delete(s.Entries, p)
			pruned++
		}
	}
	return pruned
}

func (s *SyncState) remote(p string) *RemoteEntry {
	if s.Remote == nil {
		s.Remote = make(map[string]*RemoteEntry)
	}
	return s.Remote[p]
}

// noteConflict records that originalPath has a conflict copy at copyPath.
// Recording is idempotent and survives a no-op sync.
func (s *SyncState) noteConflict(originalPath, copyPath string) {
	if originalPath == "" || copyPath == "" {
		return
	}
	if s.Conflicts == nil {
		s.Conflicts = make(map[string]string)
	}
	s.Conflicts[originalPath] = copyPath
}

// resolveConflict forgets a conflict record for an original path.
func (s *SyncState) resolveConflict(originalPath string) {
	delete(s.Conflicts, originalPath)
}

// resolveConflictCopy forgets every record pointing at a deleted conflict copy.
func (s *SyncState) resolveConflictCopy(copyPath string) {
	for original, copy := range s.Conflicts {
		if copy == copyPath {
			delete(s.Conflicts, original)
		}
	}
}

// applyRemoteEvent folds one event into the cached remote view.
func (s *SyncState) applyRemoteEvent(p string, info PathInfo) {
	s.Remote[p] = &RemoteEntry{
		ObjectID: info.ObjectID,
		SHA256:   info.SHA256,
		Size:     info.Size,
		ModTime:  info.ModTime,
		Machine:  info.Machine,
		Updated:  info.Updated,
		Legacy:   info.Legacy,
	}
}

// Paths lists one root's registered sync folders from disk.
func listSyncRoots() ([]SyncConfig, error) {
	base, err := configDir()
	if err != nil {
		return nil, err
	}
	base = filepath.Join(base, SyncDirName)
	des, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []SyncConfig
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		cfg, err := loadSyncConfig(de.Name())
		if err != nil {
			continue
		}
		out = append(out, *cfg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// loadSyncConfig reads one root's config.json.
func loadSyncConfig(rootID string) (*SyncConfig, error) {
	dir, err := syncRootDir(rootID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil, err
	}
	var cfg SyncConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func saveSyncConfig(cfg *SyncConfig) error {
	dir, err := syncRootDir(cfg.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "config.json"), data, 0600)
}

// findRootByPath returns the root whose local folder contains p, preferring the
// longest match so nested roots behave intuitively.
func findRootByPath(p string) (*SyncConfig, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, err
	}
	roots, err := listSyncRoots()
	if err != nil {
		return nil, err
	}
	var best *SyncConfig
	for i := range roots {
		local, err := filepath.Abs(roots[i].LocalPath)
		if err != nil {
			continue
		}
		if abs == local || strings.HasPrefix(abs, local+string(os.PathSeparator)) {
			if best == nil || len(local) > len(best.LocalPath) {
				best = &roots[i]
			}
		}
	}
	if best == nil {
		return nil, fmt.Errorf("%s is not inside a Tessera sync folder (see 'tessera sync list')", p)
	}
	return best, nil
}

// acquireLock takes an exclusive advisory lock for a root. The returned
// function releases it. Stale locks from a crashed process are reclaimed.
func acquireLock(rootID string) (func(), error) {
	p, err := lockFilePath(rootID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return nil, err
	}
	if data, err := os.ReadFile(p); err == nil {
		var info struct {
			PID     int       `json:"pid"`
			Started time.Time `json:"started"`
			Host    string    `json:"host"`
		}
		if json.Unmarshal(data, &info) == nil {
			if info.Host != machineName() || !processAlive(info.PID) {
				// Another machine's state dir, or a dead process: reclaim.
				_ = os.Remove(p)
			} else {
				return nil, fmt.Errorf("another sync is running for this folder (pid %d since %s); wait for it or remove %s",
					info.PID, info.Started.Format(time.RFC3339), p)
			}
		}
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"pid":     os.Getpid(),
		"started": time.Now(),
		"host":    machineName(),
	})
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("cannot take lock %s: %w", p, err)
	}
	if _, err := f.Write(payload); err != nil {
		f.Close()
		os.Remove(p)
		return nil, err
	}
	f.Close()
	return func() { os.Remove(p) }, nil
}

// processAlive reports whether a pid is alive. Signal 0 performs error
// checking without delivering anything; on Windows a live handle is required,
// so this remains a best-effort probe there.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// addRoot registers a new sync root on disk and returns its config.
func addRoot(localPath, remotePrefix string) (*SyncConfig, error) {
	abs, err := filepath.Abs(localPath)
	if err != nil {
		return nil, err
	}
	prefix, err := normalizeRemotePrefix(remotePrefix)
	if err != nil {
		return nil, err
	}
	// Refuse to register the same local folder twice.
	roots, _ := listSyncRoots()
	for _, r := range roots {
		if r.LocalPath == abs {
			return nil, fmt.Errorf("%s is already a sync folder (root %s → %s)", abs, r.ID, r.RemotePrefix)
		}
	}
	cfg := &SyncConfig{
		ID:             randomHex(8),
		LocalPath:      abs,
		RemotePrefix:   prefix,
		ConflictPolicy: conflictNewest,
		CreatedAt:      time.Now(),
	}
	if err := saveSyncConfig(cfg); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0755); err != nil {
		return nil, err
	}
	return cfg, nil
}

// removeRoot deletes a root's local state. The folder itself is left alone
// unless the caller also removed it.
func removeRoot(rootID string) error {
	dir, err := syncRootDir(rootID)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}
