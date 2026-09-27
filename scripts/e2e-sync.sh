#!/usr/bin/env bash
# End-to-end sync test: two simulated machines sharing one Tessera account.
#
# Prerequisites: an authenticated config at ~/.tessera/config.json and a built
# binary (go build -o tessera .). See TESTING.md for the full checklist.
# Override the binary with TESSERA_BIN=/path/to/tessera.
#
# Each run uploads real data under a unique remote prefix and leaves it behind;
# purge it afterwards with the cleanup snippet in TESTING.md.
set -uo pipefail
BIN="${TESSERA_BIN:-$(cd "$(dirname "$0")/.." && pwd)/tessera}"
STAMP=$(date +%s)
ROOT=/tmp/tessera-e2e
MAC=$ROOT/machineA
WIN=$ROOT/machineB
REMOTE=tessera/e2e-$STAMP
PASS=0; FAIL=0
ok(){ echo "RESULT ✓ $1"; PASS=$((PASS+1)); }
bad(){ echo "RESULT ✗ $1"; FAIL=$((FAIL+1)); }
chk(){ if [ "$2" = "$3" ]; then ok "$1 ($2)"; else bad "$1: got '$2' want '$3'"; fi; }
chkcontains(){ if echo "$2" | grep -q "$3"; then ok "$1"; else bad "$1: '$2' lacks '$3'"; fi; }
step(){ echo; echo "=== $* ==="; }

rm -rf "$ROOT"; mkdir -p "$MAC" "$WIN" "$ROOT/homeA" "$ROOT/homeB"
cp ~/.tessera/config.json "$ROOT/homeA/config.json"
cp ~/.tessera/config.json "$ROOT/homeB/config.json"
A="env TESSERA_HOME=$ROOT/homeA $BIN"
B="env TESSERA_HOME=$ROOT/homeB $BIN"

step "1. machine A uploads a folder"
mkdir -p "$MAC/Documents/notes" "$MAC/Documents/assets"
echo "quarterly report v1" > "$MAC/Documents/report.txt"
echo "meeting notes"       > "$MAC/Documents/notes/2026-09.txt"
head -c 100000 /dev/urandom > "$MAC/Documents/assets/blob.bin"
echo "junk" > "$MAC/Documents/.DS_Store"
mkdir -p "$MAC/Documents/.git"; echo "x" > "$MAC/Documents/.git/config"
$A upload "$MAC/Documents" --as "$REMOTE" 2>&1 | tail -8

step "2. machine A registers and syncs"
$A sync add "$MAC/Documents" --as "$REMOTE" >"$ROOT/add.out" 2>&1
cat "$ROOT/add.out" | grep -E 'Local:|Remote:|Root ID:'
ROOT_ID=$(grep 'Root ID:' "$ROOT/add.out" | awk '{print $3}')
chkcontains "root id recorded" "$ROOT_ID" '^[0-9a-f]\{16\}$'
$A sync --root "$ROOT_ID" 2>&1 | tail -3

step "3. machine B adopts the same remote prefix"
$B sync add "$WIN/tessera/macos/Documents" --as "$REMOTE" >"$ROOT/addB.out" 2>&1
ROOT_B=$(grep 'Root ID:' "$ROOT/addB.out" | awk '{print $3}')
$B sync --root "$ROOT_B" 2>&1 | tail -3
echo "--- files on machine B ---"
(cd "$WIN/tessera/macos/Documents" && find . -type f | sed 's|^\./||' | sort)

step "4. trees identical and junk ignored"
# Compare only what should have synced: ignored files stay local.
DIFF=$(diff -r -x '.DS_Store' -x '.git' "$MAC/Documents" "$WIN/tessera/macos/Documents" 2>&1)
if [ -z "$DIFF" ]; then
  ok "synced trees are identical"
else
  bad "trees differ"; echo "$DIFF" | head -8
fi
[ -e "$WIN/tessera/macos/Documents/.DS_Store" ] && bad ".DS_Store synced" || ok ".DS_Store not synced"
[ -e "$WIN/tessera/macos/Documents/.git" ] && bad ".git synced" || ok ".git not synced"

step "5. new file on B arrives on A"
echo "created on windows" > "$WIN/tessera/macos/Documents/from-windows.txt"
$B sync --root "$ROOT_B" >/dev/null 2>&1
$A sync --root "$ROOT_ID" 2>&1 | tail -2
chk "file arrived on A" "$(cat "$MAC/Documents/from-windows.txt" 2>/dev/null)" "created on windows"

step "6. delete on B propagates to A"
rm "$WIN/tessera/macos/Documents/notes/2026-09.txt"
$B sync --root "$ROOT_B" >/dev/null 2>&1
$A sync --root "$ROOT_ID" 2>&1 | tail -2
[ -e "$MAC/Documents/notes/2026-09.txt" ] && bad "delete did not propagate to A" || ok "delete propagated B→A"

step "7. delete on A propagates to B"
rm "$MAC/Documents/report.txt"
echo "--- A state before ---"
python3 -c "
import json,glob
f=glob.glob('$ROOT/homeA/sync/*/state.json')[0]
d=json.load(open(f))
print(' entries:', {k:v for k,v in d['entries'].items() if 'report' in k})
print(' remote :', {k:v for k,v in d['remote'].items() if 'report' in k})
"
$A sync --root "$ROOT_ID" --verbose 2>&1 | tail -6
echo "--- A state after ---"
python3 -c "
import json,glob
f=glob.glob('$ROOT/homeA/sync/*/state.json')[0]
d=json.load(open(f))
print(' entries:', {k:v for k,v in d['entries'].items() if 'report' in k})
print(' remote :', {k:v for k,v in d['remote'].items() if 'report' in k})
"
$B sync --root "$ROOT_B" --verbose 2>&1 | tail -6
[ -e "$WIN/tessera/macos/Documents/report.txt" ] && bad "delete did not propagate to B" || ok "delete propagated A→B"

step "8. new file on A arrives on B"
echo "second report" > "$MAC/Documents/report2.txt"
$A sync --root "$ROOT_ID" >/dev/null 2>&1
$B sync --root "$ROOT_B" 2>&1 | tail -2
chk "file arrived on B" "$(cat "$WIN/tessera/macos/Documents/report2.txt" 2>/dev/null)" "second report"

step "9. rename is metadata-only"
mv "$MAC/Documents/report2.txt" "$MAC/Documents/renamed-report.txt"
$A sync --root "$ROOT_ID" 2>&1 | tail -2
$B sync --root "$ROOT_B" 2>&1 | tail -2
chk "rename arrived on B" "$(cat "$WIN/tessera/macos/Documents/renamed-report.txt" 2>/dev/null)" "second report"
[ -e "$WIN/tessera/macos/Documents/report2.txt" ] && bad "old name still present on B" || ok "old name gone on B"

step "10. conflict: newest wins by default"
echo "A v1" > "$MAC/Documents/conflict.txt"; $A sync --root "$ROOT_ID" >/dev/null 2>&1
$B sync --root "$ROOT_B" >/dev/null 2>&1
sleep 1.2
echo "B newer" > "$WIN/tessera/macos/Documents/conflict.txt"; $B sync --root "$ROOT_B" >/dev/null 2>&1
sleep 1.2
echo "A newest" > "$MAC/Documents/conflict.txt"; $A sync --root "$ROOT_ID" >/dev/null 2>&1
$B sync --root "$ROOT_B" >/dev/null 2>&1
$A sync --root "$ROOT_ID" >/dev/null 2>&1
chk "newest wins converged" "$(cat "$WIN/tessera/macos/Documents/conflict.txt")" "A newest"

step "11. conflict: keep-both policy"
mkdir -p "$MAC/keepboth" "$WIN/keepboth"
$A sync add "$MAC/keepboth" --as "$REMOTE-keepboth" --policy keep-both >/dev/null 2>&1
$B sync add "$WIN/keepboth" --as "$REMOTE-keepboth" --policy keep-both >/dev/null 2>&1
KA=$($A sync list --json | python3 -c "import json,sys;print([r['id'] for r in json.load(sys.stdin) if r['local_path'].endswith('keepboth')][0])")
KB=$($B sync list --json | python3 -c "import json,sys;print([r['id'] for r in json.load(sys.stdin) if r['local_path'].endswith('keepboth')][0])")
echo "shared base" > "$MAC/keepboth/shared.txt"
$A sync --root "$KA" >/dev/null 2>&1; $B sync --root "$KB" >/dev/null 2>&1
sleep 1.2; echo "mac edit" > "$MAC/keepboth/shared.txt"; $A sync --root "$KA" >/dev/null 2>&1
sleep 1.2; echo "windows edit" > "$WIN/keepboth/shared.txt"; $B sync --root "$KB" >/dev/null 2>&1
$A sync --root "$KA" >/dev/null 2>&1; $B sync --root "$KB" >/dev/null 2>&1; $A sync --root "$KA" >/dev/null 2>&1
echo "--- mac keepboth ---"; ls "$MAC/keepboth"
echo "--- windows keepboth ---"; ls "$WIN/keepboth"
CONF=$($A sync conflicts 2>&1 | head -5)
chkcontains "conflict copy recorded" "$CONF" "conflicted copy"
N=$(ls "$WIN/keepboth" | wc -l | tr -d ' ')
chk "both versions present on windows" "$N" "2"

step "12. dry-run changes nothing"
echo "dry run file" > "$MAC/Documents/dryrun.txt"
$A sync --root "$ROOT_ID" --dry-run 2>&1 | tail -3
[ -e "$WIN/tessera/macos/Documents/dryrun.txt" ] && bad "dry-run pushed a file" || ok "dry-run made no changes"
$A sync --root "$ROOT_ID" >/dev/null 2>&1; $B sync --root "$ROOT_B" >/dev/null 2>&1
[ -e "$WIN/tessera/macos/Documents/dryrun.txt" ] && ok "real sync pushed it" || bad "real sync failed"

step "13. trash soft-delete and restore"
$A delete "$REMOTE/from-windows.txt" --yes 2>&1 | head -3
$A trash list 2>&1 | head -5
$A trash restore "$REMOTE/from-windows.txt" 2>&1 | head -3
$B sync --root "$ROOT_B" >/dev/null 2>&1
[ -e "$WIN/tessera/macos/Documents/from-windows.txt" ] || echo "  (restored file not yet re-fetched)"

step "14. versions and audit"
$A config --version-retention 2 >/dev/null 2>&1
# Establish the current object in state, then replace it twice.
$A sync --root "$ROOT_ID" >/dev/null 2>&1
echo "v2" > "$MAC/Documents/assets/blob.bin"
$A sync --root "$ROOT_ID" 2>&1 | tail -2
echo "v3" > "$MAC/Documents/assets/blob.bin"
$A sync --root "$ROOT_ID" 2>&1 | tail -2
$A versions "assets/blob.bin" 2>&1 | head -8
$A usage --by-folder 2>&1 | head -12
$A audit 2>&1 | tail -6

step "15. find"
$A find report --json 2>&1 | head -12

echo
echo "======== RESULT: $PASS passed, $FAIL failed ========"
exit $((FAIL>0))
