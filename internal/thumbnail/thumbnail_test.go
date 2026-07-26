package thumbnail_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/apenombre/freebox-proxystream/internal/config"
	"github.com/apenombre/freebox-proxystream/internal/db"
	"github.com/apenombre/freebox-proxystream/internal/ftpclient"
	"github.com/apenombre/freebox-proxystream/internal/mediasource"
	"github.com/apenombre/freebox-proxystream/internal/probe"
	"github.com/apenombre/freebox-proxystream/internal/rangeproxy"
	"github.com/apenombre/freebox-proxystream/internal/testftp"
	"github.com/apenombre/freebox-proxystream/internal/thumbnail"
)

// setup serves a clip over FTP through the range proxy, exactly as production
// does, so the generator is exercised against a remote file rather than a
// local one.
func setup(t *testing.T, seconds int) (*thumbnail.Generator, *db.Media, string) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}

	addr, root := testftp.Start(t)
	tmp := t.TempDir()

	// testsrc2 burns a running timer into the picture, and its background
	// colour changes over time, which is what makes "did we seek?" observable.
	src := filepath.Join(tmp, "src.mkv")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i",
		"testsrc2=size=640x360:rate=10:duration="+itoa(seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "20",
		src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate source: %v: %s", err, out)
	}
	testftp.Copy(t, root, "Films/clip.mkv", src)

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
	id, _, err := store.UpsertMediaFile(ctx, lib.ID, "/Films/clip.mkv", info.Size(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

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
		InternalAddr: ln.Addr().String(),
		ThumbDir:     filepath.Join(tmp, "thumbs"),
		FFmpegBin:    "ffmpeg",
		FFprobeBin:   "ffprobe",
	}

	res, err := probe.Run(ctx, cfg.FFprobeBin, cfg.RawURL(id))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := store.SaveProbe(ctx, id, *res); err != nil {
		t.Fatal(err)
	}
	m, err := store.GetMedia(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return thumbnail.New(cfg), m, cfg.ThumbDir
}

func itoa(n int) string { return string(rune('0'+n/10)) + string(rune('0'+n%10)) }

func TestGeneratesAFrameFromTheMiddle(t *testing.T) {
	if testing.Short() {
		t.Skip("requires ffmpeg")
	}
	g, m, dir := setup(t, 40)

	path, err := g.Ensure(context.Background(), m)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Written straight to its final location, not a temp area.
	if want := filepath.Join(dir, "1.jpg"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("no file written: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("file is empty")
	}
	if !g.Exists(m.ID) {
		t.Error("Exists disagrees with the file on disk")
	}
	// No leftovers from the atomic rename.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}

	// It must be a real, decodable JPEG of the right shape.
	out, err := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json",
		"-show_streams", path).Output()
	if err != nil {
		t.Fatalf("the written file is not decodable: %v", err)
	}
	var probed struct {
		Streams []struct {
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probed); err != nil {
		t.Fatal(err)
	}
	if len(probed.Streams) != 1 {
		t.Fatalf("got %d streams, want 1", len(probed.Streams))
	}
	// The source is 360 tall, so min(360, ih) must leave it untouched.
	if h := probed.Streams[0].Height; h != 360 {
		t.Errorf("height = %d, want 360 (never upscaled)", h)
	}
	if c := probed.Streams[0].CodecName; c != "mjpeg" {
		t.Errorf("codec = %s, want mjpeg", c)
	}
}

func TestSecondCallReusesTheStoredFile(t *testing.T) {
	if testing.Short() {
		t.Skip("requires ffmpeg")
	}
	g, m, _ := setup(t, 20)
	ctx := context.Background()

	path, err := g.Ensure(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if _, err := g.Ensure(ctx, m); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("second call took %s, so it regenerated instead of reusing", d)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Error("the stored frame was rewritten instead of reused")
	}
}

// A grid asks for many posters at once; the same media must not spawn several
// ffmpeg processes against the same remote file.
func TestConcurrentCallsGenerateOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("requires ffmpeg")
	}
	g, m, _ := setup(t, 20)

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = g.Ensure(context.Background(), m)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	if !g.Exists(m.ID) {
		t.Error("no thumbnail was produced")
	}
}

func TestUnprobedMediaIsRefused(t *testing.T) {
	g := thumbnail.New(&config.Config{ThumbDir: t.TempDir(), FFmpegBin: "ffmpeg"})
	if _, err := g.Ensure(context.Background(), &db.Media{ID: 1}); err == nil {
		t.Error("a media with no duration should not be attempted")
	}
}
