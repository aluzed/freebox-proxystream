package transcode_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aluzed/freebox-proxystream/internal/config"
	"github.com/aluzed/freebox-proxystream/internal/db"
	"github.com/aluzed/freebox-proxystream/internal/ftpclient"
	"github.com/aluzed/freebox-proxystream/internal/mediasource"
	"github.com/aluzed/freebox-proxystream/internal/probe"
	"github.com/aluzed/freebox-proxystream/internal/rangeproxy"
	"github.com/aluzed/freebox-proxystream/internal/testftp"
	"github.com/aluzed/freebox-proxystream/internal/transcode"
)

func requireFFmpeg(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
}

// makeSource renders a 1080p clip with 6-channel audio, so the downmix and the
// no-upscale rule can both be exercised.
func makeSource(t *testing.T, path string, seconds int) {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=1920x1080:rate=25:duration=%d", seconds),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "50",
		"-c:a", "aac", "-ac", "6",
		"-metadata:s:a:0", "language=fre",
		path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate source: %v: %s", err, out)
	}
}

type harness struct {
	cfg   *config.Config
	store *db.Store
	mgr   *transcode.Manager
	media *db.Media
}

func setup(t *testing.T, seconds int) *harness {
	t.Helper()
	requireFFmpeg(t)

	addr, root := testftp.Start(t)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "source.mkv")
	makeSource(t, src, seconds)
	testftp.Copy(t, root, "Films/Test Movie (2024).mkv", src)

	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}

	sdb, err := db.Open(filepath.Join(tmp, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sdb.Close() })
	store := db.NewStore(sdb)

	ctx := context.Background()
	lib, err := store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	if err != nil {
		t.Fatal(err)
	}
	mediaID, _, err := store.UpsertMediaFile(ctx, lib.ID, "/Films/Test Movie (2024).mkv",
		info.Size(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	// The range proxy has to be on a real port, since ffmpeg reaches it by URL.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	ftp := ftpclient.New(ftpclient.Config{Host: addr, User: testftp.User, Pass: testftp.Pass}, logger)
	t.Cleanup(func() { ftp.Close() })

	srv := &http.Server{Handler: rangeproxy.New(mediasource.New(store, ftp), logger).Handler()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	cfg := &config.Config{
		InternalAddr:             ln.Addr().String(),
		CacheDir:                 filepath.Join(tmp, "segments"),
		FFmpegBin:                "ffmpeg",
		FFprobeBin:               "ffprobe",
		SegmentDuration:          4,
		X264Preset:               "ultrafast",
		X264CRF:                  28,
		AudioBitrate:             "128k",
		DefaultHeight:            720,
		Qualities:                []config.Quality{{Height: 720, Maxrate: "3M", Bufsize: "6M"}, {Height: 1080, Maxrate: "6M", Bufsize: "12M"}},
		EncoderReadaheadSegments: 15,
		EncoderIdleTimeout:       90 * time.Second,
		MaxConcurrentEncoders:    2,
	}

	// Probe through the proxy, exactly as the scanner does.
	res, err := probe.Run(ctx, cfg.FFprobeBin, cfg.RawURL(mediaID))
	if err != nil {
		t.Fatalf("probe via range proxy: %v", err)
	}
	if err := store.SaveProbe(ctx, mediaID, *res); err != nil {
		t.Fatal(err)
	}
	media, err := store.GetMedia(ctx, mediaID)
	if err != nil {
		t.Fatal(err)
	}

	mgr := transcode.NewManager(cfg, store, logger)
	t.Cleanup(func() { mgr.Shutdown("test cleanup", 10*time.Second) })

	return &harness{cfg: cfg, store: store, mgr: mgr, media: media}
}

func TestProbeThroughRangeProxy(t *testing.T) {
	if testing.Short() {
		t.Skip("requires ffmpeg")
	}
	h := setup(t, 24)

	if h.media.Height == nil || *h.media.Height != 1080 {
		t.Errorf("height = %v, want 1080", h.media.Height)
	}
	if h.media.DurationSec == nil || *h.media.DurationSec < 23 || *h.media.DurationSec > 25 {
		t.Errorf("duration = %v, want ~24", h.media.DurationSec)
	}
	if len(h.media.AudioStreams) != 1 {
		t.Fatalf("got %d audio streams, want 1", len(h.media.AudioStreams))
	}
	if got := h.media.AudioStreams[0].Channels; got != 6 {
		t.Errorf("source channels = %d, want 6", got)
	}
	if got := h.media.AudioStreams[0].Lang; got != "fre" {
		t.Errorf("language = %q, want fre", got)
	}
}

type segInfo struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Height    int    `json:"height"`
		Channels  int    `json:"channels"`
		Profile   string `json:"profile"`
	} `json:"streams"`
	Format struct {
		Duration  string `json:"duration"`
		StartTime string `json:"start_time"`
	} `json:"format"`
}

func probeSegment(t *testing.T, path string) segInfo {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json",
		"-show_format", "-show_streams", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var si segInfo
	if err := json.Unmarshal(out, &si); err != nil {
		t.Fatal(err)
	}
	return si
}

func TestSegmentEncodingAndSeek(t *testing.T) {
	if testing.Short() {
		t.Skip("requires ffmpeg")
	}
	h := setup(t, 60)
	ctx := context.Background()
	v := transcode.ResolveVariant(h.cfg, h.media, 720, 0)

	start := time.Now()
	path, err := h.mgr.Segment(ctx, h.media, v, 0)
	if err != nil {
		t.Fatalf("segment 0: %v", err)
	}
	firstSegment := time.Since(start)
	t.Logf("first segment ready in %s", firstSegment.Round(time.Millisecond))

	si := probeSegment(t, path)
	var sawVideo, sawAudio bool
	for _, s := range si.Streams {
		switch s.CodecType {
		case "video":
			sawVideo = true
			if s.Height != 720 {
				t.Errorf("segment height = %d, want 720", s.Height)
			}
			if s.CodecName != "h264" {
				t.Errorf("video codec = %s, want h264", s.CodecName)
			}
		case "audio":
			sawAudio = true
			// Criterion 6: a 5.1 source must be downmixed, or dialogue is
			// inaudible on a phone.
			if s.Channels != 2 {
				t.Errorf("segment audio channels = %d, want 2", s.Channels)
			}
			if s.CodecName != "aac" {
				t.Errorf("audio codec = %s, want aac", s.CodecName)
			}
		}
	}
	if !sawVideo || !sawAudio {
		t.Fatalf("segment is missing streams: %+v", si.Streams)
	}

	// A cached segment must come back essentially instantly.
	start = time.Now()
	if _, err := h.mgr.Segment(ctx, h.media, v, 0); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("cached segment took %s, want under 1s", d)
	}

	// A segment inside the read-ahead window is served by the running encoder
	// rather than by restarting one.
	start = time.Now()
	if _, err := h.mgr.Segment(ctx, h.media, v, 3); err != nil {
		t.Fatalf("segment 3: %v", err)
	}
	t.Logf("read-ahead segment ready in %s", time.Since(start).Round(time.Millisecond))
}

// Seeking into a cold cache must start the encoder at the requested segment and
// encode nothing before it. This is the behaviour the whole design exists for.
func TestSeekStartsEncoderAtRequestedSegment(t *testing.T) {
	if testing.Short() {
		t.Skip("requires ffmpeg")
	}
	h := setup(t, 60)
	ctx := context.Background()
	v := transcode.ResolveVariant(h.cfg, h.media, 720, 0)

	const seekSeq = 12

	start := time.Now()
	path, err := h.mgr.Segment(ctx, h.media, v, seekSeq)
	if err != nil {
		t.Fatalf("seek to segment %d: %v", seekSeq, err)
	}
	elapsed := time.Since(start)
	t.Logf("cold seek to segment %d ready in %s", seekSeq, elapsed.Round(time.Millisecond))

	// Criterion 2: a seek into unencoded territory resumes within 8s.
	if elapsed > 8*time.Second {
		t.Errorf("cold seek took %s, want under 8s", elapsed)
	}
	if !fileHasBytes(path) {
		t.Fatalf("seek segment %s is empty", path)
	}

	for _, before := range []int{0, 5, 11} {
		if fileHasBytes(h.mgr.SegmentFile(h.media.ID, v.Key, before)) {
			t.Errorf("segment %d was encoded even though playback started at %d", before, seekSeq)
		}
	}

	// -output_ts_offset must put the segment on the source's absolute timeline,
	// so segments from different encoder runs stitch together without a
	// discontinuity when the player joins them.
	si := probeSegment(t, path)
	startTime, err := strconv.ParseFloat(si.Format.StartTime, 64)
	if err != nil {
		t.Fatalf("segment has no start_time: %q", si.Format.StartTime)
	}
	want := float64(seekSeq * h.cfg.SegmentDuration)
	if diff := startTime - want; diff > 1.0 || diff < -1.0 {
		t.Errorf("segment %d starts at %.2fs, want ~%.0fs on the source timeline",
			seekSeq, startTime, want)
	}
}

// A 1080p source asked for at 1080p must not be scaled, and must not reuse the
// 720p cache directory.
func TestQualitySwitchUsesSeparateVariants(t *testing.T) {
	if testing.Short() {
		t.Skip("requires ffmpeg")
	}
	h := setup(t, 24)
	ctx := context.Background()

	v720 := transcode.ResolveVariant(h.cfg, h.media, 720, 0)
	v1080 := transcode.ResolveVariant(h.cfg, h.media, 1080, 0)
	if v720.Key == v1080.Key {
		t.Fatal("720p and 1080p share a cache key for a 1080p source")
	}

	p720, err := h.mgr.Segment(ctx, h.media, v720, 0)
	if err != nil {
		t.Fatal(err)
	}
	p1080, err := h.mgr.Segment(ctx, h.media, v1080, 0)
	if err != nil {
		t.Fatal(err)
	}

	if got := heightOf(t, p720); got != 720 {
		t.Errorf("720p variant produced height %d", got)
	}
	if got := heightOf(t, p1080); got != 1080 {
		t.Errorf("1080p variant produced height %d, want 1080 (no upscale, no downscale)", got)
	}
}

func TestSegmentsAreRegisteredInCache(t *testing.T) {
	if testing.Short() {
		t.Skip("requires ffmpeg")
	}
	h := setup(t, 24)
	ctx := context.Background()
	v := transcode.ResolveVariant(h.cfg, h.media, 720, 0)

	if _, err := h.mgr.Segment(ctx, h.media, v, 0); err != nil {
		t.Fatal(err)
	}
	// The watcher registers asynchronously.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		total, count, err := h.store.CacheSize(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if count > 0 && total > 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Error("no segments were registered in segment_cache")
}

func heightOf(t *testing.T, path string) int {
	t.Helper()
	for _, s := range probeSegment(t, path).Streams {
		if s.CodecType == "video" {
			return s.Height
		}
	}
	return 0
}

func fileHasBytes(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

// Constructing plausible arguments is not the same as producing a playable
// segment. This runs a real hardware encode end to end, so a wrong rate-control
// flag or a bad filter chain fails here rather than in someone's living room.
func TestHardwareEncoderProducesPlayableSegments(t *testing.T) {
	if testing.Short() {
		t.Skip("requires ffmpeg")
	}
	const encoder = "h264_videotoolbox"
	if err := transcode.VerifyEncoder(context.Background(), "ffmpeg",
		mustEncoder(t, encoder)); err != nil {
		t.Skipf("%s not available here: %v", encoder, err)
	}

	h := setup(t, 24)
	h.cfg.VideoEncoder = encoder
	ctx := context.Background()

	v := transcode.ResolveVariant(h.cfg, h.media, 720, 0)
	if v.Encoder.Name != encoder {
		t.Fatalf("variant used %s", v.Encoder.Name)
	}
	if !strings.Contains(v.Key, "vt") {
		t.Errorf("cache key %q does not identify the encoder", v.Key)
	}

	path, err := h.mgr.Segment(ctx, h.media, v, 0)
	if err != nil {
		t.Fatalf("hardware encode failed: %v", err)
	}

	si := probeSegment(t, path)
	var sawVideo, sawAudio bool
	for _, s := range si.Streams {
		switch s.CodecType {
		case "video":
			sawVideo = true
			if s.CodecName != "h264" {
				t.Errorf("codec = %s, want h264", s.CodecName)
			}
			if s.Height != 720 {
				t.Errorf("height = %d, want 720", s.Height)
			}
		case "audio":
			sawAudio = true
			// The downmix must survive whichever video encoder is in use.
			if s.Channels != 2 {
				t.Errorf("channels = %d, want 2", s.Channels)
			}
		}
	}
	if !sawVideo || !sawAudio {
		t.Fatalf("segment is missing streams: %+v", si.Streams)
	}

	// Segments from a hardware encoder must sit on the same absolute timeline,
	// or seeking would stitch them together with a discontinuity.
	seek, err := h.mgr.Segment(ctx, h.media, v, 3)
	if err != nil {
		t.Fatalf("seek with a hardware encoder: %v", err)
	}
	start, err := strconv.ParseFloat(probeSegment(t, seek).Format.StartTime, 64)
	if err != nil {
		t.Fatalf("no start_time: %v", err)
	}
	if diff := start - 12; diff > 1.0 || diff < -1.0 {
		t.Errorf("segment 3 starts at %.2fs, want ~12s", start)
	}
}

func mustEncoder(t *testing.T, name string) transcode.Encoder {
	t.Helper()
	e, err := transcode.LookupEncoder(name)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
