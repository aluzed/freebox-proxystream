// Package scanner walks a library over FTP, probes new files through the range
// proxy, and names them from their own path. One background worker, one
// buffered channel: no external queue, and no online catalogue.
package scanner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aluzed/freebox-proxystream/internal/config"
	"github.com/aluzed/freebox-proxystream/internal/db"
	"github.com/aluzed/freebox-proxystream/internal/ftpclient"
	"github.com/aluzed/freebox-proxystream/internal/probe"
	"github.com/aluzed/freebox-proxystream/internal/thumbnail"
)

const (
	maxDepth     = 6
	minFileBytes = 50 << 20
	// Only scan requests are queued in memory, and there is one library per
	// request, so this is generous. Probes are NOT queued here: the media
	// table is their queue.
	queueSize = 64
	// How many rows to claim per round trip while draining.
	probeBatch = 50
	// Backstop in case a scan request is missed.
	idleSweep = 60 * time.Second
)

var videoExtensions = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".m4v": true,
	".mov": true, ".ts": true, ".wmv": true, ".webm": true,
}

type jobKind int

const (
	jobScan jobKind = iota
	// jobRefresh re-derives titles for already-probed files. It never touches
	// the FTP server, so correcting thousands of names costs nothing but a
	// pass over the database.
	jobRefresh
)

type job struct {
	kind      jobKind
	libraryID int64
}

// State is what the status panel shows. Running covers the whole pipeline, not
// just the directory walk: queued probes are the scan still working, and
// reporting "nothing running" next to a pile of pending tasks is a lie.
type State struct {
	Running     bool      `json:"running"`
	LibraryID   int64     `json:"library_id,omitempty"`
	LibraryName string    `json:"library_name,omitempty"`
	Phase       string    `json:"phase,omitempty"`
	Found       int       `json:"found"`
	Probed      int       `json:"probed"`
	ToProbe     int       `json:"to_probe"`
	Queued      int       `json:"queued"`
	PendingJobs int       `json:"pending_jobs"`
	CurrentFile string    `json:"current_file,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
}

type Scanner struct {
	cfg    *config.Config
	store  *db.Store
	ftp    directoryLister
	thumbs *thumbnail.Generator
	log    *slog.Logger

	jobs chan job

	mu    sync.Mutex
	state State
}

type directoryLister interface {
	List(context.Context, string) ([]ftpclient.Entry, error)
}

func New(cfg *config.Config, store *db.Store, ftp *ftpclient.Client,
	thumbs *thumbnail.Generator, log *slog.Logger) *Scanner {
	return &Scanner{
		cfg:    cfg,
		store:  store,
		ftp:    ftp,
		thumbs: thumbs,
		log:    log,
		jobs:   make(chan job, queueSize),
	}
}

func (s *Scanner) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state
	st.PendingJobs = len(s.jobs)
	// Derived, never accumulated. A total kept as its own counter goes wrong
	// the moment work outlives the walk that queued it: a restart requeues
	// pending probes without re-walking, and a second scan used to reset the
	// counter while the first scan's probes were still running. Both produced
	// nonsense like "152/0". done+remaining cannot disagree with itself.
	st.ToProbe = st.Probed + st.Queued
	return st
}

func (s *Scanner) update(fn func(*State)) {
	s.mu.Lock()
	fn(&s.state)
	s.mu.Unlock()
}

// EnqueueScan is non-blocking: a full queue means a scan is already backed up,
// and dropping the request is better than stalling an HTTP handler.
func (s *Scanner) EnqueueScan(libraryID int64) error {
	select {
	case s.jobs <- job{kind: jobScan, libraryID: libraryID}:
		return nil
	default:
		return errors.New("scan queue is full, try again once the current scan finishes")
	}
}

// DeriveNow re-derives one row's title immediately, for the "reset to
// automatic" action which should not wait for a background pass.
func (s *Scanner) DeriveNow(ctx context.Context, m *db.Media) error {
	return s.deriveMetadata(ctx, m)
}

// EnqueueRefresh re-matches metadata for a library without re-probing.
func (s *Scanner) EnqueueRefresh(libraryID int64) error {
	select {
	case s.jobs <- job{kind: jobRefresh, libraryID: libraryID}:
		return nil
	default:
		return errors.New("queue is full, try again shortly")
	}
}

// Run is the single worker goroutine. It alternates between servicing scan
// requests and draining the probe backlog, which lives in the media table
// rather than in memory: a library of several thousand files must not depend
// on a channel being big enough to hold it.
func (s *Scanner) Run(ctx context.Context) {
	// Pick up whatever a previous run left unfinished.
	s.drainPending(ctx)
	s.markIdle()

	ticker := time.NewTicker(idleSweep)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case j := <-s.jobs:
			s.beginWork()
			switch j.kind {
			case jobScan:
				if err := s.scanLibrary(ctx, j.libraryID); err != nil && ctx.Err() == nil {
					s.log.Error("library scan failed", "library_id", j.libraryID, "err", err)
					s.update(func(st *State) { st.LastError = err.Error() })
				}
			case jobRefresh:
				if err := s.refreshMetadata(ctx, j.libraryID); err != nil && ctx.Err() == nil {
					s.log.Error("metadata refresh failed", "library_id", j.libraryID, "err", err)
					s.update(func(st *State) { st.LastError = err.Error() })
				}
			}
			s.drainPending(ctx)
			s.markIdle()

		case <-ticker.C:
			// Backstop: anything marked for probing but never picked up.
			if n, err := s.store.CountPendingProbes(ctx); err == nil && n > 0 {
				s.beginWork()
				s.drainPending(ctx)
				s.markIdle()
			}
		}
	}
}

func (s *Scanner) beginWork() {
	s.update(func(st *State) {
		if !st.Running {
			st.Probed = 0
			st.Found = 0
			st.LastError = ""
			st.StartedAt = time.Now()
		}
		st.Running = true
	})
}

func (s *Scanner) markIdle() {
	s.update(func(st *State) {
		st.Running = false
		st.Phase = ""
		st.CurrentFile = ""
		st.StartedAt = time.Time{}
	})
}

// drainPending probes every row still waiting, in batches, until none are
// left. It stops if a round makes no progress, so a row that cannot be marked
// cannot spin the worker.
func (s *Scanner) drainPending(ctx context.Context) {
	for {
		// A requested rescan can remove stale rows from this very backlog.
		// Service it before starting another potentially slow remote probe.
		if ctx.Err() != nil || len(s.jobs) > 0 {
			return
		}
		remaining, err := s.store.CountPendingProbes(ctx)
		if err != nil {
			s.log.Warn("could not count pending probes", "err", err)
			return
		}
		s.update(func(st *State) { st.Queued = remaining })
		if remaining == 0 {
			return
		}

		ids, err := s.store.PendingProbes(ctx, probeBatch)
		if err != nil || len(ids) == 0 {
			return
		}
		s.update(func(st *State) { st.Running = true })

		for _, id := range ids {
			if ctx.Err() != nil || len(s.jobs) > 0 {
				return
			}
			if err := s.probeOne(ctx, id); err != nil && ctx.Err() == nil {
				s.log.Warn("probe failed", "media_id", id, "err", err)
			}
		}

		after, err := s.store.CountPendingProbes(ctx)
		if err != nil {
			return
		}
		if after >= remaining {
			s.log.Error("probe backlog is not shrinking, stopping this pass",
				"remaining", after)
			return
		}
	}
}

func (s *Scanner) scanLibrary(ctx context.Context, libraryID int64) error {
	lib, err := s.store.GetLibrary(ctx, libraryID)
	if err != nil {
		return err
	}

	s.update(func(st *State) {
		st.Running = true
		st.LibraryID = lib.ID
		st.LibraryName = lib.Name
		st.Phase = "listing"
		st.Found = 0
		if st.StartedAt.IsZero() {
			st.StartedAt = time.Now()
		}
	})

	s.log.Info("library scan started", "library_id", lib.ID, "path", lib.FTPPath)

	files, unreadable, err := s.walk(ctx, lib.FTPPath, 0)
	if err != nil {
		// FTP 550 also means permission denied. Only a successful listing
		// of the parent can confirm that this source was actually removed.
		if ctx.Err() != nil || !s.directoryMissing(ctx, lib.FTPPath) {
			return fmt.Errorf("walk %s: %w", lib.FTPPath, err)
		}
		s.log.Info("library directory no longer exists", "path", lib.FTPPath)
	}

	s.update(func(st *State) { st.Phase = "indexing"; st.Found = len(files) })

	seen := make([]string, 0, len(files))
	var newCount int
	for _, f := range files {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		seen = append(seen, f.Path)
		_, needsProbe, err := s.store.UpsertMediaFile(ctx, lib.ID, f.Path, f.Size, f.Mtime)
		if err != nil {
			return fmt.Errorf("index %s: %w", f.Path, err)
		}
		if needsProbe {
			newCount++
		}
	}

	if len(unreadable) > 0 {
		s.log.Warn("some directories could not be listed, nothing below them was removed",
			"library_id", lib.ID, "dirs", len(unreadable))
	}
	removed, err := s.store.DeleteMissing(ctx, lib.ID, seen, unreadable)
	if err != nil {
		return fmt.Errorf("remove missing media: %w", err)
	}
	for _, id := range removed {
		s.dropDerived(id)
	}
	if len(unreadable) > 0 {
		return fmt.Errorf("analyse incomplète : %d dossier(s) non parcouru(s), contenu conservé (premier : %s)",
			len(unreadable), unreadable[0])
	}

	if err := s.store.TouchLibraryScan(ctx, lib.ID); err != nil {
		return fmt.Errorf("record scan time: %w", err)
	}

	s.log.Info("library scan finished",
		"library_id", lib.ID, "files", len(files), "to_probe", newCount, "removed", len(removed))
	return nil
}

func (s *Scanner) directoryMissing(ctx context.Context, dir string) bool {
	dir = ftpclient.NormalizePath(dir)
	if dir == "/" {
		return false
	}
	entries, err := s.ftp.List(ctx, path.Dir(dir))
	if err != nil || ctx.Err() != nil {
		return false
	}
	for _, entry := range entries {
		if entry.Path == dir {
			return false
		}
	}
	return true
}

// dropDerived deletes what was generated from a file that no longer exists.
// The database rows go with the media through ON DELETE CASCADE; these files
// have no owner left once the row is gone.
func (s *Scanner) dropDerived(mediaID int64) {
	if s.thumbs != nil {
		if err := s.thumbs.Remove(mediaID); err != nil {
			s.log.Debug("could not remove thumbnail", "media_id", mediaID, "err", err)
		}
	}
	dir := filepath.Join(s.cfg.CacheDir, strconv.FormatInt(mediaID, 10))
	if err := os.RemoveAll(dir); err != nil {
		s.log.Debug("could not remove cached segments", "media_id", mediaID, "err", err)
	}
}

// refreshMetadata re-derives titles across an already-probed library. Used
// when the naming rules change: re-probing would re-read every file over FTP
// for information that has not changed.
func (s *Scanner) refreshMetadata(ctx context.Context, libraryID int64) error {
	lib, err := s.store.GetLibrary(ctx, libraryID)
	if err != nil {
		return err
	}
	ids, err := s.store.ProbedMediaIDs(ctx, libraryID)
	if err != nil {
		return err
	}

	s.log.Info("metadata refresh started", "library_id", libraryID, "files", len(ids))
	s.update(func(st *State) {
		st.LibraryID = lib.ID
		st.LibraryName = lib.Name
		st.Phase = "metadata"
		st.Probed = 0
		st.Queued = len(ids)
	})

	for i, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		m, err := s.store.GetMedia(ctx, id)
		if err != nil {
			continue
		}
		s.update(func(st *State) {
			st.CurrentFile = path.Base(m.FTPPath)
			st.Probed = i + 1
			st.Queued = len(ids) - i - 1
		})
		if err := s.deriveMetadata(ctx, m); err != nil {
			s.log.Debug("refresh failed", "media_id", id, "err", err)
		}
	}

	s.log.Info("metadata refresh finished", "library_id", libraryID, "files", len(ids))
	return nil
}

// walk returns the video files below dir, and the directories it could not
// look inside. The second list is what keeps a failed listing from being taken
// for a deletion: the caller leaves everything below those paths untouched.
func (s *Scanner) walk(ctx context.Context, dir string, depth int) ([]ftpclient.Entry, []string, error) {
	if depth > maxDepth {
		// Not read, so not known to be gone either.
		return nil, []string{dir}, nil
	}
	entries, err := s.ftp.List(ctx, dir)
	if err != nil {
		return nil, nil, err
	}

	var out []ftpclient.Entry
	var unreadable []string
	for _, e := range entries {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if e.IsDir {
			sub, subUnreadable, err := s.walk(ctx, e.Path, depth+1)
			if err != nil {
				// One unreadable directory should not abort the whole scan.
				s.log.Warn("skipping unreadable directory", "path", e.Path, "err", err)
				unreadable = append(unreadable, e.Path)
				continue
			}
			out = append(out, sub...)
			unreadable = append(unreadable, subUnreadable...)
			continue
		}
		if !isVideoFile(e.Name) || e.Size < minFileBytes {
			continue
		}
		out = append(out, e)
		s.update(func(st *State) { st.Found = len(out) })
	}
	return out, unreadable, nil
}

func isVideoFile(name string) bool {
	return videoExtensions[strings.ToLower(path.Ext(name))]
}

// probeOne isolates a single file. Nothing about one badly named or corrupt
// item justifies killing the scanner, let alone the server, and a panic here
// would otherwise take the whole process with it.
func (s *Scanner) probeOne(ctx context.Context, mediaID int64) (err error) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("recovered from a panic while probing",
				"media_id", mediaID, "panic", r, "stack", string(debug.Stack()))
			// Park the row permanently: a panic is deterministic for a given
			// file, so an automatic retry would reproduce it. The manual
			// retry button remains available.
			s.store.RecordProbeFailure(context.WithoutCancel(ctx), mediaID,
				fmt.Sprintf("internal error while analysing this file: %v", r), true)
			err = fmt.Errorf("panic while probing media %d: %v", mediaID, r)
		}
	}()
	return s.probeMedia(ctx, mediaID)
}

func (s *Scanner) probeMedia(ctx context.Context, mediaID int64) error {
	m, err := s.store.GetMedia(ctx, mediaID)
	if err != nil {
		return err
	}
	if err := s.store.SetMediaStatus(ctx, mediaID, db.StatusProbing, ""); err != nil {
		return err
	}
	s.update(func(st *State) {
		st.Phase = "probing"
		st.CurrentFile = path.Base(m.FTPPath)
	})

	res, err := probe.Run(ctx, s.cfg.FFprobeBin, s.cfg.RawURL(mediaID))
	if err != nil {
		// A file that is not a video stays parked; a network hiccup gets
		// another go later. Guessing from the message afterwards is fragile,
		// so probe.Run classifies at the point of failure.
		perm := probe.IsPermanent(err)
		if rerr := s.store.RecordProbeFailure(ctx, mediaID,
			truncate(err.Error(), 500), perm); rerr != nil {
			s.log.Warn("could not record probe failure", "media_id", mediaID, "err", rerr)
		}
		return fmt.Errorf("ffprobe %s (permanent=%v): %w", m.FTPPath, perm, err)
	}
	if err := s.store.SaveProbe(ctx, mediaID, *res); err != nil {
		return err
	}

	if err := s.deriveMetadata(ctx, m); err != nil {
		s.log.Warn("could not derive metadata", "media_id", mediaID, "err", err)
	}

	// Grab the poster frame now rather than on first view, so the grid is
	// already populated when the scan finishes. Best effort: a file that
	// cannot be decoded is still perfectly listable.
	if s.thumbs != nil {
		fresh, err := s.store.GetMedia(ctx, mediaID)
		if err == nil {
			if _, err := s.thumbs.Ensure(ctx, fresh); err != nil && ctx.Err() == nil {
				s.log.Debug("thumbnail generation failed", "media_id", mediaID, "err", err)
			}
		}
	}

	s.update(func(st *State) { st.Probed++ })
	return s.store.SetMediaStatus(ctx, mediaID, db.StatusReady, "")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
