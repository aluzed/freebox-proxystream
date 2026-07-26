package transcode

import (
	"strings"
	"testing"

	"github.com/aluzed/freebox-proxystream/internal/config"
	"github.com/aluzed/freebox-proxystream/internal/db"
)

func testConfig() *config.Config {
	return &config.Config{
		SegmentDuration: 4,
		X264CRF:         23,
		AudioBitrate:    "128k",
		DefaultHeight:   720,
		VideoEncoder:    "libx264",
		Qualities: []config.Quality{
			{Height: 720, Label: "720p", Maxrate: "3M", Bufsize: "6M"},
			{Height: 1080, Label: "1080p", Maxrate: "6M", Bufsize: "12M"},
		},
	}
}

func intp(v int) *int { return &v }

func TestNeverUpscales(t *testing.T) {
	cases := []struct {
		name      string
		requested int
		source    *int
		want      int
	}{
		{"4K source down to 720", 720, intp(2160), 720},
		{"4K source down to 1080", 1080, intp(2160), 1080},
		{"1080 source stays 1080", 1080, intp(1080), 1080},
		{"480p source stays 480p at 720", 720, intp(480), 480},
		{"480p source stays 480p at 1080", 1080, intp(480), 480},
		{"576p source stays 576p", 1080, intp(576), 576},
		{"odd source height rounded down", 720, intp(541), 540},
		{"unknown source height uses request", 720, nil, 720},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveHeight(tc.requested, tc.source); got != tc.want {
				t.Errorf("EffectiveHeight(%d, %v) = %d, want %d", tc.requested, tc.source, got, tc.want)
			}
		})
	}
}

// Nothing restricts how big a source may be. An oversized file is ordinary
// input that simply comes out downscaled: the cap is on the encode, not on what
// you are allowed to keep in the library.
func TestOversizedSourcesAreDownscaledNotRefused(t *testing.T) {
	cfg := testConfig()

	for _, sourceHeight := range []int{1440, 2160, 4320} {
		m := &db.Media{Height: intp(sourceHeight)}

		if got := ResolveVariant(cfg, m, 720, 0).Height; got != 720 {
			t.Errorf("%dp source at 720p came out at %dp, want 720", sourceHeight, got)
		}
		if got := ResolveVariant(cfg, m, 1080, 0).Height; got != 1080 {
			t.Errorf("%dp source at 1080p came out at %dp, want 1080", sourceHeight, got)
		}
	}
}

// A source smaller than both renditions produces one encode, not two identical
// ones under different labels.
func TestSmallSourceSharesOneVariantKey(t *testing.T) {
	cfg := testConfig()
	m := &db.Media{Height: intp(480)}

	at720 := ResolveVariant(cfg, m, 720, 0)
	at1080 := ResolveVariant(cfg, m, 1080, 0)

	if at720.Key != at1080.Key {
		t.Errorf("480p source produced two cache keys: %q and %q", at720.Key, at1080.Key)
	}
	if !strings.Contains(at720.Key, "480p") {
		t.Errorf("key = %q, want it to mention the effective height", at720.Key)
	}
}

func TestVariantKeyShape(t *testing.T) {
	cfg := testConfig()
	m := &db.Media{Height: intp(2160)}

	v := ResolveVariant(cfg, m, 720, 2)
	if v.Key != "v2_x264_720p_crf23_aac128_a2" {
		t.Errorf("key = %q, want v2_x264_720p_crf23_aac128_a2", v.Key)
	}
	if v.Maxrate != "3M" || v.Bufsize != "6M" {
		t.Errorf("720p rate = %s/%s, want 3M/6M", v.Maxrate, v.Bufsize)
	}

	v = ResolveVariant(cfg, m, 1080, 0)
	if v.Key != "v2_x264_1080p_crf23_aac128_a0" {
		t.Errorf("key = %q", v.Key)
	}
	if v.Maxrate != "6M" || v.Bufsize != "12M" {
		t.Errorf("1080p rate = %s/%s, want 6M/12M", v.Maxrate, v.Bufsize)
	}
}

// Anything outside the two offered renditions falls back to the default rather
// than 404ing, so a stale bookmark still plays.
func TestUnknownQualityFallsBackToDefault(t *testing.T) {
	cfg := testConfig()
	m := &db.Media{Height: intp(2160)}
	for _, requested := range []int{0, 480, 1440, 2160, 4320} {
		v := ResolveVariant(cfg, m, requested, 0)
		if v.Height != 720 {
			t.Errorf("requested %d: height = %d, want the 720 default", requested, v.Height)
		}
	}
}

func TestDefaultAudioIndexPrefersConfiguredLanguage(t *testing.T) {
	m := &db.Media{AudioStreams: []db.AudioStream{
		{AudioIndex: 0, Lang: "eng"},
		{AudioIndex: 1, Lang: "fre"},
		{AudioIndex: 2, Lang: "spa"},
	}}
	if got := DefaultAudioIndex(m, "fr-FR"); got != 1 {
		t.Errorf("got audio %d, want 1 (fre)", got)
	}
	if got := DefaultAudioIndex(m, "en-US"); got != 0 {
		t.Errorf("got audio %d, want 0 (eng)", got)
	}
	// No match falls back to the first track.
	if got := DefaultAudioIndex(m, "ja-JP"); got != 0 {
		t.Errorf("got audio %d, want 0", got)
	}
	// No audio at all must not panic.
	if got := DefaultAudioIndex(&db.Media{}, "fr-FR"); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestSegmentCount(t *testing.T) {
	cases := []struct {
		duration float64
		want     int
	}{
		{0, 0},
		{1, 1},
		{4, 1},
		{4.001, 2},
		{5349.48, 1338},
	}
	for _, tc := range cases {
		if got := SegmentCount(tc.duration, 4); got != tc.want {
			t.Errorf("SegmentCount(%v) = %d, want %d", tc.duration, got, tc.want)
		}
	}
}
