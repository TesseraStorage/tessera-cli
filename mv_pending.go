package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// An interrupted folder move leaves some files at the destination and the
// rest at the source. Re-running the same command would then see an existing
// destination folder and nest the rest one level deeper (dst/name/...). The
// destination folder a folder move chose is recorded here until the move
// finishes, so a re-run continues into the same folder.

type pendingMv struct {
	Account   string `json:"account,omitempty"`
	Src       string `json:"src"`
	Dst       string `json:"dst"`
	DstPrefix string `json:"dst_prefix"`
}

func pendingMvFile() string {
	dir, err := configDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "mv-pending.json")
}

func pendingMvKey(p string) string {
	p = strings.ReplaceAll(filepath.ToSlash(strings.TrimSpace(p)), `\`, "/")
	return strings.Trim(p, "/")
}

func loadPendingMvs() []pendingMv {
	f := pendingMvFile()
	if f == "" {
		return nil
	}
	data, err := os.ReadFile(f)
	if err != nil {
		return nil
	}
	var out []pendingMv
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

func storePendingMvs(list []pendingMv) {
	f := pendingMvFile()
	if f == "" {
		return
	}
	if len(list) == 0 {
		_ = os.Remove(f)
		return
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(f), 0700)
	_ = writeFileAtomic(f, data, 0600)
}

// pendingMvPrefix returns the destination folder of an unfinished run of the
// same folder move, or "".
func pendingMvPrefix(account, src, dst string) string {
	src, dst = pendingMvKey(src), pendingMvKey(dst)
	for _, p := range loadPendingMvs() {
		if p.Account == account && p.Src == src && p.Dst == dst {
			return p.DstPrefix
		}
	}
	return ""
}

func savePendingMv(account, src, dst, prefix string) {
	src, dst = pendingMvKey(src), pendingMvKey(dst)
	var keep []pendingMv
	for _, p := range loadPendingMvs() {
		if !(p.Account == account && p.Src == src && p.Dst == dst) {
			keep = append(keep, p)
		}
	}
	keep = append(keep, pendingMv{Account: account, Src: src, Dst: dst, DstPrefix: prefix})
	storePendingMvs(keep)
}

func clearPendingMv(account, src, dst string) {
	src, dst = pendingMvKey(src), pendingMvKey(dst)
	var keep []pendingMv
	for _, p := range loadPendingMvs() {
		if !(p.Account == account && p.Src == src && p.Dst == dst) {
			keep = append(keep, p)
		}
	}
	storePendingMvs(keep)
}
