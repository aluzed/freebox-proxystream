package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

func NewStore(sdb *sql.DB) *Store { return &Store{db: sdb} }

func (s *Store) DB() *sql.DB { return s.db }

// ---------- libraries ----------

func (s *Store) CreateLibrary(ctx context.Context, ftpPath, name, kind string) (*Library, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO libraries (ftp_path, name, kind, created_at) VALUES (?, ?, ?, ?)`,
		ftpPath, name, kind, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Library{ID: id, FTPPath: ftpPath, Name: name, Kind: kind, CreatedAt: now}, nil
}

func (s *Store) ListLibraries(ctx context.Context) ([]Library, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ftp_path, name, kind, created_at, last_scan FROM libraries ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Library{}
	for rows.Next() {
		var l Library
		if err := rows.Scan(&l.ID, &l.FTPPath, &l.Name, &l.Kind, &l.CreatedAt, &l.LastScan); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) GetLibrary(ctx context.Context, id int64) (*Library, error) {
	var l Library
	err := s.db.QueryRowContext(ctx,
		`SELECT id, ftp_path, name, kind, created_at, last_scan FROM libraries WHERE id = ?`, id).
		Scan(&l.ID, &l.FTPPath, &l.Name, &l.Kind, &l.CreatedAt, &l.LastScan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &l, err
}

func (s *Store) DeleteLibrary(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM libraries WHERE id = ?`, id)
	return err
}

func (s *Store) TouchLibraryScan(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE libraries SET last_scan = ? WHERE id = ?`, time.Now().UTC(), id)
	return err
}

// ---------- media ----------

const mediaCols = `id, library_id, ftp_path, size_bytes, ftp_mtime,
	probed_at, duration_sec, video_codec, width, height, fps, audio_streams, sub_streams,
	title, year, season, episode, overview, user_edited,
	status, error_msg, created_at`

func scanMedia(sc interface{ Scan(...any) error }) (*Media, error) {
	var m Media
	var audioJSON, subJSON *string
	err := sc.Scan(&m.ID, &m.LibraryID, &m.FTPPath, &m.SizeBytes, &m.FTPMtime,
		&m.ProbedAt, &m.DurationSec, &m.VideoCodec, &m.Width, &m.Height, &m.FPS, &audioJSON, &subJSON,
		&m.Title, &m.Year, &m.Season, &m.Episode, &m.Overview, &m.UserEdited,
		&m.Status, &m.ErrorMsg, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	m.AudioStreams = unmarshalAudio(audioJSON)
	m.SubStreams = unmarshalSubs(subJSON)
	return &m, nil
}

func (s *Store) GetMedia(ctx context.Context, id int64) (*Media, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+mediaCols+` FROM media WHERE id = ?`, id)
	m, err := scanMedia(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

type MediaFilter struct {
	LibraryID int64
	Query     string
	Status    string
	Limit     int
	Offset    int
}

func (s *Store) ListMedia(ctx context.Context, f MediaFilter) ([]Media, int, error) {
	var where []string
	var args []any
	if f.LibraryID > 0 {
		where = append(where, "library_id = ?")
		args = append(args, f.LibraryID)
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, "(COALESCE(title,'') LIKE ? OR ftp_path LIKE ?)")
		like := "%" + q + "%"
		args = append(args, like, like)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM media`+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	if f.Limit <= 0 {
		f.Limit = 100
	}
	q := `SELECT ` + mediaCols + ` FROM media` + clause +
		` ORDER BY COALESCE(title, ftp_path), COALESCE(season,0), COALESCE(episode,0) LIMIT ? OFFSET ?`
	args = append(args, f.Limit, f.Offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []Media{}
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *m)
	}
	return out, total, rows.Err()
}

// DirEntry is a folder as it appears when browsing a library, with the number
// of media anywhere beneath it.
type DirEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"` // relative to the library root, no leading slash
	Count int    `json:"count"`
	// CoverMediaID is a representative item whose frame stands in for the
	// folder.
	CoverMediaID int64 `json:"cover_media_id,omitempty"`
}

// escapeLike neutralises the wildcards in a path used as a LIKE prefix.
// Underscores are ordinary characters in release names and would otherwise
// match anything.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// BrowseLibrary returns one level of a library: the folders directly inside
// rel, and the media sitting directly in it. The source directory tree is the
// user's own filing, so the UI mirrors it rather than flattening everything.
func (s *Store) BrowseLibrary(ctx context.Context, lib *Library, rel string) ([]DirEntry, []Media, error) {
	prefix := strings.TrimSuffix(lib.FTPPath, "/")
	if rel = strings.Trim(rel, "/"); rel != "" {
		prefix += "/" + rel
	}
	prefix += "/"

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+mediaCols+` FROM media
		 WHERE library_id = ? AND ftp_path LIKE ? ESCAPE '\'
		 ORDER BY ftp_path`,
		lib.ID, escapeLike(prefix)+"%")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	type folder struct {
		count   int
		coverID int64
	}
	dirs := map[string]*folder{}
	var order []string
	files := []Media{}

	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, nil, err
		}
		rest := strings.TrimPrefix(m.FTPPath, prefix)
		i := strings.Index(rest, "/")
		if i < 0 {
			files = append(files, *m)
			continue
		}

		name := rest[:i]
		f, seen := dirs[name]
		if !seen {
			f = &folder{}
			dirs[name] = f
			order = append(order, name)
		}
		f.count++

		// Every media can produce a frame, so the first one is as good a
		// cover as any.
		if f.coverID == 0 {
			f.coverID = m.ID
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	sort.Slice(order, func(i, j int) bool {
		return strings.ToLower(order[i]) < strings.ToLower(order[j])
	})
	out := make([]DirEntry, 0, len(order))
	for _, name := range order {
		child := name
		if rel != "" {
			child = rel + "/" + name
		}
		f := dirs[name]
		out = append(out, DirEntry{
			Name:         name,
			Path:         child,
			Count:        f.count,
			CoverMediaID: f.coverID,
		})
	}

	sort.SliceStable(files, func(i, j int) bool {
		a, b := files[i], files[j]
		if sa, sb := deref(a.Season), deref(b.Season); sa != sb {
			return sa < sb
		}
		if ea, eb := deref(a.Episode), deref(b.Episode); ea != eb {
			return ea < eb
		}
		return strings.ToLower(a.DisplayTitle()) < strings.ToLower(b.DisplayTitle())
	})
	return out, files, nil
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// UpsertMediaFile registers a file seen by a scan. Returns the media id and
// whether the row is new or its size/mtime changed (both mean it needs a probe).
func (s *Store) UpsertMediaFile(ctx context.Context, libraryID int64, ftpPath string, size int64, mtime time.Time) (int64, bool, error) {
	var id int64
	var oldSize int64
	var status string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, size_bytes, status FROM media WHERE ftp_path = ?`, ftpPath).Scan(&id, &oldSize, &status)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := s.db.ExecContext(ctx,
			`INSERT INTO media (library_id, ftp_path, size_bytes, ftp_mtime, status, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			libraryID, ftpPath, size, mtime, StatusNew, time.Now().UTC())
		if err != nil {
			return 0, false, err
		}
		id, err = res.LastInsertId()
		return id, true, err
	case err != nil:
		return 0, false, err
	}

	changed := oldSize != size
	needsProbe := changed || status == StatusNew || status == StatusError
	if changed {
		// Different bytes: forget any previous verdict.
		_, err = s.db.ExecContext(ctx,
			`UPDATE media SET library_id = ?, size_bytes = ?, ftp_mtime = ?, status = ?,
			 error_msg = NULL, error_permanent = 0, probe_attempts = 0,
			 next_attempt_at = NULL WHERE id = ?`,
			libraryID, size, mtime, StatusNew, id)
		return id, true, err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE media SET library_id = ?, size_bytes = ?, ftp_mtime = ? WHERE id = ?`,
		libraryID, size, mtime, id)
	return id, needsProbe, err
}

// MarkMissing flags rows of a library whose file was not seen by the last scan.
// Rows are never deleted silently.
func (s *Store) MarkMissing(ctx context.Context, libraryID int64, seen []string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS scan_seen (ftp_path TEXT PRIMARY KEY)`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM scan_seen`); err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO scan_seen (ftp_path) VALUES (?)`)
	if err != nil {
		return 0, err
	}
	for _, p := range seen {
		if _, err := stmt.ExecContext(ctx, p); err != nil {
			stmt.Close()
			return 0, err
		}
	}
	stmt.Close()

	res, err := tx.ExecContext(ctx,
		`UPDATE media SET status = ?, error_msg = 'file disappeared from FTP during scan'
		 WHERE library_id = ? AND status != ? AND ftp_path NOT IN (SELECT ftp_path FROM scan_seen)`,
		StatusError, libraryID, StatusError)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), tx.Commit()
}

// RecordProbeFailure stores the outcome and schedules the next attempt. A
// permanent failure is parked; a transient one backs off exponentially and is
// given up on after MaxProbeAttempts.
func (s *Store) RecordProbeFailure(ctx context.Context, id int64, msg string, isPermanent bool) error {
	if isPermanent {
		_, err := s.db.ExecContext(ctx,
			`UPDATE media SET status = ?, error_msg = ?, error_permanent = 1,
			 next_attempt_at = NULL WHERE id = ?`, StatusError, msg, id)
		return err
	}

	var attempts int
	if err := s.db.QueryRowContext(ctx,
		`SELECT probe_attempts FROM media WHERE id = ?`, id).Scan(&attempts); err != nil {
		return err
	}
	attempts++

	// 1, 2, 4, 8, 16 minutes.
	backoff := time.Duration(1<<min(attempts-1, 4)) * time.Minute
	next := time.Now().UTC().Add(backoff)

	_, err := s.db.ExecContext(ctx,
		`UPDATE media SET status = ?, error_msg = ?, error_permanent = 0,
		 probe_attempts = ?, next_attempt_at = ? WHERE id = ?`,
		StatusError, msg, attempts, next, id)
	return err
}

// ResetProbeAttempts puts a row back at the front of the queue, for the manual
// retry button and for a file whose bytes changed.
func (s *Store) ResetProbeAttempts(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE media SET status = ?, error_msg = NULL, error_permanent = 0,
		 probe_attempts = 0, next_attempt_at = NULL WHERE id = ?`, StatusNew, id)
	return err
}

// ProbeRetryInfo backs the "will retry" vs "gave up" distinction in the UI.
func (s *Store) ProbeRetryInfo(ctx context.Context, id int64) (attempts int, permanent bool, next *time.Time, err error) {
	var perm int
	err = s.db.QueryRowContext(ctx,
		`SELECT probe_attempts, error_permanent, next_attempt_at FROM media WHERE id = ?`, id).
		Scan(&attempts, &perm, &next)
	return attempts, perm == 1, next, err
}

func (s *Store) SetMediaStatus(ctx context.Context, id int64, status string, errMsg string) error {
	var e *string
	if errMsg != "" {
		e = &errMsg
	}
	_, err := s.db.ExecContext(ctx, `UPDATE media SET status = ?, error_msg = ? WHERE id = ?`, status, e, id)
	return err
}

type ProbeResult struct {
	DurationSec float64
	VideoCodec  string
	Width       int
	Height      int
	FPS         float64
	Audio       []AudioStream
	Subs        []SubStream
}

func (s *Store) SaveProbe(ctx context.Context, id int64, p ProbeResult) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE media SET probed_at = ?, duration_sec = ?, video_codec = ?, width = ?, height = ?,
		 fps = ?, audio_streams = ?, sub_streams = ? WHERE id = ?`,
		time.Now().UTC(), p.DurationSec, p.VideoCodec, p.Width, p.Height, p.FPS,
		marshalJSON(p.Audio), marshalJSON(p.Subs), id)
	return err
}

// MetadataResult is what the file's own path tells us. There is no online
// catalogue: a synopsis is the owner's to write.
type MetadataResult struct {
	Title    string
	Year     *int
	Season   *int
	Episode  *int
	Overview string
}

func (s *Store) SaveMetadata(ctx context.Context, id int64, m MetadataResult) error {
	nz := func(v string) *string {
		if v == "" {
			return nil
		}
		return &v
	}
	_, err := s.db.ExecContext(ctx,
		// user_edited = 0 guards the words someone typed by hand.
		`UPDATE media SET title = ?, year = ?, season = ?, episode = ?, overview = ?
		 WHERE id = ? AND user_edited = 0`,
		nz(m.Title), m.Year, m.Season, m.Episode, nz(m.Overview), id)
	return err
}

// MaxProbeAttempts bounds retries of a transient failure. Five attempts with
// the backoff below spans roughly half an hour, which covers a reboot of the
// Freebox or a flaky link without hammering it.
const MaxProbeAttempts = 5

// pendingWhere selects rows still owed a probe: never attempted, interrupted
// mid-probe, or failed transiently and now due for another go.
//
// `probe_attempts > 0` is load-bearing. Only RecordProbeFailure sets it, so a
// row marked 'error' by any other path (a missing file, a recovered panic)
// stays out of the queue. Without it those rows re-enter immediately, fail the
// same way, and spin the worker forever.
const pendingWhere = `
	(status IN ('new', 'probing')
	 OR (status = 'error'
	     AND error_permanent = 0
	     AND probe_attempts > 0
	     AND probe_attempts < ?
	     AND (next_attempt_at IS NULL OR next_attempt_at <= ?)))`

// CountPendingProbes is the real size of the work queue. The scanner keeps no
// in-memory backlog, so this is authoritative.
func (s *Store) CountPendingProbes(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM media WHERE`+pendingWhere,
		MaxProbeAttempts, time.Now().UTC()).Scan(&n)
	return n, err
}

// LibraryProgress is the per-library breakdown the status panel shows. One
// global counter cannot say which library is stuck when several exist.
type LibraryProgress struct {
	LibraryID int64  `json:"library_id"`
	Name      string `json:"name"`
	Total     int    `json:"total"`
	Ready     int    `json:"ready"`
	Pending   int    `json:"pending"`
	Retryable int    `json:"retryable"`
	Failed    int    `json:"failed"`
}

// ProgressByLibrary counts every library in one pass, so the panel costs a
// single query however many libraries exist.
func (s *Store) ProgressByLibrary(ctx context.Context) ([]LibraryProgress, error) {
	now := time.Now().UTC()
	rows, err := s.db.QueryContext(ctx, `
		SELECT l.id, l.name,
		  COUNT(m.id),
		  COALESCE(SUM(CASE WHEN m.status = 'ready' THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN `+pendingWhere+` THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN m.status = 'error' AND m.error_permanent = 0
		                     AND m.probe_attempts > 0 AND m.probe_attempts < ?
		                     AND m.next_attempt_at > ? THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN m.status = 'error'
		                     AND (m.error_permanent = 1 OR m.probe_attempts >= ?)
		                    THEN 1 ELSE 0 END), 0)
		FROM libraries l
		LEFT JOIN media m ON m.library_id = l.id
		GROUP BY l.id, l.name
		ORDER BY l.name`,
		MaxProbeAttempts, now, MaxProbeAttempts, now, MaxProbeAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []LibraryProgress{}
	for rows.Next() {
		var p LibraryProgress
		if err := rows.Scan(&p.LibraryID, &p.Name, &p.Total, &p.Ready,
			&p.Pending, &p.Retryable, &p.Failed); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountRetryable is how many rows are waiting for their backoff to expire,
// which the status panel shows so a stalled library is explainable.
func (s *Store) CountRetryable(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM media
		 WHERE status = 'error' AND error_permanent = 0 AND probe_attempts < ?
		   AND next_attempt_at > ?`,
		MaxProbeAttempts, time.Now().UTC()).Scan(&n)
	return n, err
}

// PendingProbes returns the next batch to probe, oldest first.
// CountVideosInFolder counts the media sitting directly in a file's own
// folder. Several videos together is strong evidence of a series: a season
// folder holds episodes, whereas a film usually sits alone or among unrelated
// titles.
func (s *Store) CountVideosInFolder(ctx context.Context, libraryID int64, ftpPath string) (int, error) {
	dir := ftpPath
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		dir = dir[:i+1]
	}
	prefix := escapeLike(dir)
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM media
		 WHERE library_id = ?
		   AND ftp_path LIKE ? ESCAPE '\'
		   AND ftp_path NOT LIKE ? ESCAPE '\'`,
		libraryID, prefix+"%", prefix+"%/%").Scan(&n)
	return n, err
}

// ProbedMediaIDs lists everything already probed in a library, for a metadata
// refresh that must not touch the FTP server again.
func (s *Store) ProbedMediaIDs(ctx context.Context, libraryID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM media WHERE library_id = ? AND probed_at IS NOT NULL ORDER BY id`,
		libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SaveUserDetails records a title and synopsis written by hand, and marks the
// row so nothing derived overwrites them afterwards.
func (s *Store) SaveUserDetails(ctx context.Context, id int64, title, overview string) error {
	nz := func(v string) *string {
		v = strings.TrimSpace(v)
		if v == "" {
			return nil
		}
		return &v
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE media SET title = ?, overview = ?, user_edited = 1 WHERE id = ?`,
		nz(title), nz(overview), id)
	return err
}

// ClearUserDetails hands the row back to the automatic naming.
func (s *Store) ClearUserDetails(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE media SET user_edited = 0, overview = NULL WHERE id = ?`, id)
	return err
}

func (s *Store) PendingProbes(ctx context.Context, limit int) ([]int64, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM media WHERE`+pendingWhere+` ORDER BY id LIMIT ?`,
		MaxProbeAttempts, time.Now().UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---------- progress ----------

func (s *Store) SaveProgress(ctx context.Context, mediaID int64, pos float64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO playback_progress (media_id, position_sec, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(media_id) DO UPDATE SET position_sec = excluded.position_sec, updated_at = excluded.updated_at`,
		mediaID, pos, time.Now().UTC())
	return err
}

func (s *Store) GetProgress(ctx context.Context, mediaID int64) (*Progress, error) {
	var p Progress
	err := s.db.QueryRowContext(ctx,
		`SELECT media_id, position_sec, updated_at FROM playback_progress WHERE media_id = ?`, mediaID).
		Scan(&p.MediaID, &p.PositionSec, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

// ---------- segment cache ----------

type CacheEntry struct {
	MediaID    int64
	VariantKey string
	Seq        int
	SizeBytes  int64
}

func (s *Store) RegisterSegment(ctx context.Context, e CacheEntry) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO segment_cache (media_id, variant_key, seq, size_bytes, created_at, last_used_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(media_id, variant_key, seq) DO UPDATE SET size_bytes = excluded.size_bytes`,
		e.MediaID, e.VariantKey, e.Seq, e.SizeBytes, now, now)
	return err
}

func (s *Store) TouchSegment(ctx context.Context, mediaID int64, variant string, seq int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE segment_cache SET last_used_at = ? WHERE media_id = ? AND variant_key = ? AND seq = ?`,
		time.Now().UTC(), mediaID, variant, seq)
	return err
}

func (s *Store) CacheSize(ctx context.Context) (int64, int64, error) {
	var total sql.NullInt64
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes),0), COUNT(*) FROM segment_cache`).
		Scan(&total, &count)
	return total.Int64, count, err
}

// EvictCandidates returns cache rows ordered oldest-used first, excluding media
// that currently has a running encoder session.
func (s *Store) EvictCandidates(ctx context.Context, exclude map[int64]bool, limit int) ([]CacheEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT media_id, variant_key, seq, size_bytes FROM segment_cache ORDER BY last_used_at ASC LIMIT ?`,
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CacheEntry
	for rows.Next() {
		var e CacheEntry
		if err := rows.Scan(&e.MediaID, &e.VariantKey, &e.Seq, &e.SizeBytes); err != nil {
			return nil, err
		}
		if exclude[e.MediaID] {
			continue
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) ExpiredSegments(ctx context.Context, before time.Time, exclude map[int64]bool) ([]CacheEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT media_id, variant_key, seq, size_bytes FROM segment_cache WHERE last_used_at < ?`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CacheEntry
	for rows.Next() {
		var e CacheEntry
		if err := rows.Scan(&e.MediaID, &e.VariantKey, &e.Seq, &e.SizeBytes); err != nil {
			return nil, err
		}
		if exclude[e.MediaID] {
			continue
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSegment(ctx context.Context, e CacheEntry) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM segment_cache WHERE media_id = ? AND variant_key = ? AND seq = ?`,
		e.MediaID, e.VariantKey, e.Seq)
	return err
}

func (s *Store) AllSegments(ctx context.Context) ([]CacheEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT media_id, variant_key, seq, size_bytes FROM segment_cache`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CacheEntry
	for rows.Next() {
		var e CacheEntry
		if err := rows.Scan(&e.MediaID, &e.VariantKey, &e.Seq, &e.SizeBytes); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) ClearSegmentCache(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM segment_cache`)
	return err
}

func (s *Store) MediaIDsForPaths(ctx context.Context, paths []string) (map[string]int64, error) {
	out := map[string]int64{}
	if len(paths) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(paths)), ",")
	args := make([]any, len(paths))
	for i, p := range paths {
		args[i] = p
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT ftp_path, id FROM media WHERE ftp_path IN (%s)`, ph), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var id int64
		if err := rows.Scan(&p, &id); err != nil {
			return nil, err
		}
		out[p] = id
	}
	return out, rows.Err()
}
