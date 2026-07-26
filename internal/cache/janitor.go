// Package cache keeps the on-disk segment store and its database index in
// agreement, and enforces the TTL and size ceiling.
package cache

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/apenombre/freebox-proxystream/internal/config"
	"github.com/apenombre/freebox-proxystream/internal/db"
)

// ActiveSet reports which media currently have a running encoder, so their
// segments are never evicted from under them.
type ActiveSet interface {
	ActiveMediaIDs() map[int64]bool
}

type Janitor struct {
	cfg    *config.Config
	store  *db.Store
	active ActiveSet
	log    *slog.Logger
}

func NewJanitor(cfg *config.Config, store *db.Store, active ActiveSet, log *slog.Logger) *Janitor {
	return &Janitor{cfg: cfg, store: store, active: active, log: log}
}

func (j *Janitor) Run(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := j.Sweep(ctx); err != nil {
				j.log.Warn("cache sweep failed", "err", err)
			}
		}
	}
}

// Sweep drops expired entries, then evicts by least-recently-used until the
// total is back under the ceiling.
func (j *Janitor) Sweep(ctx context.Context) error {
	active := j.active.ActiveMediaIDs()

	expired, err := j.store.ExpiredSegments(ctx, time.Now().UTC().Add(-j.cfg.CacheTTL), active)
	if err != nil {
		return fmt.Errorf("list expired: %w", err)
	}
	var freed int64
	for _, e := range expired {
		freed += j.remove(ctx, e)
	}

	total, count, err := j.store.CacheSize(ctx)
	if err != nil {
		return fmt.Errorf("cache size: %w", err)
	}

	var evicted int
	for total > j.cfg.CacheMaxBytes {
		candidates, err := j.store.EvictCandidates(ctx, active, 500)
		if err != nil {
			return fmt.Errorf("evict candidates: %w", err)
		}
		if len(candidates) == 0 {
			j.log.Warn("cache over budget but nothing evictable",
				"total_bytes", total, "max_bytes", j.cfg.CacheMaxBytes)
			break
		}
		for _, e := range candidates {
			n := j.remove(ctx, e)
			total -= n
			freed += n
			evicted++
			if total <= j.cfg.CacheMaxBytes {
				break
			}
		}
	}

	if len(expired) > 0 || evicted > 0 {
		j.log.Info("cache swept",
			"expired", len(expired), "evicted", evicted,
			"freed_bytes", freed, "entries_before", count)
	}
	j.pruneEmptyDirs()
	return nil
}

func (j *Janitor) remove(ctx context.Context, e db.CacheEntry) int64 {
	path := filepath.Join(j.cfg.CacheDir, strconv.FormatInt(e.MediaID, 10), e.VariantKey,
		fmt.Sprintf("seg_%05d.ts", e.Seq))
	size := e.SizeBytes
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		j.log.Warn("remove segment failed", "path", path, "err", err)
		return 0
	}
	if err := j.store.DeleteSegment(ctx, e); err != nil {
		j.log.Warn("deregister segment failed", "media_id", e.MediaID, "seq", e.Seq, "err", err)
	}
	return size
}

var segmentRe = regexp.MustCompile(`^seg_(\d{5})\.ts$`)

// Reconcile brings the index and the filesystem back into agreement at
// startup, in both directions: the cache surviving a restart is most of its
// value, but only if the index survives with it.
func (j *Janitor) Reconcile(ctx context.Context) error {
	indexed, err := j.store.AllSegments(ctx)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(indexed))
	var dropped int
	for _, e := range indexed {
		path := filepath.Join(j.cfg.CacheDir, strconv.FormatInt(e.MediaID, 10), e.VariantKey,
			fmt.Sprintf("seg_%05d.ts", e.Seq))
		if info, err := os.Stat(path); err != nil || info.Size() == 0 {
			if err := j.store.DeleteSegment(ctx, e); err == nil {
				dropped++
			}
			continue
		}
		known[path] = true
	}

	var adopted, orphaned int
	err = filepath.WalkDir(j.cfg.CacheDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		// Leftover ffmpeg temp files from a hard kill are never valid.
		if filepath.Ext(name) == ".tmp" {
			os.Remove(path)
			return nil
		}
		mm := segmentRe.FindStringSubmatch(name)
		if mm == nil {
			return nil
		}
		if known[path] {
			return nil
		}

		rel, rerr := filepath.Rel(j.cfg.CacheDir, path)
		if rerr != nil {
			return nil
		}
		dir := filepath.Dir(rel)
		variantKey := filepath.Base(dir)
		mediaIDStr := filepath.Base(filepath.Dir(dir))
		mediaID, cerr := strconv.ParseInt(mediaIDStr, 10, 64)
		if cerr != nil {
			return nil
		}
		if _, gerr := j.store.GetMedia(ctx, mediaID); gerr != nil {
			os.Remove(path)
			orphaned++
			return nil
		}
		seq, _ := strconv.Atoi(mm[1])
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if rerr := j.store.RegisterSegment(ctx, db.CacheEntry{
			MediaID: mediaID, VariantKey: variantKey, Seq: seq, SizeBytes: info.Size(),
		}); rerr == nil {
			adopted++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	if dropped > 0 || adopted > 0 || orphaned > 0 {
		j.log.Info("cache reconciled", "dropped", dropped, "adopted", adopted, "orphaned", orphaned)
	}
	return nil
}

// Clear empties the cache entirely. Callers must stop encoders first.
func (j *Janitor) Clear(ctx context.Context) error {
	if err := j.store.ClearSegmentCache(ctx); err != nil {
		return err
	}
	entries, err := os.ReadDir(j.cfg.CacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(j.cfg.CacheDir, e.Name())); err != nil {
			return err
		}
	}
	j.log.Info("segment cache cleared")
	return nil
}

func (j *Janitor) pruneEmptyDirs() {
	mediaDirs, err := os.ReadDir(j.cfg.CacheDir)
	if err != nil {
		return
	}
	for _, md := range mediaDirs {
		if !md.IsDir() {
			continue
		}
		mediaPath := filepath.Join(j.cfg.CacheDir, md.Name())
		variants, err := os.ReadDir(mediaPath)
		if err != nil {
			continue
		}
		for _, vd := range variants {
			vp := filepath.Join(mediaPath, vd.Name())
			if entries, err := os.ReadDir(vp); err == nil && len(entries) == 0 {
				os.Remove(vp)
			}
		}
		if entries, err := os.ReadDir(mediaPath); err == nil && len(entries) == 0 {
			os.Remove(mediaPath)
		}
	}
}
