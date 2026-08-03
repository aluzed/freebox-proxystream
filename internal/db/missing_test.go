package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/aluzed/freebox-proxystream/internal/db"
)

func seed(t *testing.T, store *db.Store, libraryID int64, paths ...string) map[string]int64 {
	t.Helper()
	ids := map[string]int64{}
	for _, p := range paths {
		id, _, err := store.UpsertMediaFile(context.Background(), libraryID, p, 1<<30, time.Now().UTC())
		if err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
		ids[p] = id
	}
	return ids
}

func remaining(t *testing.T, store *db.Store, libraryID int64) []string {
	t.Helper()
	items, _, err := store.ListMedia(context.Background(), db.MediaFilter{LibraryID: libraryID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(items))
	for _, m := range items {
		out = append(out, m.FTPPath)
	}
	return out
}

// Deleting from the Freebox has to be enough: a rescan that only flagged the
// row left the file, and the folder holding it, sitting in the library.
func TestRescanDropsWhatIsNoLongerOnTheServer(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	lib, _ := store.CreateLibrary(ctx, "/Series", "Séries", db.KindTV)
	seed(t, store, lib.ID,
		"/Series/Show/S01/e01.mkv",
		"/Series/Show/S01/e02.mkv",
		"/Series/Gone/e01.mkv",
		"/Series/Gone/e02.mkv")

	removed, err := store.DeleteMissing(ctx, lib.ID, []string{
		"/Series/Show/S01/e01.mkv",
		"/Series/Show/S01/e02.mkv",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed %d rows, want the 2 under /Series/Gone", len(removed))
	}

	got := remaining(t, store, lib.ID)
	if len(got) != 2 {
		t.Fatalf("library still holds %v", got)
	}
	// The folder is only the shared prefix of its media, so an empty one is
	// gone by construction.
	for _, p := range got {
		if p == "/Series/Gone/e01.mkv" || p == "/Series/Gone/e02.mkv" {
			t.Errorf("%s survived the rescan", p)
		}
	}
}

// A directory the walk could not list says nothing about its contents. Taking
// that silence for a deletion would wipe a library, and its playback progress
// with it, on one FTP hiccup.
func TestUnreadableDirectoryIsNotTakenForADeletion(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	lib, _ := store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	seed(t, store, lib.ID,
		"/Films/ok.mkv",
		"/Films/Offline/a.mkv",
		"/Films/Offline/b.mkv",
		"/Films/Deleted/c.mkv")

	removed, err := store.DeleteMissing(ctx, lib.ID,
		[]string{"/Films/ok.mkv"}, []string{"/Films/Offline"})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Fatalf("removed %d rows, want only /Films/Deleted/c.mkv", len(removed))
	}

	got := remaining(t, store, lib.ID)
	if len(got) != 3 {
		t.Fatalf("library holds %v, want ok.mkv plus both files under Offline", got)
	}
}

// Underscores are ordinary characters in release names; used raw as a LIKE
// prefix they would match any character and shield unrelated folders.
func TestUnreadableGuardDoesNotOverreachOnWildcards(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	lib, _ := store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	seed(t, store, lib.ID, "/Films/Some_Dir/a.mkv", "/Films/SomeXDir/b.mkv")

	removed, err := store.DeleteMissing(ctx, lib.ID, nil, []string{"/Films/Some_Dir"})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Fatalf("removed %d rows, want only /Films/SomeXDir/b.mkv", len(removed))
	}
	if got := remaining(t, store, lib.ID); len(got) != 1 || got[0] != "/Films/Some_Dir/a.mkv" {
		t.Errorf("library holds %v, want just the protected directory", got)
	}
}

// Progress rows hang off the media through ON DELETE CASCADE, and a leftover
// one would re-attach to whatever id SQLite hands out next.
func TestRemovedMediaTakesItsProgressWithIt(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	lib, _ := store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	ids := seed(t, store, lib.ID, "/Films/gone.mkv")
	id := ids["/Films/gone.mkv"]
	if err := store.SaveProgress(ctx, id, 900, 5400); err != nil {
		t.Fatal(err)
	}

	if _, err := store.DeleteMissing(ctx, lib.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if p, err := store.GetProgress(ctx, id); err == nil && p != nil {
		t.Errorf("progress %+v outlived its media", p)
	}
}
