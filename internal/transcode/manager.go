package transcode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/aluzed/freebox-proxystream/internal/config"
	"github.com/aluzed/freebox-proxystream/internal/db"
)

var ErrSegmentTimeout = errors.New("timed out waiting for segment")

const (
	segmentPollInterval = 100 * time.Millisecond
	segmentWaitTimeout  = 30 * time.Second
)

type Manager struct {
	cfg   *config.Config
	store *db.Store
	log   *slog.Logger

	mu       sync.Mutex
	sessions map[sessionKey]*Session
}

func NewManager(cfg *config.Config, store *db.Store, log *slog.Logger) *Manager {
	return &Manager{
		cfg:      cfg,
		store:    store,
		log:      log,
		sessions: map[sessionKey]*Session{},
	}
}

func (m *Manager) VariantDir(mediaID int64, variantKey string) string {
	return filepath.Join(m.cfg.CacheDir, strconv.FormatInt(mediaID, 10), variantKey)
}

func (m *Manager) SegmentFile(mediaID int64, variantKey string, seq int) string {
	return SegmentPath(m.VariantDir(mediaID, variantKey), seq)
}

// Segment resolves a segment request, in the order that makes seeking free:
// on disk, else covered by a running encoder, else start one here.
func (m *Manager) Segment(ctx context.Context, media *db.Media, v Variant, seq int) (string, error) {
	if seq < 0 {
		return "", fmt.Errorf("negative segment %d", seq)
	}
	path := m.SegmentFile(media.ID, v.Key, seq)

	if fileExists(path) {
		m.touchSegment(media.ID, v.Key, seq)
		if s := m.session(media.ID, v.Key); s != nil {
			s.touch()
		}
		return path, nil
	}

	sess, err := m.ensureSession(ctx, media, v, seq)
	if err != nil {
		return "", err
	}
	if err := m.waitForSegment(ctx, sess, path, seq); err != nil {
		return "", err
	}
	m.touchSegment(media.ID, v.Key, seq)
	return path, nil
}

func (m *Manager) session(mediaID int64, variantKey string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[sessionKey{mediaID, variantKey}]
}

func (m *Manager) ensureSession(ctx context.Context, media *db.Media, v Variant, seq int) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := sessionKey{media.ID, v.Key}
	if s := m.sessions[key]; s != nil {
		if !s.exited() && s.covers(seq, m.cfg.EncoderReadaheadSegments) {
			s.touch()
			return s, nil
		}
		reason := "seek"
		if s.exited() {
			reason = "exited"
		}
		s.stop(reason)
		delete(m.sessions, key)
	}

	m.makeRoomLocked()

	// The session outlives this request: the player will ask for the following
	// segments, and killing the encoder when one request ends would defeat the
	// whole point. It is bounded by the idle reaper instead.
	sess, err := startSession(context.WithoutCancel(ctx), m.cfg, m.log, media.ID, v, seq,
		m.VariantDir(media.ID, v.Key),
		func(s int, size int64) {
			err := m.store.RegisterSegment(context.Background(), db.CacheEntry{
				MediaID: media.ID, VariantKey: v.Key, Seq: s, SizeBytes: size,
			})
			if err != nil {
				m.log.Warn("register segment failed", "media_id", media.ID, "seq", s, "err", err)
			}
		})
	if err != nil {
		return nil, err
	}

	m.sessions[key] = sess
	go func() {
		<-sess.Done()
		m.mu.Lock()
		if m.sessions[key] == sess {
			delete(m.sessions, key)
		}
		m.mu.Unlock()
	}()
	return sess, nil
}

// makeRoomLocked enforces MAX_CONCURRENT_ENCODERS by evicting the session that
// has gone unrequested the longest.
func (m *Manager) makeRoomLocked() {
	for len(m.sessions) >= m.cfg.MaxConcurrentEncoders {
		var victimKey sessionKey
		var victim *Session
		for k, s := range m.sessions {
			if victim == nil || s.LastRequest().Before(victim.LastRequest()) {
				victim, victimKey = s, k
			}
		}
		if victim == nil {
			return
		}
		victim.stop("encoder slot reclaimed")
		delete(m.sessions, victimKey)
	}
}

func (m *Manager) waitForSegment(ctx context.Context, s *Session, path string, seq int) error {
	deadline := time.NewTimer(segmentWaitTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(segmentPollInterval)
	defer ticker.Stop()

	for {
		if fileExists(path) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.Done():
			// The process may have written the final segment on its way out.
			for range 10 {
				if fileExists(path) {
					return nil
				}
				time.Sleep(segmentPollInterval)
			}
			return fmt.Errorf("encoder exited before producing segment %d: %s",
				seq, s.stderr.tail(5))
		case <-deadline.C:
			return fmt.Errorf("%w %d after %s (encoder head %d): %s",
				ErrSegmentTimeout, seq, segmentWaitTimeout, s.Head(), s.stderr.tail(3))
		case <-ticker.C:
		}
	}
}

func (m *Manager) touchSegment(mediaID int64, variantKey string, seq int) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.store.TouchSegment(ctx, mediaID, variantKey, seq); err != nil {
			m.log.Debug("touch segment failed", "media_id", mediaID, "seq", seq, "err", err)
		}
	}()
}

// StopMedia kills every session for a media, whatever the variant. Used when a
// player switches quality or audio track, and before clearing the cache.
func (m *Manager) StopMedia(mediaID int64, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, s := range m.sessions {
		if k.mediaID == mediaID {
			s.stop(reason)
			delete(m.sessions, k)
		}
	}
}

func (m *Manager) StopAll(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, s := range m.sessions {
		s.stop(reason)
		delete(m.sessions, k)
	}
}

// Shutdown stops every encoder and waits for the processes to actually exit.
// Returning before they do would leave orphaned ffmpeg processes writing into
// directories the caller is about to remove.
func (m *Manager) Shutdown(reason string, timeout time.Duration) {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for k, s := range m.sessions {
		s.stop(reason)
		sessions = append(sessions, s)
		delete(m.sessions, k)
	}
	m.mu.Unlock()

	deadline := time.After(timeout)
	for _, s := range sessions {
		select {
		case <-s.Done():
		case <-deadline:
			m.log.Warn("encoder did not exit before the shutdown deadline",
				"media_id", s.MediaID, "variant", s.Variant.Key)
			return
		}
	}
}

// ActiveMediaIDs is what the cache janitor uses to avoid deleting segments out
// from under a running encoder.
func (m *Manager) ActiveMediaIDs() map[int64]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[int64]bool{}
	for k := range m.sessions {
		out[k.mediaID] = true
	}
	return out
}

type SessionStatus struct {
	MediaID     int64     `json:"media_id"`
	VariantKey  string    `json:"variant_key"`
	Height      int       `json:"height"`
	AudioIndex  int       `json:"audio_index"`
	StartSeq    int       `json:"start_seq"`
	Head        int       `json:"head"`
	StartedAt   time.Time `json:"started_at"`
	LastRequest time.Time `json:"last_request"`
}

func (m *Manager) Status() []SessionStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionStatus, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, SessionStatus{
			MediaID:     s.MediaID,
			VariantKey:  s.Variant.Key,
			Height:      s.Variant.Height,
			AudioIndex:  s.Variant.AudioIndex,
			StartSeq:    s.StartSeq,
			Head:        s.Head(),
			StartedAt:   s.StartedAt,
			LastRequest: s.LastRequest(),
		})
	}
	return out
}

// EncoderLog returns the captured ffmpeg stderr for a media, for the detail view.
func (m *Manager) EncoderLog(mediaID int64) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, s := range m.sessions {
		if k.mediaID == mediaID {
			return s.StderrTail()
		}
	}
	return nil
}

// Run reaps idle encoders until the context is cancelled.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.Shutdown("shutdown", 10*time.Second)
			return
		case <-ticker.C:
			m.reapIdle()
		}
	}
}

func (m *Manager) reapIdle() {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-m.cfg.EncoderIdleTimeout)
	for k, s := range m.sessions {
		if s.LastRequest().Before(cutoff) {
			s.stop("idle timeout")
			delete(m.sessions, k)
		}
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Size() > 0
}
