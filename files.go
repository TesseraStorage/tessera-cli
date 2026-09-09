package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.sia.tech/indexd/slabs"
	siastorage "go.sia.tech/siastorage"
)

const (
	dataShards   = 10
	parityShards = 20
)

func cmdList() {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client, appKey, err := connectAPI(cfg)
	if err != nil {
		fatal("%v", err)
	}

	type row struct {
		name      string
		size      uint64
		updatedAt time.Time
	}
	var rows []row

	var cursor slabs.Cursor
	for {
		evs, err := client.ListObjects(ctx, appKey, cursor, 100)
		if err != nil {
			fatal("list: %v", err)
		}
		if len(evs) == 0 {
			break
		}
		for _, ev := range evs {
			if ev.Deleted {
				continue
			}
			name := ev.Key.String()[:12] + "..."
			var sz uint64
			if ev.Object != nil {
				for _, ss := range ev.Object.Slabs {
					sz += uint64(ss.Length)
				}
			}
			rows = append(rows, row{name: name, size: sz, updatedAt: ev.UpdatedAt})
		}
		last := evs[len(evs)-1]
		next := slabs.Cursor{Key: last.Key, After: last.UpdatedAt}
		if sameCursor(cursor, next) {
			break
		}
		cursor = next
	}

	if len(rows) == 0 {
		fmt.Println("No files.")
		return
	}

	fmt.Printf("%-36s  %10s  %s\n", "NAME", "SIZE", "DATE")
	fmt.Println(strings.Repeat("-", 70))
	for _, r := range rows {
		fmt.Printf("%-36s  %10s  %s\n", r.name, formatBytes(r.size), r.updatedAt.Format("2006-01-02 15:04"))
	}
}

func cmdUpload(path string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	fi, err := os.Stat(path)
	if err != nil {
		fatal("cannot read %s: %v", path, err)
	}
	name := filepath.Base(path)

	fmt.Printf("Uploading %s (%s)...\n", name, formatBytes(uint64(fi.Size())))

	f, err := os.Open(path)
	if err != nil {
		fatal("open: %v", err)
	}
	defer f.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	sdk, cleanup, err := connectSDK(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	meta, _ := json.Marshal(map[string]string{"name": name})

	obj := siastorage.NewEmptyObject()
	obj.UpdateMetadata(meta)

	start := time.Now()
	err = sdk.Upload(ctx, &obj, f,
		siastorage.WithRedundancy(dataShards, parityShards),
	)
	if err != nil {
		fatal("upload: %v", err)
	}

	fmt.Print("Pinning... ")
	if err := sdk.PinObject(ctx, obj); err != nil {
		fatal("pin: %v", err)
	}

	elapsed := time.Since(start)
	fmt.Printf("done (%.1fs)\n", elapsed.Seconds())
	fmt.Printf("Stored as: %s\n", name)
}

func cmdDownload(name string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	sdk, cleanup, err := connectSDK(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	obj, err := findObject(ctx, *sdk, name)
	if err != nil {
		fatal("%v", err)
	}

	fmt.Printf("Downloading %s (%s)...\n", name, formatBytes(obj.Size()))

	start := time.Now()
	rc, err := sdk.Download(obj)
	if err != nil {
		fatal("download: %v", err)
	}
	defer rc.Close()

	out, err := os.Create(name)
	if err != nil {
		fatal("create %s: %v", name, err)
	}
	defer out.Close()

	hasher := sha256.New()
	tee := io.TeeReader(rc, hasher)
	n, err := io.Copy(out, tee)
	if err != nil {
		fatal("read: %v", err)
	}

	elapsed := time.Since(start)
	speed := float64(n) / elapsed.Seconds() / (1 << 20)
	fmt.Printf("Downloaded %s in %.1fs (%.2f MB/s)\n", formatBytes(uint64(n)), elapsed.Seconds(), speed)
	fmt.Printf("SHA-256: %s\n", hex.EncodeToString(hasher.Sum(nil)))
	fmt.Printf("Saved as: %s\n", name)
}

func cmdDelete(name string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sdk, cleanup, err := connectSDK(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	obj, err := findObject(ctx, *sdk, name)
	if err != nil {
		fatal("%v", err)
	}

	fmt.Printf("Delete %s? This cannot be undone. [y/N] ", name)
	var a string
	fmt.Scanln(&a)
	if strings.ToLower(a) != "y" {
		fmt.Println("Aborted.")
		return
	}

	if err := sdk.DeleteObject(ctx, obj.ID()); err != nil {
		fatal("delete: %v", err)
	}
	fmt.Printf("Deleted: %s\n", name)
}

func cmdShare(name string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sdk, cleanup, err := connectSDK(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	obj, err := findObject(ctx, *sdk, name)
	if err != nil {
		fatal("%v", err)
	}

	sharedURL, err := sdk.CreateSharedObjectURL(ctx, obj.ID(), time.Now().Add(30*24*time.Hour))
	if err != nil {
		fatal("share: %v", err)
	}

	fmt.Printf("Share link (valid 30 days):\n  %s\n", sharedURL)
	fmt.Println("Anyone with this link can download the file.")
	fmt.Println()
	fmt.Printf("Download with:  tessera fetch \"%s\" %s\n", sharedURL, name)
}

func cmdFetch(sharedURL, outName string) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("Not logged in. Run 'tessera login' first.")
	}

	// Derive a default output name from the object ID in the URL
	if outName == "" {
		parts := strings.Split(sharedURL, "/")
		for i, p := range parts {
			if p == "objects" && i+1 < len(parts) {
				outName = "download-" + parts[i+1][:12]
				break
			}
		}
	}
	if outName == "" {
		outName = "download"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	sdk, cleanup, err := connectSDK(ctx, cfg)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	fmt.Printf("Downloading from shared link...\n")

	start := time.Now()
	rc, err := sdk.DownloadSharedObject(ctx, sharedURL)
	if err != nil {
		fatal("download: %v", err)
	}
	defer rc.Close()

	out, err := os.Create(outName)
	if err != nil {
		fatal("create %s: %v", outName, err)
	}
	defer out.Close()

	hasher := sha256.New()
	tee := io.TeeReader(rc, hasher)
	n, err := io.Copy(out, tee)
	if err != nil {
		fatal("read: %v", err)
	}

	elapsed := time.Since(start)
	speed := float64(n) / elapsed.Seconds() / (1 << 20)
	fmt.Printf("Downloaded %s in %.1fs (%.2f MB/s)\n", formatBytes(uint64(n)), elapsed.Seconds(), speed)
	fmt.Printf("SHA-256: %s\n", hex.EncodeToString(hasher.Sum(nil)))
	fmt.Printf("Saved as: %s\n", outName)
}
