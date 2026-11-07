package main

import (
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
	"rsc.io/qr"
)

// QR codes for USDC on Base.
//
// The encoder is rsc.io/qr. The terminal picture is a half-block renderer in
// this file (two modules per row, monochrome, no color codes). Heavier QR
// libraries are not used.
//
// Shown by default on `tessera topup` when it prints a USDC-on-Base deposit
// address, and on `tessera wallet fund`. Never for other coins, invoice mode,
// x402, --json, --quiet, or when stdout is not a terminal of at least
// qrMinColumns.

const qrMinColumns = 60

// qrQuietZone is the white margin around the modules, in modules.
const qrQuietZone = 4

// qrStyle selects the payload. The zero value is the EIP-681 payment URI.
type qrStyle int

const (
	qrEIP681 qrStyle = iota
	qrAddress
	qrOff
)

// depositQR is one decision about whether to print a QR, and what it encodes.
type depositQR struct {
	Address  string
	Currency string
	Network  string
	Amount   string // decimal USDC; empty omits uint256
	Expires  string // RFC3339 send-before; empty omits the clock
	Style    qrStyle
	JSON     bool
	Quiet    bool
	Invoice  bool
	X402     bool
}

// qrStdoutTerminal reports whether the stream printDepositQR writes to is a
// terminal. It follows os.Stdout, so a caller that has swapped in a pipe
// (tests, `|`) does not get a QR even when fd 1 is still a terminal.
var qrStdoutTerminal = func() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// stdoutColumns is the width of stdout, or 0 when it cannot be read.
var stdoutColumns = func() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w < 0 {
		return 0
	}
	return w
}

// qrClockLocation is the zone used for the "before HH:MM" caption.
var qrClockLocation = func() *time.Location { return time.Local }

// parseQRStyle reads --no-qr and --qr=address. The default is the payment URI.
func parseQRStyle(args []string) (qrStyle, error) {
	no := hasFlag(args, "--no-qr")
	raw, ok := flagValue(args, "--qr")
	present := ok || hasFlag(args, "--qr")
	if no && present {
		return 0, fmt.Errorf("--no-qr and --qr cannot be combined")
	}
	if no {
		return qrOff, nil
	}
	if !present {
		return qrEIP681, nil
	}
	if !ok || strings.TrimSpace(raw) == "" {
		return 0, fmt.Errorf("--qr requires a value: --qr=address")
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "address":
		return qrAddress, nil
	default:
		return 0, fmt.Errorf("--qr=%s is not valid; use --qr=address for a plain-address QR", raw)
	}
}

// isUSDCOnBase reports whether this deposit is USDC on Base. Other coins,
// including USDC on other networks, are not.
func isUSDCOnBase(currency, network string) bool {
	c := strings.ToLower(strings.TrimSpace(currency))
	n := strings.ToLower(strings.TrimSpace(network))
	if c == "—" || c == "" {
		return false
	}
	switch c {
	case "usdcbase":
		return true
	case "usdc":
		return n == "base" || n == "base-mainnet"
	default:
		return false
	}
}

// isEVMAddress reports a 0x-prefixed 20-byte hex address.
func isEVMAddress(s string) bool {
	if len(s) != 42 || !strings.HasPrefix(s, "0x") {
		return false
	}
	for _, c := range s[2:] {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// usdcBaseUnits converts a decimal USDC amount to 6-decimal base units.
// The 7th decimal digit rounds half up. The result is a decimal integer string.
func usdcBaseUnits(amount string) (string, error) {
	amount = strings.TrimSpace(amount)
	if amount == "" || amount == "—" {
		return "", fmt.Errorf("missing USDC amount")
	}
	if strings.HasPrefix(amount, "+") {
		amount = strings.TrimSpace(amount[1:])
	}
	if amount == "" || strings.ContainsAny(amount, "eE+-") {
		return "", fmt.Errorf("not a USDC amount: %q", amount)
	}
	if strings.Count(amount, ".") > 1 {
		return "", fmt.Errorf("not a USDC amount: %q", amount)
	}
	whole, frac, _ := strings.Cut(amount, ".")
	if whole == "" {
		whole = "0"
	}
	if !allDigits(whole) || !allDigits(frac) {
		return "", fmt.Errorf("not a USDC amount: %q", amount)
	}
	roundUp := false
	if len(frac) > 6 {
		if frac[6] >= '5' {
			roundUp = true
		}
		frac = frac[:6]
	} else {
		frac += strings.Repeat("0", 6-len(frac))
	}
	n := new(big.Int)
	if _, ok := n.SetString(whole+frac, 10); !ok {
		return "", fmt.Errorf("not a USDC amount: %q", amount)
	}
	if roundUp {
		n.Add(n, big.NewInt(1))
	}
	return n.String(), nil
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// depositQRPayload builds the EIP-681 payment URI, or the plain address when
// style is qrAddress. uint256 is omitted when there is no amount (wallet fund).
func depositQRPayload(address, amount string, style qrStyle) (string, error) {
	if !isEVMAddress(address) {
		return "", fmt.Errorf("not an EVM address")
	}
	if style == qrAddress {
		return address, nil
	}
	contract, ok := usdcContractForChain(8453)
	if !ok {
		return "", fmt.Errorf("no USDC contract for Base")
	}
	payload := fmt.Sprintf("ethereum:%s@8453/transfer?address=%s", contract, address)
	if strings.TrimSpace(amount) == "" || amount == "—" {
		return payload, nil
	}
	units, err := usdcBaseUnits(amount)
	if err != nil {
		return "", err
	}
	if units == "0" {
		return payload, nil
	}
	return payload + "&uint256=" + units, nil
}

// depositQRAllowed is the TTY / json / quiet / coin gate. It does not encode.
func depositQRAllowed(p depositQR, tty bool, columns int) bool {
	if p.Style == qrOff || p.JSON || p.Quiet || p.Invoice || p.X402 {
		return false
	}
	if os.Getenv("TESSERA_QUIET") == "1" {
		p.Quiet = true
	}
	if p.Quiet {
		return false
	}
	if !isUSDCOnBase(p.Currency, p.Network) || !isEVMAddress(p.Address) {
		return false
	}
	if !tty || columns < qrMinColumns {
		return false
	}
	return true
}

// sendBeforeClock formats an RFC3339 expiry as HH:MM in the caption zone.
// An empty or unparseable value returns "" so the caption drops the clause.
func sendBeforeClock(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "—" {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return ""
	}
	loc := qrClockLocation()
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("15:04")
}

// depositQRCaption is the line printed with the QR. "exact amount" is included
// when the payload carries uint256. "before HH:MM" is included when a
// send-before time exists.
func depositQRCaption(exact bool, hhmm string) string {
	parts := []string{"USDC on Base only"}
	if exact {
		parts = append(parts, "exact amount")
	}
	if hhmm != "" {
		parts = append(parts, "before "+hhmm)
	}
	return strings.Join(parts, ", ")
}

// renderQR draws payload as half-block rows. The int is the column count
// (one module per column, including the quiet zone).
func renderQR(payload string) (string, int, error) {
	code, err := qr.Encode(payload, qr.M)
	if err != nil {
		return "", 0, err
	}
	n := code.Size + qrQuietZone*2
	var b strings.Builder
	for y := 0; y < n; y += 2 {
		for x := 0; x < n; x++ {
			b.WriteString(halfBlock(qrModule(code, x, y), qrModule(code, x, y+1)))
		}
		b.WriteByte('\n')
	}
	return b.String(), n, nil
}

func qrModule(code *qr.Code, x, y int) bool {
	x -= qrQuietZone
	y -= qrQuietZone
	if x < 0 || y < 0 || x >= code.Size || y >= code.Size {
		return false
	}
	return code.Black(x, y)
}

// halfBlock is one terminal cell covering two QR modules. No color codes.
func halfBlock(top, bot bool) string {
	switch {
	case top && bot:
		return "█"
	case top:
		return "▀"
	case bot:
		return "▄"
	default:
		return " "
	}
}

// printDepositQR writes a QR when the gate allows it and the picture fits
// the terminal. A failure to encode is silent: the address text already printed.
func printDepositQR(w io.Writer, p depositQR) {
	tty := qrStdoutTerminal()
	cols := stdoutColumns()
	if !depositQRAllowed(p, tty, cols) {
		return
	}
	payload, err := depositQRPayload(p.Address, p.Amount, p.Style)
	if err != nil {
		return
	}
	rendered, width, err := renderQR(payload)
	if err != nil || width > cols {
		return
	}
	exact := p.Style != qrAddress && strings.Contains(payload, "&uint256=")
	caption := depositQRCaption(exact, sendBeforeClock(p.Expires))
	fmt.Fprintln(w)
	fmt.Fprint(w, rendered)
	fmt.Fprintln(w, caption)
}
