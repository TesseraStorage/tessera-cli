package main

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

// VersionsPrefix is the reserved remote namespace for retained versions. The
// sync engine never materialises anything under it.
const VersionsPrefix = ".tessera-versions"

// VersionRef points at one retained copy of a path.
type VersionRef struct {
	ObjectID string    `json:"object_id"`
	Path     string    `json:"path"`
	At       time.Time `json:"at"`
	SHA256   string    `json:"sha256,omitempty"`
	Size     int64     `json:"size,omitempty"`
}

// versionNamespace returns the namespace that holds versions of rel.
//
// It is derived from the file's own path rather than from the sync root id, so
// the namespace always sits beside the file: for
// "<prefix>/notes/report.pdf" it is "<prefix>/.tessera-versions/report.pdf".
// The distinction matters because the root id is a local identifier while the
// prefix is part of the remote path.
func versionNamespace(prefix, rel string) string {
	return path.Join(prefix, VersionsPrefix, path.Base(rel))
}

// isVersionPath reports whether a remote path belongs to the version store.
func isVersionPath(remotePrefix, remotePath string) bool {
	return strings.HasPrefix(remotePath, path.Join(remotePrefix, VersionsPrefix)+"/") ||
		remotePath == path.Join(remotePrefix, VersionsPrefix)
}

// keepVersion moves an object about to be superseded into the version
// namespace, then prunes anything beyond the retention limit. When retention is
// zero the object is deleted outright.
func (e *SyncEngine) keepVersionRef(ctx context.Context, rel string, st *EntryState) error {
	if st == nil || st.ObjectID == "" || st.BaseSHA256 == "" {
		return nil
	}
	retain := versionRetention()
	from := PathInfo{
		ObjectID: st.ObjectID,
		Path:     e.remotePath(rel),
		Name:     path.Base(rel),
		SHA256:   st.BaseSHA256,
		Size:     st.Size,
		ModTime:  st.ModTime,
	}

	if retain <= 0 {
		// With retention off the replaced copy would otherwise be destroyed
		// the moment it is superseded. Move it to the trash namespace instead
		// so the delete retention window still applies; `trash empty` is the
		// only place that truly destroys data.
		trashPath := path.Join(TrashPrefix, shortHash(from.SHA256), from.Path)
		if _, err := e.rt.Reattach(ctx, from, trashPath, e.cfg.ID); err != nil {
			// Reattaching is best effort; if it fails, keep the object pinned
			// rather than deleting it.
			return fmt.Errorf("retain to trash: %w", err)
		}
		recordTrashEntry(TrashedEntry{
			OriginalPath: from.Path,
			TrashPath:    trashPath,
			ObjectID:     from.ObjectID,
			SHA256:       from.SHA256,
			Size:         from.Size,
			DeletedAt:    time.Now(),
		})
		return nil
	}
	at := time.Now()
	vpath := path.Join(versionNamespace(e.cfg.RemotePrefix, rel), fmt.Sprintf("%s-%s", at.UTC().Format("20060102-150405"), shortHash(from.SHA256)))
	if _, err := e.rt.Reattach(ctx, from, vpath, e.cfg.ID); err != nil {
		return fmt.Errorf("retain version: %w", err)
	}

	// Remember it locally so `tessera versions` does not need to rescan.
	cur := e.state.entry(rel)
	cur.Versions = append(cur.Versions, VersionRef{
		ObjectID: from.ObjectID,
		Path:     vpath,
		At:       at,
		SHA256:   from.SHA256,
		Size:     from.Size,
	})
	sort.Slice(cur.Versions, func(i, j int) bool { return cur.Versions[i].At.Before(cur.Versions[j].At) })

	// Prune the oldest beyond the limit.
	for len(cur.Versions) > retain {
		old := cur.Versions[0]
		cur.Versions = cur.Versions[1:]
		if err := e.rt.DeleteObject(ctx, old.ObjectID); err != nil {
			e.verbose("  could not prune version %s: %v", old.Path, err)
		}
	}
	return nil
}

// shortHash abbreviates a content hash for use in version names.
func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
