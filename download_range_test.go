package main

import (
	"strings"
	"testing"

	siastorage "go.sia.tech/siastorage"
)

func TestParseDownloadRange(t *testing.T) {
	cases := []struct {
		args    []string
		want    byteRange
		wantErr string
	}{
		{args: []string{"a.mp4"}, want: byteRange{}},
		{args: []string{"a.mp4", "--offset", "100"}, want: byteRange{Set: true, Offset: 100}},
		{args: []string{"a.mp4", "--length=50"}, want: byteRange{Set: true, Length: 50}},
		{args: []string{"a.mp4", "--offset=0", "--length", "1"}, want: byteRange{Set: true, Offset: 0, Length: 1}},
		{args: []string{"a.mp4", "--offset", "-5"}, wantErr: "must not be negative"},
		{args: []string{"a.mp4", "--length", "-1"}, wantErr: "must not be negative"},
		{args: []string{"a.mp4", "--offset", "abc"}, wantErr: "whole number"},
		{args: []string{"a.mp4", "--offset", "1.5"}, wantErr: "whole number"},
		{args: []string{"a.mp4", "--length", "0"}, wantErr: "at least 1 byte"},
		{args: []string{"a.mp4", "--offset"}, wantErr: "needs a value"},
		{args: []string{"a.mp4", "--length="}, wantErr: "needs a value"},
	}
	for _, c := range cases {
		got, err := parseDownloadRange(c.args)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%v: err = %v, want %q", c.args, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v: unexpected err %v", c.args, err)
			continue
		}
		if got != c.want {
			t.Errorf("%v: got %+v, want %+v", c.args, got, c.want)
		}
	}
}

func TestRangeFlagsAreNotPositional(t *testing.T) {
	pos := positional([]string{"video.mp4", "--offset", "100", "--length", "200", "--out", "x"})
	if len(pos) != 1 || pos[0] != "video.mp4" {
		t.Fatalf("positional = %v, want [video.mp4]", pos)
	}
}

func TestByteRangeResolve(t *testing.T) {
	cases := []struct {
		in      byteRange
		size    int64
		want    byteRange
		clamped bool
		wantErr string
	}{
		{in: byteRange{}, size: 10, want: byteRange{}},
		{in: byteRange{Set: true, Offset: 2}, size: 10, want: byteRange{Set: true, Offset: 2, Length: 8}},
		{in: byteRange{Set: true, Length: 4}, size: 10, want: byteRange{Set: true, Length: 4}},
		{in: byteRange{Set: true, Offset: 8, Length: 5}, size: 10, want: byteRange{Set: true, Offset: 8, Length: 2}, clamped: true},
		{in: byteRange{Set: true, Offset: 9, Length: 1}, size: 10, want: byteRange{Set: true, Offset: 9, Length: 1}},
		{in: byteRange{Set: true, Offset: 10}, size: 10, wantErr: "past the end"},
		{in: byteRange{Set: true, Offset: 0}, size: 0, wantErr: "past the end"},
	}
	for _, c := range cases {
		got, clamped, err := c.in.resolve(c.size)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%+v/%d: err = %v, want %q", c.in, c.size, err, c.wantErr)
			}
			continue
		}
		if err != nil || got != c.want || clamped != c.clamped {
			t.Errorf("%+v/%d: got %+v clamped=%v err=%v, want %+v clamped=%v", c.in, c.size, got, clamped, err, c.want, c.clamped)
		}
	}
}

// TestDownloadOptionsPassRange confirms the SDK receives WithDownloadRange with
// the resolved offset/length, and nothing for a full download.
func TestDownloadOptionsPassRange(t *testing.T) {
	var calls [][2]uint64
	orig := withDownloadRange
	withDownloadRange = func(off, n uint64) siastorage.DownloadOption {
		calls = append(calls, [2]uint64{off, n})
		return orig(off, n)
	}
	defer func() { withDownloadRange = orig }()

	if opts := (byteRange{}).downloadOptions(); len(opts) != 0 || len(calls) != 0 {
		t.Fatalf("full download: opts=%d calls=%v, want none", len(opts), calls)
	}
	r, _, err := byteRange{Set: true, Offset: 41943040, Length: 1000}.resolve(100 << 20)
	if err != nil {
		t.Fatal(err)
	}
	opts := r.downloadOptions()
	if len(opts) != 1 || len(calls) != 1 || calls[0] != [2]uint64{41943040, 1000} {
		t.Fatalf("range: opts=%d calls=%v", len(opts), calls)
	}
}

func TestDefaultRangeName(t *testing.T) {
	got := defaultRangeName("videos/a.mp4", byteRange{Set: true, Offset: 100, Length: 100})
	if got != "a.mp4.bytes-100-199" {
		t.Fatalf("got %q", got)
	}
}

func TestDownloadHelpMentionsRange(t *testing.T) {
	h := commandHelp["download"]
	for _, want := range []string{"--offset <bytes>", "--length <bytes>", "only the slabs covering", "restart from the beginning", "resume"} {
		if !strings.Contains(h, want) {
			t.Errorf("download help missing %q", want)
		}
	}
}
