package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// MetadataVersion is bumped whenever the envelope below changes shape.
const MetadataVersion = 1

// The indexer caps object metadata at slabs.MaxMetadataSize (1024 bytes) after
// encryption, so the envelope stays deliberately small. A path plus hashes
// costs roughly 250 bytes, leaving ample headroom.
type metaEnvelope struct {
	// Name is the base filename. Kept for compatibility with older clients
	// (the pre-sync CLI matched objects by this field).
	Name string  `json:"name,omitempty"`
	V    int     `json:"v"`
	TS   *tsMeta `json:"tessera,omitempty"`
}

// tsMeta is the Tessera sync descriptor attached to every object.
type tsMeta struct {
	// Root is the sync root id. It keeps independent sync roots that share one
	// account from colliding on identical relative paths.
	Root string `json:"root,omitempty"`
	// Path is the slash-separated path relative to the root. Always uses "/"
	// so a folder synced from Windows is addressable from macOS and Linux.
	Path string `json:"path"`
	// SHA256 is the hex content hash. This is what makes three-way
	// reconciliation possible without trusting mtimes.
	SHA256 string `json:"sha256"`
	// Size is the file size in bytes.
	Size int64 `json:"size"`
	// Mode is the octal permission string (informational; not enforced).
	Mode string `json:"mode,omitempty"`
	// ModTime is the source file's mtime in UTC, RFC3339Nano.
	ModTime time.Time `json:"mtime"`
	// Machine is the hostname that uploaded the object. Used for conflict
	// copies and for `tessera info`.
	Machine string `json:"machine,omitempty"`
}

// NormalizeRelPath cleans a user- or OS-supplied relative path into the
// canonical slash-separated form used in metadata and sync state. It rejects
// absolute paths and any component that would escape the sync root.
func NormalizeRelPath(p string) (string, error) {
	// On Unix filepath.ToSlash is a no-op, but a path originating from a
	// Windows machine still uses backslashes, so normalise them explicitly.
	p = strings.ReplaceAll(filepath.ToSlash(p), `\`, "/")
	p = path.Clean(p)
	if p == "." || p == "/" || p == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("absolute path not allowed: %s", p)
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", fmt.Errorf("path escapes its root: %s", p)
	}
	return p, nil
}

// normalizeRemotePrefix validates a sync root's remote prefix. It is applied to
// the relative path before it is embedded in metadata.
func normalizeRemotePrefix(prefix string) (string, error) {
	prefix = strings.ReplaceAll(filepath.ToSlash(strings.TrimSpace(prefix)), `\`, "/")
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return "", fmt.Errorf("remote prefix must not be empty")
	}
	return path.Clean(prefix), nil
}

// encodeMetadata builds the metadata payload for a file.
func encodeMetadata(name string, m *tsMeta) ([]byte, error) {
	env := metaEnvelope{
		Name: name,
		V:    MetadataVersion,
		TS:   m,
	}
	data, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	if len(data) > 1000 {
		return nil, fmt.Errorf("metadata for %s is too large (%d bytes, limit 1024)", name, len(data))
	}
	return data, nil
}

// decodeMetadata parses an object's metadata. It returns a nil tsMeta when the
// object predates the sync format, so callers can still surface Name.
func decodeMetadata(raw json.RawMessage) (metaEnvelope, error) {
	var env metaEnvelope
	if len(raw) == 0 {
		return env, nil
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		// Some very old objects stored a bare {"name": "..."} map. Try that.
		var legacy map[string]string
		if err2 := json.Unmarshal(raw, &legacy); err2 == nil {
			return metaEnvelope{Name: legacy["name"], V: 0}, nil
		}
		return env, fmt.Errorf("decode metadata: %w", err)
	}
	return env, nil
}

// logicalPath returns the path an object should be addressed by. Objects
// without sync metadata (uploaded by an older CLI) fall back to their name so
// they remain reachable.
func (e metaEnvelope) logicalPath() string {
	if e.TS != nil && e.TS.Path != "" {
		return e.TS.Path
	}
	return e.Name
}

// sharesRoot reports whether this envelope belongs to the given sync root.
func (e metaEnvelope) sharesRoot(rootID string) bool {
	return e.TS != nil && e.TS.Root != "" && e.TS.Root == rootID
}

// errNotFound is returned when a logical path has no live object.
var errNotFound = errors.New("object not found")

// ErrObjectDeleted is used to signal that a path was deleted remotely and the
// local copy must be removed.
var ErrObjectDeleted = errors.New("object deleted remotely")
