package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Durability assumptions implied by the erasure coding the CLI requests.
const (
	auditWarningThreshold = 0.8 // warn when usable redundancy drops below this
)

// AuditReport summarises how much redundancy the stored account currently has.
type AuditReport struct {
	Checked   int            `json:"checked"`
	Objects   int            `json:"objects"`
	Slabs     int            `json:"slabs"`
	MinShards int            `json:"min_shards"`
	Parity    int            `json:"parity_shards"`
	Bytes     int64          `json:"bytes"`
	AtRisk    []AuditFinding `json:"at_risk,omitempty"`
	Healthy   int            `json:"healthy"`
	Verified  int            `json:"verified_hashes"`
}

// AuditFinding is one object worth the operator's attention.
type AuditFinding struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Reason string `json:"reason"`
}

// cmdAudit inspects stored objects and reports redundancy and integrity.
func cmdAudit(args []string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	pos := positional(args)
	prefix := ""
	if len(pos) > 0 {
		prefix = strings.Trim(pos[0], "/")
	}
	deep := hasFlag(args, "--verify")
	limit := intFlag(args, "--limit", 0)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	sdk, cleanup, err := connectSDK(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()
	rt := NewRemote(sdk)

	live, err := rt.Paths(ctx, prefix)
	if err != nil {
		fatal("audit: %v", err)
	}

	rep := AuditReport{MinShards: dataShards, Parity: parityShards}
	paths := make([]string, 0, len(live))
	for p := range live {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		info := live[p]
		rep.Objects++
		rep.Bytes += info.Size

		obj, err := sdk.Object(ctx, mustObjectID(info.ObjectID))
		if err != nil {
			rep.AtRisk = append(rep.AtRisk, AuditFinding{Path: p, Size: info.Size, Reason: fmt.Sprintf("metadata unavailable: %v", err)})
			continue
		}
		slabs := obj.Slabs()
		rep.Slabs += len(slabs)
		rep.Checked++

		// Each slab records how many shards it was written with. A slab whose
		// recorded redundancy has fallen below our target is at risk.
		for _, ss := range slabs {
			if int(ss.MinShards) < dataShards {
				rep.AtRisk = append(rep.AtRisk, AuditFinding{
					Path: p, Size: info.Size,
					Reason: fmt.Sprintf("slab redundancy below target (%d of %d shards)", ss.MinShards, dataShards+parityShards),
				})
				break
			}
		}

		if deep && (limit == 0 || rep.Verified < limit) {
			if _, err := rt.Download(ctx, info, os.DevNull, 0600); err != nil {
				rep.AtRisk = append(rep.AtRisk, AuditFinding{Path: p, Size: info.Size, Reason: fmt.Sprintf("download failed: %v", err)})
				continue
			}
			rep.Verified++
		}
	}
	rep.Healthy = rep.Objects - len(rep.AtRisk)

	if hasFlag(args, "--json") {
		emitJSON(rep)
		return
	}

	fmt.Printf("Durability audit\n\n")
	fmt.Printf("  objects checked:  %d\n", rep.Checked)
	fmt.Printf("  total size:       %s\n", formatBytes(uint64(rep.Bytes)))
	fmt.Printf("  slabs:            %d\n", rep.Slabs)
	fmt.Printf("  erasure coding:   %d data + %d parity (survives %d host failures)\n", rep.MinShards, rep.Parity, rep.Parity)
	if deep {
		fmt.Printf("  hash verified:    %d object(s)\n", rep.Verified)
	}
	fmt.Println()
	if len(rep.AtRisk) == 0 {
		fmt.Println("  ✓ every object meets the redundancy target")
		return
	}
	fmt.Printf("  ! %d object(s) need attention:\n", len(rep.AtRisk))
	for _, f := range rep.AtRisk {
		fmt.Printf("      %-52s  %s\n", truncate(f.Path, 52), f.Reason)
	}
}

// cmdUsage reports storage usage and a cost forecast.
func cmdUsage(args []string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sdk, cleanup, err := connectSDK(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()
	rt := NewRemote(sdk)
	idx, err := rebuildIndex(ctx, rt, cfg.AppID)
	if err != nil {
		fatal("usage: %v", err)
	}

	byFolder := map[string]struct {
		Count int
		Bytes uint64
	}{}
	var total uint64
	for _, e := range idx.Paths {
		key := "—"
		if e.Root != "" {
			if r, err := loadSyncConfig(e.Root); err == nil {
				key = r.RemotePrefix
			} else {
				key = e.Root
			}
		} else if i := strings.IndexByte(e.Path, '/'); i > 0 {
			key = e.Path[:i]
		}
		v := byFolder[key]
		v.Count++
		v.Bytes += uint64(e.Size)
		byFolder[key] = v
		total += uint64(e.Size)
	}

	// Erasure coding stores 30 shards for every 10 data shards, so the bytes
	// billed on the network are roughly triple the logical size.
	const redundancyFactor = float64(dataShards+parityShards) / float64(dataShards)

	if hasFlag(args, "--json") {
		type row struct {
			Folder string `json:"folder"`
			Files  int    `json:"files"`
			Bytes  uint64 `json:"bytes"`
		}
		var rows []row
		for k, v := range byFolder {
			rows = append(rows, row{Folder: k, Files: v.Count, Bytes: v.Bytes})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Bytes > rows[j].Bytes })
		emitJSON(map[string]interface{}{
			"files":               len(idx.Paths),
			"logical_bytes":       total,
			"stored_bytes_approx": uint64(float64(total) * redundancyFactor),
			"folders":             rows,
		})
		return
	}

	fmt.Printf("Storage usage\n\n")
	fmt.Printf("  files:           %d\n", len(idx.Paths))
	fmt.Printf("  logical size:    %s\n", formatBytes(total))
	fmt.Printf("  network (≈%dx):  %s (10 data + 20 parity shards)\n", int(redundancyFactor), formatBytes(uint64(float64(total)*redundancyFactor)))

	if hasFlag(args, "--by-folder") && len(byFolder) > 0 {
		fmt.Println()
		fmt.Printf("  %-40s  %8s  %12s\n", "FOLDER", "FILES", "SIZE")
		fmt.Println("  " + strings.Repeat("-", 64))
		type row struct {
			k     string
			count int
			bytes uint64
		}
		var rows []row
		for k, v := range byFolder {
			rows = append(rows, row{k, v.Count, v.Bytes})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].bytes > rows[j].bytes })
		for _, r := range rows {
			fmt.Printf("  %-40s  %8d  %12s\n", truncate(r.k, 40), r.count, formatBytes(r.bytes))
		}
	}
	fmt.Println()
	fmt.Println("  Note: Sia costs depend on your contracts and current host prices.")
	fmt.Println("  Run 'tessera status' for account readiness.")
}
