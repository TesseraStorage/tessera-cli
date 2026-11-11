package main

import (
	"fmt"
	"math/big"
	"net/url"
	"strings"
)

// depositQRPayloadFor chooses the QR contents for any deposit coin.
//
// USDC on Base keeps the existing EIP-681 builder in depositQRPayload, byte
// for byte. Other coins use a payment URI when a widely supported scheme
// exists and the amount can be formatted without guessing; everything else
// is the plain address. --qr=address always encodes the address alone.
func depositQRPayloadFor(p depositQR) (string, error) {
	address := strings.TrimSpace(p.Address)
	if address == "" || address == "—" {
		return "", fmt.Errorf("missing deposit address")
	}
	if p.Style == qrAddress {
		return address, nil
	}
	if isUSDCOnBase(p.Currency, p.Network) {
		return depositQRPayload(address, p.Amount, p.Style)
	}
	code := normalizeCoinCode(p.Currency)
	if payload, ok, err := paymentURIFor(code, address, p.Amount); err != nil {
		return "", err
	} else if ok {
		return payload, nil
	}
	return address, nil
}

func normalizeCoinCode(currency string) string {
	return strings.ToLower(strings.TrimSpace(currency))
}

// paymentURIFor returns a scheme-specific payment URI. ok is false when this
// coin should use a plain-address QR (unknown code, or a token we will not
// guess a contract for).
func paymentURIFor(code, address, amount string) (string, bool, error) {
	switch code {
	case "btc":
		payload, err := amountURI("bitcoin", address, "amount", amount)
		return payload, true, err
	case "ltc":
		payload, err := amountURI("litecoin", address, "amount", amount)
		return payload, true, err
	case "doge":
		payload, err := amountURI("dogecoin", address, "amount", amount)
		return payload, true, err
	case "bch":
		payload, err := bchURI(address, amount)
		return payload, true, err
	case "xmr":
		payload, err := amountURI("monero", address, "tx_amount", amount)
		return payload, true, err
	case "eth":
		payload, err := ethereumNativeURI(address, amount)
		return payload, true, err
	}
	if tok, ok := eip681TokenByCode[code]; ok {
		payload, err := eip681TransferURI(tok, address, amount)
		return payload, true, err
	}
	return "", false, nil
}

func amountURI(scheme, address, amountKey, amount string) (string, error) {
	addr := stripURIScheme(address, scheme)
	if addr == "" {
		return "", fmt.Errorf("missing %s address", scheme)
	}
	payload := scheme + ":" + addr
	dec, err := passThroughDecimal(amount)
	if err != nil || dec == "" {
		return payload, nil
	}
	return payload + "?" + url.Values{amountKey: {dec}}.Encode(), nil
}

func bchURI(address, amount string) (string, error) {
	addr := stripURIScheme(address, "bitcoincash")
	if addr == "" {
		return "", fmt.Errorf("missing bitcoincash address")
	}
	payload := "bitcoincash:" + addr
	dec, err := passThroughDecimal(amount)
	if err != nil || dec == "" {
		return payload, nil
	}
	return payload + "?" + url.Values{"amount": {dec}}.Encode(), nil
}

func ethereumNativeURI(address, amount string) (string, error) {
	if !isEVMAddress(address) {
		return address, nil
	}
	payload := fmt.Sprintf("ethereum:%s@1", address)
	dec, err := passThroughDecimal(amount)
	if err != nil || dec == "" {
		return payload, nil
	}
	wei, err := tokenBaseUnits(dec, 18)
	if err != nil || wei == "0" {
		return payload, nil
	}
	return payload + "?value=" + wei, nil
}

func eip681TransferURI(tok eip681Token, address, amount string) (string, error) {
	if !isEVMAddress(address) {
		return address, nil
	}
	payload := fmt.Sprintf("ethereum:%s@%d/transfer?address=%s", tok.contract, tok.chainID, address)
	dec, err := passThroughDecimal(amount)
	if err != nil || dec == "" {
		return payload, nil
	}
	units, err := tokenBaseUnits(dec, tok.decimals)
	if err != nil || units == "0" {
		return payload, nil
	}
	return payload + "&uint256=" + units, nil
}

func stripURIScheme(address, scheme string) string {
	address = strings.TrimSpace(address)
	prefix := scheme + ":"
	if strings.HasPrefix(strings.ToLower(address), prefix) {
		return strings.TrimSpace(address[len(prefix):])
	}
	return address
}

// passThroughDecimal accepts a decimal amount string without converting it
// through float64, so QR URIs cannot pick up binary artifacts. Empty / em-dash
// means "omit the amount". Scientific notation and signs are rejected.
func passThroughDecimal(amount string) (string, error) {
	amount = strings.TrimSpace(amount)
	if amount == "" || amount == "—" {
		return "", nil
	}
	if strings.HasPrefix(amount, "+") || strings.HasPrefix(amount, "-") || strings.ContainsAny(amount, "eE") {
		return "", fmt.Errorf("not a decimal amount: %q", amount)
	}
	if strings.Count(amount, ".") > 1 {
		return "", fmt.Errorf("not a decimal amount: %q", amount)
	}
	whole, frac, _ := strings.Cut(amount, ".")
	if whole == "" {
		whole = "0"
	}
	if !allDigits(whole) || !allDigits(frac) {
		return "", fmt.Errorf("not a decimal amount: %q", amount)
	}
	return amount, nil
}

// tokenBaseUnits converts a decimal token amount to an integer string in
// decimals base units. The next digit after decimals rounds half up.
// Parsing is decimal-only (no float64) so values such as 0.1 stay exact.
func tokenBaseUnits(amount string, decimals int) (string, error) {
	if decimals < 0 || decimals > 36 {
		return "", fmt.Errorf("unsupported decimals %d", decimals)
	}
	dec, err := passThroughDecimal(amount)
	if err != nil {
		return "", err
	}
	if dec == "" {
		return "", fmt.Errorf("missing token amount")
	}
	whole, frac, _ := strings.Cut(dec, ".")
	if whole == "" {
		whole = "0"
	}
	roundUp := false
	if len(frac) > decimals {
		if frac[decimals] >= '5' {
			roundUp = true
		}
		frac = frac[:decimals]
	} else {
		frac += strings.Repeat("0", decimals-len(frac))
	}
	n := new(big.Int)
	if _, ok := n.SetString(whole+frac, 10); !ok {
		return "", fmt.Errorf("not a token amount: %q", amount)
	}
	if roundUp {
		n.Add(n, big.NewInt(1))
	}
	return n.String(), nil
}

// eip681Token is a Circle- or Tether-published ERC-20 used in EIP-681 URIs.
// Codes are NOWPayments pay_currency values from tessera topup --coins.
// Contracts not listed on the issuer page are omitted on purpose.
type eip681Token struct {
	contract string
	chainID  uint64
	decimals int
}

// Sources:
//
//	USDC: https://developers.circle.com/stablecoins/usdc-contract-addresses
//	USDT: https://tether.to/en/supported-protocols/
//
// Not mapped (no official issuer listing we will treat as authoritative, or
// not an EVM payment URI): usdcbsc, maticusdce, opusdce, usdtbsc, usdtarb,
// usdtmatic, usdtop, usdtopbnb, usdtsol, usdttrc20, usdtton, usdcsol.
var eip681TokenByCode = map[string]eip681Token{
	// Circle native USDC
	"usdc":      {contract: "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48", chainID: 1, decimals: 6},
	"usdcarb":   {contract: "0xaf88d065e77c8cC2239327C5EDb3A432268e5831", chainID: 42161, decimals: 6},
	"usdcop":    {contract: "0x0b2C639c533813f4Aa9D7837CAf62653d097Ff85", chainID: 10, decimals: 6},
	"usdcmatic": {contract: "0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359", chainID: 137, decimals: 6},
	"usdcarc20": {contract: "0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E", chainID: 43114, decimals: 6},
	// Tether-published USDT (EVM entries that name a USDT contract)
	"usdterc20": {contract: "0xdAC17F958D2ee523a2206206994597C13D831ec7", chainID: 1, decimals: 6},
	"usdtarc20": {contract: "0x9702230A8Ea53601f5cD2dc00fDBc13d4dF4A8c7", chainID: 43114, decimals: 6},
	"usdtcelo":  {contract: "0x48065fbBE25f71C9282ddf5e1cD6D6A887483D5e", chainID: 42220, decimals: 6},
}

func payloadIncludesAmount(payload string) bool {
	return strings.Contains(payload, "&uint256=") ||
		strings.Contains(payload, "?value=") ||
		strings.Contains(payload, "&value=") ||
		strings.Contains(payload, "?amount=") ||
		strings.Contains(payload, "&amount=") ||
		strings.Contains(payload, "?tx_amount=") ||
		strings.Contains(payload, "&tx_amount=")
}

func extraIDFromResponse(resp map[string]any) string {
	for _, key := range []string{
		"payin_extra_id",
		"extra_id",
		"destination_tag",
		"memo",
		"payinExtraId",
		"payin_extraid",
	} {
		s := topupField(resp, key)
		if s != "" && s != "—" {
			return s
		}
	}
	return ""
}

func otherQRCaption(exact bool, hhmm string) string {
	var parts []string
	if exact {
		parts = append(parts, "exact amount")
	} else {
		parts = append(parts, "address only — enter the amount shown above")
	}
	if hhmm != "" {
		parts = append(parts, "before "+hhmm)
	}
	return strings.Join(parts, ", ")
}
