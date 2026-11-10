package main

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func optObj(path string, size int64, id string, slabs ...optimizeSlabSlice) optimizeObject {
	return optimizeObject{
		Info:  PathInfo{Path: path, Size: size, ObjectID: id, SHA256: "h-" + id},
		Slabs: slabs,
	}
}

func optSlab(id string, length uint32) optimizeSlabSlice {
	return optimizeSlabSlice{ID: id, Length: length}
}

func TestBuildOptimizePlanSeparateUploads(t *testing.T) {
	const n = 5
	const fileSize int64 = 1 << 20
	var objects []optimizeObject
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("%d", i)
		objects = append(objects, optObj("folder-"+id+"/file.txt", fileSize, "obj"+id, optSlab("slab"+id, uint32(fileSize))))
	}
	plan := buildOptimizePlan(objects, "", listedOptimizeRates())
	if plan.NothingToDo {
		t.Fatalf("expected a packing opportunity, got %#v", plan)
	}
	if plan.BlocksNow != n || plan.BlocksAfter != 1 {
		t.Fatalf("blocks now/after = %d/%d, want %d/1", plan.BlocksNow, plan.BlocksAfter, n)
	}
	if len(plan.Files) != n {
		t.Fatalf("files = %d, want %d", len(plan.Files), n)
	}
	wantSaved := uint64(n-1) * uint64(slabBytes)
	if plan.SavedBytes != wantSaved {
		t.Fatalf("saved = %d, want %d", plan.SavedBytes, wantSaved)
	}
	wantLogical := uint64(n) * uint64(fileSize)
	if plan.LogicalBytes != wantLogical {
		t.Fatalf("logical = %d, want %d", plan.LogicalBytes, wantLogical)
	}
	wantDL := usdForBytes(wantLogical, listedDownloadUSDPerTB)
	wantUL := usdForBytes(uint64(slabBytes), listedUploadUSDPerTB)
	if math.Abs(plan.CostDownloadUSD-wantDL) > 1e-18 || math.Abs(plan.CostUploadUSD-wantUL) > 1e-18 {
		t.Fatalf("costs dl=%g ul=%g, want dl=%g ul=%g", plan.CostDownloadUSD, plan.CostUploadUSD, wantDL, wantUL)
	}
	if math.Abs(plan.CostOneOffUSD-(wantDL+wantUL)) > 1e-18 {
		t.Fatalf("one-off %g, want %g", plan.CostOneOffUSD, wantDL+wantUL)
	}
	wantMonthly := usdForBytes(wantSaved, listedStorageUSDPerTBMonth)
	if math.Abs(plan.SavingsPerMonthUSD-wantMonthly) > 1e-18 {
		t.Fatalf("monthly %g, want %g", plan.SavingsPerMonthUSD, wantMonthly)
	}
	if plan.PaybackMonths <= 0 {
		t.Fatalf("payback months = %g, want > 0", plan.PaybackMonths)
	}
	if plan.EstimatedDuration == "" {
		t.Fatal("missing estimated duration")
	}
}

func TestBuildOptimizePlanAlreadyPacked(t *testing.T) {
	const fileSize uint32 = 1 << 20
	objects := []optimizeObject{
		optObj("a.txt", int64(fileSize), "oa", optSlab("shared", fileSize)),
		optObj("b.txt", int64(fileSize), "ob", optSlab("shared", fileSize)),
		optObj("c.txt", int64(fileSize), "oc", optSlab("shared", fileSize)),
	}
	plan := buildOptimizePlan(objects, "", listedOptimizeRates())
	if !plan.NothingToDo || plan.BlocksNow != 1 || plan.BlocksAfter != 1 {
		t.Fatalf("already-packed plan = %#v", plan)
	}
}

func TestBuildOptimizePlanTailSlabPlusSmallFile(t *testing.T) {
	// A 50 MiB file occupies one full slab plus a 10 MiB tail; a separate
	// 1 MiB file occupies a third slab. Packed together they fit in two.
	const MiB = 1 << 20
	objects := []optimizeObject{
		optObj("big.bin", 50*MiB, "big",
			optSlab("full", uint32(slabBytes)),
			optSlab("tail", 10*MiB)),
		optObj("small.txt", MiB, "small", optSlab("tiny", MiB)),
	}
	plan := buildOptimizePlan(objects, "", listedOptimizeRates())
	if plan.NothingToDo || plan.BlocksNow != 3 || plan.BlocksAfter != 2 {
		t.Fatalf("got now=%d after=%d nothing=%v", plan.BlocksNow, plan.BlocksAfter, plan.NothingToDo)
	}
}

func TestBuildOptimizePlanPrefixAndSharedClosure(t *testing.T) {
	const MiB = 1 << 20
	objects := []optimizeObject{
		optObj("keep/a.txt", MiB, "oa", optSlab("s1", MiB)),
		optObj("other/b.txt", MiB, "ob", optSlab("s1", MiB)), // shares s1
		optObj("skip/c.txt", MiB, "oc", optSlab("s2", MiB)),
	}
	plan := buildOptimizePlan(objects, "keep", listedOptimizeRates())
	// a and b share an already-partial slab; packing them alone cannot drop
	// below one block (2 MiB). skip/c is outside the prefix and does not share.
	if plan.BlocksNow != 1 {
		t.Fatalf("now = %d, want 1 (only the shared slab)", plan.BlocksNow)
	}
	if !plan.NothingToDo {
		t.Fatalf("shared pair under a prefix should not claim a saving: %#v", plan)
	}

	// Two separately uploaded files under the prefix, plus an outsider on
	// its own slab: only the prefix files are selected.
	objects = []optimizeObject{
		optObj("keep/a.txt", MiB, "oa", optSlab("s1", MiB)),
		optObj("keep/b.txt", MiB, "ob", optSlab("s2", MiB)),
		optObj("skip/c.txt", MiB, "oc", optSlab("s3", MiB)),
	}
	plan = buildOptimizePlan(objects, "keep", listedOptimizeRates())
	if plan.NothingToDo || plan.BlocksNow != 2 || plan.BlocksAfter != 1 || len(plan.Files) != 2 {
		t.Fatalf("prefix plan = now=%d after=%d files=%d nothing=%v", plan.BlocksNow, plan.BlocksAfter, len(plan.Files), plan.NothingToDo)
	}
	for _, f := range plan.Files {
		if strings.HasPrefix(f.Path, "skip/") {
			t.Fatalf("outsider included: %s", f.Path)
		}
	}

	// A file outside the prefix that shares a slab with a selected file
	// must be pulled in so the shared slab can be unpinned.
	objects = []optimizeObject{
		optObj("keep/a.txt", MiB, "oa", optSlab("s1", MiB)),
		optObj("other/b.txt", MiB, "ob", optSlab("s1", MiB), optSlab("s2", MiB)),
		optObj("keep/c.txt", MiB, "oc", optSlab("s3", MiB)),
	}
	plan = buildOptimizePlan(objects, "keep", listedOptimizeRates())
	paths := map[string]optimizeFile{}
	for _, f := range plan.Files {
		paths[f.Path] = f
	}
	if _, ok := paths["other/b.txt"]; !ok {
		t.Fatalf("shared sibling not pulled in: %v", paths)
	}
	if !paths["other/b.txt"].Shared {
		t.Fatal("shared sibling should be marked Shared")
	}
}

func TestBuildOptimizePlanSkipsTrashAndVersions(t *testing.T) {
	const MiB = 1 << 20
	objects := []optimizeObject{
		optObj(TrashPrefix+"/old.txt", MiB, "ot", optSlab("st", MiB)),
		optObj(VersionsPrefix+"/old.txt/1", MiB, "ov", optSlab("sv", MiB)),
		optObj("live.txt", MiB, "ol", optSlab("sl", MiB)),
	}
	plan := buildOptimizePlan(objects, "", listedOptimizeRates())
	if plan.BlocksNow != 1 || len(plan.Files) != 1 || plan.Files[0].Path != "live.txt" {
		t.Fatalf("reserved paths leaked into the plan: %#v", plan)
	}
	if !plan.NothingToDo {
		t.Fatal("a single under-filled live file cannot pack further")
	}
}

func TestOptimizeHintFromAccount(t *testing.T) {
	_, ok := optimizeHintFromAccount(map[string]any{
		"raw_bytes":  float64(125829120),
		"open_slabs": float64(3),
	})
	if ok {
		t.Fatal("exactly packed fixture should not hint")
	}

	h, ok := optimizeHintFromAccount(map[string]any{
		"raw_bytes":  float64(5 << 20),
		"open_slabs": float64(10),
	})
	if !ok {
		t.Fatal("expected a hint for 10 slabs holding 5 MiB")
	}
	if h.BlocksNow != 10 || h.BlocksAfter != 1 {
		t.Fatalf("hint blocks %d → %d", h.BlocksNow, h.BlocksAfter)
	}
	if h.SavedBytes != 9*uint64(slabBytes) {
		t.Fatalf("saved %d", h.SavedBytes)
	}
	if !strings.Contains(h.Message, "10 blocks could be packed into 1") {
		t.Fatalf("message = %q", h.Message)
	}
	if !strings.Contains(h.Message, "run tessera optimize") {
		t.Fatalf("message missing command: %q", h.Message)
	}

	if _, ok := optimizeHintFromAccount(map[string]any{"open_slabs": float64(4)}); ok {
		t.Fatal("missing raw_bytes must skip the hint")
	}
	if _, ok := optimizeHintFromAccount(nil); ok {
		t.Fatal("nil account must skip the hint")
	}

	// Fall back to stored_bytes when open_slabs is absent.
	h, ok = optimizeHintFromAccount(map[string]any{
		"raw_bytes":    float64(1 << 20),
		"stored_bytes": float64(4 * slabBytes),
	})
	if !ok || h.BlocksNow != 4 || h.BlocksAfter != 1 {
		t.Fatalf("stored_bytes fallback hint = %#v ok=%v", h, ok)
	}

	// Live GET /account can report open_slabs=1 after separate uploads while
	// stored_bytes still shows five billed 40 MiB blocks. Use the billed size.
	h, ok = optimizeHintFromAccount(map[string]any{
		"raw_bytes":    float64(5 << 20),
		"open_slabs":   float64(1),
		"stored_bytes": float64(5 * slabBytes),
	})
	if !ok || h.BlocksNow != 5 || h.BlocksAfter != 1 {
		t.Fatalf("stored_bytes must win over a low open_slabs: %#v ok=%v", h, ok)
	}
}

func TestFormatPlanUSDSmallAmounts(t *testing.T) {
	// One 40 MiB upload at $1/TB is $0.00004194304; displayUSD rounds to
	// six decimals below $1, matching tessera rates.
	s := formatPlanUSD(usdForBytes(uint64(slabBytes), 1))
	if s != "$0.000042" {
		t.Fatalf("upload of one block = %s, want $0.000042", s)
	}
}

func TestRefuseOptimizeIfUnfunded(t *testing.T) {
	if err := refuseOptimizeIfUnfunded(map[string]any{"balance_usd": 1.0}, 0.0001); err != nil {
		t.Fatalf("funded account refused: %v", err)
	}
	err := refuseOptimizeIfUnfunded(map[string]any{"balance_usd": 0.000001}, 0.01)
	if err == nil {
		t.Fatal("expected a top-up error when balance is below the one-off cost")
	}
	if _, ok := err.(*paymentRequiredError); !ok {
		t.Fatalf("error type %T, want paymentRequiredError", err)
	}
	// $0 with an unused technical-validation upload is allowed to try.
	if err := refuseOptimizeIfUnfunded(map[string]any{
		"balance_usd": 0.0,
		"readiness":   map[string]any{"can_upload": true},
	}, 0.01); err != nil {
		t.Fatalf("validation allowance should be allowed to try: %v", err)
	}
}
