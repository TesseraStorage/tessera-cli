package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// IndexEntry is one live object in the local index.
type IndexEntry struct {
	Path string `json:"path"`
	Name string `json:"name"`
	// Root is the sync root *id* that owns this path (not the remote prefix).
	Root string `json:"root,omitempty"`
	// RootID records which sync folder manages this path, so per-root
	// namespaces (versions, trash) can be derived without guessing.
	RootID   string    `json:"sync_root,omitempty"`
	ObjectID string    `json:"object_id"`
	SHA256   string    `json:"sha256,omitempty"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"mod_time,omitempty"`
	Updated  time.Time `json:"updated"`
	Machine  string    `json:"machine,omitempty"`
	Legacy   bool      `json:"legacy,omitempty"`
}

// Index is a local cache of the account's objects, keyed by logical path. It
// exists so `list`/`find`/`download`/`delete` do not have to walk the whole
// object-event log on the indexer for every invocation.
type Index struct {
	Schema    int                    `json:"schema"`
	BuiltAt   time.Time              `json:"built_at"`
	AccountID string                 `json:"account_id"`
	Paths     map[string]*IndexEntry `json:"paths"`

	// byHashCache is a derived lookup rebuilt on first use; it is never
	// serialised.
	byHashCache map[string]*IndexEntry `json:"-"`
}

const indexSchema = 1

func indexPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "index.json"), nil
}

// loadIndex reads the local index, returning an empty one when absent.
func loadIndex() (*Index, error) {
	p, err := indexPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &Index{Schema: indexSchema, Paths: map[string]*IndexEntry{}}, nil
		}
		return nil, err
	}
	idx := &Index{Schema: indexSchema, Paths: map[string]*IndexEntry{}}
	if err := json.Unmarshal(data, idx); err != nil {
		return nil, fmt.Errorf("corrupt index at %s (run 'tessera index --rebuild'): %w", p, err)
	}
	if idx.Schema != indexSchema {
		return nil, fmt.Errorf("index at %s uses schema %d (run 'tessera index --rebuild')", p, idx.Schema)
	}
	if idx.Paths == nil {
		idx.Paths = make(map[string]*IndexEntry)
	}
	return idx, nil
}

func (i *Index) save() error {
	p, err := indexPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	i.Schema = indexSchema
	data, err := json.Marshal(i)
	if err != nil {
		return err
	}
	return writeFileAtomic(p, data, 0600)
}

func (i *Index) put(info PathInfo) {
	if i.Paths == nil {
		i.Paths = make(map[string]*IndexEntry)
	}
	i.byHashCache = nil
	i.Paths[info.Path] = &IndexEntry{
		Path:     info.Path,
		Name:     info.Name,
		RootID:   info.Root,
		ObjectID: info.ObjectID,
		SHA256:   info.SHA256,
		Size:     info.Size,
		ModTime:  info.ModTime,
		Updated:  info.Updated,
		Machine:  info.Machine,
		Legacy:   info.Legacy,
	}
}

func (i *Index) remove(path string) {
	delete(i.Paths, path)
	i.byHashCache = nil
}

// rebuildIndex reconstructs the index from the indexer's full event history.
func rebuildIndex(ctx context.Context, rt *Remote, accountID string) (*Index, error) {
	live, err := rt.LivePaths(ctx)
	if err != nil {
		return nil, err
	}
	idx := &Index{
		Schema:    indexSchema,
		BuiltAt:   time.Now(),
		AccountID: accountID,
		Paths:     make(map[string]*IndexEntry, len(live)),
	}
	for _, info := range live {
		idx.put(info)
	}
	if err := idx.save(); err != nil {
		return nil, err
	}
	return idx, nil
}

// ensureIndex loads the index, building it from the indexer when missing or
// when it has gone stale relative to the account (for example after a sync on
// another machine). It is deliberately cheap: no rebuild unless needed.
func ensureIndex(ctx context.Context, rt *Remote, cfg *Config) (*Index, error) {
	idx, err := loadIndex()
	if err != nil {
		return nil, err
	}
	if idx.AccountID != cfg.AppID || len(idx.Paths) == 0 {
		return rebuildIndex(ctx, rt, cfg.AppID)
	}
	return idx, nil
}

// refreshIndex rebuilds the index from the network. Mutating commands call it
// so they never act on a cache that predates a sync on this or another
// machine. Cost is one pass over the object-event log.
func refreshIndex(ctx context.Context, rt *Remote, cfg *Config) (*Index, error) {
	return rebuildIndex(ctx, rt, cfg.AppID)
}

// SortedPaths returns index entries ordered by path.
func (i *Index) SortedPaths() []*IndexEntry {
	out := make([]*IndexEntry, 0, len(i.Paths))
	for _, e := range i.Paths {
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Path < out[b].Path })
	return out
}

// TotalSize sums the live objects' sizes.
func (i *Index) TotalSize() uint64 {
	var total uint64
	for _, e := range i.Paths {
		if e.Size > 0 {
			total += uint64(e.Size)
		}
	}
	return total
}

// lookup resolves a user-supplied path or name to an index entry. Exact path
// wins; a unique basename is accepted as a convenience, and an ambiguous
// basename is reported rather than silently picking one.
func (i *Index) lookup(name string) (*IndexEntry, error) {
	if e, ok := i.Paths[name]; ok {
		return e, nil
	}
	var matches []*IndexEntry
	for _, e := range i.Paths {
		if e.Name == name || e.Path == name {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("%w: %s", errNotFound, name)
	case 1:
		return matches[0], nil
	default:
		var opts []string
		for _, m := range matches {
			opts = append(opts, m.Path)
		}
		sort.Strings(opts)
		const showMax = 8
		shown := opts
		if len(shown) > showMax {
			shown = shown[:showMax]
		}
		msg := fmt.Sprintf("%q matches %d files; use an explicit path, one of:\n    %s",
			name, len(opts), strings.Join(shown, "\n    "))
		if len(opts) > showMax {
			msg += fmt.Sprintf("\n    ... and %d more", len(opts)-showMax)
		}
		return nil, errors.New(msg)
	}
}
