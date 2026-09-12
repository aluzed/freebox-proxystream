package scanner

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aluzed/freebox-proxystream/internal/config"
	"github.com/aluzed/freebox-proxystream/internal/db"
	"github.com/aluzed/freebox-proxystream/internal/ftpclient"
	"github.com/aluzed/freebox-proxystream/internal/testftp"
	"github.com/aluzed/freebox-proxystream/internal/thumbnail"
)

func scanEnv(t *testing.T) (*Scanner, string) {
	t.Helper()
	addr, root := testftp.Start(t)
	tmp := t.TempDir()
	sdb, err := db.Open(filepath.Join(tmp, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sdb.Close() })
	log := slog.New(slog.DiscardHandler)
	client := ftpclient.New(ftpclient.Config{Host: addr, User: testftp.User, Pass: testftp.Pass}, log)
	t.Cleanup(func() { client.Close() })
	cfg := &config.Config{CacheDir: filepath.Join(tmp, "segments"), ThumbDir: filepath.Join(tmp, "thumbs"), SubtitleDir: filepath.Join(tmp, "subs")}
	return New(cfg, db.NewStore(sdb), client, thumbnail.New(cfg), log), root
}

func scanFile(t *testing.T, root, name string) {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(minFileBytes); err != nil {
		t.Fatal(err)
	}
}

func TestScanRemovesDeletedFolderAndDerivedFiles(t *testing.T) {
	s, root := scanEnv(t)
	ctx := context.Background()
	lib, err := s.store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	if err != nil {
		t.Fatal(err)
	}
	scanFile(t, root, "Films/Gone/movie.mkv")
	scanFile(t, root, "Films/Keep/movie.mkv")
	if err := s.scanLibrary(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	items, _, err := s.store.ListMedia(ctx, db.MediaFilter{LibraryID: lib.ID})
	if err != nil {
		t.Fatal(err)
	}
	var gone int64
	for _, m := range items {
		if m.FTPPath == "/Films/Gone/movie.mkv" {
			gone = m.ID
		}
	}
	if gone == 0 {
		t.Fatal("deleted fixture was not indexed")
	}
	poster := s.thumbs.Path(gone)
	if err := os.MkdirAll(filepath.Dir(poster), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(poster, []byte("poster"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "Films/Gone")); err != nil {
		t.Fatal(err)
	}
	if err := s.scanLibrary(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.GetMedia(ctx, gone); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("deleted media survived: %v", err)
	}
	if _, err := os.Stat(poster); !os.IsNotExist(err) {
		t.Fatalf("poster survived: %v", err)
	}
	dirs, _, err := s.store.BrowseLibrary(ctx, lib, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || dirs[0].Name != "Keep" {
		t.Fatalf("folders: %+v", dirs)
	}
}

func TestScanOfDeletedLibraryRootRemovesStaleMedia(t *testing.T) {
	s, root := scanEnv(t)
	ctx := context.Background()
	lib, err := s.store.CreateLibrary(ctx, "/Films/Gone", "Gone", db.KindMovie)
	if err != nil {
		t.Fatal(err)
	}
	scanFile(t, root, "Films/Gone/movie.mkv")
	if err := s.scanLibrary(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "Films/Gone")); err != nil {
		t.Fatal(err)
	}
	if err := s.scanLibrary(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	_, count, err := s.store.ListMedia(ctx, db.MediaFilter{LibraryID: lib.ID})
	if err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	// A configured source remains available for reappearance or manual removal.
	if _, err := s.store.GetLibrary(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
}

func TestScanReportsCleanupFailure(t *testing.T) {
	s, root := scanEnv(t)
	ctx := context.Background()
	lib, err := s.store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Films"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.store.UpsertMediaFile(ctx, lib.ID, "/Films/gone.mkv", minFileBytes, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, err = s.store.DB().Exec(`CREATE TRIGGER reject_delete BEFORE DELETE ON media BEGIN SELECT RAISE(ABORT, 'cleanup failed'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.scanLibrary(ctx, lib.ID); err == nil {
		t.Fatal("failed cleanup reported success")
	}
	lib, err = s.store.GetLibrary(ctx, lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lib.LastScan != nil {
		t.Fatal("failed cleanup recorded as successful scan")
	}
}

// Directory access errors and interrupted listings must not erase progress.
type listFunc func(context.Context, string) ([]ftpclient.Entry, error)

func (f listFunc) List(ctx context.Context, dir string) ([]ftpclient.Entry, error) {
	return f(ctx, dir)
}

func TestScanPreservesUnreadableDirectories(t *testing.T) {
	for _, mode := range []string{"root denied", "parent denied", "child denied", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := scanEnv(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			lib, err := s.store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
			if err != nil {
				t.Fatal(err)
			}
			id, _, err := s.store.UpsertMediaFile(ctx, lib.ID, "/Films/Protected/movie.mkv", minFileBytes, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			s.ftp = listFunc(func(_ context.Context, dir string) ([]ftpclient.Entry, error) {
				switch {
				case mode == "cancelled":
					cancel()
					return nil, context.Canceled
				case dir == "/" && mode != "parent denied":
					return []ftpclient.Entry{{Path: "/Films", Name: "Films", IsDir: true}}, nil
				case dir == "/Films" && mode == "child denied":
					return []ftpclient.Entry{{Path: "/Films/Protected", Name: "Protected", IsDir: true}}, nil
				default:
					return nil, errors.New("550 Permission denied")
				}
			})
			if err := s.scanLibrary(ctx, lib.ID); err == nil {
				t.Fatal("incomplete scan reported success")
			}
			if _, err := s.store.GetMedia(context.Background(), id); err != nil {
				t.Fatalf("protected media removed: %v", err)
			}
		})
	}
}

func TestQueuedScanPrecedesProbeBacklog(t *testing.T) {
	s, root := scanEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lib, err := s.store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Films"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.store.UpsertMediaFile(ctx, lib.ID, "/Films/gone.mkv", minFileBytes, time.Now()); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "probe-started")
	script := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s.cfg.FFprobeBin = script
	if err := s.EnqueueScan(lib.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("queued scan did not finish")
		case <-tick.C:
			lib, err := s.store.GetLibrary(ctx, lib.ID)
			if err != nil {
				t.Fatal(err)
			}
			if lib.LastScan == nil {
				continue
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("probed deleted file before scanning: %v", err)
			}
			return
		}
	}
}
