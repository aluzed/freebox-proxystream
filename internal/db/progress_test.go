package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/aluzed/freebox-proxystream/internal/db"
)

func TestProgressByLibraryCountsEachSeparately(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	films, _ := store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	series, _ := store.CreateLibrary(ctx, "/Series", "Séries", db.KindTV)

	// Films: 2 ready, 1 still queued.
	for i, st := range []string{db.StatusReady, db.StatusReady, db.StatusNew} {
		id, _, _ := store.UpsertMediaFile(ctx, films.ID,
			"/Films/f"+string(rune('a'+i))+".mkv", 1<<30, time.Now().UTC())
		store.SetMediaStatus(ctx, id, st, "")
	}
	// Séries: 1 ready, 1 permanently broken, 1 backing off.
	idReady, _, _ := store.UpsertMediaFile(ctx, series.ID, "/Series/a.mkv", 1<<30, time.Now().UTC())
	store.SetMediaStatus(ctx, idReady, db.StatusReady, "")
	idBroken, _, _ := store.UpsertMediaFile(ctx, series.ID, "/Series/b.mkv", 1<<30, time.Now().UTC())
	store.RecordProbeFailure(ctx, idBroken, "no video stream", true)
	idFlaky, _, _ := store.UpsertMediaFile(ctx, series.ID, "/Series/c.mkv", 1<<30, time.Now().UTC())
	store.RecordProbeFailure(ctx, idFlaky, "timed out", false)

	progress, err := store.ProgressByLibrary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) != 2 {
		t.Fatalf("got %d libraries, want 2", len(progress))
	}

	by := map[string]db.LibraryProgress{}
	for _, p := range progress {
		by[p.Name] = p
	}

	f := by["Films"]
	if f.Total != 3 || f.Ready != 2 || f.Pending != 1 || f.Failed != 0 {
		t.Errorf("Films = %+v, want total 3 / ready 2 / pending 1 / failed 0", f)
	}

	s := by["Séries"]
	if s.Total != 3 || s.Ready != 1 || s.Failed != 1 || s.Retryable != 1 {
		t.Errorf("Séries = %+v, want total 3 / ready 1 / failed 1 / retryable 1", s)
	}
	// The two libraries must not bleed into each other.
	if f.Failed != 0 || f.Retryable != 0 {
		t.Errorf("Séries' failures leaked into Films: %+v", f)
	}
}

// An empty library still appears, otherwise a freshly added one vanishes from
// the panel until its first file is found.
func TestEmptyLibraryStillReported(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	if _, err := store.CreateLibrary(ctx, "/Empty", "Vide", db.KindMovie); err != nil {
		t.Fatal(err)
	}
	progress, err := store.ProgressByLibrary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) != 1 || progress[0].Total != 0 {
		t.Errorf("got %+v, want one library with no media", progress)
	}
}
