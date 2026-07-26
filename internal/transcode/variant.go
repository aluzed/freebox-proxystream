package transcode

import (
	"fmt"
	"math"
	"strings"

	"github.com/apenombre/freebox-proxystream/internal/config"
	"github.com/apenombre/freebox-proxystream/internal/db"
)

// Variant is a fully resolved output configuration: the thing a cache
// directory and an ffmpeg invocation are derived from.
type Variant struct {
	Height     int // effective encode height, never above the source
	AudioIndex int // relative index, as used in `-map 0:a:N`
	Maxrate    string
	Bufsize    string
	Encoder    Encoder
	Key        string
}

// EffectiveHeight clamps a requested height to the source and rounds down to
// an even value. Upscaling is never done: a 480p source stays 480p, which also
// means the 720p and 1080p requests share one cache entry for it.
func EffectiveHeight(requested int, source *int) int {
	h := requested
	if source != nil && *source > 0 && *source < h {
		h = *source
	}
	if h%2 != 0 {
		h--
	}
	if h < 2 {
		h = 2
	}
	return h
}

// ResolveVariant maps a request (quality height + audio track) onto a Variant.
func ResolveVariant(cfg *config.Config, m *db.Media, requestedHeight, audioIndex int) Variant {
	q := cfg.Quality(requestedHeight)
	h := EffectiveHeight(q.Height, m.Height)
	if audioIndex < 0 {
		audioIndex = 0
	}
	// An unknown encoder was already rejected at startup; fall back rather
	// than fail a playback request.
	enc, err := LookupEncoder(cfg.VideoEncoder)
	if err != nil {
		enc = encoderX264
	}

	return Variant{
		Height:     h,
		AudioIndex: audioIndex,
		Maxrate:    q.Maxrate,
		Bufsize:    q.Bufsize,
		Encoder:    enc,
		Key:        variantKey(enc, h, cfg.X264CRF, cfg.AudioBitrate, audioIndex),
	}
}

// variantKey identifies an output configuration. The encoder is part of it:
// x264 and NVENC at the same settings produce different bitstreams, and
// letting them share a cache directory would stitch mismatched segments
// together. Bumped to v2 when the encoder became selectable.
func variantKey(enc Encoder, height, crf int, audioBitrate string, audioIndex int) string {
	return fmt.Sprintf("v2_%s_%dp_crf%d_aac%s_a%d",
		enc.Tag, height, crf, strings.TrimSuffix(audioBitrate, "k"), audioIndex)
}

// DefaultAudioIndex picks the first track matching the configured language,
// else the first track.
func DefaultAudioIndex(m *db.Media, lang string) int {
	want := strings.ToLower(lang)
	if i := strings.IndexAny(want, "-_"); i > 0 {
		want = want[:i]
	}
	for _, a := range m.AudioStreams {
		if matchesLang(a.Lang, want) {
			return a.AudioIndex
		}
	}
	if len(m.AudioStreams) > 0 {
		return m.AudioStreams[0].AudioIndex
	}
	return 0
}

// matchesLang compares an ISO 639-1 or 639-2 tag loosely, since containers use
// both ("fr", "fre", "fra").
func matchesLang(streamLang, want string) bool {
	s := strings.ToLower(strings.TrimSpace(streamLang))
	if s == "" || want == "" {
		return false
	}
	if s == want || strings.HasPrefix(s, want) {
		return true
	}
	alias := map[string][]string{
		"fr": {"fre", "fra", "french"},
		"en": {"eng", "english"},
		"es": {"spa", "esp", "spanish"},
		"de": {"ger", "deu", "german"},
		"it": {"ita", "italian"},
		"pt": {"por", "portuguese"},
		"ja": {"jpn", "japanese"},
		"nl": {"dut", "nld", "dutch"},
	}
	for _, a := range alias[want] {
		if s == a {
			return true
		}
	}
	return false
}

// SegmentCount is derived from duration alone, before anything is encoded.
func SegmentCount(durationSec float64, segmentDuration int) int {
	if durationSec <= 0 || segmentDuration <= 0 {
		return 0
	}
	return int(math.Ceil(durationSec / float64(segmentDuration)))
}
