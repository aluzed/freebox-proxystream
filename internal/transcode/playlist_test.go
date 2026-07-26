package transcode

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func segURL(seq int) string { return fmt.Sprintf("seg_%05d.ts", seq) }

func TestPlaylistIsCompleteVODBeforeAnyEncoding(t *testing.T) {
	// 5349.48s / 4 = 1337.37 -> 1338 segments, last one 1.48s long.
	pl := Playlist(5349.48, 4, segURL)

	for _, want := range []string{
		"#EXTM3U",
		"#EXT-X-VERSION:3",
		"#EXT-X-TARGETDURATION:4",
		"#EXT-X-MEDIA-SEQUENCE:0",
		"#EXT-X-PLAYLIST-TYPE:VOD",
		"#EXT-X-ENDLIST",
	} {
		if !strings.Contains(pl, want) {
			t.Errorf("playlist missing %q", want)
		}
	}

	lines := strings.Split(strings.TrimSpace(pl), "\n")
	var segs []string
	var infs []string
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "#EXTINF:"):
			infs = append(infs, strings.TrimSuffix(strings.TrimPrefix(l, "#EXTINF:"), ","))
		case !strings.HasPrefix(l, "#"):
			segs = append(segs, l)
		}
	}

	if len(segs) != 1338 {
		t.Fatalf("got %d segments, want 1338", len(segs))
	}
	if segs[0] != "seg_00000.ts" || segs[1337] != "seg_01337.ts" {
		t.Errorf("first=%q last=%q", segs[0], segs[1337])
	}
	if infs[0] != "4.000" {
		t.Errorf("first EXTINF = %q, want 4.000", infs[0])
	}
	if infs[1337] != "1.480" {
		t.Errorf("last EXTINF = %q, want 1.480", infs[1337])
	}

	// The declared durations must add back up to the source duration, or the
	// player's seek target maps to the wrong segment.
	var total float64
	for _, s := range infs {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			t.Fatal(err)
		}
		total += v
	}
	if diff := total - 5349.48; diff > 0.01 || diff < -0.01 {
		t.Errorf("durations sum to %.3f, want 5349.48", total)
	}
}

func TestPlaylistExactMultipleHasNoZeroLengthTail(t *testing.T) {
	pl := Playlist(40, 4, segURL)
	if strings.Contains(pl, "#EXTINF:0.000") {
		t.Error("playlist contains a zero-length segment")
	}
	if n := strings.Count(pl, "#EXTINF:"); n != 10 {
		t.Errorf("got %d segments, want 10", n)
	}
	if !strings.HasSuffix(strings.TrimSpace(pl), "#EXT-X-ENDLIST") {
		t.Error("playlist does not end with ENDLIST")
	}
}

func TestPlaylistZeroDuration(t *testing.T) {
	pl := Playlist(0, 4, segURL)
	if strings.Count(pl, "#EXTINF:") != 0 {
		t.Error("zero duration produced segments")
	}
	if !strings.Contains(pl, "#EXT-X-ENDLIST") {
		t.Error("missing ENDLIST")
	}
}
