package transcode

import (
	"fmt"
	"strings"
)

// Playlist renders a complete VOD playlist from the probed duration alone,
// before a single frame has been encoded. The player therefore sees an
// ordinary finite asset and will seek anywhere in it; the segment endpoint
// turns that seek into "does this file exist yet?".
func Playlist(durationSec float64, segmentDuration int, segURL func(seq int) string) string {
	n := SegmentCount(durationSec, segmentDuration)

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", segmentDuration)
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")

	full := float64(segmentDuration)
	for seq := range n {
		d := full
		if seq == n-1 {
			d = durationSec - float64(n-1)*full
			// Guard against a duration that is an exact multiple, and against
			// float noise producing a zero-length final segment.
			if d <= 0.001 || d > full {
				d = full
			}
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n", d, segURL(seq))
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}
