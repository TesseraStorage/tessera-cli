package main

import (
	"encoding/hex"
	"errors"
	"strings"

	"go.sia.tech/core/types"
)

// Host preflight probes were removed. They masked Sia network reality
// (unreachable hosts, async funding) and added latency plus extra host load.
// Transfers now surface the SDK result directly. Payment-required errors are
// still rewritten into top-up guidance.

func isHostShortageError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no more hosts") || strings.Contains(s, "not enough shards") ||
		strings.Contains(s, "not enough hosts") || strings.Contains(s, "too few shards")
}

// explainTransferError maps a payment-required failure to top-up guidance.
// Host-shortage and all other errors pass through unchanged.
func explainTransferError(cfg *Config, err error, upload bool) error {
	if _, ok := asPaymentRequired(err); ok {
		return explainPaymentRequired(err)
	}
	if pe, ok := shortageAfterPaymentRequired(err); ok {
		return pe
	}
	return err
}

// appKeyPublicHex returns the hex public key for a stored app key, or "" when
// the key is malformed.
func appKeyPublicHex(appKeyHex string) string {
	b, err := hex.DecodeString(appKeyHex)
	if err != nil || len(b) != 64 {
		return ""
	}
	pk := types.PrivateKey(b).PublicKey()
	return hex.EncodeToString(pk[:])
}

// explainSyncTransferError maps a payment-required sync failure to top-up
// guidance. Other errors pass through.
func explainSyncTransferError(err error, upload bool) error {
	if _, ok := asPaymentRequired(err); ok {
		return explainPaymentRequired(err)
	}
	if pe, ok := shortageAfterPaymentRequired(err); ok {
		return pe
	}
	return err
}

// explainFatalHostShortage rewrites a payment-required fatal message into
// top-up guidance. Other messages pass through.
func explainFatalHostShortage(msg string) string {
	if pe, ok := asPaymentRequired(errors.New(msg)); ok {
		return pe.Error()
	}
	return msg
}
