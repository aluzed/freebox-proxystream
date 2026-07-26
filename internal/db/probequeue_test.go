package db_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/aluzed/freebox-proxystream/internal/db"
)

func newStore(t *testing.T) *db.Store {
	t.Helper()
	sdb, err := db.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sdb.Close() })
	return db.NewStore(sdb)
}

// The probe backlog lives in the media table, not in a fixed-size channel. A
// library of several thousand files used to lose everything past the 256th
// slot, leaving those rows stuck at 'new' and unplayable forever.
func TestProbeBacklogSurvivesLibrariesLargerThanAnyQueue(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	lib, err := store.CreateLibrary(ctx, "/Series", "Séries", db.KindTV)
	if err != nil {
		t.Fatal(err)
	}

	const total = 3000
	for i := range total {
		path := filepath.Join("/Series", "show", "file"+itoa(i)+".mkv")
		if _, _, err := store.UpsertMediaFile(ctx, lib.ID, path, 1<<30, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}

	pending, err := store.CountPendingProbes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != total {
		t.Fatalf("pending = %d, want all %d files queued", pending, total)
	}

	// Drain in batches, exactly as the worker does.
	drained := 0
	for {
		ids, err := store.PendingProbes(ctx, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) == 0 {
			break
		}
		if len(ids) > 50 {
			t.Fatalf("batch of %d exceeds the requested limit", len(ids))
		}
		for _, id := range ids {
			if err := store.SetMediaStatus(ctx, id, db.StatusReady, ""); err != nil {
				t.Fatal(err)
			}
			drained++
		}
		if drained > total {
			t.Fatal("drained more rows than exist, the queue is not converging")
		}
	}

	if drained != total {
		t.Errorf("drained %d of %d files", drained, total)
	}
	if pending, _ := store.CountPendingProbes(ctx); pending != 0 {
		t.Errorf("%d files left behind", pending)
	}
}

// A probe that fails must leave the row out of the queue, or the worker spins
// on it forever.
func TestFailedProbeLeavesTheQueue(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	lib, _ := store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	id, _, err := store.UpsertMediaFile(ctx, lib.ID, "/Films/broken.mkv", 1<<30, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	if err := store.SetMediaStatus(ctx, id, db.StatusError, "not a video"); err != nil {
		t.Fatal(err)
	}
	pending, err := store.CountPendingProbes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Errorf("a failed probe is still queued (%d pending)", pending)
	}
}

// An interrupted run leaves rows marked 'probing'; they must be retried.
func TestInterruptedProbeIsRetried(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	lib, _ := store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	id, _, _ := store.UpsertMediaFile(ctx, lib.ID, "/Films/x.mkv", 1<<30, time.Now().UTC())
	if err := store.SetMediaStatus(ctx, id, db.StatusProbing, ""); err != nil {
		t.Fatal(err)
	}

	pending, err := store.CountPendingProbes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Errorf("pending = %d, want the interrupted row to be retried", pending)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
