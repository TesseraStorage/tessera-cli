package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"go.sia.tech/core/types"
	"go.sia.tech/indexd/slabs"
	siastorage "go.sia.tech/siastorage"
)

// PathInfo describes the live remote object stored at one logical path.
type PathInfo struct {
	ObjectID string // object key, hex (its identity)
	Path     string // logical path, slash-separated
	Name     string // base filename
	// Root is the sync root *id* that owns this path, when known. It is not the
	// remote prefix: the prefix is part of Path.
	Root    string
	SHA256  string    // content hash
	Size    int64     // bytes
	ModTime time.Time // source mtime
	Machine string    // uploading host
	Updated time.Time // indexer event time
	Legacy  bool      // uploaded by a pre-sync client (no hash available)
}

// sameContent reports whether two remote entries describe identical bytes.
func (p PathInfo) sameContent(o PathInfo) bool {
	if p.SHA256 != "" && o.SHA256 != "" {
		return p.SHA256 == o.SHA256
	}
	// Legacy objects have no hash; fall back to id/size equality.
	return p.ObjectID == o.ObjectID
}

// Remote is the object-store side of a sync: an account's object events plus
// path-aware upload/download helpers.
type Remote struct {
	sdk *siastorage.SDK
}

// NewRemote wraps an SDK.
func NewRemote(sdk *siastorage.SDK) *Remote { return &Remote{sdk: sdk} }

// Events walks every object event, oldest first, calling fn for each. The SDK
// preserves deletion events, which is what makes remote deletes visible.
func (r *Remote) Events(ctx context.Context, fn func(siastorage.ObjectEvent) error) error {
	var cursor slabs.Cursor
	for {
		evs, err := r.sdk.ObjectEvents(ctx, cursor, 100)
		if err != nil {
			return fmt.Errorf("list objects: %w", err)
		}
		if len(evs) == 0 {
			return nil
		}
		for _, ev := range evs {
			if err := fn(ev); err != nil {
				return err
			}
		}
		last := evs[len(evs)-1]
		next := slabs.Cursor{Key: last.Key, After: last.UpdatedAt}
		if sameCursor(cursor, next) {
			return nil
		}
		cursor = next
	}
}

// pathFromEvent extracts the logical path and hash of an event's object.
func pathFromEvent(ev siastorage.ObjectEvent) (PathInfo, bool) {
	if ev.Object == nil {
		return PathInfo{}, false
	}
	env, err := decodeMetadata(ev.Object.Metadata())
	if err != nil {
		return PathInfo{}, false
	}
	p := env.logicalPath()
	if p == "" {
		return PathInfo{}, false
	}
	info := PathInfo{
		ObjectID: ev.Key.String(),
		Path:     p,
		Name:     env.Name,
		Updated:  ev.UpdatedAt,
		Size:     int64(ev.Object.Size()),
	}
	if info.Name == "" {
		info.Name = path.Base(p)
	}
	if env.TS != nil {
		info.Root = env.TS.Root
		_ = env.TS.Root
		info.SHA256 = env.TS.SHA256
		info.Size = env.TS.Size
		info.ModTime = env.TS.ModTime
		info.Machine = env.TS.Machine
		info.Legacy = env.TS.SHA256 == ""
	} else {
		info.Legacy = true
	}
	return info, true
}

// LivePaths materialises the current live object for every logical path. It is
// used by the local index rebuild and by commands that need a point-in-time
// snapshot. Deleted events are ignored; the most recently updated live event
// wins a path.
func (r *Remote) LivePaths(ctx context.Context) (map[string]PathInfo, error) {
	live := make(map[string]PathInfo)
	err := r.Events(ctx, func(ev siastorage.ObjectEvent) error {
		if ev.Deleted || ev.Object == nil {
			return nil
		}
		info, ok := pathFromEvent(ev)
		if !ok {
			return nil
		}
		// Events arrive in (updatedAt, key) order, but a same-second update can
		// land either way, so prefer the newer timestamp explicitly.
		if prev, dup := live[info.Path]; dup && prev.Updated.After(info.Updated) {
			return nil
		}
		live[info.Path] = info
		return nil
	})
	if err != nil {
		return nil, err
	}
	return live, nil
}

// Paths returns live objects whose path starts with prefix (slash-separated).
// An empty prefix returns everything.
//
// Version and trash entries store their namespace *relative to the object path*
// (see versions.go), so a requested prefix that falls inside one of those
// namespaces is matched against the part after the marker instead. Without this
// a call like Paths("<prefix>/.tessera-versions/a.txt") would never match the
// stored "<prefix>/.tessera-versions/"+"a.txt/<stamp>-<sha>".
func (r *Remote) Paths(ctx context.Context, prefix string) (map[string]PathInfo, error) {
	all, err := r.LivePaths(ctx)
	if err != nil {
		return nil, err
	}
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return all, nil
	}
	out := make(map[string]PathInfo)
	for p, info := range all {
		if matchesNamespacePrefix(p, prefix) {
			out[p] = info
		}
	}
	return out, nil
}

// matchesNamespacePrefix reports whether stored path p belongs to the requested
// namespace prefix, accounting for entries held in the version/trash stores.
func matchesNamespacePrefix(p, prefix string) bool {
	if p == prefix || strings.HasPrefix(p, prefix+"/") {
		return true
	}
	for _, ns := range []string{VersionsPrefix, TrashPrefix} {
		marker := "/" + ns + "/"
		i := strings.Index(p, marker)
		if i < 0 {
			continue
		}
		// The part of the request that lives inside the namespace.
		rel := ""
		if strings.HasPrefix(prefix, p[:i+len(marker)]) {
			rel = strings.TrimPrefix(prefix, p[:i+len(marker)])
		} else {
			continue
		}
		storedRel := p[i+len(marker):]
		if rel == "" || strings.HasPrefix(storedRel, rel+"/") {
			return true
		}
	}
	return false
}

// HeadPath looks up a single logical path. Legacy objects are matched by their
// bare name too, so files uploaded before this version stay reachable.
func (r *Remote) HeadPath(ctx context.Context, p string) (PathInfo, error) {
	live, err := r.LivePaths(ctx)
	if err != nil {
		return PathInfo{}, err
	}
	if info, ok := live[p]; ok {
		return info, nil
	}
	// Backwards compatibility: a legacy object whose metadata has no path is
	// addressed by its filename.
	for _, info := range live {
		if info.Legacy && (info.Name == p || info.Path == p) {
			return info, nil
		}
	}
	return PathInfo{}, fmt.Errorf("%w: %s (run 'tessera list %s' to see current paths)", errNotFound, p, path.Dir(p))
}

// uploadRequest describes one file to store.
type uploadRequest struct {
	LocalPath string
	RelPath   string // logical path (remote prefix already applied)
	Name      string
	Hash      string
	Size      int64
	Mode      os.FileMode
	ModTime   time.Time
	Root      string
}

// Upload streams a file to the network and pins it, attaching the sync
// descriptor to the object metadata. It returns the new object's PathInfo.
func (r *Remote) Upload(ctx context.Context, req uploadRequest, onProgress func(int64)) (PathInfo, error) {
	f, err := os.Open(req.LocalPath)
	if err != nil {
		return PathInfo{}, fmt.Errorf("open %s: %w", req.LocalPath, err)
	}
	defer f.Close()

	meta, err := encodeMetadata(req.Name, &tsMeta{
		Root:    req.Root,
		Path:    req.RelPath,
		SHA256:  req.Hash,
		Size:    req.Size,
		Mode:    fmt.Sprintf("%04o", req.Mode.Perm()),
		ModTime: req.ModTime.UTC().Truncate(time.Millisecond),
		Machine: machineName(),
	})
	if err != nil {
		return PathInfo{}, err
	}

	obj := siastorage.NewEmptyObject()
	obj.UpdateMetadata(meta)

	err = r.sdk.Upload(ctx, &obj, f,
		siastorage.WithRedundancy(dataShards, parityShards),
	)
	if err != nil {
		return PathInfo{}, fmt.Errorf("upload %s: %w", req.RelPath, err)
	}
	if err := r.sdk.PinObject(ctx, obj); err != nil {
		return PathInfo{}, fmt.Errorf("pin %s: %w", req.RelPath, err)
	}
	if onProgress != nil {
		onProgress(req.Size)
	}

	return PathInfo{
		ObjectID: obj.ID().String(),
		Path:     req.RelPath,
		Name:     req.Name,
		Root:     req.Root,
		SHA256:   req.Hash,
		Size:     req.Size,
		ModTime:  req.ModTime,
		Machine:  machineName(),
		Updated:  time.Now(),
	}, nil
}

// DeleteObject unpins an object by its hex key. Deleting a nonexistent object
// is a no-op, which makes retries safe.
func (r *Remote) DeleteObject(ctx context.Context, objectID string) error {
	key, err := parseObjectID(objectID)
	if err != nil {
		return fmt.Errorf("bad object id %q: %w", objectID, err)
	}
	if err := r.sdk.DeleteObject(ctx, key); err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

// Download streams an object to a local file atomically and sets its mtime to
// the recorded source mtime so subsequent runs agree it is unchanged.
func (r *Remote) Download(ctx context.Context, info PathInfo, dest string, mode os.FileMode) (int64, error) {
	key, err := parseObjectID(info.ObjectID)
	if err != nil {
		return 0, fmt.Errorf("bad object reference for %s: %w", info.Path, err)
	}
	obj, err := r.sdk.Object(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("fetch %s (object %s): %w — if this persists, run 'tessera index --rebuild'", info.Path, shortHash(info.ObjectID), err)
	}
	rc, err := r.sdk.Download(obj)
	if err != nil {
		return 0, fmt.Errorf("download %s: %w", info.Path, err)
	}
	defer rc.Close()

	n, err := copyFileAtomic(dest, rc, mode)
	if err != nil {
		return n, err
	}
	if !info.ModTime.IsZero() {
		_ = os.Chtimes(dest, info.ModTime, info.ModTime)
	}
	return n, nil
}

// Reattach rewrites an object's metadata to point at a new path without
// re-uploading its data.
//
// An object has exactly one logical path, so this is a move, not a copy: when
// reusing an object for a second path the caller must clear the original path
// by passing oldPath == "", otherwise the first path would be stolen (it would
// simply stop existing under its own name).
func (r *Remote) Reattach(ctx context.Context, from PathInfo, newPath, root string) (PathInfo, error) {
	if from.SHA256 == "" {
		return PathInfo{}, fmt.Errorf("cannot reattach legacy object %s without a hash", from.Path)
	}
	key, err := parseObjectID(from.ObjectID)
	if err != nil {
		return PathInfo{}, err
	}
	obj, err := r.sdk.Object(ctx, key)
	if err != nil {
		return PathInfo{}, fmt.Errorf("read %s (object %s): %w", from.Path, shortHash(from.ObjectID), err)
	}
	meta, err := encodeMetadata(path.Base(newPath), &tsMeta{
		Root:    root,
		Path:    newPath,
		SHA256:  from.SHA256,
		Size:    from.Size,
		ModTime: from.ModTime,
		Machine: machineName(),
	})
	if err != nil {
		return PathInfo{}, err
	}
	// Re-attaching to the same path is a metadata refresh; either way the
	// previous object identity is fully described by the metadata we write.
	obj.UpdateMetadata(meta)
	if err := r.sdk.PinObject(ctx, obj); err != nil {
		return PathInfo{}, fmt.Errorf("reattach %s: %w", newPath, err)
	}

	// Verify the rewrite landed before reporting success. Reads are subject to
	// the indexer's own consistency, so retry briefly; a silent failure here
	// would make a path disappear from every machine.
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		got, err := r.sdk.Object(ctx, key)
		if err == nil {
			env, derr := decodeMetadata(got.Metadata())
			if derr == nil && env.TS != nil && env.TS.Path == newPath {
				from.Path = newPath
				from.Root = root
				from.Name = path.Base(newPath)
				from.Updated = time.Now()
				return from, nil
			}
			lastErr = fmt.Errorf("metadata still reports %q", env.logicalPath())
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return PathInfo{}, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return PathInfo{}, fmt.Errorf("reattach %s did not take effect: %w", newPath, lastErr)
}

// machineName returns a stable short hostname used in conflict copies.
func machineName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}

// parseObjectID converts a hex object key into the SDK's hash type.
func parseObjectID(s string) (types.Hash256, error) {
	var h types.Hash256
	if err := h.UnmarshalText([]byte(s)); err != nil {
		return h, err
	}
	return h, nil
}

// isNotFound reports whether an error is the indexer's missing-object response.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "object not found") ||
		strings.Contains(s, "not found") ||
		strings.Contains(s, "404")
}

// sortedPaths gives deterministic ordering for output and tests.
func sortedPaths(m map[string]PathInfo) []string {
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// marshalMetadata is a test seam for verifying the envelope round-trip.
func marshalMetadata(name string, m *tsMeta) (json.RawMessage, error) {
	b, err := encodeMetadata(name, m)
	return json.RawMessage(b), err
}
