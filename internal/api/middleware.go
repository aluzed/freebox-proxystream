package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/apenombre/freebox-proxystream/internal/db"
)

const sessionCookie = "proxystream_session"

type ctxKey int

const userKey ctxKey = iota

func userFrom(ctx context.Context) *db.User {
	u, _ := ctx.Value(userKey).(*db.User)
	return u
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user := s.authenticate(r); user != nil {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
			return
		}
		s.denyAuth(w, r)
	})
}

func (s *Server) authenticate(r *http.Request) *db.User {
	// Optional shared token, for scripts and curl.
	if s.cfg.APIToken != "" {
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			if db.ConstantTimeEqual(strings.TrimPrefix(h, "Bearer "), s.cfg.APIToken) {
				return &db.User{ID: 0, Username: "api-token"}
			}
		}
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	user, err := s.store.SessionUser(r.Context(), c.Value)
	if err != nil {
		return nil
	}
	return user
}

// denyAuth answers in whatever dialect the caller speaks: JSON for the API,
// an htmx redirect header for fragments, a plain redirect for pages.
func (s *Server) denyAuth(w http.ResponseWriter, r *http.Request) {
	target := "/login"
	if r.URL.Path != "/" {
		target += "?next=" + urlQueryEscape(r.URL.RequestURI())
	}

	switch {
	case strings.HasPrefix(r.URL.Path, "/api/"):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
	case r.Header.Get("HX-Request") == "true":
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusUnauthorized)
	default:
		http.Redirect(w, r, target, http.StatusSeeOther)
	}
}

func urlQueryEscape(s string) string {
	return strings.NewReplacer("&", "%26", "?", "%3F", "#", "%23", " ", "%20").Replace(s)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// Segment requests are the hot path and would drown the log.
		if strings.Contains(r.URL.Path, "/seg_") && rec.status < 400 {
			return
		}
		level := slogLevelFor(rec.status)
		s.log.Log(r.Context(), level, "http",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration", time.Since(start).Round(time.Millisecond))
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic serving request",
					"path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()))
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
		r.ResponseWriter.WriteHeader(code)
	}
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}

// Flush lets HLS responses stream through the recorder unbuffered.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func pathInt64(r *http.Request, name string) (int64, error) {
	return strconv.ParseInt(r.PathValue(name), 10, 64)
}

func queryInt(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// loadMedia resolves the {id} path value, answering 404 itself when missing.
func (s *Server) loadMedia(w http.ResponseWriter, r *http.Request) (*db.Media, bool) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid media id")
		return nil, false
	}
	m, err := s.store.GetMedia(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "media not found")
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return nil, false
	}
	return m, true
}

func slogLevelFor(status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}
