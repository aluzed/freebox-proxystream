// Package subs extracts embedded text subtitle tracks to WebVTT on demand.
// Bitmap tracks (PGS, VobSub) are out of scope: no browser can decode them and
// burning them in would force a separate encode per track.
package subs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/aluzed/freebox-proxystream/internal/config"
	"github.com/aluzed/freebox-proxystream/internal/db"
	"github.com/aluzed/freebox-proxystream/internal/probe"
)

const extractTimeout = 5 * time.Minute

type Extractor struct {
	cfg *config.Config

	mu       sync.Mutex
	inFlight map[string]*sync.Once
}

func New(cfg *config.Config) *Extractor {
	return &Extractor{cfg: cfg, inFlight: map[string]*sync.Once{}}
}

func (e *Extractor) path(mediaID int64, subIndex int) string {
	return filepath.Join(e.cfg.SubtitleDir, fmt.Sprintf("%d_%d.vtt", mediaID, subIndex))
}

// Extract returns the path to a cached WebVTT file, producing it if needed.
func (e *Extractor) Extract(ctx context.Context, m *db.Media, subIndex int) (string, error) {
	var track *db.SubStream
	for i := range m.SubStreams {
		if m.SubStreams[i].SubIndex == subIndex {
			track = &m.SubStreams[i]
			break
		}
	}
	if track == nil {
		return "", fmt.Errorf("subtitle track %d not found", subIndex)
	}
	if !probe.IsTextSubtitle(track.Codec) {
		return "", fmt.Errorf("subtitle track %d is %s, which needs burn-in and is not supported",
			subIndex, track.Codec)
	}

	dest := e.path(m.ID, subIndex)
	if info, err := os.Stat(dest); err == nil && info.Size() > 0 {
		return dest, nil
	}
	if err := os.MkdirAll(e.cfg.SubtitleDir, 0o755); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, extractTimeout)
	defer cancel()

	// Extraction reads the whole file over FTP, so two players asking for the
	// same track must not run it twice.
	e.mu.Lock()
	once, ok := e.inFlight[dest]
	if !ok {
		once = &sync.Once{}
		e.inFlight[dest] = once
	}
	e.mu.Unlock()

	var runErr error
	once.Do(func() {
		runErr = e.run(ctx, m.ID, subIndex, dest)
		e.mu.Lock()
		delete(e.inFlight, dest)
		e.mu.Unlock()
	})
	if runErr != nil {
		return "", runErr
	}
	if info, err := os.Stat(dest); err != nil || info.Size() == 0 {
		return "", fmt.Errorf("subtitle extraction produced nothing for track %d", subIndex)
	}
	return dest, nil
}

func (e *Extractor) run(ctx context.Context, mediaID int64, subIndex int, dest string) error {
	tmp := dest + ".tmp"
	defer os.Remove(tmp)

	cmd := exec.CommandContext(ctx, e.cfg.FFmpegBin,
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-i", e.cfg.RawURL(mediaID),
		"-map", fmt.Sprintf("0:s:%d", subIndex),
		"-f", "webvtt",
		tmp,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("extract subtitle %d: %w: %s", subIndex, err, out)
	}
	return os.Rename(tmp, dest)
}
