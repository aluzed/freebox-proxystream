package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/apenombre/freebox-proxystream/internal/config"
	"github.com/apenombre/freebox-proxystream/internal/db"
	"github.com/apenombre/freebox-proxystream/internal/transcode"
)

// mediaView is the detail payload: everything the player needs in one call.
type mediaView struct {
	*db.Media
	DisplayTitle  string        `json:"display_title"`
	Progress      *db.Progress  `json:"progress"`
	Qualities     []qualityView `json:"qualities"`
	DefaultAudio  int           `json:"default_audio"`
	DefaultHeight int           `json:"default_height"`
	SegmentCount  int           `json:"segment_count"`
	EncoderLog    []string      `json:"encoder_log,omitempty"`
}

type qualityView struct {
	// Height is the value to send back; Label is what the user will actually
	// get. They differ whenever the source is smaller than the rendition.
	Height    int    `json:"height"`
	Label     string `json:"label"`
	Effective int    `json:"effective_height"`
	Native    bool   `json:"native"`
}

func (s *Server) mediaView(r *http.Request, m *db.Media) *mediaView {
	progress, _ := s.store.GetProgress(r.Context(), m.ID)

	qualities := buildQualities(s.cfg, m)

	var segCount int
	if m.DurationSec != nil {
		segCount = transcode.SegmentCount(*m.DurationSec, s.cfg.SegmentDuration)
	}

	return &mediaView{
		Media:         m,
		DisplayTitle:  m.DisplayTitle(),
		Progress:      progress,
		Qualities:     qualities,
		DefaultAudio:  transcode.DefaultAudioIndex(m, s.cfg.PreferredLang),
		DefaultHeight: s.cfg.DefaultHeight,
		SegmentCount:  segCount,
		EncoderLog:    s.mgr.EncoderLog(m.ID),
	}
}

// buildQualities lists the renditions that are actually distinct for this
// source. A 480p file collapses both settings onto 480p, so offering "720p"
// and "1080p" would be two names for one identical encode; it gets a single
// entry instead. Labels state the real output height rather than the
// rendition's nominal name.
func buildQualities(cfg *config.Config, m *db.Media) []qualityView {
	out := make([]qualityView, 0, len(cfg.Qualities))
	seen := map[int]bool{}

	for _, q := range cfg.Qualities {
		eff := transcode.EffectiveHeight(q.Height, m.Height)
		if seen[eff] {
			continue
		}
		seen[eff] = true

		native := m.Height != nil && eff == *m.Height
		label := fmt.Sprintf("%dp", eff)
		if native {
			label += " (qualité source)"
		}
		out = append(out, qualityView{
			Height: q.Height, Label: label, Effective: eff, Native: native,
		})
	}
	return out
}

func (s *Server) apiListMedia(w http.ResponseWriter, r *http.Request) {
	f := db.MediaFilter{
		LibraryID: int64(queryInt(r, "library_id", 0)),
		Query:     r.URL.Query().Get("q"),
		Status:    r.URL.Query().Get("status"),
		Limit:     queryInt(r, "limit", 100),
		Offset:    queryInt(r, "offset", 0),
	}
	items, total, err := s.store.ListMedia(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "limit": f.Limit, "offset": f.Offset,
	})
}

func (s *Server) apiGetMedia(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMedia(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.mediaView(r, m))
}

// resolveVariant reads the quality and audio choices off the query string.
func (s *Server) resolveVariant(r *http.Request, m *db.Media) transcode.Variant {
	height := queryInt(r, "quality", s.cfg.DefaultHeight)
	audio := queryInt(r, "audio", transcode.DefaultAudioIndex(m, s.cfg.PreferredLang))
	return transcode.ResolveVariant(s.cfg, m, height, audio)
}

func (s *Server) apiPlaylist(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMedia(w, r)
	if !ok {
		return
	}
	if m.DurationSec == nil || *m.DurationSec <= 0 {
		writeError(w, http.StatusConflict, "media has not been probed yet")
		return
	}
	v := s.resolveVariant(r, m)

	// The playlist is complete before anything is encoded: it is derived from
	// the duration alone, which is what makes seeking work.
	query := fmt.Sprintf("?quality=%d&audio=%d", v.Height, v.AudioIndex)
	pl := transcode.Playlist(*m.DurationSec, s.cfg.SegmentDuration, func(seq int) string {
		return fmt.Sprintf("seg_%05d.ts%s", seq, query)
	})

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(pl))
}

var (
	segmentFileRe  = regexp.MustCompile(`^seg_(\d+)\.ts$`)
	subtitleFileRe = regexp.MustCompile(`^(\d+)\.vtt$`)
)

// apiMediaFile routes the two playback URLs that live under a media id.
func (s *Server) apiMediaFile(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	if file == "master.m3u8" {
		s.apiPlaylist(w, r)
		return
	}
	if mm := segmentFileRe.FindStringSubmatch(file); mm != nil {
		seq, err := strconv.Atoi(mm[1])
		if err != nil || seq < 0 {
			writeError(w, http.StatusBadRequest, "invalid segment number")
			return
		}
		s.apiSegment(w, r, seq)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) apiSegment(w http.ResponseWriter, r *http.Request, seq int) {
	m, ok := s.loadMedia(w, r)
	if !ok {
		return
	}
	v := s.resolveVariant(r, m)

	path, err := s.mgr.Segment(r.Context(), m, v, seq)
	if err != nil {
		if r.Context().Err() != nil {
			// The player moved on; not worth an error response.
			return
		}
		s.log.Error("segment failed",
			"media_id", m.ID, "seq", seq, "variant", v.Key, "err", err)
		status := http.StatusInternalServerError
		if errors.Is(err, transcode.ErrSegmentTimeout) {
			status = http.StatusGatewayTimeout
		}
		writeError(w, status, "could not produce segment")
		return
	}

	w.Header().Set("Content-Type", "video/mp2t")
	// Segments are immutable for a given variant key, so the browser may keep
	// them for as long as it likes.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, path)
}

func (s *Server) apiSubtitles(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMedia(w, r)
	if !ok {
		return
	}
	mm := subtitleFileRe.FindStringSubmatch(r.PathValue("file"))
	if mm == nil {
		writeError(w, http.StatusBadRequest, "invalid subtitle track")
		return
	}
	idx, err := strconv.Atoi(mm[1])
	if err != nil || idx < 0 {
		writeError(w, http.StatusBadRequest, "invalid subtitle index")
		return
	}

	path, err := s.subs.Extract(r.Context(), m, idx)
	if err != nil {
		s.log.Warn("subtitle extraction failed", "media_id", m.ID, "idx", idx, "err", err)
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, path)
}

// apiPoster serves the frame grabbed from the middle of the video. There is no
// online catalogue any more: the picture comes from the file itself.
func (s *Server) apiPoster(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMedia(w, r)
	if !ok {
		return
	}
	if s.thumbs.Exists(m.ID) {
		s.serveThumb(w, r, m)
		return
	}
	if _, err := s.thumbs.Ensure(r.Context(), m); err != nil {
		if r.Context().Err() == nil {
			s.log.Debug("no frame available", "media_id", m.ID, "err", err)
		}
		http.NotFound(w, r)
		return
	}
	s.serveThumb(w, r, m)
}

func (s *Server) serveThumb(w http.ResponseWriter, r *http.Request, m *db.Media) {
	// A frame can be regenerated if the file is replaced, so a stale one
	// should not stick around for a week.
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, s.thumbs.Path(m.ID))
}

func (s *Server) apiRetryProbe(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMedia(w, r)
	if !ok {
		return
	}
	if err := s.store.ResetProbeAttempts(r.Context(), m.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "could not queue a retry")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "probe queued"})
}

func (s *Server) apiSaveDetails(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMedia(w, r)
	if !ok {
		return
	}
	var body struct {
		Title    string `json:"title"`
		Overview string `json:"overview"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if strings.TrimSpace(body.Title) == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	if err := s.store.SaveUserDetails(r.Context(), m.ID, body.Title, body.Overview); err != nil {
		writeError(w, http.StatusInternalServerError, "could not save")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiSaveProgress(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMedia(w, r)
	if !ok {
		return
	}
	var body struct {
		PositionSec float64 `json:"position_sec"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if body.PositionSec < 0 {
		body.PositionSec = 0
	}
	if err := s.store.SaveProgress(r.Context(), m.ID, body.PositionSec); err != nil {
		writeError(w, http.StatusInternalServerError, "could not save progress")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
