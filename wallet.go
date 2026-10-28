package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"
)

// Local x402 wallet (OPT-IN).
//
// `tessera wallet init` generates a secp256k1 key and stores it in a separate
// 0600 file inside the same 0700 config directory as the billing credential.
// Nothing creates a wallet implicitly: `topup` without --wallet never reads or
// writes this file, and the key never leaves the machine. Only the EIP-3009
// authorization signature is sent to the server.
const walletFileName = "wallet.json"

// walletKey is the on-disk wallet document. Address is stored for display
// only and is always re-derived from the key before use, so a tampered
// address field can never change what gets signed.
type walletKey struct {
	PrivateKey string `json:"private_key"`
	Address    string `json:"address"`
}

func walletFilePath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, walletFileName), nil
}

// loadWallet reads the wallet file. A missing file is a distinct error so the
// caller can print the init guidance rather than a generic read failure.
func loadWallet() (*walletKey, error) {
	path, err := walletFilePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var w walletKey
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("corrupt wallet at %s: %w", path, err)
	}
	if _, err := walletPrivateKey(&w); err != nil {
		return nil, err
	}
	return &w, nil
}

// saveWallet writes the wallet with 0600 permissions and a 0700 directory,
// matching the credential file's discipline. writeFileAtomic means an existing
// looser mode is replaced rather than inherited.
func saveWallet(w *walletKey) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("cannot create config directory: %w", err)
	}
	path, err := walletFilePath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data, 0600)
}

// walletPrivateKey parses and validates the stored hex key.
func walletPrivateKey(w *walletKey) (*secp256k1.PrivateKey, error) {
	raw, err := decodeHex(w.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("wallet private key is not valid hex: %w", err)
	}
	if len(raw) != 32 {
		return nil, errors.New("wallet private key must be 32 bytes")
	}
	key := secp256k1.PrivKeyFromBytes(raw)
	if key.Key.IsZero() {
		return nil, errors.New("wallet private key is zero")
	}
	return key, nil
}

// newWalletKey generates a fresh key and returns the file document.
func newWalletKey() (*walletKey, error) {
	key, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	return &walletKey{
		PrivateKey: "0x" + hex.EncodeToString(key.Serialize()),
		Address:    walletAddress(key),
	}, nil
}

// walletAddress renders the EIP-55 checksummed address of a key.
func walletAddress(key *secp256k1.PrivateKey) string {
	pub := key.PubKey().SerializeUncompressed()
	return checksumAddress(keccak256(pub[1:])[12:])
}

// checksumAddress applies EIP-55 mixed-case checksumming to a 20-byte address.
func checksumAddress(addr []byte) string {
	lower := hex.EncodeToString(addr)
	hash := keccak256([]byte(lower))
	out := make([]byte, len(lower))
	for i, c := range []byte(lower) {
		if c >= 'a' && c <= 'f' && hash[i/2]>>(uint(1-i%2)*4)&0xf >= 8 {
			out[i] = c - 32
			continue
		}
		out[i] = c
	}
	return "0x" + string(out)
}

// keccak256 is Ethereum's Keccak-256 (not NIST SHA3-256).
func keccak256(data ...[]byte) []byte {
	h := sha3.NewLegacyKeccak256()
	for _, part := range data {
		h.Write(part)
	}
	return h.Sum(nil)
}

// decodeHex accepts 0x-prefixed or bare hex.
func decodeHex(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
	return hex.DecodeString(s)
}

// encodeUint256 renders a non-negative integer as a 32-byte big-endian word.
func encodeUint256(v *big.Int) []byte {
	out := make([]byte, 32)
	v.FillBytes(out)
	return out
}

// encodeAddressWord left-pads a 20-byte address to a 32-byte word.
func encodeAddressWord(addr []byte) []byte {
	out := make([]byte, 32)
	copy(out[12:], addr)
	return out
}

// encodeString renders a dynamic string as an ABI head (offset) plus tail.
func encodeString(s string) []byte {
	body := []byte(s)
	padded := make([]byte, (len(body)+31)/32*32)
	copy(padded, body)
	out := make([]byte, 32, 32+32+len(padded))
	new(big.Int).SetInt64(32).FillBytes(out)
	out = append(out, encodeUint256(big.NewInt(int64(len(body))))...)
	out = append(out, padded...)
	return out
}

// EIP-712 type hashes for the x402 `exact` scheme on an EIP-3009 token.
var (
	eip712DomainTypeHash = keccak256([]byte(
		"EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)",
	))
	transferWithAuthorizationTypeHash = keccak256([]byte(
		"TransferWithAuthorization(address from,address to,uint256 value," +
			"uint256 validAfter,uint256 validBefore,bytes32 nonce)",
	))
)

// eip712DomainSeparator hashes the token's EIP-712 domain.
func eip712DomainSeparator(name, version string, chainID uint64, contract []byte) []byte {
	chain := new(big.Int).SetUint64(chainID)
	return keccak256(
		eip712DomainTypeHash,
		keccak256([]byte(name)),
		keccak256([]byte(version)),
		encodeUint256(chain),
		encodeAddressWord(contract),
	)
}

// transferAuthorizationDigest builds the EIP-712 digest the token verifies.
//
// Field order and types mirror the contract's
// `TransferWithAuthorization(address,address,uint256,uint256,uint256,bytes32)`.
func transferAuthorizationDigest(
	domainSeparator []byte,
	from, to []byte,
	value, validAfter, validBefore *big.Int,
	nonce []byte,
) []byte {
	return keccak256(
		[]byte{0x19, 0x01},
		domainSeparator,
		keccak256(
			transferWithAuthorizationTypeHash,
			encodeAddressWord(from),
			encodeAddressWord(to),
			encodeUint256(value),
			encodeUint256(validAfter),
			encodeUint256(validBefore),
			nonce,
		),
	)
}

// signDigest returns the 65-byte r||s||v signature with v in {27,28}, the
// encoding EIP-3009 and every USDC implementation expect.
func signDigest(key *secp256k1.PrivateKey, digest []byte) ([]byte, error) {
	sig := ecdsa.Sign(key, digest)
	sigR := sig.R()
	sigS := sig.S()
	r := sigR.Bytes()
	s := sigS.Bytes()
	// Decred's compact form is [recovery+27][r][s]; try each recovery id and
	// keep the one that recovers this key, rather than assuming a value.
	compact := make([]byte, 65)
	copy(compact[1:33], r[:])
	copy(compact[33:], s[:])
	for _, v := range []byte{0, 1} {
		compact[0] = v + 27
		pub, _, err := ecdsa.RecoverCompact(compact, digest)
		if err != nil {
			continue
		}
		if pub.IsEqual(key.PubKey()) {
			out := make([]byte, 65)
			copy(out[:32], r[:])
			copy(out[32:64], s[:])
			out[64] = v + 27
			return out, nil
		}
	}
	return nil, errors.New("could not determine the signature recovery id")
}

// walletUsageText is printed by `tessera wallet` without a subcommand and in
// help output. It deliberately says nothing about how payments are processed
// server-side.
const walletUsageText = `Usage:
  tessera wallet init    Create a local wallet for x402 payments (opt-in)
  tessera wallet show    Show the wallet address and USDC balance
  tessera wallet fund    Explain how to fund the wallet

The wallet is optional. The default 'tessera topup <amount>' path needs no
wallet: it prints a payment address you can fund from any wallet or exchange.`

// runWallet dispatches the wallet subcommands. The wallet is opt-in: nothing
// here runs unless the user asks for it.
func runWallet(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println(walletUsageText)
		return nil
	}
	switch args[0] {
	case "init":
		return runWalletInit(args[1:])
	case "show":
		return runWalletShow(args[1:])
	case "fund":
		return runWalletFund(args[1:])
	default:
		return fmt.Errorf("unknown wallet command %q\n\n%s", args[0], walletUsageText)
	}
}

// runWalletInit generates and stores a new key. It refuses to overwrite an
// existing wallet: replacing a key can strand funds at the old address.
func runWalletInit(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: tessera wallet init")
	}
	path, err := walletFilePath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("a local wallet already exists at %s; run 'tessera wallet show'", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check wallet file: %w", err)
	}
	wallet, err := newWalletKey()
	if err != nil {
		return fmt.Errorf("generate wallet: %w", err)
	}
	if err := saveWallet(wallet); err != nil {
		return fmt.Errorf("save wallet: %w", err)
	}
	fmt.Println("Wallet created (local only).")
	fmt.Println()
	fmt.Printf("  Address:  %s\n", wallet.Address)
	fmt.Printf("  Key file: %s (0600)\n", path)
	fmt.Println()
	fmt.Println("Send USDC on Base to this address. You control the key; back it up.")
	return nil
}

// runWalletShow prints the address and its on-chain USDC balance. The balance
// lookup uses a public Base RPC and sends only the address; no key material or
// billing credential leaves the machine.
func runWalletShow(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: tessera wallet show")
	}
	wallet, err := loadWallet()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("no local wallet; run 'tessera wallet init' first")
		}
		return err
	}
	fmt.Printf("Address: %s\n", wallet.Address)
	balance, chainID, supported, err := usdcBalance(wallet.Address)
	if err != nil {
		fmt.Fprintf(os.Stderr, "USDC balance unavailable: %v\n", err)
		return nil
	}
	if !supported {
		fmt.Fprintf(os.Stderr, "USDC balance unavailable on chain %d; set TESSERA_WALLET_RPC_URL to a Base RPC to read it.\n", chainID)
		return nil
	}
	fmt.Printf("USDC on Base: $%s\n", balance)
	return nil
}

// runWalletFund explains funding without moving anything or touching the key.
func runWalletFund(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: tessera wallet fund")
	}
	wallet, err := loadWallet()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("no local wallet; run 'tessera wallet init' first")
		}
		return err
	}
	fmt.Println("Fund this wallet with USDC on Base.")
	fmt.Println()
	fmt.Printf("  Address: %s\n", wallet.Address)
	fmt.Println()
	fmt.Println("Ways to fund it:")
	fmt.Println("  - Withdraw USDC from an exchange to Base (ERC-20) at this address.")
	fmt.Println("  - Send USDC on Base from another wallet you control.")
	fmt.Println()
	fmt.Println("Send only USDC on Base. Other assets or networks may be lost. The key")
	fmt.Println("stays on this machine; back it up.")
	return nil
}

// signX402Payment builds and signs an x402 v2 payment header for one challenge.
//
// The authorization binds the wallet address, the challenge's payTo, the exact
// atomic amount and the server-issued nonce. The signed digest follows EIP-712
// with the token's on-chain domain, so a signature that verifies here is the
// same one the token contract accepts.
func signX402Payment(challenge map[string]any, key *secp256k1.PrivateKey, now time.Time) (string, error) {
	accept, ok := firstX402Accept(challenge)
	if !ok {
		return "", errors.New("x402 challenge is missing accepts[0]")
	}
	network := topupField(accept, "network")
	chainID, err := chainIDFromNetwork(network)
	if err != nil {
		return "", err
	}
	asset, err := addressBytes(topupField(accept, "asset"))
	if err != nil {
		return "", fmt.Errorf("challenge asset: %w", err)
	}
	payTo, err := addressBytes(topupField(accept, "payTo"))
	if err != nil {
		return "", fmt.Errorf("challenge payTo: %w", err)
	}
	amount, ok := new(big.Int).SetString(topupField(accept, "amount"), 10)
	if !ok || amount.Sign() <= 0 {
		return "", fmt.Errorf("challenge amount %q is not a positive integer", topupField(accept, "amount"))
	}
	extra, _ := accept["extra"].(map[string]any)
	name := topupField(extra, "name")
	version := topupField(extra, "version")
	if name == "—" || version == "—" {
		return "", errors.New("challenge extra is missing the token name or version")
	}
	nonce, err := decodeHex(topupField(extra, "x402ChallengeNonce"))
	if err != nil {
		return "", fmt.Errorf("challenge nonce: %w", err)
	}
	if len(nonce) != 32 {
		return "", errors.New("challenge nonce must be 32 bytes")
	}
	maxTimeout, err := uintField(accept, "maxTimeoutSeconds")
	if err != nil || maxTimeout == 0 {
		maxTimeout = 600
	}
	from, err := addressBytes(walletAddress(key))
	if err != nil {
		return "", err
	}
	validBefore := now.Unix() + int64(maxTimeout)
	domain := eip712DomainSeparator(name, version, chainID, asset)
	digest := transferAuthorizationDigest(domain, from, payTo, amount, big.NewInt(0), big.NewInt(validBefore), nonce)
	signature, err := signDigest(key, digest)
	if err != nil {
		return "", err
	}
	payload := map[string]any{
		"x402Version": 2,
		"accepted":    accept,
		"payload": map[string]any{
			"signature": "0x" + hex.EncodeToString(signature),
			"authorization": map[string]any{
				"from":        walletAddress(key),
				"to":          topupField(accept, "payTo"),
				"value":       amount.String(),
				"validAfter":  "0",
				"validBefore": strconv.FormatInt(validBefore, 10),
				"nonce":       "0x" + hex.EncodeToString(nonce),
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// addressBytes validates a 0x-prefixed 20-byte address.
func addressBytes(s string) ([]byte, error) {
	raw, err := decodeHex(s)
	if err != nil {
		return nil, fmt.Errorf("%q is not hex: %w", s, err)
	}
	if len(raw) != 20 {
		return nil, fmt.Errorf("%q is not a 20-byte address", s)
	}
	return raw, nil
}

// uintField reads a non-negative integer field that may arrive as a JSON
// number or a string. It returns 0 when the field is absent.
func uintField(m map[string]any, key string) (uint64, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, nil
	}
	switch t := v.(type) {
	case float64:
		if t < 0 || t != math.Trunc(t) {
			return 0, fmt.Errorf("%s is not a non-negative integer", key)
		}
		return uint64(t), nil
	case string:
		n, err := strconv.ParseUint(strings.TrimSpace(t), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s is not a non-negative integer: %w", key, err)
		}
		return n, nil
	case json.Number:
		n, err := strconv.ParseUint(t.String(), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s is not a non-negative integer: %w", key, err)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("%s has an unexpected type %T", key, v)
	}
}

// chainIDFromNetwork maps an x402 network label to its EVM chain id. The
// eip155:<id> form is accepted so a future network needs no code change.
func chainIDFromNetwork(network string) (uint64, error) {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "base", "base-mainnet":
		return 8453, nil
	case "base-sepolia":
		return 84532, nil
	}
	if id, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(network)), "eip155:"); ok {
		n, err := strconv.ParseUint(id, 10, 64)
		if err == nil && n > 0 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("unsupported x402 network %q", network)
}

// usdcContractForChain returns the canonical USDC contract where one is known.
func usdcContractForChain(chainID uint64) (string, bool) {
	switch chainID {
	case 8453:
		return "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", true
	case 84532:
		return "0x036CbD53842c5426634e7929541eC2318f3dCF7e", true
	}
	return "", false
}

// walletRPCURL is a public read-only Base RPC. TESSERA_WALLET_RPC_URL overrides
// it for tests or a private endpoint.
func walletRPCURL() string {
	if v := strings.TrimSpace(os.Getenv("TESSERA_WALLET_RPC_URL")); v != "" {
		return v
	}
	return "https://mainnet.base.org"
}

// rpcResult performs one JSON-RPC call and returns its result document. Only
// public chain data is sent; no credential is attached.
func rpcResult(method string, params []any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, walletRPCURL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "tessera-cli/1.0")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("rpc returned invalid JSON: %w", err)
	}
	if out.Error != nil {
		return nil, errors.New(out.Error.Message)
	}
	return out.Result, nil
}

// usdcBalance reads USDC balanceOf for address over the configured RPC. The
// bool is false when the chain has no known USDC contract, in which case the
// chain id is still returned for the message.
func usdcBalance(address string) (balance string, chainID uint64, supported bool, err error) {
	chainRaw, err := rpcResult("eth_chainId", []any{})
	if err != nil {
		return "", 0, false, err
	}
	var chainHex string
	if err := json.Unmarshal(chainRaw, &chainHex); err != nil {
		return "", 0, false, fmt.Errorf("rpc chain id: %w", err)
	}
	chainID, err = strconv.ParseUint(strings.TrimPrefix(chainHex, "0x"), 16, 64)
	if err != nil {
		return "", 0, false, fmt.Errorf("rpc chain id %q: %w", chainHex, err)
	}
	contract, ok := usdcContractForChain(chainID)
	if !ok {
		return "", chainID, false, nil
	}
	addr, err := addressBytes(address)
	if err != nil {
		return "", chainID, false, err
	}
	data := "0x70a08231" + hex.EncodeToString(encodeAddressWord(addr))
	callRaw, err := rpcResult("eth_call", []any{
		map[string]any{"to": contract, "data": data},
		"latest",
	})
	if err != nil {
		return "", chainID, false, err
	}
	var result string
	if err := json.Unmarshal(callRaw, &result); err != nil {
		return "", chainID, false, fmt.Errorf("rpc balance: %w", err)
	}
	atomic, ok := new(big.Int).SetString(strings.TrimPrefix(result, "0x"), 16)
	if !ok {
		return "", chainID, false, fmt.Errorf("rpc balance %q is not hex", result)
	}
	return formatUSDC(atomic), chainID, true, nil
}

// formatUSDC renders a 6-decimal atomic amount as dollars.
func formatUSDC(atomic *big.Int) string {
	if atomic == nil || atomic.Sign() == 0 {
		return "0.000000"
	}
	neg := atomic.Sign() < 0
	abs := new(big.Int).Abs(atomic)
	whole := new(big.Int).Quo(abs, big.NewInt(1_000_000))
	frac := new(big.Int).Mod(abs, big.NewInt(1_000_000))
	out := fmt.Sprintf("%s.%06d", whole.String(), frac.Int64())
	if neg {
		out = "-" + out
	}
	return out
}
