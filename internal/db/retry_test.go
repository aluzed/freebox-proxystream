package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/aluzed/freebox-proxystream/internal/db"
)

func newMedia(t *testing.T, store *db.Store, path string) int64 {
	t.Helper()
	ctx := context.Background()
	lib, err := store.CreateLibrary(ctx, "/Films", "Films"+path, db.KindMovie)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := store.UpsertMediaFile(ctx, lib.ID, path, 1<<30, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A file that is not a video is parked for good: retrying costs an FTP read and
// cannot change the answer.
func TestPermanentFailureIsNeverRetried(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	id := newMedia(t, store, "/Films/not-a-video.mkv")

	if err := store.RecordProbeFailure(ctx, id, "no video stream in this file", true); err != nil {
		t.Fatal(err)
	}

	pending, err := store.CountPendingProbes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Errorf("a permanently failed file is still queued (%d pending)", pending)
	}

	_, permanent, next, err := store.ProbeRetryInfo(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !permanent {
		t.Error("the failure was not recorded as permanent")
	}
	if next != nil {
		t.Error("a permanent failure should not schedule another attempt")
	}
}

// A network hiccup is worth another go, after a backoff.
func TestTransientFailureBacksOffThenGivesUp(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	id := newMedia(t, store, "/Films/flaky.mkv")

	var lastBackoff time.Duration
	for attempt := 1; attempt <= db.MaxProbeAttempts; attempt++ {
		if err := store.RecordProbeFailure(ctx, id, "ffprobe timed out", false); err != nil {
			t.Fatal(err)
		}

		attempts, permanent, next, err := store.ProbeRetryInfo(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if permanent {
			t.Fatal("a transient failure was marked permanent")
		}
		if attempts != attempt {
			t.Fatalf("attempts = %d, want %d", attempts, attempt)
		}
		if next == nil {
			t.Fatal("no next attempt scheduled")
		}

		// Backoff grows, so a persistent outage is not hammered.
		backoff := time.Until(*next)
		if attempt > 1 && backoff <= lastBackoff {
			t.Errorf("attempt %d backs off %s, not more than the previous %s",
				attempt, backoff.Round(time.Second), lastBackoff.Round(time.Second))
		}
		lastBackoff = backoff

		// While waiting, it is not in the queue.
		pending, _ := store.CountPendingProbes(ctx)
		if pending != 0 {
			t.Errorf("attempt %d: file is queued during its backoff", attempt)
		}
		retryable, _ := store.CountRetryable(ctx)
		if attempt < db.MaxProbeAttempts && retryable != 1 {
			t.Errorf("attempt %d: retryable = %d, want 1", attempt, retryable)
		}
	}

	// After the last attempt it is given up on, even though it never became
	// a permanent failure.
	retryable, _ := store.CountRetryable(ctx)
	if retryable != 0 {
		t.Errorf("still retryable after %d attempts", db.MaxProbeAttempts)
	}
	pending, _ := store.CountPendingProbes(ctx)
	if pending != 0 {
		t.Errorf("exhausted file is back in the queue (%d pending)", pending)
	}
}

// Once the backoff expires the file returns to the queue on its own.
func TestFileReturnsToTheQueueWhenTheBackoffExpires(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	id := newMedia(t, store, "/Films/comes-back.mkv")

	if err := store.RecordProbeFailure(ctx, id, "connection reset", false); err != nil {
		t.Fatal(err)
	}
	if pending, _ := store.CountPendingProbes(ctx); pending != 0 {
		t.Fatal("queued before its backoff expired")
	}

	// Move the schedule into the past, as the clock would.
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE media SET next_attempt_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Minute), id); err != nil {
		t.Fatal(err)
	}

	pending, err := store.CountPendingProbes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Errorf("pending = %d, want the file back in the queue", pending)
	}
}

// The manual retry button clears the verdict, including a permanent one.
func TestManualRetryClearsEverything(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	id := newMedia(t, store, "/Films/manual.mkv")

	store.RecordProbeFailure(ctx, id, "no video stream", true)
	if err := store.ResetProbeAttempts(ctx, id); err != nil {
		t.Fatal(err)
	}

	attempts, permanent, next, err := store.ProbeRetryInfo(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || permanent || next != nil {
		t.Errorf("after a manual retry: attempts=%d permanent=%v next=%v", attempts, permanent, next)
	}
	if pending, _ := store.CountPendingProbes(ctx); pending != 1 {
		t.Error("the file was not put back in the queue")
	}
}

// A file whose bytes changed deserves a clean slate, even if the old one was
// judged permanently broken: it may well be a different file now.
func TestChangedFileForgetsItsPreviousVerdict(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	lib, _ := store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	id, _, err := store.UpsertMediaFile(ctx, lib.ID, "/Films/replaced.mkv", 1<<30, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	store.RecordProbeFailure(ctx, id, "invalid data found", true)

	// Rescan sees a different size.
	sameID, needsProbe, err := store.UpsertMediaFile(ctx, lib.ID, "/Films/replaced.mkv",
		2<<30, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if sameID != id {
		t.Fatalf("the row was duplicated: %d then %d", id, sameID)
	}
	if !needsProbe {
		t.Error("a changed file was not marked for probing")
	}

	attempts, permanent, _, err := store.ProbeRetryInfo(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if permanent || attempts != 0 {
		t.Errorf("the old verdict survived a file change: attempts=%d permanent=%v",
			attempts, permanent)
	}
}

// An unchanged file that already succeeded must not be re-probed on every scan.
func TestUnchangedReadyFileIsNotRequeued(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	lib, _ := store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	mtime := time.Now().UTC()
	id, _, _ := store.UpsertMediaFile(ctx, lib.ID, "/Films/stable.mkv", 1<<30, mtime)
	store.SetMediaStatus(ctx, id, db.StatusReady, "")

	_, needsProbe, err := store.UpsertMediaFile(ctx, lib.ID, "/Films/stable.mkv", 1<<30, mtime)
	if err != nil {
		t.Fatal(err)
	}
	if needsProbe {
		t.Error("an unchanged, already probed file was queued again")
	}
	if pending, _ := store.CountPendingProbes(ctx); pending != 0 {
		t.Error("rescanning an unchanged library is not a no-op")
	}
}

// Rows marked 'error' outside the probe path (a file that vanished, a
// recovered panic) must not re-enter the queue. They would fail identically,
// be re-queued, and spin the worker forever.
func TestErrorsFromOtherPathsDoNotLoop(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	id := newMedia(t, store, "/Films/vanished.mkv")

	if err := store.SetMediaStatus(ctx, id, db.StatusError, "file disappeared"); err != nil {
		t.Fatal(err)
	}

	pending, err := store.CountPendingProbes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Errorf("a row failed outside the probe path is queued (%d pending): "+
			"the worker would loop on it", pending)
	}
	// It is still recoverable by hand.
	if err := store.ResetProbeAttempts(ctx, id); err != nil {
		t.Fatal(err)
	}
	if pending, _ := store.CountPendingProbes(ctx); pending != 1 {
		t.Error("a manual retry could not revive it")
	}
}
