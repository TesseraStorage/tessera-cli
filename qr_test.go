package main

import (
	"strings"
	"testing"
	"time"
)

func TestUSDCBaseUnits(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"1", "1000000", true},
		{"0.38", "380000", true},
		{"10.199555", "10199555", true},
		{"0.000001", "1", true},
		{"0.0000004", "0", true},
		{"0.0000005", "1", true},
		{"1.2345674", "1234567", true},
		{"1.2345675", "1234568", true},
		{"0001.50", "1500000", true},
		{".5", "500000", true},
		{"", "", false},
		{"—", "", false},
		{"abc", "", false},
		{"1e3", "", false},
		{"-1", "", false},
		{"1.2.3", "", false},
	}
	for _, tc := range cases {
		got, err := usdcBaseUnits(tc.in)
		if tc.ok {
			if err != nil || got != tc.want {
				t.Errorf("usdcBaseUnits(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("usdcBaseUnits(%q) = %q, want an error", tc.in, got)
		}
	}
}

func TestDepositQRPayload(t *testing.T) {
	const addr = "0x52908400098527886E0F7030069857D2E4169EE7"
	const contract = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
	got, err := depositQRPayload(addr, "10.199555", qrEIP681)
	if err != nil {
		t.Fatal(err)
	}
	want := "ethereum:" + contract + "@8453/transfer?address=" + addr + "&uint256=10199555"
	if got != want {
		t.Fatalf("payload =\n%s\nwant\n%s", got, want)
	}
	plain, err := depositQRPayload(addr, "10.199555", qrAddress)
	if err != nil || plain != addr {
		t.Fatalf("address payload = %q, %v", plain, err)
	}
	noAmt, err := depositQRPayload(addr, "", qrEIP681)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(noAmt, "uint256") {
		t.Fatalf("deposit-address payload included uint256: %s", noAmt)
	}
	if !strings.HasPrefix(noAmt, "ethereum:"+contract+"@8453/transfer?address=") {
		t.Fatalf("deposit-address payload = %s", noAmt)
	}
	if _, err := depositQRPayload("not-an-address", "1", qrEIP681); err == nil {
		t.Fatal("non-address accepted")
	}
}

func TestDepositQRCaption(t *testing.T) {
	if got, want := depositQRCaption(true, "15:04"), "USDC on Base only, exact amount, before 15:04"; got != want {
		t.Fatalf("caption = %q", got)
	}
	if got, want := depositQRCaption(true, ""), "USDC on Base only, exact amount"; got != want {
		t.Fatalf("caption without clock = %q", got)
	}
	if got, want := depositQRCaption(false, ""), "USDC on Base only"; got != want {
		t.Fatalf("deposit caption = %q", got)
	}
	old := qrClockLocation
	qrClockLocation = func() *time.Location { return time.UTC }
	t.Cleanup(func() { qrClockLocation = old })
	if got := sendBeforeClock("2026-10-28T11:55:00Z"); got != "11:55" {
		t.Fatalf("clock = %q", got)
	}
	if sendBeforeClock("") != "" || sendBeforeClock("—") != "" || sendBeforeClock("not-a-time") != "" {
		t.Fatal("missing expiry should omit the clock")
	}
}

func TestDepositQRGating(t *testing.T) {
	addr := "0x52908400098527886E0F7030069857D2E4169EE7"
	base := depositQR{Address: addr, Currency: "usdcbase", Network: "base", Amount: "1"}
	if !depositQRAllowed(base, true, 60) {
		t.Fatal("tty of 60 columns should show the QR")
	}
	if !depositQRAllowed(base, true, 80) {
		t.Fatal("wide tty should show the QR")
	}
	if depositQRAllowed(base, true, 59) {
		t.Fatal("59 columns must not show the QR")
	}
	if depositQRAllowed(base, false, 80) {
		t.Fatal("piped stdout must not show the QR")
	}
	off := base
	off.JSON = true
	if depositQRAllowed(off, true, 80) {
		t.Fatal("--json must not show the QR")
	}
	off = base
	off.Quiet = true
	if depositQRAllowed(off, true, 80) {
		t.Fatal("--quiet must not show the QR")
	}
	t.Setenv("TESSERA_QUIET", "1")
	if depositQRAllowed(base, true, 80) {
		t.Fatal("TESSERA_QUIET=1 must not show the QR")
	}
	t.Setenv("TESSERA_QUIET", "")
	off = base
	off.Style = qrOff
	if depositQRAllowed(off, true, 80) {
		t.Fatal("--no-qr must not show the QR")
	}
	off = base
	off.Invoice = true
	if depositQRAllowed(off, true, 80) {
		t.Fatal("invoice mode must not show the QR")
	}
	off = base
	off.Currency = "btc"
	if !depositQRAllowed(off, true, 80) {
		t.Fatal("btc should show the QR")
	}
	off = base
	off.Currency = "usdc"
	off.Network = "eth"
	if !depositQRAllowed(off, true, 80) {
		t.Fatal("USDC on Ethereum should show the QR")
	}
	off = base
	off.Address = ""
	if depositQRAllowed(off, true, 80) {
		t.Fatal("missing address must not show the QR")
	}
	off = base
	off.Currency = "usdc"
	off.Network = "base"
	if !depositQRAllowed(off, true, 80) {
		t.Fatal("usdc on base should show the QR")
	}
}

func TestQRRenderDecodesToPayload(t *testing.T) {
	const addr = "0x52908400098527886E0F7030069857D2E4169EE7"
	payloads := []string{
		"hello",
		mustPayload(t, addr, "10.199555", qrEIP681),
		mustPayload(t, addr, "", qrEIP681),
		mustPayload(t, addr, "1", qrAddress),
	}
	for _, payload := range payloads {
		rendered, width, err := renderQR(payload)
		if err != nil {
			t.Fatalf("render %q: %v", payload, err)
		}
		if strings.Contains(rendered, "\x1b") {
			t.Fatal("QR renderer emitted a color code")
		}
		if width < 21 {
			t.Fatalf("width %d", width)
		}
		got, err := decodeHalfBlockQR(rendered)
		if err != nil {
			t.Fatalf("decode %q: %v\n%s", payload, err, rendered)
		}
		if got != payload {
			t.Fatalf("decoded %q, want %q", got, payload)
		}
	}
}

func TestTopupPrintsDecodableQR(t *testing.T) {
	stubQRTerminal(t, true, 100)
	old := qrClockLocation
	qrClockLocation = func() *time.Location { return time.UTC }
	t.Cleanup(func() { qrClockLocation = old })

	fake := &fakeDirectAPI{topupResp: directTopupResponse()}
	out := captureStdout(t, func() {
		if err := runTopup(fake, topupRequest{amount: "10"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "USDC on Base only, exact amount, before 11:55") {
		t.Fatalf("caption missing:\n%s", out)
	}
	if strings.Contains(out, "\x1b") {
		t.Fatal("topup QR used color")
	}
	payload, err := decodeHalfBlockQR(qrBlock(out))
	if err != nil {
		t.Fatal(err)
	}
	want, err := depositQRPayload("0x52908400098527886E0F7030069857D2E4169EE7", "10.199555", qrEIP681)
	if err != nil {
		t.Fatal(err)
	}
	if payload != want {
		t.Fatalf("topup QR decoded %q, want %q", payload, want)
	}

	// --qr=address encodes the bare address.
	out = captureStdout(t, func() {
		if err := runTopup(fake, topupRequest{amount: "10", qr: qrAddress}); err != nil {
			t.Fatal(err)
		}
	})
	payload, err = decodeHalfBlockQR(qrBlock(out))
	if err != nil {
		t.Fatal(err)
	}
	if payload != "0x52908400098527886E0F7030069857D2E4169EE7" {
		t.Fatalf("address QR decoded %q", payload)
	}

	// Gates that must print no QR.
	for _, req := range []topupRequest{
		{amount: "10", qr: qrOff},
		{amount: "10", json: true},
		{amount: "10", quiet: true},
		{amount: "10", invoice: true},
	} {
		out = captureStdout(t, func() {
			resp := directTopupResponse()
			if req.invoice {
				delete(resp, "pay_address")
			}
			f := &fakeDirectAPI{topupResp: resp}
			if err := runTopup(f, req); err != nil {
				t.Fatal(err)
			}
		})
		if strings.ContainsAny(out, "▀▄█") {
			t.Fatalf("QR printed for %+v:\n%s", req, out)
		}
	}
	btc := directTopupResponse()
	btc["pay_currency"] = "btc"
	btc["network"] = "btc"
	btc["pay_address"] = "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"
	btc["pay_amount"] = "0.01"
	out = captureStdout(t, func() {
		if err := runTopup(&fakeDirectAPI{topupResp: btc}, topupRequest{amount: "10"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.ContainsAny(out, "▀▄█") {
		t.Fatalf("QR missing for btc:\n%s", out)
	}
	payload, err = decodeHalfBlockQR(qrBlock(out))
	if err != nil {
		t.Fatal(err)
	}
	if payload != "bitcoin:bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4?amount=0.01" {
		t.Fatalf("btc QR decoded %q", payload)
	}
}

func TestParseQRFlags(t *testing.T) {
	req, err := parseTopupArgs([]string{"5", "--no-qr"})
	if err != nil || req.qr != qrOff {
		t.Fatalf("no-qr: %+v %v", req, err)
	}
	req, err = parseTopupArgs([]string{"5", "--qr=address"})
	if err != nil || req.qr != qrAddress {
		t.Fatalf("qr=address: %+v %v", req, err)
	}
	req, err = parseTopupArgs([]string{"5", "--qr", "address", "--quiet"})
	if err != nil || req.qr != qrAddress || !req.quiet {
		t.Fatalf("qr address --quiet: %+v %v", req, err)
	}
	if _, err := parseTopupArgs([]string{"5", "--no-qr", "--qr=address"}); err == nil {
		t.Fatal("conflicting QR flags accepted")
	}
	if !takesValue("--qr") {
		t.Fatal("--qr must take a value")
	}
	if takesValue("--no-qr") {
		t.Fatal("--no-qr is a boolean")
	}
}

func stubQRTerminal(t *testing.T, tty bool, cols int) {
	t.Helper()
	oldT, oldC := qrStdoutTerminal, stdoutColumns
	qrStdoutTerminal = func() bool { return tty }
	stdoutColumns = func() int { return cols }
	t.Cleanup(func() {
		qrStdoutTerminal, stdoutColumns = oldT, oldC
	})
}

func mustPayload(t *testing.T, addr, amount string, style qrStyle) string {
	t.Helper()
	s, err := depositQRPayload(addr, amount, style)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// qrBlock returns the half-block picture inside a larger printout.
func qrBlock(out string) string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if isHalfBlockLine(line) {
			lines = append(lines, line)
			continue
		}
		if len(lines) > 0 {
			break
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func isHalfBlockLine(line string) bool {
	if line == "" {
		return false
	}
	for _, r := range line {
		switch r {
		case ' ', '▀', '▄', '█':
		default:
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- decoder
//
// A lossless decoder for QR codes this CLI renders (byte mode, versions 1–12).
// It reverses rsc.io/qr's placement: format bits, mask, column sweep, and the
// block interleave. No error correction — the picture is the one we just drew.

type qrLev struct{ blocks, check int }
type qrVT struct {
	apos, astride, bytes int
	lev                  [4]qrLev
}

// qrVersions matches rsc.io/qr/coding vtab for versions 1–12. Index 0 is unused.
// Level order is L, M, Q, H.
var qrVersions = []qrVT{
	{},
	{100, 100, 26, [4]qrLev{{1, 7}, {1, 10}, {1, 13}, {1, 17}}},
	{16, 100, 44, [4]qrLev{{1, 10}, {1, 16}, {1, 22}, {1, 28}}},
	{20, 100, 70, [4]qrLev{{1, 15}, {1, 26}, {2, 18}, {2, 22}}},
	{24, 100, 100, [4]qrLev{{1, 20}, {2, 18}, {2, 26}, {4, 16}}},
	{28, 100, 134, [4]qrLev{{1, 26}, {2, 24}, {4, 18}, {4, 22}}},
	{32, 100, 172, [4]qrLev{{2, 18}, {4, 16}, {4, 24}, {4, 28}}},
	{20, 16, 196, [4]qrLev{{2, 20}, {4, 18}, {6, 18}, {5, 26}}},
	{22, 18, 242, [4]qrLev{{2, 24}, {4, 22}, {6, 22}, {6, 26}}},
	{24, 20, 292, [4]qrLev{{2, 30}, {5, 22}, {8, 20}, {8, 24}}},
	{26, 22, 346, [4]qrLev{{4, 18}, {5, 26}, {8, 24}, {8, 28}}},
	{28, 24, 404, [4]qrLev{{4, 20}, {5, 30}, {8, 28}, {11, 24}}},
	{30, 26, 466, [4]qrLev{{4, 24}, {8, 22}, {10, 26}, {11, 28}}},
}

func decodeHalfBlockQR(rendered string) (string, error) {
	mod, err := halfBlocksToMatrix(rendered)
	if err != nil {
		return "", err
	}
	return decodeQRMatrix(mod)
}

func halfBlocksToMatrix(rendered string) ([][]bool, error) {
	rendered = strings.TrimSuffix(rendered, "\n")
	if rendered == "" {
		return nil, errQR("empty")
	}
	lines := strings.Split(rendered, "\n")
	width := len([]rune(lines[0]))
	var rows [][]bool
	for _, line := range lines {
		runes := []rune(line)
		if len(runes) != width {
			return nil, errQR("ragged QR line")
		}
		top := make([]bool, width)
		bot := make([]bool, width)
		for i, r := range runes {
			switch r {
			case ' ':
			case '▀':
				top[i] = true
			case '▄':
				bot[i] = true
			case '█':
				top[i], bot[i] = true, true
			default:
				return nil, errQR("unexpected rune in QR")
			}
		}
		rows = append(rows, top, bot)
	}
	minR, maxR, minC, maxC := len(rows), -1, width, -1
	for y, row := range rows {
		for x, black := range row {
			if !black {
				continue
			}
			if y < minR {
				minR = y
			}
			if y > maxR {
				maxR = y
			}
			if x < minC {
				minC = x
			}
			if x > maxC {
				maxC = x
			}
		}
	}
	if maxR < minR || maxC < minC || maxR-minR != maxC-minC {
		return nil, errQR("QR bounds are not square")
	}
	size := maxR - minR + 1
	out := make([][]bool, size)
	for y := 0; y < size; y++ {
		out[y] = append([]bool(nil), rows[minR+y][minC:maxC+1]...)
	}
	return out, nil
}

type qrError string

func (e qrError) Error() string { return string(e) }
func errQR(s string) error      { return qrError("qr: " + s) }

func decodeQRMatrix(mod [][]bool) (string, error) {
	siz := len(mod)
	if siz < 21 || (siz-17)%4 != 0 {
		return "", errQR("bad size")
	}
	version := (siz - 17) / 4
	if version < 1 || version >= len(qrVersions) {
		return "", errQR("version out of range")
	}
	fb, ok := readFormat(mod, true)
	if !ok {
		fb, ok = readFormat(mod, false)
	}
	if !ok {
		return "", errQR("format info")
	}
	level := int((fb>>13)&3) ^ 1
	mask := int((fb >> 10) & 7)
	if level < 0 || level > 3 {
		return "", errQR("level")
	}
	fn := functionModules(version)
	bits := readDataBits(mod, fn, mask)
	raw := bitsToBytes(bits)
	vt := qrVersions[version]
	lev := vt.lev[level]
	dataBytes := vt.bytes - lev.blocks*lev.check
	if dataBytes <= 0 || len(raw) < dataBytes {
		return "", errQR("short codeword stream")
	}
	nde := dataBytes / lev.blocks
	extra := dataBytes % lev.blocks
	plain := deinterleave(raw[:dataBytes], lev.blocks, nde, extra)
	return decodeBytePayload(plain, version)
}

func readFormat(mod [][]bool, primary bool) (uint32, bool) {
	siz := len(mod)
	var fb uint32
	const invert = 0x5412
	for i := 0; i < 15; i++ {
		r, c := formatCoord(i, siz, primary)
		var bit uint32
		if mod[r][c] {
			bit = 1
		}
		if invert&(1<<uint(i)) != 0 {
			bit ^= 1
		}
		fb |= bit << uint(i)
	}
	return fb, formatValid(fb)
}

func formatCoord(i, siz int, primary bool) (row, col int) {
	if primary {
		switch {
		case i < 6:
			return i, 8
		case i < 8:
			return i + 1, 8
		case i < 9:
			return 8, 7
		default:
			return 8, 14 - i
		}
	}
	if i < 8 {
		return 8, siz - 1 - i
	}
	return siz - 1 - (14 - i), 8
}

func formatValid(fb uint32) bool {
	rem := fb
	const poly = 0x537
	for i := 14; i >= 10; i-- {
		if rem&(1<<uint(i)) != 0 {
			rem ^= poly << uint(i-10)
		}
	}
	return rem == 0
}

func functionModules(version int) [][]bool {
	siz := 17 + version*4
	fn := make([][]bool, siz)
	for i := range fn {
		fn[i] = make([]bool, siz)
	}
	for i := 0; i < siz; i++ {
		fn[i][6] = true
		fn[6][i] = true
	}
	markFinder(fn, 0, 0)
	markFinder(fn, siz-7, 0)
	markFinder(fn, 0, siz-7)
	info := qrVersions[version]
	for x := 4; x+5 < siz; {
		for y := 4; y+5 < siz; {
			if !((x < 7 && y < 7) || (x < 7 && y+5 >= siz-7) || (x+5 >= siz-7 && y < 7)) {
				for dy := 0; dy < 5; dy++ {
					for dx := 0; dx < 5; dx++ {
						fn[y+dy][x+dx] = true
					}
				}
			}
			if y == 4 {
				y = info.apos
			} else {
				y += info.astride
			}
		}
		if x == 4 {
			x = info.apos
		} else {
			x += info.astride
		}
	}
	if version >= 7 {
		for x := 0; x < 6; x++ {
			for y := 0; y < 3; y++ {
				fn[siz-11+y][x] = true
				fn[x][siz-11+y] = true
			}
		}
	}
	fn[siz-8][8] = true
	for i := 0; i < 15; i++ {
		r, c := formatCoord(i, siz, true)
		fn[r][c] = true
		r, c = formatCoord(i, siz, false)
		fn[r][c] = true
	}
	return fn
}

func markFinder(fn [][]bool, x, y int) {
	siz := len(fn)
	for dy := 0; dy < 7; dy++ {
		for dx := 0; dx < 7; dx++ {
			fn[y+dy][x+dx] = true
		}
	}
	for dy := -1; dy < 8; dy++ {
		if yy := y + dy; yy >= 0 && yy < siz {
			if x > 0 {
				fn[yy][x-1] = true
			}
			if x+7 < siz {
				fn[yy][x+7] = true
			}
		}
	}
	for dx := -1; dx < 8; dx++ {
		if xx := x + dx; xx >= 0 && xx < siz {
			if y > 0 {
				fn[y-1][xx] = true
			}
			if y+7 < siz {
				fn[y+7][xx] = true
			}
		}
	}
}

func readDataBits(mod, fn [][]bool, mask int) []bool {
	siz := len(mod)
	var bits []bool
	take := func(y, x int) {
		if y < 0 || x < 0 || y >= siz || x >= siz || fn[y][x] {
			return
		}
		black := mod[y][x]
		if maskFlip(mask, y, x) {
			black = !black
		}
		bits = append(bits, black)
	}
	for x := siz; x > 0; {
		for y := siz - 1; y >= 0; y-- {
			take(y, x-1)
			take(y, x-2)
		}
		x -= 2
		if x == 7 {
			x--
		}
		for y := 0; y < siz; y++ {
			take(y, x-1)
			take(y, x-2)
		}
		x -= 2
	}
	return bits
}

func maskFlip(mask, y, x int) bool {
	switch mask {
	case 0:
		return (y+x)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (y+x)%3 == 0
	case 4:
		return (y/2+x/3)%2 == 0
	case 5:
		return y*x%2+y*x%3 == 0
	case 6:
		return (y*x%2+y*x%3)%2 == 0
	default:
		return (y*x%3+(y+x)%2)%2 == 0
	}
}

func bitsToBytes(bits []bool) []byte {
	n := len(bits) / 8
	out := make([]byte, n)
	for i := 0; i < n*8; i++ {
		if bits[i] {
			out[i/8] |= 1 << uint(7-i%8)
		}
	}
	return out
}

func deinterleave(raw []byte, nblock, nde, extra int) []byte {
	blocks := make([][]byte, nblock)
	for i := 0; i < nblock; i++ {
		n := nde
		if i >= nblock-extra {
			n++
		}
		blocks[i] = make([]byte, n)
	}
	idx := 0
	for i := 0; i < nde+1; i++ {
		for b := 0; b < nblock; b++ {
			if i < len(blocks[b]) {
				blocks[b][i] = raw[idx]
				idx++
			}
		}
	}
	var out []byte
	for _, b := range blocks {
		out = append(out, b...)
	}
	return out
}

func decodeBytePayload(data []byte, version int) (string, error) {
	r := bitReader{b: data}
	mode, ok := r.read(4)
	if !ok || mode != 0x4 {
		return "", errQR("not byte mode")
	}
	nbits := 8
	if version > 9 {
		nbits = 16
	}
	n, ok := r.read(nbits)
	if !ok {
		return "", errQR("length")
	}
	buf := make([]byte, n)
	for i := 0; i < int(n); i++ {
		b, ok := r.read(8)
		if !ok {
			return "", errQR("truncated payload")
		}
		buf[i] = byte(b)
	}
	return string(buf), nil
}

type bitReader struct {
	b []byte
	i int
}

func (r *bitReader) read(n int) (uint, bool) {
	if r.i+n > len(r.b)*8 {
		return 0, false
	}
	var v uint
	for k := 0; k < n; k++ {
		v <<= 1
		bit := r.b[r.i/8] >> uint(7-r.i%8) & 1
		v |= uint(bit)
		r.i++
	}
	return v, true
}
