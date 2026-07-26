package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/apenombre/freebox-proxystream/internal/db"
	"github.com/apenombre/freebox-proxystream/internal/scanner"
	"github.com/apenombre/freebox-proxystream/internal/web"
)

// render writes a full page, adding the context every layout needs.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	if _, ok := data["User"]; !ok {
		data["User"] = userFrom(r.Context())
	}
	if _, ok := data["Nav"]; !ok {
		data["Nav"] = ""
	}

	// Render to a buffer so a template error does not emit a half-written page.
	var buf bytes.Buffer
	if err := s.tpl.Render(&buf, page, data); err != nil {
		s.log.Error("template render failed", "page", page, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func (s *Server) renderFragment(w http.ResponseWriter, r *http.Request, name string, data any) {
	var buf bytes.Buffer
	if err := s.tpl.RenderFragment(&buf, name, data); err != nil {
		s.log.Error("fragment render failed", "fragment", name, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

// ---------- pages ----------

func (s *Server) pageLibrary(w http.ResponseWriter, r *http.Request) {
	// The page is a shell; the grid loads itself. Carrying the current
	// library and path through means a deep link, a reload or the back button
	// all land on the same folder rather than at the top of the tree.
	libraryID := int64(queryInt(r, "library_id", 0))
	rel := r.URL.Query().Get("path")

	initial := ""
	if libraryID > 0 {
		initial = "?library_id=" + strconv.FormatInt(libraryID, 10)
		if rel != "" {
			initial += "&path=" + url.QueryEscape(rel)
		}
	}

	s.render(w, r, "library.html", map[string]any{
		"Title":        "Bibliothèque",
		"Nav":          "library",
		"LibraryID":    libraryID,
		"Path":         rel,
		"InitialQuery": template.URL(initial),
	})
}

func (s *Server) pageDetail(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMedia(w, r)
	if !ok {
		return
	}
	view := s.mediaView(r, m)

	// Offer a resume only when it is genuinely mid-film: not in the opening
	// seconds, not in the closing minute.
	var resumeAt float64
	if view.Progress != nil && m.DurationSec != nil {
		p := view.Progress.PositionSec
		if p > 30 && p < *m.DurationSec-60 {
			resumeAt = p
		}
	}

	s.render(w, r, "detail.html", map[string]any{
		"Title":         m.DisplayTitle(),
		"Nav":           "library",
		"Media":         m,
		"Qualities":     view.Qualities,
		"DefaultHeight": view.DefaultHeight,
		"DefaultAudio":  view.DefaultAudio,
		"SegmentCount":  view.SegmentCount,
		"EncoderLog":    view.EncoderLog,
		"ResumeAt":      resumeAt,
		"Probing":       s.probingData(m),
		"Failure":       s.failureData(r, m),
	})
}

// playerConfig is handed to the browser as one JSON blob rather than as a
// scattering of data- attributes.
type playerConfig struct {
	MediaID     int64   `json:"media_id"`
	Title       string  `json:"title"`
	PlaylistURL string  `json:"playlist_url"`
	ProgressURL string  `json:"progress_url"`
	SubtitleURL string  `json:"subtitle_url,omitempty"`
	SubtitleLbl string  `json:"subtitle_label,omitempty"`
	StartAt     float64 `json:"start_at"`
	Duration    float64 `json:"duration"`
}

func (s *Server) pagePlayer(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMedia(w, r)
	if !ok {
		return
	}
	if m.Status != db.StatusReady || m.DurationSec == nil {
		http.Redirect(w, r, "/media/"+strconv.FormatInt(m.ID, 10), http.StatusSeeOther)
		return
	}

	v := s.resolveVariant(r, m)
	startAt := 0.0
	if t := r.URL.Query().Get("t"); t != "" {
		if f, err := strconv.ParseFloat(t, 64); err == nil && f > 0 {
			startAt = f
		}
	}

	cfg := playerConfig{
		MediaID: m.ID,
		Title:   m.DisplayTitle(),
		PlaylistURL: "/api/media/" + strconv.FormatInt(m.ID, 10) +
			"/master.m3u8?quality=" + strconv.Itoa(v.Height) +
			"&audio=" + strconv.Itoa(v.AudioIndex),
		ProgressURL: "/api/media/" + strconv.FormatInt(m.ID, 10) + "/progress",
		StartAt:     startAt,
		Duration:    *m.DurationSec,
	}

	if sub := queryInt(r, "sub", -1); sub >= 0 {
		for _, st := range m.SubStreams {
			if st.SubIndex == sub && !st.BurnInRequired {
				cfg.SubtitleURL = "/api/media/" + strconv.FormatInt(m.ID, 10) +
					"/subs/" + strconv.Itoa(sub) + ".vtt"
				cfg.SubtitleLbl = st.Lang
				if cfg.SubtitleLbl == "" {
					cfg.SubtitleLbl = "Sous-titres"
				}
			}
		}
	}

	blob, err := json.Marshal(cfg)
	if err != nil {
		http.Error(w, "could not build player config", http.StatusInternalServerError)
		return
	}

	s.render(w, r, "player.html", map[string]any{
		"Title": m.DisplayTitle(),
		"Nav":   "library",
		"Media": m,
		// The player attaches document-level key handlers and an hls.js
		// instance. Swapping the body under it would leave those bound to a
		// video element that no longer exists, so it loads and leaves the
		// old-fashioned way.
		"NoBoost": true,
		// Safe inside <script type="application/json">: the payload is
		// marshalled JSON, and template.JS keeps it unescaped.
		"PlayerConfig": template.JS(blob),
	})
}

// Settings is split across sub-screens: libraries, server state, and the
// account. Cramming them onto one page put the password form next to a cache
// button, which is not where it belongs.
// failureData distinguishes a file that will be retried from one that has been
// given up on, so "erreur" is an actionable state rather than a dead end.
func (s *Server) failureData(r *http.Request, m *db.Media) map[string]any {
	if m.Status != db.StatusError {
		return nil
	}
	attempts, permanent, next, err := s.store.ProbeRetryInfo(r.Context(), m.ID)
	if err != nil {
		return map[string]any{"Message": web.DerefStr(m.ErrorMsg)}
	}
	return map[string]any{
		"Message":     web.DerefStr(m.ErrorMsg),
		"Attempts":    attempts,
		"MaxAttempts": db.MaxProbeAttempts,
		"Permanent":   permanent,
		"NextAttempt": next,
		"WillRetry":   !permanent && attempts < db.MaxProbeAttempts,
		"GaveUp":      !permanent && attempts >= db.MaxProbeAttempts,
	}
}

func (s *Server) pageSettings(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/settings/libraries", http.StatusSeeOther)
}

func (s *Server) pageSettingsLibraries(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "settings_libraries.html", map[string]any{
		"Title": "Bibliothèques", "Nav": "settings"})
}

func (s *Server) pageSettingsStatus(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "settings_status.html", map[string]any{
		"Title": "État du serveur", "Nav": "settings"})
}

func (s *Server) pageSettingsAccount(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	// The shared API token has no account behind it, so there is nothing to
	// show or change here.
	if user == nil || user.ID == 0 {
		http.Redirect(w, r, "/settings/libraries", http.StatusSeeOther)
		return
	}
	fresh, err := s.store.GetUser(r.Context(), user.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	s.render(w, r, "settings_account.html", map[string]any{
		"Title": "Compte", "Nav": "settings", "User": fresh})
}

// ---------- fragments ----------

type crumb struct {
	Name string
	Path string
}

// buildCrumbs turns "Saison 1/Extras" into one clickable entry per segment.
func buildCrumbs(rel string) []crumb {
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return nil
	}
	parts := strings.Split(rel, "/")
	out := make([]crumb, 0, len(parts))
	for i, p := range parts {
		out = append(out, crumb{Name: p, Path: strings.Join(parts[:i+1], "/")})
	}
	return out
}

// fragMediaGrid serves the library view. Browsing follows the directory tree
// the user built on the Freebox: flattening a carefully filed collection into
// one long grid throws away the work they did organising it. Searching stays
// flat, because a search is expected to look everywhere.
func (s *Server) fragMediaGrid(w http.ResponseWriter, r *http.Request) {
	fail := func() {
		s.renderFragment(w, r, "alert", map[string]any{
			"Level": "error", "Message": "Erreur de base de données."})
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	libraryID := int64(queryInt(r, "library_id", 0))
	rel := r.URL.Query().Get("path")

	if query != "" {
		items, total, err := s.store.ListMedia(r.Context(), db.MediaFilter{
			LibraryID: libraryID,
			Query:     query,
			Limit:     queryInt(r, "limit", 200),
		})
		if err != nil {
			fail()
			return
		}
		s.renderFragment(w, r, "media_grid", s.gridData(r, map[string]any{
			"Search": true, "Query": query, "Files": items, "Total": total}))
		return
	}

	if libraryID == 0 {
		libs, err := s.store.ListLibraries(r.Context())
		if err != nil {
			fail()
			return
		}
		s.renderFragment(w, r, "media_grid", map[string]any{"Libraries": libs})
		return
	}

	lib, err := s.store.GetLibrary(r.Context(), libraryID)
	if err != nil {
		// A deleted library should drop the user back to the top, not 500.
		s.fragMediaGridRoot(w, r)
		return
	}
	dirs, files, err := s.store.BrowseLibrary(r.Context(), lib, rel)
	if err != nil {
		fail()
		return
	}
	data := s.gridData(r, map[string]any{
		"Library": lib,
		"Path":    rel,
		"Crumbs":  buildCrumbs(rel),
		"Dirs":    decorateDirs(dirs),
		"Files":   files,
	})
	if looksLikeEpisodes(files) {
		data["Episodes"] = s.episodeRows(r, files)
	}
	s.renderFragment(w, r, "media_grid", data)
}

// dirView is a folder as shown: the raw name is what the disk says, the label
// is what a person wants to read.
type dirView struct {
	db.DirEntry
	Label  string
	Season *int
}

func decorateDirs(dirs []db.DirEntry) []dirView {
	out := make([]dirView, 0, len(dirs))
	for _, d := range dirs {
		label, season := scanner.FolderLabel(d.Name)
		out = append(out, dirView{DirEntry: d, Label: label, Season: season})
	}
	return out
}

// looksLikeEpisodes decides by content, not by the library's declared kind: a
// documentary series filed under Films is still a list of episodes, and a
// folder of standalone films under a TV library is not. A clear majority
// carrying an episode number is the signal.
func looksLikeEpisodes(files []db.Media) bool {
	if len(files) < 2 {
		return false
	}
	numbered := 0
	for _, m := range files {
		if m.Episode != nil && *m.Episode > 0 {
			numbered++
		}
	}
	return numbered*4 >= len(files)*3
}

// gridData adds the self-refresh wiring. The grid only polls while something on
// it is still being probed, so a settled library costs nothing.
func (s *Server) gridData(r *http.Request, data map[string]any) map[string]any {
	pending := false
	if files, ok := data["Files"].([]db.Media); ok {
		for _, m := range files {
			if m.Status == db.StatusNew || m.Status == db.StatusProbing {
				pending = true
				break
			}
		}
	}
	data["Pending"] = pending
	data["RefreshURL"] = template.URL(r.URL.RequestURI())
	return data
}

// episodeRow is one line in an episode list: the poster grid is the wrong
// shape for twenty near-identical thumbnails whose titles differ only in the
// episode number.
type episodeRow struct {
	Media    db.Media
	Label    string
	Title    string
	Duration float64
	Progress float64
	Percent  int
	Watched  bool
}

func (s *Server) episodeRows(r *http.Request, files []db.Media) []episodeRow {
	out := make([]episodeRow, 0, len(files))
	for _, m := range files {
		row := episodeRow{Media: m, Title: m.DisplayTitle()}

		switch {
		case m.Season != nil && m.Episode != nil:
			row.Label = fmt.Sprintf("S%02dE%02d", *m.Season, *m.Episode)
		case m.Episode != nil:
			row.Label = fmt.Sprintf("E%02d", *m.Episode)
		}
		// The label already carries the numbering, so strip any copy of it at
		// the front of the episode title.
		if row.Label != "" {
			if _, after, found := strings.Cut(row.Title, " - "+row.Label+" - "); found {
				row.Title = after
			}
		}
		if m.DurationSec != nil {
			row.Duration = *m.DurationSec
		}

		if p, err := s.store.GetProgress(r.Context(), m.ID); err == nil && p != nil {
			row.Progress = p.PositionSec
			if row.Duration > 0 {
				row.Percent = int(p.PositionSec * 100 / row.Duration)
				// The last minute counts as finished, not as 97%.
				row.Watched = p.PositionSec >= row.Duration-60
			}
		}
		out = append(out, row)
	}
	return out
}

func (s *Server) fragMediaGridRoot(w http.ResponseWriter, r *http.Request) {
	libs, err := s.store.ListLibraries(r.Context())
	if err != nil {
		s.renderFragment(w, r, "alert", map[string]any{
			"Level": "error", "Message": "Erreur de base de données."})
		return
	}
	s.renderFragment(w, r, "media_grid", map[string]any{"Libraries": libs})
}

// fragMediaState is polled by the detail page while a file is still being
// probed. Once it is done the answer is an HX-Refresh, so the page reloads in
// full and picks up the title, artwork and stream list together rather than
// swapping in a play button beside stale metadata.
func (s *Server) fragMediaState(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMedia(w, r)
	if !ok {
		return
	}
	if m.Status == db.StatusNew || m.Status == db.StatusProbing {
		s.renderFragment(w, r, "probing", s.probingData(m))
		return
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusOK)
}

// probingData tells the user where their file sits in the queue, so "waiting
// for analysis" is an answerable statement rather than a shrug.
func (s *Server) probingData(m *db.Media) map[string]any {
	return map[string]any{"Media": m, "Pending": s.scanner.State().Queued}
}

// fragRetryProbe puts a failed file back at the front of the queue.
func (s *Server) fragRetryProbe(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMedia(w, r)
	if !ok {
		return
	}
	if err := s.store.ResetProbeAttempts(r.Context(), m.ID); err != nil {
		s.renderFragment(w, r, "alert", map[string]any{
			"Level": "error", "Message": "Impossible de relancer l'analyse."})
		return
	}
	s.log.Info("probe retry requested", "media_id", m.ID)
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusOK)
}

// fragSaveDetails stores a title and synopsis written by hand. Nothing derived
// from the filename will overwrite them afterwards.
func (s *Server) fragSaveDetails(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMedia(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderFragment(w, r, "alert", map[string]any{
			"Level": "error", "Message": "Formulaire invalide."})
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		s.renderFragment(w, r, "alert", map[string]any{
			"Level": "error", "Message": "Le titre ne peut pas être vide."})
		return
	}
	if err := s.store.SaveUserDetails(r.Context(), m.ID, title,
		r.FormValue("overview")); err != nil {
		s.renderFragment(w, r, "alert", map[string]any{
			"Level": "error", "Message": "Enregistrement impossible."})
		return
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusOK)
}

// fragResetDetails hands the row back to the automatic naming.
func (s *Server) fragResetDetails(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMedia(w, r)
	if !ok {
		return
	}
	if err := s.store.ClearUserDetails(r.Context(), m.ID); err != nil {
		s.renderFragment(w, r, "alert", map[string]any{
			"Level": "error", "Message": "Réinitialisation impossible."})
		return
	}
	// Re-derive immediately so the page does not come back empty.
	if fresh, err := s.store.GetMedia(r.Context(), m.ID); err == nil {
		s.scanner.DeriveNow(r.Context(), fresh)
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) fragBrowse(w http.ResponseWriter, r *http.Request) {
	entries, path, err := s.browse(r)
	data := map[string]any{"Path": path, "Entries": entries}
	if err != nil {
		data["Error"] = err.Error()
	}
	s.renderFragment(w, r, "browse", data)
}

func (s *Server) fragLibraries(w http.ResponseWriter, r *http.Request) {
	libs, err := s.store.ListLibraries(r.Context())
	if err != nil {
		s.renderFragment(w, r, "alert", map[string]any{
			"Level": "error", "Message": "Erreur de base de données."})
		return
	}
	s.renderFragment(w, r, "libraries", map[string]any{"Libraries": libs})
}

func (s *Server) fragCreateLibrary(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderFragment(w, r, "alert", map[string]any{
			"Level": "error", "Message": "Formulaire invalide."})
		return
	}
	ftpPath, name, kind, err := validateLibrary(
		r.FormValue("ftp_path"), r.FormValue("name"), r.FormValue("kind"))
	if err != nil {
		s.renderFragment(w, r, "alert", map[string]any{"Level": "error", "Message": err.Error()})
		return
	}

	lib, err := s.store.CreateLibrary(r.Context(), ftpPath, name, kind)
	if err != nil {
		s.renderFragment(w, r, "alert", map[string]any{
			"Level": "error", "Message": "Cette bibliothèque existe déjà."})
		return
	}
	if err := s.scanner.EnqueueScan(lib.ID); err != nil {
		s.log.Warn("could not enqueue initial scan", "library_id", lib.ID, "err", err)
	}

	w.Header().Set("HX-Trigger", "libraries-changed")
	s.renderFragment(w, r, "alert", map[string]any{
		"Level": "success", "Message": "Bibliothèque créée, analyse lancée."})
}

func (s *Server) fragDeleteLibrary(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		s.renderFragment(w, r, "alert", map[string]any{
			"Level": "error", "Message": "Identifiant invalide."})
		return
	}
	if err := s.store.DeleteLibrary(r.Context(), id); err != nil {
		s.renderFragment(w, r, "alert", map[string]any{
			"Level": "error", "Message": "Suppression impossible."})
		return
	}
	s.fragLibraries(w, r)
}

func (s *Server) fragScanLibrary(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		s.renderFragment(w, r, "alert", map[string]any{
			"Level": "error", "Message": "Identifiant invalide."})
		return
	}
	data := s.statusData(r, nil)
	if err := s.scanner.EnqueueScan(id); err != nil {
		data["Level"], data["Message"] = "error", err.Error()
	} else {
		data["Level"], data["Message"] = "success", "Analyse lancée."
	}
	s.renderFragment(w, r, "status", data)
}

// fragRefreshLibrary re-derives titles without re-probing, for when the naming
// rules change under an already-scanned library.
func (s *Server) fragRefreshLibrary(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		s.renderFragment(w, r, "alert", map[string]any{
			"Level": "error", "Message": "Identifiant invalide."})
		return
	}
	data := s.statusData(r, nil)
	if err := s.scanner.EnqueueRefresh(id); err != nil {
		data["Level"], data["Message"] = "error", err.Error()
	} else {
		data["Level"], data["Message"] = "success",
			"Réassociation des métadonnées lancée : aucun fichier ne sera relu sur le FTP."
	}
	s.renderFragment(w, r, "status", data)
}

func (s *Server) fragStatus(w http.ResponseWriter, r *http.Request) {
	s.renderFragment(w, r, "status", s.statusData(r, nil))
}

// statusData carries the configured limits the panel quotes back to the user,
// so the copy stays true if the configuration changes.
func (s *Server) statusData(r *http.Request, extra map[string]any) map[string]any {
	data := map[string]any{
		"Status":      s.status(r),
		"IdleTimeout": s.cfg.EncoderIdleTimeout.String(),
		"MaxEncoders": s.cfg.MaxConcurrentEncoders,
	}
	for k, v := range extra {
		data[k] = v
	}
	return data
}

func (s *Server) fragClearCache(w http.ResponseWriter, r *http.Request) {
	// Encoders write into the directories about to be removed, so wait for
	// them to actually exit rather than merely signalling them.
	s.mgr.Shutdown("cache cleared", 10*time.Second)

	data := s.statusData(r, nil)
	if err := s.janitor.Clear(r.Context()); err != nil {
		data["Level"], data["Message"] = "error", "Erreur lors du vidage du cache : "+err.Error()
	} else {
		data["Level"], data["Message"] = "success", "Cache vidé."
	}
	data["Status"] = s.status(r)
	s.renderFragment(w, r, "status", data)
}
