package main

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// Vectors produced by the server's Python stack (eth-account 0.14.0) for the
// same key, domain and authorization. They pin the EIP-712 encoding exactly:
// a wrong type hash, field order, padding or domain field changes the digest,
// and a wrong digest changes every signature byte.
const (
	vectorKeyHex    = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"
	vectorAddress   = "0x2c7536E3605D9C16a7a3D7b1898e529396a65c23"
	vectorDigestHex = "7a3dcd1577695b822fb036932ca0a7d9540d91aa0f632c586b25a21b5d9d3eef"
	vectorSigHex    = "b19377a2680152d181e405ed0db5f285fda439e7a76b146a5656a2d9a24fd24b" +
		"2e89f0913281e9f599c9dc19476d51690dbf4dcc306024db4e20947e817cb3721c"
	vectorNonce = "abababababababababababababababababababababababababababababababab"
)

func vectorParts(t *testing.T) (key *secp256k1.PrivateKey, domain, digest, nonce []byte, from, to []byte, value, validAfter, validBefore *big.Int) {
	t.Helper()
	raw, err := hex.DecodeString(vectorKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	key = secp256k1.PrivKeyFromBytes(raw)
	domain = eip712DomainSeparator(
		"USDC", "2", 84532,
		mustHex(t, "036CbD53842c5426634e7929541eC2318f3dCF7e"),
	)
	from = mustHex(t, vectorAddress)
	to = mustHex(t, "1111111111111111111111111111111111111111")
	nonce = mustHex(t, vectorNonce)
	value = big.NewInt(5000000)
	validAfter = big.NewInt(0)
	validBefore = big.NewInt(1893456000)
	digest = transferAuthorizationDigest(domain, from, to, value, validAfter, validBefore, nonce)
	return
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := decodeHex(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWalletAddressMatchesPython(t *testing.T) {
	raw, _ := hex.DecodeString(vectorKeyHex)
	key := secp256k1.PrivKeyFromBytes(raw)
	if got := walletAddress(key); got != vectorAddress {
		t.Fatalf("walletAddress = %s, want %s", got, vectorAddress)
	}
}

func TestTransferAuthorizationDigestMatchesPython(t *testing.T) {
	_, _, digest, _, _, _, _, _, _ := vectorParts(t)
	if got := hex.EncodeToString(digest); got != vectorDigestHex {
		t.Fatalf("digest = %s, want %s", got, vectorDigestHex)
	}
}

func TestSignDigestMatchesPython(t *testing.T) {
	key, _, digest, _, _, _, _, _, _ := vectorParts(t)
	sig, err := signDigest(key, digest)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(sig); got != vectorSigHex {
		t.Fatalf("signature = %s, want %s", got, vectorSigHex)
	}
	if sig[64] != 27 && sig[64] != 28 {
		t.Fatalf("v = %d, want 27 or 28", sig[64])
	}
}

func TestChecksumAddressKnownVector(t *testing.T) {
	// EIP-55's own reference vector.
	addr := mustHex(t, "5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed")
	if got := checksumAddress(addr); got != "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed" {
		t.Fatalf("checksumAddress = %s", got)
	}
}
