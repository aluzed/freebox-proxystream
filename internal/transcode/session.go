package transcode

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/apenombre/freebox-proxystream/internal/config"
)

const stderrRingSize = 50

// ringBuffer keeps the last N ffmpeg stderr lines. ffmpeg's stderr is the only
// useful diagnostic when an encode fails, so it is always captured.
type ringBuffer struct {
	mu    sync.Mutex
	lines []string
}

func (r *ringBuffer) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > stderrRingSize {
		r.lines = r.lines[len(r.lines)-stderrRingSize:]
	}
}

func (r *ringBuffer) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}

func (r *ringBuffer) tail(n int) string {
	lines := r.snapshot()
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

type sessionKey struct {
	mediaID int64
	variant string
}

// Session is one ffmpeg process producing a contiguous run of segments
// starting at StartSeq. Nothing about it is persisted: sessions do not survive
// a restart, only their segments do.
type Session struct {
	MediaID   int64
	Variant   Variant
	StartSeq  int
	StartedAt time.Time

	dir    string
	tmpDir string
	cmd    *exec.Cmd
	cancel context.CancelFunc
	stderr *ringBuffer
	log    *slog.Logger

	mu          sync.Mutex
	head        int // highest segment seen on disk; StartSeq-1 before the first
	lastRequest time.Time
	stopReason  string

	done    chan struct{}
	exitErr error
}

// onSegment is called for each newly observed segment file.
type onSegment func(seq int, size int64)

func startSession(
	parent context.Context,
	cfg *config.Config,
	log *slog.Logger,
	mediaID int64,
	v Variant,
	startSeq int,
	dir string,
	notify onSegment,
) (*Session, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create segment dir: %w", err)
	}
	tmpDir, err := os.MkdirTemp("", "proxystream-hls-")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}

	ctx, cancel := context.WithCancel(parent)
	args := buildArgs(cfg, mediaID, v, startSeq, dir, tmpDir)

	cmd := exec.CommandContext(ctx, cfg.FFmpegBin, args...)
	// SIGTERM lets ffmpeg flush and close the segment it is writing; SIGKILL
	// follows if it has not exited.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = nil

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		os.RemoveAll(tmpDir)
		return nil, err
	}

	s := &Session{
		MediaID:     mediaID,
		Variant:     v,
		StartSeq:    startSeq,
		StartedAt:   time.Now(),
		dir:         dir,
		tmpDir:      tmpDir,
		cmd:         cmd,
		cancel:      cancel,
		stderr:      &ringBuffer{},
		log:         log,
		head:        startSeq - 1,
		lastRequest: time.Now(),
		done:        make(chan struct{}),
	}

	if err := cmd.Start(); err != nil {
		cancel()
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	log.Info("encoder started",
		"media_id", mediaID, "variant", v.Key, "start_seq", startSeq, "height", v.Height)

	go s.drainStderr(stderrPipe)
	go s.watchSegments(ctx, notify)
	go s.wait()

	return s, nil
}

func buildArgs(cfg *config.Config, mediaID int64, v Variant, startSeq int, dir, tmpDir string) []string {
	segDur := cfg.SegmentDuration
	startTime := startSeq * segDur

	args := []string{
		"-hide_banner", "-loglevel", "warning", "-nostdin",
	}
	// Some encoders need a device opened before the input is read.
	if v.Encoder.inputArgs != nil {
		args = append(args, v.Encoder.inputArgs(cfg)...)
	}

	args = append(args,
		// Input seek: ffmpeg turns this into Range requests against the proxy
		// and jumps straight there. After -i it would decode from the start.
		"-ss", fmt.Sprintf("%d", startTime),
		"-i", cfg.RawURL(mediaID),

		"-map", "0:v:0",
		"-map", fmt.Sprintf("0:a:%d", v.AudioIndex),

		"-vf", v.Encoder.filter(cfg, v.Height),
	)
	args = append(args, v.Encoder.codecArgs(cfg, v)...)

	return append(args,
		// A keyframe exactly on every segment boundary, so independently
		// encoded runs stitch together without stutter at the joins.
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", segDur),
		"-fps_mode", "cfr",

		// Stereo downmix. Without it a 5.1 source loses its centre channel to
		// the client downmix and dialogue becomes inaudible on a phone.
		"-c:a", "aac",
		"-ac", "2",
		"-b:a", cfg.AudioBitrate,

		"-f", "hls",
		"-hls_time", fmt.Sprintf("%d", segDur),
		"-hls_playlist_type", "vod",
		"-hls_segment_type", "mpegts",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments+temp_file",
		"-start_number", fmt.Sprintf("%d", startSeq),
		"-hls_segment_filename", filepath.Join(dir, "seg_%05d.ts"),

		// Timestamps stay on the absolute timeline of the source. Segments
		// from a session that started at seq 0 and one that started at seq 300
		// then carry coherent PTS, so the player joins them without a
		// discontinuity when a seek stitches two runs together. The TS muxer
		// otherwise adds its default 1.4s preload on top of the offset.
		"-output_ts_offset", fmt.Sprintf("%d", startTime),
		"-muxdelay", "0",
		"-muxpreload", "0",

		filepath.Join(tmpDir, "ignored.m3u8"),
	)
}

func (s *Session) drainStderr(r io.ReadCloser) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			s.stderr.add(line)
		}
	}
}

// watchSegments advances head as files appear. `+temp_file` means ffmpeg
// renames each segment into place, so a visible final name is a complete file.
func (s *Session) watchSegments(ctx context.Context, notify onSegment) {
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	for {
		for {
			s.mu.Lock()
			next := s.head + 1
			s.mu.Unlock()

			info, err := os.Stat(SegmentPath(s.dir, next))
			if err != nil {
				break
			}
			s.mu.Lock()
			s.head = next
			s.mu.Unlock()
			if notify != nil {
				notify(next, info.Size())
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-ticker.C:
		}
	}
}

func (s *Session) wait() {
	err := s.cmd.Wait()
	s.mu.Lock()
	s.exitErr = err
	s.mu.Unlock()
	close(s.done)
	os.RemoveAll(s.tmpDir)

	reason := s.Reason()
	if reason == "" {
		reason = "completed"
	}
	if err != nil && reason == "completed" {
		s.log.Warn("encoder exited with error",
			"media_id", s.MediaID, "variant", s.Variant.Key, "start_seq", s.StartSeq,
			"err", err, "stderr", s.stderr.tail(5))
	} else {
		s.log.Info("encoder stopped",
			"media_id", s.MediaID, "variant", s.Variant.Key, "start_seq", s.StartSeq,
			"head", s.Head(), "reason", reason,
			"elapsed", time.Since(s.StartedAt).Round(time.Second))
	}
}

func (s *Session) Head() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.head
}

func (s *Session) Reason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopReason
}

func (s *Session) LastRequest() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastRequest
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastRequest = time.Now()
	s.mu.Unlock()
}

// covers reports whether this session is going to produce seq soon enough that
// waiting beats restarting an encoder at seq.
func (s *Session) covers(seq, readahead int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq < s.StartSeq {
		return false
	}
	if seq <= s.head {
		// It was written; a caller only gets here in the brief window between
		// the rename and its own stat, so waiting is correct.
		return true
	}
	return seq-s.head <= readahead
}

func (s *Session) stop(reason string) {
	s.mu.Lock()
	if s.stopReason == "" {
		s.stopReason = reason
	}
	s.mu.Unlock()
	s.cancel()
}

func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) StderrTail() []string { return s.stderr.snapshot() }

func (s *Session) exited() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// SegmentPath is the on-disk name for a segment inside a variant directory.
func SegmentPath(dir string, seq int) string {
	return filepath.Join(dir, fmt.Sprintf("seg_%05d.ts", seq))
}
