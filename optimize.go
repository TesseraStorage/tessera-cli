package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.sia.tech/indexd/slabs"
	siastorage "go.sia.tech/siastorage"
)

const (
	listedStorageUSDPerTBMonth = 2.50
	listedUploadUSDPerTB       = 1.0
	listedDownloadUSDPerTB     = 10.0
	bytesPerTB                 = 1e12
	// optimizeXferBytesPerSec is a conservative combined transfer rate used
	// only to estimate how long a run will take.
	optimizeXferBytesPerSec = 10 << 20
)

// optimizeRates holds the prices used to cost a plan. Values come from the
// live GET /billing/rates document when available, otherwise the listed card
// (storage $2.50/TB-month, uploads $1/TB, downloads $10/TB). TB = 10^12 bytes.
type optimizeRates struct {
	StoragePerTBMonth float64 `json:"storage_usd_per_tb_month"`
	UploadPerTB       float64 `json:"upload_usd_per_tb"`
	DownloadPerTB     float64 `json:"download_usd_per_tb"`
	Source            string  `json:"source"`
}

type optimizeSlabSlice struct {
	ID     string
	Length uint32
}

type optimizeObject struct {
	Info  PathInfo
	Slabs []optimizeSlabSlice
}

// optimizeFile is one file the plan would rewrite.
type optimizeFile struct {
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	ObjectID string    `json:"object_id"`
	SHA256   string    `json:"sha256,omitempty"`
	ModTime  time.Time `json:"mod_time,omitempty"`
	Root     string    `json:"root,omitempty"`
	Shared   bool      `json:"shared_slab,omitempty"`
}

// optimizePlan is the packing proposal shown before any data moves.
type optimizePlan struct {
	Files              []optimizeFile `json:"files"`
	BlocksNow          int            `json:"blocks_now"`
	BlocksAfter        int            `json:"blocks_after"`
	StoredBytesNow     uint64         `json:"stored_bytes_now"`
	StoredBytesAfter   uint64         `json:"stored_bytes_after"`
	SavedBytes         uint64         `json:"saved_bytes"`
	LogicalBytes       uint64         `json:"logical_bytes"`
	CostDownloadUSD    float64        `json:"cost_download_usd"`
	CostUploadUSD      float64        `json:"cost_upload_usd"`
	CostOneOffUSD      float64        `json:"cost_one_off_usd"`
	SavingsPerMonthUSD float64        `json:"savings_per_month_usd"`
	PaybackMonths      float64        `json:"payback_months,omitempty"`
	EstimatedDuration  string         `json:"estimated_duration"`
	NothingToDo        bool           `json:"nothing_to_do"`
	Reason             string         `json:"reason,omitempty"`
	Rates              optimizeRates  `json:"rates"`
	Prefix             string         `json:"prefix,omitempty"`
}

// optimizeHint is the lightweight suggestion tessera account prints when
// GET /account already shows that packing would free whole blocks.
type optimizeHint struct {
	BlocksNow   uint64 `json:"blocks_now"`
	BlocksAfter uint64 `json:"blocks_after"`
	SavedBytes  uint64 `json:"saved_bytes"`
	Message     string `json:"message"`
}

func cmdOptimize(args []string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	dry := hasFlag(args, "--dry-run")
	yes := hasFlag(args, "--yes") || hasFlag(args, "-y")
	asJSON := hasFlag(args, "--json")
	prefix := ""
	if pos := positional(args); len(pos) > 0 {
		prefix = strings.Trim(filepath.ToSlash(pos[0]), "/")
	}

	rates := listedOptimizeRates()
	if doc, err := fetchRatesDoc(cfg); err == nil {
		rates = ratesFromDoc(doc)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()

	rt, idx, cleanup, err := connectForWrite(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	if !asJSON {
		fmt.Println("Inspecting stored files…")
	}

	infos, err := allLiveInfos(ctx, rt)
	if err != nil {
		fatal("list files: %v", err)
	}
	newest := newestLiveByPath(infos)
	var layouts []optimizeObject
	for _, info := range newest {
		if isReservedRemotePath(info.Path) {
			continue
		}
		ss, err := objectSlabSlices(ctx, rt, info.ObjectID)
		if err != nil {
			fatal("read %s: %v", info.Path, err)
		}
		layouts = append(layouts, layoutFromSlices(info, ss))
	}

	plan := buildOptimizePlan(layouts, prefix, rates)
	apply := !dry && !plan.NothingToDo && (yes || (!asJSON && stdinIsTerminal()))

	if asJSON && !apply {
		emitJSON(optimizeResultJSON(plan, false, 0))
		return
	}
	if !asJSON {
		printOptimizePlan(plan)
	}
	if plan.NothingToDo || dry {
		return
	}
	if !yes {
		ok, asked := promptYesNo("Proceed?", false)
		if !asked || !ok {
			fmt.Println("Aborted.")
			return
		}
	}

	if acct, _, err := fetchAccountSummary(); err == nil {
		if err := refuseOptimizeIfUnfunded(acct, plan.CostOneOffUSD); err != nil {
			fatal("%s", err)
		}
	}

	requireTransferReady(ctx, cfg, true)

	if n, err := cleanupOptimizeLeftovers(ctx, rt, infos); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not unpin leftover copies from a previous run: %v\n", err)
	} else if n > 0 && !asJSON {
		fmt.Printf("Unpinned %d leftover cop%s from a previous run.\n", n, pluralY(n))
	}

	unpinned, err := applyOptimize(ctx, rt, idx, plan)
	if err != nil {
		fatal("%v", explainTransferError(cfg, err, true))
	}
	if asJSON {
		emitJSON(optimizeResultJSON(plan, true, unpinned))
		return
	}
	fmt.Printf("Packed %d file%s into %d block%s. Unpinned %d previous cop%s.\n",
		len(plan.Files), pluralS(len(plan.Files)),
		plan.BlocksAfter, pluralS(plan.BlocksAfter),
		unpinned, pluralY(unpinned))
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func listedOptimizeRates() optimizeRates {
	return optimizeRates{
		StoragePerTBMonth: listedStorageUSDPerTBMonth,
		UploadPerTB:       listedUploadUSDPerTB,
		DownloadPerTB:     listedDownloadUSDPerTB,
		Source:            "listed",
	}
}

func fetchRatesDoc(cfg *Config) (map[string]any, error) {
	c := Config{}
	if cfg != nil {
		c = *cfg
	}
	if c.IndexerURL == "" {
		c.IndexerURL = defaultIndexer
	}
	if c.BillingAPIURL == "" {
		c.BillingAPIURL = defaultBillingURL
	}
	r, ok := newBillingClient(&c).(ratesAPI)
	if !ok {
		return nil, errors.New("this build cannot read the rate card")
	}
	return r.Rates()
}

func ratesFromDoc(doc map[string]any) optimizeRates {
	r := listedOptimizeRates()
	r.Source = "live"
	if v := parseRateField(doc, "storage", "usd_per_tb_month"); v > 0 {
		r.StoragePerTBMonth = v
	}
	if v := parseRateField(doc, "upload", "usd_per_tb"); v > 0 {
		r.UploadPerTB = v
	}
	if v := parseRateField(doc, "download", "usd_per_tb"); v > 0 {
		r.DownloadPerTB = v
	}
	return r
}

func parseRateField(doc map[string]any, section, key string) float64 {
	s := rateStr(ratesSection(doc, section), key)
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

func layoutFromSlices(info PathInfo, ss []slabs.SlabSlice) optimizeObject {
	o := optimizeObject{Info: info}
	for _, s := range ss {
		o.Slabs = append(o.Slabs, optimizeSlabSlice{
			ID:     s.Digest().String(),
			Length: s.Length,
		})
	}
	return o
}

func objectSlabSlices(ctx context.Context, rt *Remote, objectID string) ([]slabs.SlabSlice, error) {
	key, err := parseObjectID(objectID)
	if err != nil {
		return nil, err
	}
	obj, err := rt.sdk.Object(ctx, key)
	if err != nil {
		return nil, err
	}
	return obj.Slabs(), nil
}

func allLiveInfos(ctx context.Context, rt *Remote) ([]PathInfo, error) {
	var out []PathInfo
	err := rt.Events(ctx, func(ev siastorage.ObjectEvent) error {
		if ev.Deleted || ev.Object == nil {
			return nil
		}
		info, ok := pathFromEvent(ev)
		if ok {
			out = append(out, info)
		}
		return nil
	})
	return out, err
}

func newestLiveByPath(infos []PathInfo) []PathInfo {
	latest := make(map[string]PathInfo, len(infos))
	for _, info := range infos {
		prev, ok := latest[info.Path]
		if !ok || info.Updated.After(prev.Updated) {
			latest[info.Path] = info
		}
	}
	out := make([]PathInfo, 0, len(latest))
	for _, info := range latest {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func isReservedRemotePath(p string) bool {
	p = strings.Trim(p, "/")
	for _, ns := range []string{TrashPrefix, VersionsPrefix} {
		if p == ns || strings.HasPrefix(p, ns+"/") || strings.Contains(p, "/"+ns+"/") {
			return true
		}
	}
	return false
}

func pathMatchesPrefix(p, prefix string) bool {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return true
	}
	p = strings.Trim(p, "/")
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}

// buildOptimizePlan works out which objects sit in under-filled slabs (taking
// the closure of anything that shares a slab) and what packing them together
// would save. It never decides for the user: a plan with zero savings is
// reported as nothing to do so we do not spend money to stay in place.
func buildOptimizePlan(objects []optimizeObject, prefix string, rates optimizeRates) optimizePlan {
	plan := optimizePlan{Prefix: prefix, Rates: rates}
	byID := make(map[string]optimizeObject, len(objects))
	slabUsed := map[string]uint64{}
	slabObjects := map[string][]string{}

	for _, o := range objects {
		if o.Info.ObjectID == "" || isReservedRemotePath(o.Info.Path) {
			continue
		}
		byID[o.Info.ObjectID] = o
		seen := map[string]bool{}
		for _, sl := range o.Slabs {
			if sl.ID == "" {
				continue
			}
			slabUsed[sl.ID] += uint64(sl.Length)
			if !seen[sl.ID] {
				slabObjects[sl.ID] = append(slabObjects[sl.ID], o.Info.ObjectID)
				seen[sl.ID] = true
			}
		}
	}

	underfilled := map[string]bool{}
	for id, used := range slabUsed {
		if used < uint64(slabBytes) {
			underfilled[id] = true
		}
	}

	selected := map[string]bool{}
	for _, o := range byID {
		if !pathMatchesPrefix(o.Info.Path, prefix) {
			continue
		}
		for _, sl := range o.Slabs {
			if underfilled[sl.ID] {
				selected[o.Info.ObjectID] = true
				break
			}
		}
	}

	changed := true
	for changed {
		changed = false
		for _, oids := range slabObjects {
			hit := false
			for _, oid := range oids {
				if selected[oid] {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
			for _, oid := range oids {
				if !selected[oid] {
					selected[oid] = true
					changed = true
				}
			}
		}
	}

	nowSlabs := map[string]bool{}
	var files []optimizeFile
	var logical uint64
	for oid := range selected {
		o := byID[oid]
		size := o.Info.Size
		if size < 0 {
			size = 0
		}
		logical += uint64(size)
		files = append(files, optimizeFile{
			Path:     o.Info.Path,
			Size:     o.Info.Size,
			ObjectID: o.Info.ObjectID,
			SHA256:   o.Info.SHA256,
			ModTime:  o.Info.ModTime,
			Root:     o.Info.Root,
			Shared:   prefix != "" && !pathMatchesPrefix(o.Info.Path, prefix),
		})
		for _, sl := range o.Slabs {
			if sl.ID != "" {
				nowSlabs[sl.ID] = true
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	now := len(nowSlabs)
	after := 0
	if logical > 0 {
		after = int((logical + uint64(slabBytes) - 1) / uint64(slabBytes))
	}
	plan.Files = files
	plan.BlocksNow = now
	plan.BlocksAfter = after
	plan.LogicalBytes = logical
	plan.StoredBytesNow = uint64(now) * uint64(slabBytes)
	plan.StoredBytesAfter = uint64(after) * uint64(slabBytes)
	if plan.StoredBytesNow > plan.StoredBytesAfter {
		plan.SavedBytes = plan.StoredBytesNow - plan.StoredBytesAfter
	}

	if now == 0 {
		plan.NothingToDo = true
		plan.Reason = "No under-filled blocks found."
		return plan
	}
	if after >= now {
		plan.NothingToDo = true
		plan.Reason = "Stored blocks are already as few as the data requires."
		return plan
	}

	plan.CostDownloadUSD = usdForBytes(logical, rates.DownloadPerTB)
	plan.CostUploadUSD = usdForBytes(plan.StoredBytesAfter, rates.UploadPerTB)
	plan.CostOneOffUSD = plan.CostDownloadUSD + plan.CostUploadUSD
	plan.SavingsPerMonthUSD = usdForBytes(plan.SavedBytes, rates.StoragePerTBMonth)
	if plan.SavingsPerMonthUSD > 0 {
		plan.PaybackMonths = plan.CostOneOffUSD / plan.SavingsPerMonthUSD
	}
	plan.EstimatedDuration = formatPlanDuration(estimateOptimizeDuration(logical, plan.StoredBytesAfter))
	return plan
}

func usdForBytes(bytes uint64, usdPerTB float64) float64 {
	return float64(bytes) / bytesPerTB * usdPerTB
}

func estimateOptimizeDuration(downloadBytes, uploadBytes uint64) time.Duration {
	secs := float64(downloadBytes+uploadBytes)/float64(optimizeXferBytesPerSec) + 5
	if secs < 5 {
		secs = 5
	}
	return time.Duration(secs * float64(time.Second))
}

func formatPlanUSD(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "$0.00"
	}
	return "$" + displayUSD(strconv.FormatFloat(f, 'f', 10, 64))
}

func formatPlanDuration(d time.Duration) string {
	if d < 2*time.Second {
		return "a few seconds"
	}
	if d < time.Minute {
		return fmt.Sprintf("about %ds", int(d.Round(time.Second).Seconds()))
	}
	if d < time.Hour {
		mins := int(math.Round(d.Minutes()))
		if mins < 1 {
			mins = 1
		}
		if mins == 1 {
			return "about 1 minute"
		}
		return fmt.Sprintf("about %d minutes", mins)
	}
	return fmt.Sprintf("about %.1f hours", d.Hours())
}

func formatPayback(months float64) string {
	if months <= 0 || math.IsInf(months, 0) || math.IsNaN(months) {
		return "n/a"
	}
	if months < 1 {
		days := months * 30
		if days < 1 {
			return "less than a day"
		}
		return fmt.Sprintf("about %.0f days", days)
	}
	if months < 18 {
		return fmt.Sprintf("about %.1f months", months)
	}
	return fmt.Sprintf("about %.1f years", months/12)
}

func printOptimizePlan(plan optimizePlan) {
	fmt.Println("Optimize plan")
	fmt.Println("-------------")
	if plan.Prefix != "" {
		fmt.Printf("Prefix: %s\n", plan.Prefix)
	}
	if len(plan.Files) == 0 {
		fmt.Println(plan.Reason)
		return
	}
	fmt.Printf("Files (%d):\n", len(plan.Files))
	for _, f := range plan.Files {
		note := ""
		if f.Shared {
			note = "  (shares a block with a file under the prefix)"
		}
		fmt.Printf("  %-44s  %10s%s\n", truncate(f.Path, 44), formatBytes(uint64(f.Size)), note)
	}
	fmt.Println()
	fmt.Printf("Blocks now:               %d  (%s stored)\n", plan.BlocksNow, formatBytes(plan.StoredBytesNow))
	fmt.Printf("Blocks after:             %d  (%s stored)\n", plan.BlocksAfter, formatBytes(plan.StoredBytesAfter))
	if plan.SavedBytes > 0 {
		fmt.Printf("Savings:                  %d blocks (%s stored)\n", plan.BlocksNow-plan.BlocksAfter, formatBytes(plan.SavedBytes))
	} else {
		fmt.Printf("Savings:                  none\n")
	}
	fmt.Println()
	if plan.NothingToDo {
		if plan.Reason != "" {
			fmt.Println(plan.Reason)
		}
		return
	}
	src := "live rates"
	if plan.Rates.Source != "live" {
		src = "listed rates"
	}
	fmt.Printf("One-off cost (%s):\n", src)
	fmt.Printf("  Download (%s real data): %s\n", formatBytes(plan.LogicalBytes), formatPlanUSD(plan.CostDownloadUSD))
	fmt.Printf("  Upload (%d new block%s):           %s\n", plan.BlocksAfter, pluralS(plan.BlocksAfter), formatPlanUSD(plan.CostUploadUSD))
	fmt.Printf("  Total:                            %s\n", formatPlanUSD(plan.CostOneOffUSD))
	fmt.Println()
	fmt.Printf("Monthly storage savings:            %s\n", formatPlanUSD(plan.SavingsPerMonthUSD))
	fmt.Printf("Estimated payback:                  %s\n", formatPayback(plan.PaybackMonths))
	fmt.Printf("Estimated time to run:              %s\n", plan.EstimatedDuration)
}

func optimizeResultJSON(plan optimizePlan, applied bool, unpinned int) map[string]any {
	return map[string]any{
		"files":                 plan.Files,
		"blocks_now":            plan.BlocksNow,
		"blocks_after":          plan.BlocksAfter,
		"stored_bytes_now":      plan.StoredBytesNow,
		"stored_bytes_after":    plan.StoredBytesAfter,
		"saved_bytes":           plan.SavedBytes,
		"logical_bytes":         plan.LogicalBytes,
		"cost_download_usd":     plan.CostDownloadUSD,
		"cost_upload_usd":       plan.CostUploadUSD,
		"cost_one_off_usd":      plan.CostOneOffUSD,
		"savings_per_month_usd": plan.SavingsPerMonthUSD,
		"payback_months":        plan.PaybackMonths,
		"estimated_duration":    plan.EstimatedDuration,
		"nothing_to_do":         plan.NothingToDo,
		"reason":                plan.Reason,
		"rates":                 plan.Rates,
		"prefix":                plan.Prefix,
		"applied":               applied,
		"unpinned":              unpinned,
	}
}

func parseBalanceFloat(acct map[string]any) (float64, bool) {
	v, ok := acct["balance_usd"]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

func uploadsAllowedDespiteZero(acct map[string]any) bool {
	if r, ok := acct["readiness"].(map[string]any); ok {
		if v, ok := r["can_upload"].(bool); ok {
			return v
		}
		if v, ok := r["top_up_required"].(bool); ok && v {
			return false
		}
	}
	if v := buildTrialView(acct); v != nil {
		return v.CanUpload
	}
	return false
}

func refuseOptimizeIfUnfunded(acct map[string]any, cost float64) error {
	bal, ok := parseBalanceFloat(acct)
	if !ok {
		return nil
	}
	if bal+1e-12 >= cost {
		return nil
	}
	if bal <= 0 && uploadsAllowedDespiteZero(acct) {
		// Technical validation may still cover a single-slab upload; let the
		// server decide and surface a 402 through the usual path if not.
		return nil
	}
	msg := fmt.Sprintf("Insufficient balance — top up to continue (this run costs %s; balance is %s)", formatPlanUSD(cost), formatPlanUSD(bal))
	return &paymentRequiredError{Message: msg}
}

func cleanupOptimizeLeftovers(ctx context.Context, rt *Remote, infos []PathInfo) (int, error) {
	byPath := map[string][]PathInfo{}
	for _, info := range infos {
		if isReservedRemotePath(info.Path) || info.ObjectID == "" {
			continue
		}
		byPath[info.Path] = append(byPath[info.Path], info)
	}
	n := 0
	for _, list := range byPath {
		if len(list) < 2 {
			continue
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Updated.After(list[j].Updated) })
		keep := list[0]
		for _, extra := range list[1:] {
			if extra.SHA256 == "" || extra.SHA256 != keep.SHA256 || extra.ObjectID == keep.ObjectID {
				continue
			}
			if err := rt.DeleteObject(ctx, extra.ObjectID); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

func applyOptimize(ctx context.Context, rt *Remote, idx *Index, plan optimizePlan) (int, error) {
	tmp, err := os.MkdirTemp("", "tessera-optimize-*")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(tmp)

	type item struct {
		file  optimizeFile
		local string
	}
	items := make([]item, 0, len(plan.Files))
	for i, f := range plan.Files {
		dest := filepath.Join(tmp, fmt.Sprintf("%d-%s", i, path.Base(f.Path)))
		info := PathInfo{
			ObjectID: f.ObjectID,
			Path:     f.Path,
			Name:     path.Base(f.Path),
			Root:     f.Root,
			SHA256:   f.SHA256,
			Size:     f.Size,
			ModTime:  f.ModTime,
		}
		if _, err := rt.Download(ctx, info, dest, 0644); err != nil {
			return 0, fmt.Errorf("download %s: %w", f.Path, err)
		}
		hash := f.SHA256
		if hash == "" {
			h, herr := hashFile(dest)
			if herr != nil {
				return 0, fmt.Errorf("hash %s: %w", f.Path, herr)
			}
			hash = h
			f.SHA256 = h
		}
		items = append(items, item{file: f, local: dest})
		_ = hash
	}

	reqs := make([]uploadRequest, len(items))
	for i, it := range items {
		reqs[i] = uploadRequest{
			LocalPath: it.local,
			RelPath:   it.file.Path,
			Name:      path.Base(it.file.Path),
			Hash:      it.file.SHA256,
			Size:      it.file.Size,
			ModTime:   it.file.ModTime,
			Root:      it.file.Root,
		}
	}
	outcomes := rt.UploadBatch(ctx, reqs, nil)
	for i, oc := range outcomes {
		f := items[i].file
		if oc.Err != nil {
			return 0, fmt.Errorf("upload %s: %w", f.Path, oc.Err)
		}
		if oc.Info.SHA256 != "" && f.SHA256 != "" && oc.Info.SHA256 != f.SHA256 {
			return 0, fmt.Errorf("verify %s: stored hash does not match", f.Path)
		}
		if oc.Info.Size != 0 && oc.Info.Size != f.Size {
			return 0, fmt.Errorf("verify %s: stored size %d != %d", f.Path, oc.Info.Size, f.Size)
		}
		if oc.Info.Path != "" && oc.Info.Path != f.Path {
			return 0, fmt.Errorf("verify %s: stored at %s", f.Path, oc.Info.Path)
		}
	}

	unpinned := 0
	for i, it := range items {
		newInfo := outcomes[i].Info
		if newInfo.Path == "" {
			newInfo.Path = it.file.Path
		}
		if newInfo.Name == "" {
			newInfo.Name = path.Base(it.file.Path)
		}
		if newInfo.SHA256 == "" {
			newInfo.SHA256 = it.file.SHA256
		}
		if newInfo.Size == 0 {
			newInfo.Size = it.file.Size
		}
		if newInfo.ModTime.IsZero() {
			newInfo.ModTime = it.file.ModTime
		}
		if newInfo.Root == "" {
			newInfo.Root = it.file.Root
		}
		oldID := it.file.ObjectID
		if oldID != "" && oldID != newInfo.ObjectID {
			if err := rt.DeleteObject(ctx, oldID); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not unpin previous copy of %s: %v\n", it.file.Path, err)
			} else {
				unpinned++
			}
		}
		idx.put(newInfo)
		recordReplaceInSyncState(it.file.Path, newInfo)
	}
	if err := updateIndex(func(fresh *Index) error {
		for _, it := range items {
			if ent := idx.Paths[it.file.Path]; ent != nil {
				fresh.Paths[it.file.Path] = ent
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not update local index: %v\n", err)
	}
	return unpinned, nil
}

func recordReplaceInSyncState(remotePath string, info PathInfo) {
	roots, err := listSyncRoots()
	if err != nil {
		return
	}
	for i := range roots {
		r := &roots[i]
		rel, ours := (&SyncEngine{cfg: r}).localPathForRemote(remotePath)
		if !ours {
			continue
		}
		st, err := loadSyncState(r.ID)
		if err != nil {
			continue
		}
		copied := info
		copied.Path = rel
		st.applyRemoteEvent(rel, copied)
		ent := st.entry(rel)
		if info.SHA256 != "" {
			ent.BaseSHA256 = info.SHA256
		}
		ent.ObjectID = info.ObjectID
		ent.RemotePath = remotePath
		ent.Size = info.Size
		ent.ModTime = info.ModTime
		ent.DeletedLocal = false
		ent.DeletedRemote = false
		_ = st.save()
	}
}

// optimizeHintFromAccount derives a cheap packing hint from GET /account
// fields already in hand (open_slabs or stored_bytes, plus raw_bytes). It
// never makes another network call and returns ok=false on missing data.
func optimizeHintFromAccount(acct map[string]any) (optimizeHint, bool) {
	var hint optimizeHint
	if acct == nil {
		return hint, false
	}
	raw, hasRaw := accountUint64(acct, "raw_bytes")
	if !hasRaw {
		return hint, false
	}
	// Billed size is stored_bytes (whole 40 MiB blocks). open_slabs can
	// under-count after separate uploads have sealed, so prefer the larger
	// of the two when both are present.
	var slabsNow uint64
	var haveNow bool
	if stored, ok := accountUint64(acct, "stored_bytes"); ok {
		slabsNow = (stored + uint64(slabBytes) - 1) / uint64(slabBytes)
		haveNow = true
	}
	if n, ok := accountUint64(acct, "open_slabs"); ok {
		if !haveNow || n > slabsNow {
			slabsNow = n
			haveNow = true
		}
	}
	if !haveNow {
		return hint, false
	}
	after := (raw + uint64(slabBytes) - 1) / uint64(slabBytes)
	if raw == 0 {
		after = 0
	}
	if slabsNow <= after {
		return hint, false
	}
	saved := (slabsNow - after) * uint64(slabBytes)
	hint = optimizeHint{
		BlocksNow:   slabsNow,
		BlocksAfter: after,
		SavedBytes:  saved,
		Message:     fmt.Sprintf("%d blocks could be packed into %d, saving %s, run tessera optimize", slabsNow, after, formatBytes(saved)),
	}
	return hint, true
}
