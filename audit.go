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

// cmdUsage reports storage usage, a cost forecast, and (with billing
// credentials) the billing account section.
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

	usageReport(args, cfg, idx)
}

// usageFolder is one folder row in the usage report.
type usageFolder struct {
	Files int
	Bytes uint64
}

// collectUsageStorage buckets the index by folder and sums the logical bytes.
// Folder names come from the owning sync root's remote prefix when it is
// registered, the path's first segment otherwise, or an em dash for rootless
// top-level files. This is the untouched storage half of `usage`.
func collectUsageStorage(idx *Index) (map[string]usageFolder, uint64) {
	byFolder := map[string]usageFolder{}
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
		v.Files++
		v.Bytes += uint64(e.Size)
		byFolder[key] = v
		total += uint64(e.Size)
	}
	return byFolder, total
}

// parenIf wraps text in parentheses when cond holds, else "".
func parenIf(cond bool, text string) string {
	if !cond || text == "" {
		return ""
	}
	return "(" + text + ")"
}

// usageReport renders `usage`. The storage block prints the same local-file
// view with or without billing; when a billing credential is stored it appends
// the account section (network size from GET /account, open slabs, egress,
// balance, period end, days left; and, with --cost, the netted PAYG egress
// estimate). A billing fetch failure degrades to a warning so the storage
// report — and the command's exit status — stand alone. The no-credential
// --cost hint goes to stderr so --json stdout stays parseable.
func usageReport(args []string, cfg *Config, idx *Index) {
	byFolder, total := collectUsageStorage(idx)
	cost := hasFlag(args, "--cost")
	hasBilling := credentialLabel(cfg) != ""

	var billing map[string]any
	var billingErr error
	if hasBilling {
		billing, billingErr = newBillingClient(cfg).Account()
	}

	if hasFlag(args, "--json") {
		out := usageStorageJSON(idx, total, byFolder)
		switch {
		case hasBilling && billingErr == nil:
			out["billing"] = usageBillingJSON(billing, cost)
		case hasBilling:
			out["billing_error"] = describeBillingError(billingErr)
		}
		if !hasBilling && cost {
			usageCostHint()
		}
		emitJSON(out)
		return
	}

	printUsageStorage(idx, total, byFolder, hasFlag(args, "--by-folder"))
	switch {
	case hasBilling && billingErr != nil:
		fmt.Println()
		fmt.Println("Billing")
		fmt.Println()
		fmt.Printf("  unavailable:     %s\n", describeBillingError(billingErr))
	case hasBilling:
		printUsageBilling(billing, cost)
	case cost:
		usageCostHint()
	}
}

// usageStorageJSON is the local storage half of `usage --json`: the files this
// machine synced and their logical sizes. Network size is not estimated here;
// with a billing credential the backend's real network_bytes appears in the
// billing object.
func usageStorageJSON(idx *Index, total uint64, byFolder map[string]usageFolder) map[string]interface{} {
	type row struct {
		Folder string `json:"folder"`
		Files  int    `json:"files"`
		Bytes  uint64 `json:"bytes"`
	}
	var rows []row
	for k, v := range byFolder {
		rows = append(rows, row{Folder: k, Files: v.Files, Bytes: v.Bytes})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Bytes > rows[j].Bytes })
	return map[string]interface{}{
		"files":         len(idx.Paths),
		"logical_bytes": total,
		"folders":       rows,
	}
}

// printUsageStorage prints the storage block, unchanged from before the billing
// section existed.
func printUsageStorage(idx *Index, total uint64, byFolder map[string]usageFolder, byFolderTable bool) {
	fmt.Printf("Storage usage\n\n")
	fmt.Printf("  files:           %d\n", len(idx.Paths))
	fmt.Printf("  your files:      %s (logical size, as uploaded from this machine)\n", formatBytes(total))

	if byFolderTable && len(byFolder) > 0 {
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
			rows = append(rows, row{k, v.Files, v.Bytes})
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

// Billing price card constants used by the --cost estimate.
const (
	// egressUSDPerTB is the PAYG egress rate from the price card.
	egressUSDPerTB = 5.0
	// decimalTBBytes is a decimal terabyte (1e12 bytes), the unit the price
	// card prices in — not 2^40.
	decimalTBBytes = 1e12
	// egressEstimateLabel is the mandatory --cost disclaimer.
	egressEstimateLabel = "estimate only; allowance netted; excludes plan fees/credits ($5/TB PAYG)"
)

// egressEstimateUSD returns the PAYG egress overage estimate formatted to
// cents: billable = max(0, used − allowance), then billable / 1e12 × $5, the
// price card's decimal-TB rate. An allowance that covers the usage yields
// "0.00".
func egressEstimateUSD(used, allowance uint64) string {
	var billable uint64
	if used > allowance {
		billable = used - allowance
	}
	usd := float64(billable) / decimalTBBytes * egressUSDPerTB
	return fmt.Sprintf("%.2f", usd)
}

// usageCostHint points at the two ways to get a billing credential. It is
// printed only when --cost was requested with none stored, and goes to stderr
// so it can never corrupt --json stdout.
func usageCostHint() {
	fmt.Fprintf(os.Stderr, "tessera usage --cost: %v\n", errNoBillingCredentials)
}

// usageBillingJSON copies the GET /account fields the usage section shows,
// preserving the server's raw values (nulls included). These keys are additive
// to the storage contract and appear only when a billing credential is stored;
// --cost adds the estimate field.
func usageBillingJSON(acct map[string]any, cost bool) map[string]any {
	out := map[string]any{
		"network_bytes":          acct["network_bytes"],
		"stored_bytes":           acct["stored_bytes"],
		"raw_bytes":              acct["raw_bytes"],
		"open_slabs":             acct["open_slabs"],
		"egress_used_bytes":      acct["egress_used_bytes"],
		"allowance_egress_bytes": acct["allowance_egress_bytes"],
		"balance_usd":            acct["balance_usd"],
		"period_end":             acct["period_end"],
		"days_left":              acct["days_left"],
	}
	out["size_note"] = strings.Join(sizeExplanation, " ")
	if v := buildTrialView(acct); v != nil {
		out["trial"] = v
	}
	if cost && egressMeasuredInBytes(acct) {
		used, _ := accountUint64(acct, "egress_used_bytes")
		allowance, _ := accountUint64(acct, "allowance_egress_bytes")
		out["egress_estimate_usd"] = egressEstimateUSD(used, allowance)
	} else if cost {
		out["egress_estimate_usd"] = nil
	}
	honestEgressJSON(out, acct)
	return out
}

// printUsageBilling renders the appended billing section. Network size is the
// backend's real network_bytes from GET /account, never a client-side estimate;
// missing and null fields render as an em dash. --cost appends the netted PAYG
// egress estimate with its exact disclaimer label.
func printUsageBilling(acct map[string]any, cost bool) {
	fmt.Println()
	fmt.Println("Billing")
	fmt.Println()
	field := func(label, value string) {
		fmt.Printf("  %-16s %s\n", label+":", dashIfEmpty(value))
	}
	field("logical size", joinNonEmpty(" ", accountBytes(acct, "raw_bytes"), parenIf(accountBytes(acct, "raw_bytes") != "", "your files, all machines")))
	field("stored", joinNonEmpty(" ", accountBytes(acct, "stored_bytes"), parenIf(slabCountText(acct) != "", slabCountText(acct)+" of 40 MiB")))
	field("network size", joinNonEmpty(" ", accountBytes(acct, "network_bytes"), parenIf(accountBytes(acct, "network_bytes") != "", "3× redundancy across hosts")))
	field("downloads", egressDisplay(acct))
	balance := accountBalanceUSD(acct, "balance_usd")
	if balance != "" {
		balance = "$" + balance
	}
	field("balance", balance)
	field("period ends", periodEndText(acct))
	days := daysLeftEmptyText(acct)
	if n, ok := accountUint64(acct, "days_left"); ok {
		days = "about " + humanDays(n) + " (estimate at today's storage charge for what you store now)"
	}
	field("days left", days)
	if cost {
		if egressMeasuredInBytes(acct) {
			usedN, _ := accountUint64(acct, "egress_used_bytes")
			allowanceN, _ := accountUint64(acct, "allowance_egress_bytes")
			field("egress estimate", "$"+egressEstimateUSD(usedN, allowanceN))
			fmt.Printf("    %s\n", egressEstimateLabel)
		} else {
			field("egress estimate", "")
			fmt.Println("    downloads are " + egressNote + "; watch the balance instead")
		}
	}
	if v := buildTrialView(acct); v != nil {
		fmt.Println()
		printTrialBlock(v, "  ", 16)
	}
	fmt.Println()
	printSizeExplanation("  ")
}
