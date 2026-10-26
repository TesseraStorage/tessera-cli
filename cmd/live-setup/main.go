package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	siastorage "go.sia.tech/siastorage"
)

// One-off live-account setup helper for the Phase 0 experiment. It completes
// the same headless connection approval the CLI's login flow performs, using a
// connect key minted by moneyd's admin demo endpoint.
func main() {
	indexer := os.Getenv("TESSERA_LIVE_INDEXER")
	connectKey := os.Getenv("TESSERA_LIVE_CONNECT_KEY")
	out := os.Getenv("TESSERA_LIVE_OUT")
	if indexer == "" || connectKey == "" || out == "" {
		fmt.Fprintln(os.Stderr, "TESSERA_LIVE_INDEXER, TESSERA_LIVE_CONNECT_KEY and TESSERA_LIVE_OUT are required")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	appID := siastorage.GenerateAppID()
	builder := siastorage.NewBuilder(indexer, siastorage.AppMetadata{
		ID:          appID,
		Name:        "tessera live experiment",
		Description: "Phase 0 packed-upload verification",
		ServiceURL:  indexer,
	})
	respURL, err := builder.RequestConnection(ctx)
	if err != nil {
		fatal("request connection: %v", err)
	}

	body, _ := json.Marshal(map[string]bool{"approve": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, respURL, bytes.NewReader(body))
	if err != nil {
		fatal("approve request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("", connectKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fatal("approve: %v", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fatal("approve: HTTP %d", resp.StatusCode)
	}
	if err := builder.WaitForApproval(ctx); err != nil {
		fatal("wait for approval: %v", err)
	}
	sdk, err := builder.Register(ctx, siastorage.NewSeedPhrase())
	if err != nil {
		fatal("register: %v", err)
	}
	defer sdk.Close()

	result := map[string]string{
		"app_id":  appID.String(),
		"app_key": hex.EncodeToString(sdk.AppKey()),
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	if err := os.WriteFile(out, data, 0600); err != nil {
		fatal("write %s: %v", out, err)
	}
	fmt.Printf("wrote app credentials for %s\n", appID.String())
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
