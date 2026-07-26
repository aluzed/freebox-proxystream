// Package probe runs ffprobe against the range proxy. Going through the proxy
// means ffprobe reads the header and the index only, never the whole file.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/aluzed/freebox-proxystream/internal/db"
)

const Timeout = 60 * time.Second

// Failure separates "this file is not a video" from "the network hiccupped".
// Only the second is worth retrying, and guessing from the error text later is
// fragile, so the distinction is made here where the cause is known.
type Failure struct {
	Err       error
	Permanent bool
}

func (f *Failure) Error() string { return f.Err.Error() }
func (f *Failure) Unwrap() error { return f.Err }

func permanent(format string, args ...any) error {
	return &Failure{Err: fmt.Errorf(format, args...), Permanent: true}
}

func transient(format string, args ...any) error {
	return &Failure{Err: fmt.Errorf(format, args...), Permanent: false}
}

// IsPermanent reports whether retrying is pointless.
func IsPermanent(err error) bool {
	var f *Failure
	if errors.As(err, &f) {
		return f.Permanent
	}
	// Unclassified failures get the benefit of the doubt and are retried.
	return false
}

// ffprobe's own words for a file it cannot make sense of, as opposed to one it
// could not reach.
var permanentSigns = []string{
	"invalid data found when processing input",
	"moov atom not found",
	"unknown format",
	"could not find codec parameters",
	"end of file",
	"decoder not found",
}

func classify(stderr string) bool {
	s := strings.ToLower(stderr)
	for _, sign := range permanentSigns {
		if strings.Contains(s, sign) {
			return true
		}
	}
	return false
}

type rawStream struct {
	Index        int               `json:"index"`
	CodecName    string            `json:"codec_name"`
	CodecType    string            `json:"codec_type"`
	Width        int               `json:"width"`
	Height       int               `json:"height"`
	Channels     int               `json:"channels"`
	AvgFrameRate string            `json:"avg_frame_rate"`
	RFrameRate   string            `json:"r_frame_rate"`
	Disposition  map[string]int    `json:"disposition"`
	Tags         map[string]string `json:"tags"`
}

type rawFormat struct {
	Duration string            `json:"duration"`
	Tags     map[string]string `json:"tags"`
}

type rawProbe struct {
	Streams []rawStream `json:"streams"`
	Format  rawFormat   `json:"format"`
}

// Bitmap subtitles cannot be rendered by any browser. They are surfaced in the
// API flagged for burn-in and disabled in the UI rather than silently dropped.
var textSubCodecs = map[string]bool{
	"subrip": true, "ass": true, "ssa": true, "mov_text": true,
	"webvtt": true, "text": true, "srt": true,
}

func IsTextSubtitle(codec string) bool { return textSubCodecs[strings.ToLower(codec)] }

// Run probes a URL (in practice the loopback range proxy) and maps the result
// onto the columns the media table stores.
func Run(ctx context.Context, ffprobeBin, url string) (*db.ProbeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffprobeBin,
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		url,
	)
	out, err := cmd.Output()
	if err != nil {
		// A timeout is the network being slow, not the file being broken.
		if ctx.Err() != nil {
			return nil, transient("ffprobe timed out after %s", Timeout)
		}
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			msg := strings.TrimSpace(string(ee.Stderr))
			if classify(msg) {
				return nil, permanent("ffprobe rejected this file: %s", msg)
			}
			return nil, transient("ffprobe failed: %v: %s", err, msg)
		}
		return nil, transient("ffprobe failed: %w", err)
	}

	var raw rawProbe
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, permanent("ffprobe returned output we cannot parse: %v", err)
	}
	return mapProbe(&raw)
}

func mapProbe(raw *rawProbe) (*db.ProbeResult, error) {
	res := &db.ProbeResult{
		Audio: []db.AudioStream{},
		Subs:  []db.SubStream{},
	}
	if raw.Format.Duration != "" {
		d, err := strconv.ParseFloat(raw.Format.Duration, 64)
		if err == nil {
			res.DurationSec = d
		}
	}

	var audioIdx, subIdx int
	videoSeen := false
	for _, s := range raw.Streams {
		switch s.CodecType {
		case "video":
			// Cover art is stored as a video stream; skip anything that is not
			// the first real one.
			if videoSeen || s.Disposition["attached_pic"] == 1 {
				continue
			}
			videoSeen = true
			res.VideoCodec = s.CodecName
			res.Width = s.Width
			res.Height = s.Height
			res.FPS = parseRate(s.AvgFrameRate, s.RFrameRate)
		case "audio":
			res.Audio = append(res.Audio, db.AudioStream{
				Index:      s.Index,
				AudioIndex: audioIdx,
				Codec:      s.CodecName,
				Lang:       s.Tags["language"],
				Channels:   s.Channels,
				Title:      s.Tags["title"],
			})
			audioIdx++
		case "subtitle":
			res.Subs = append(res.Subs, db.SubStream{
				Index:          s.Index,
				SubIndex:       subIdx,
				Codec:          s.CodecName,
				Lang:           s.Tags["language"],
				Title:          s.Tags["title"],
				Forced:         s.Disposition["forced"] == 1,
				BurnInRequired: !IsTextSubtitle(s.CodecName),
			})
			subIdx++
		}
	}

	// Both of these are properties of the file, so retrying changes nothing.
	if !videoSeen {
		return nil, permanent("no video stream in this file")
	}
	if res.DurationSec <= 0 {
		return nil, permanent("no usable duration in this file")
	}
	return res, nil
}

// parseRate reads ffprobe's "num/den" rate strings, preferring the average.
func parseRate(candidates ...string) float64 {
	for _, c := range candidates {
		num, den, ok := strings.Cut(c, "/")
		if !ok {
			continue
		}
		n, err1 := strconv.ParseFloat(num, 64)
		d, err2 := strconv.ParseFloat(den, 64)
		if err1 != nil || err2 != nil || d == 0 || n == 0 {
			continue
		}
		return n / d
	}
	return 0
}

func asExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}
