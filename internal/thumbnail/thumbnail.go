// Package thumbnail grabs a single frame from the middle of a file and stores
// it permanently on disk. It is not part of the segment cache: a poster is
// cheap to keep and expensive to regenerate (an FTP seek plus a decode), so it
// is never evicted.
package thumbnail

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/aluzed/freebox-proxystream/internal/config"
	"github.com/aluzed/freebox-proxystream/internal/db"
)

// A frame from a remote file costs one seek plus one decode, so allow well
// more than a local grab would need but do not hang a page load forever.
const generateTimeout = 90 * time.Second

// Height of the stored image. Wide enough for a grid tile on a retina screen,
// small enough that a few thousand of them stay negligible on disk.
const thumbHeight = 360

type Generator struct {
	cfg *config.Config

	mu      sync.Mutex
	pending map[int64]chan struct{}
}

func New(cfg *config.Config) *Generator {
	return &Generator{cfg: cfg, pending: map[int64]chan struct{}{}}
}

// Path is derived from the media id alone, so no database column is needed:
// the file's existence is the record.
func (g *Generator) Path(mediaID int64) string {
	return filepath.Join(g.cfg.ThumbDir, strconv.FormatInt(mediaID, 10)+".jpg")
}

func (g *Generator) Exists(mediaID int64) bool {
	info, err := os.Stat(g.Path(mediaID))
	return err == nil && info.Size() > 0
}

// Ensure returns the path to the stored frame, generating it if absent.
// Concurrent callers for the same media wait for the first one rather than
// each spawning their own ffmpeg against the same remote file.
func (g *Generator) Ensure(ctx context.Context, m *db.Media) (string, error) {
	path := g.Path(m.ID)
	if g.Exists(m.ID) {
		return path, nil
	}
	if m.DurationSec == nil || *m.DurationSec <= 0 {
		return "", fmt.Errorf("media %d has no known duration yet", m.ID)
	}

	g.mu.Lock()
	wait, inFlight := g.pending[m.ID]
	if inFlight {
		g.mu.Unlock()
		select {
		case <-wait:
			if g.Exists(m.ID) {
				return path, nil
			}
			return "", fmt.Errorf("thumbnail generation failed for media %d", m.ID)
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	done := make(chan struct{})
	g.pending[m.ID] = done
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		delete(g.pending, m.ID)
		g.mu.Unlock()
		close(done)
	}()

	if err := g.generate(ctx, m); err != nil {
		return "", err
	}
	return path, nil
}

func (g *Generator) generate(ctx context.Context, m *db.Media) error {
	if err := os.MkdirAll(g.cfg.ThumbDir, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, generateTimeout)
	defer cancel()

	// The middle of the file: past the titles, before the credits, and far
	// more representative than the first frame, which is usually black.
	seek := *m.DurationSec / 2

	dest := g.Path(m.ID)
	tmp := dest + ".tmp"
	defer os.Remove(tmp)

	cmd := exec.CommandContext(ctx, g.cfg.FFmpegBin,
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		// Input seek, so ffmpeg jumps there over HTTP Range instead of
		// decoding from the start of a possibly enormous file.
		"-ss", strconv.FormatFloat(seek, 'f', 3, 64),
		"-i", g.cfg.RawURL(m.ID),
		"-map", "0:v:0",
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=-2:min(%d\\,ih)", thumbHeight),
		"-q:v", "3",
		"-f", "image2",
		tmp,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("thumbnail for media %d: %w: %s", m.ID, err, out)
	}

	info, err := os.Stat(tmp)
	if err != nil || info.Size() == 0 {
		return fmt.Errorf("thumbnail for media %d produced no image", m.ID)
	}
	// Rename so a reader never sees a half-written file.
	return os.Rename(tmp, dest)
}

// Remove drops a stored frame, used when its media disappears.
func (g *Generator) Remove(mediaID int64) error {
	err := os.Remove(g.Path(mediaID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
