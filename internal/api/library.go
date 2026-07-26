package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/aluzed/freebox-proxystream/internal/db"
	"github.com/aluzed/freebox-proxystream/internal/ftpclient"
)

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// browse is a live FTP listing. It is never cached: it only runs while someone
// is looking at the picker.
func (s *Server) browse(r *http.Request) ([]ftpclient.Entry, string, error) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}
	path = ftpclient.NormalizePath(path)
	entries, err := s.ftp.List(r.Context(), path)
	if err != nil {
		return nil, path, err
	}

	// Directories first, then files, each alphabetically.
	dirs := make([]ftpclient.Entry, 0, len(entries))
	files := make([]ftpclient.Entry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir {
			dirs = append(dirs, e)
		} else {
			files = append(files, e)
		}
	}
	sortEntries(dirs)
	sortEntries(files)
	return append(dirs, files...), path, nil
}

func sortEntries(e []ftpclient.Entry) {
	for i := 1; i < len(e); i++ {
		for j := i; j > 0 && strings.ToLower(e[j].Name) < strings.ToLower(e[j-1].Name); j-- {
			e[j], e[j-1] = e[j-1], e[j]
		}
	}
}

func (s *Server) apiBrowse(w http.ResponseWriter, r *http.Request) {
	entries, path, err := s.browse(r)
	if err != nil {
		writeError(w, http.StatusBadGateway, "FTP listing failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "entries": entries})
}

func (s *Server) apiListLibraries(w http.ResponseWriter, r *http.Request) {
	libs, err := s.store.ListLibraries(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"libraries": libs})
}

func validateLibrary(ftpPath, name, kind string) (string, string, string, error) {
	ftpPath = ftpclient.NormalizePath(strings.TrimSpace(ftpPath))
	name = strings.TrimSpace(name)
	kind = strings.TrimSpace(strings.ToLower(kind))

	if ftpPath == "/" {
		return "", "", "", errors.New("choose a directory, not the FTP root")
	}
	if name == "" {
		return "", "", "", errors.New("name is required")
	}
	if kind != db.KindMovie && kind != db.KindTV {
		return "", "", "", errors.New(`kind must be "movie" or "tv"`)
	}
	return ftpPath, name, kind, nil
}

func (s *Server) apiCreateLibrary(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FTPPath string `json:"ftp_path"`
		Name    string `json:"name"`
		Kind    string `json:"kind"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	ftpPath, name, kind, err := validateLibrary(body.FTPPath, body.Name, body.Kind)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	lib, err := s.store.CreateLibrary(r.Context(), ftpPath, name, kind)
	if err != nil {
		writeError(w, http.StatusConflict, "could not create library: "+err.Error())
		return
	}
	if err := s.scanner.EnqueueScan(lib.ID); err != nil {
		s.log.Warn("could not enqueue initial scan", "library_id", lib.ID, "err", err)
	}
	writeJSON(w, http.StatusCreated, lib)
}

func (s *Server) apiDeleteLibrary(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return
	}
	if err := s.store.DeleteLibrary(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete library")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiScanLibrary(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return
	}
	if _, err := s.store.GetLibrary(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, "library not found")
		return
	}
	if err := s.scanner.EnqueueScan(id); err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "scan queued"})
}

func (s *Server) apiRefreshLibrary(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return
	}
	if _, err := s.store.GetLibrary(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, "library not found")
		return
	}
	if err := s.scanner.EnqueueRefresh(id); err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "metadata refresh queued"})
}

type statusView struct {
	Encoders   any                  `json:"encoders"`
	Scan       any                  `json:"scan"`
	Retryable  int                  `json:"retryable"`
	Libraries  []db.LibraryProgress `json:"libraries"`
	CacheBytes int64                `json:"cache_bytes"`
	CacheMax   int64                `json:"cache_max_bytes"`
	CacheItems int64                `json:"cache_items"`
	FTPHost    string               `json:"ftp_host"`
}

func (s *Server) status(r *http.Request) statusView {
	total, count, err := s.store.CacheSize(r.Context())
	if err != nil {
		s.log.Warn("cache size query failed", "err", err)
	}
	retryable, err := s.store.CountRetryable(r.Context())
	if err != nil {
		s.log.Warn("retryable count failed", "err", err)
	}
	libraries, err := s.store.ProgressByLibrary(r.Context())
	if err != nil {
		s.log.Warn("per-library progress failed", "err", err)
	}
	return statusView{
		Encoders:   s.mgr.Status(),
		Scan:       s.scanner.State(),
		Retryable:  retryable,
		Libraries:  libraries,
		CacheBytes: total,
		CacheMax:   s.cfg.CacheMaxBytes,
		CacheItems: count,
		FTPHost:    s.cfg.FTPHost,
	}
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.status(r))
}
