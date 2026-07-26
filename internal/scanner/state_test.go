package scanner

import (
	"log/slog"
	"testing"
)

func newTestScanner(t *testing.T) *Scanner {
	t.Helper()
	return &Scanner{
		log:  slog.New(slog.DiscardHandler),
		jobs: make(chan job, queueSize),
	}
}

// The progress fraction is derived, so it can never contradict itself. It used
// to be an accumulated counter, which produced "152/0" whenever probes outlived
// the walk that queued them.
func TestProgressTotalIsAlwaysAtLeastTheDoneCount(t *testing.T) {
	s := newTestScanner(t)
	s.update(func(st *State) { st.Probed = 152; st.Queued = 103 })

	got := s.State()
	if got.ToProbe != 255 {
		t.Errorf("ToProbe = %d, want 255 (152 done + 103 remaining)", got.ToProbe)
	}
	if got.ToProbe < got.Probed {
		t.Errorf("nonsensical progress %d/%d", got.Probed, got.ToProbe)
	}
}

func TestProgressStaysCoherentAsTheQueueDrains(t *testing.T) {
	s := newTestScanner(t)
	for done := range 11 {
		s.update(func(st *State) { st.Probed = done; st.Queued = 10 - done })
		st := s.State()
		if st.ToProbe != 10 {
			t.Errorf("after %d done: total = %d, want a stable 10", done, st.ToProbe)
		}
		if st.Probed > st.ToProbe {
			t.Errorf("after %d done: %d/%d is impossible", done, st.Probed, st.ToProbe)
		}
	}
}

func TestEmptyQueueReportsNothingPending(t *testing.T) {
	s := newTestScanner(t)
	st := s.State()
	if st.Running || st.Queued != 0 || st.ToProbe != 0 {
		t.Errorf("idle scanner reports %+v", st)
	}
}

// Scan requests are still an in-memory channel, and a full one must be refused
// rather than block an HTTP handler.
func TestScanQueueRejectsWhenFull(t *testing.T) {
	s := newTestScanner(t)
	for i := range queueSize {
		if err := s.EnqueueScan(int64(i)); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if err := s.EnqueueScan(9999); err == nil {
		t.Error("a full queue accepted another scan")
	}
}
