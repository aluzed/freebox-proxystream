// Package rangeproxy exposes remote FTP files as a seekable HTTP resource on
// loopback. ffmpeg and ffprobe read from here instead of from a pipe, which is
// what lets `-ss` seek inside a remote file without downloading it.
package rangeproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Source resolves a media id to something openable at a byte offset.
type Source interface {
	Lookup(ctx context.Context, mediaID int64) (path string, size int64, err error)
	OpenAt(ctx context.Context, path string, offset int64) (io.ReadCloser, error)
}

type Server struct {
	src Source
	log *slog.Logger
}

func New(src Source, log *slog.Logger) *Server {
	return &Server{src: src, log: log}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/raw/{id}", s.serveRaw)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})
	return mux
}

func (s *Server) serveRaw(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad media id", http.StatusBadRequest)
		return
	}

	ftpPath, size, err := s.src.Lookup(r.Context(), id)
	if err != nil {
		http.Error(w, "media not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "application/octet-stream")

	start, end, hasRange, err := parseRange(r.Header.Get("Range"), size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
		return
	}

	length := end - start + 1
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if hasRange {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	if r.Method == http.MethodHead {
		return
	}
	if length == 0 {
		return
	}

	rc, err := s.src.OpenAt(r.Context(), ftpPath, start)
	if err != nil {
		s.log.Error("range proxy open failed", "media_id", id, "offset", start, "err", err)
		return // headers are already written; the client sees a truncated body
	}

	// The FTP read blocks in the transport, so cancellation has to close the
	// reader from the outside. Without this the connection leaks whenever
	// ffmpeg is killed mid-segment.
	done := make(chan struct{})
	go func() {
		select {
		case <-r.Context().Done():
			rc.Close()
		case <-done:
		}
	}()

	started := time.Now()
	n, err := io.CopyN(w, rc, length)
	close(done)
	rc.Close()

	if err != nil && !errors.Is(err, io.EOF) && r.Context().Err() == nil {
		s.log.Warn("range proxy copy ended early",
			"media_id", id, "offset", start, "want", length, "got", n,
			"elapsed", time.Since(started).Round(time.Millisecond), "err", err)
	}
}

// parseRange handles the single-range forms this project needs:
//
//	bytes=START-END, bytes=START-, bytes=-SUFFIX
//
// Multi-range is rejected with 416, as allowed by the spec.
func parseRange(header string, size int64) (start, end int64, ok bool, err error) {
	if header == "" {
		return 0, size - 1, false, nil
	}
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) {
		return 0, 0, false, errors.New("unsupported range unit")
	}
	spec := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if strings.Contains(spec, ",") {
		return 0, 0, false, errors.New("multi-range not supported")
	}
	dash := strings.Index(spec, "-")
	if dash < 0 {
		return 0, 0, false, errors.New("malformed range")
	}
	startStr := strings.TrimSpace(spec[:dash])
	endStr := strings.TrimSpace(spec[dash+1:])

	switch {
	case startStr == "":
		// suffix form: last N bytes
		n, perr := strconv.ParseInt(endStr, 10, 64)
		if perr != nil || n <= 0 {
			return 0, 0, false, errors.New("malformed suffix range")
		}
		if n > size {
			n = size
		}
		start = size - n
		end = size - 1
	default:
		start, err = strconv.ParseInt(startStr, 10, 64)
		if err != nil || start < 0 {
			return 0, 0, false, errors.New("malformed range start")
		}
		if start >= size {
			return 0, 0, false, errors.New("range start beyond end of file")
		}
		if endStr == "" {
			end = size - 1
		} else {
			end, err = strconv.ParseInt(endStr, 10, 64)
			if err != nil || end < start {
				return 0, 0, false, errors.New("malformed range end")
			}
			if end >= size {
				end = size - 1
			}
		}
	}
	return start, end, true, nil
}
