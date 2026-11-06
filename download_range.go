package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	siastorage "go.sia.tech/siastorage"
)

// byteRange is a partial-read request from `tessera download --offset/--length`.
// The zero value means "the whole object".
type byteRange struct {
	Set    bool   // --offset or --length was given
	Offset uint64 // first byte to read
	Length uint64 // bytes to read; 0 with Set means "to the end of the file"
}

// withDownloadRange is a seam so tests can confirm Remote.DownloadRange hands
// the range to the SDK; production uses the SDK option directly.
var withDownloadRange = siastorage.WithDownloadRange

// parseByteCount parses a non-negative decimal byte count for flag name.
func parseByteCount(name, v string) (uint64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, fmt.Errorf("%s needs a value in bytes (for example %s 1048576)", name, name)
	}
	if strings.HasPrefix(v, "-") {
		return 0, fmt.Errorf("%s must not be negative (got %s)", name, v)
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a whole number of bytes (got %q)", name, v)
	}
	return n, nil
}

// parseDownloadRange reads --offset and --length from args. Neither flag means
// a full download.
func parseDownloadRange(args []string) (byteRange, error) {
	var r byteRange
	for _, name := range []string{"--offset", "--length"} {
		if hasValueFlag(args, name) {
			v, _ := flagValue(args, name)
			n, err := parseByteCount(name, v)
			if err != nil {
				return byteRange{}, err
			}
			r.Set = true
			if name == "--offset" {
				r.Offset = n
			} else {
				if n == 0 {
					return byteRange{}, fmt.Errorf("--length must be at least 1 byte (omit it to read to the end of the file)")
				}
				r.Length = n
			}
		}
	}
	return r, nil
}

// hasValueFlag reports whether name appears as "--name value", "--name=value"
// or a bare trailing "--name".
func hasValueFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

// resolve clamps the range to an object of size bytes, as the SDK does, and
// rejects an offset at or past the end. It returns the number of bytes that
// will be read and whether the requested length was cut short.
func (r byteRange) resolve(size int64) (byteRange, bool, error) {
	if !r.Set {
		return r, false, nil
	}
	if size < 0 {
		size = 0
	}
	sz := uint64(size)
	if r.Offset >= sz {
		return r, false, fmt.Errorf("--offset %d is past the end of the file (size %d bytes; last valid offset %d)", r.Offset, sz, max(int64(sz)-1, 0))
	}
	avail := sz - r.Offset
	clamped := false
	if r.Length == 0 {
		r.Length = avail
	} else if r.Length > avail {
		r.Length = avail
		clamped = true
	}
	return r, clamped, nil
}

// end returns the exclusive end offset of a resolved range.
func (r byteRange) end() uint64 { return r.Offset + r.Length }

// downloadOptions turns a resolved range into SDK options.
func (r byteRange) downloadOptions() []siastorage.DownloadOption {
	if !r.Set {
		return nil
	}
	return []siastorage.DownloadOption{withDownloadRange(r.Offset, r.Length)}
}

// defaultRangeName is the local file name for a range read without --out, so
// a partial read never overwrites a full copy: video.mp4 -> video.mp4.bytes-100-199.
func defaultRangeName(remotePath string, r byteRange) string {
	return fmt.Sprintf("%s.bytes-%d-%d", filepath.Base(remotePath), r.Offset, r.end()-1)
}
