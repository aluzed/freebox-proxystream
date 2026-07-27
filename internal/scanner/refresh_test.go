package scanner

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/aluzed/freebox-proxystream/internal/config"
	"github.com/aluzed/freebox-proxystream/internal/db"
)

// The naming rules change under libraries that are already scanned, and the
// only way to apply them is the refresh action. This runs the real path,
// store included, on names taken from a real collection: a mistake anywhere
// between ProbedMediaIDs and SaveMetadata shows up as a stale title here.
func TestRefreshRewritesTitlesInPlace(t *testing.T) {
	ctx := context.Background()
	sdb, err := db.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	store := db.NewStore(sdb)

	lib, err := store.CreateLibrary(ctx, "/Vidéos", "Vidéos", db.KindTV)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		path    string
		title   string
		episode int
	}{
		{"/Vidéos/Detective Conan/Saison 02/Detective.Conan.Ep.043.Le.Kidnapping.De.Conan.Edogawa.DVDRip.x264.AC3.Chap.mkv",
			"Le Kidnapping De Conan Edogawa", 43},
		{"/Vidéos/Detective Conan/Saison 02/Detective.Conan.Ep.044.L.Anniversaire.De.Monsieur.Hotta.DVDRip.x264.AC3.Chap.mkv",
			"L Anniversaire De Monsieur Hotta", 44},
		{"/Vidéos/Docteur Globule - Integrale VF (1994-1998)/Docteur.Globule.E02.Toutou.Ou.Rien[UpToTen].mp4",
			"Toutou Ou Rien", 2},
		{"/Vidéos/Docteur Globule - Integrale VF (1994-1998)/Docteur.Globule.E03.Accroc.Dans.La.Jungle[UpToTen].mp4",
			"Accroc Dans La Jungle", 3},
		// Nothing but tags past the numbering: the show name stands in.
		{"/Vidéos/Le Bus Magique/Le Bus Magique S01 - Repack par Davicroquette/Le_Bus_Magique_S01E01-_Repack_par_Davicroquette.mkv",
			"Le Bus Magique", 1},
		{"/Vidéos/Le Bus Magique/Le Bus Magique S01 - Repack par Davicroquette/Le_Bus_Magique_S01E02-_Repack_par_Davicroquette.mkv",
			"Le Bus Magique", 2},
	}

	ids := make([]int64, len(cases))
	for i, tc := range cases {
		id, _, err := store.UpsertMediaFile(ctx, lib.ID, tc.path, 1<<30, time.Unix(0, 0).UTC())
		if err != nil {
			t.Fatal(err)
		}
		// Only probed rows are refreshed, and probing is what sets probed_at.
		if err := store.SaveProbe(ctx, id, db.ProbeResult{DurationSec: 1200}); err != nil {
			t.Fatal(err)
		}
		if err := store.SetMediaStatus(ctx, id, db.StatusReady, ""); err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}

	sc := New(&config.Config{}, store, nil, nil, slog.New(slog.DiscardHandler))
	if err := sc.refreshMetadata(ctx, lib.ID); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	for i, tc := range cases {
		m, err := store.GetMedia(ctx, ids[i])
		if err != nil {
			t.Fatal(err)
		}
		if got := m.DisplayTitle(); got != tc.title {
			t.Errorf("%s\n  title = %q, want %q", tc.path, got, tc.title)
		}
		if m.Episode == nil || *m.Episode != tc.episode {
			t.Errorf("%s\n  episode = %v, want %d", tc.path, m.Episode, tc.episode)
		}
	}
}

// A title the user typed is theirs; a refresh must not take it back.
func TestRefreshLeavesEditedTitlesAlone(t *testing.T) {
	ctx := context.Background()
	sdb, err := db.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	store := db.NewStore(sdb)

	lib, _ := store.CreateLibrary(ctx, "/Vidéos", "Vidéos", db.KindTV)
	id, _, err := store.UpsertMediaFile(ctx, lib.ID,
		"/Vidéos/Conan/Saison 02/Detective.Conan.Ep.043.Le.Kidnapping.mkv", 1<<30, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	store.SaveProbe(ctx, id, db.ProbeResult{DurationSec: 1200})
	store.SetMediaStatus(ctx, id, db.StatusReady, "")
	if err := store.SaveUserDetails(ctx, id, "Mon titre à moi", ""); err != nil {
		t.Fatal(err)
	}

	sc := New(&config.Config{}, store, nil, nil, slog.New(slog.DiscardHandler))
	if err := sc.refreshMetadata(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}

	m, _ := store.GetMedia(ctx, id)
	if m.DisplayTitle() != "Mon titre à moi" {
		t.Errorf("refresh overwrote a hand-written title with %q", m.DisplayTitle())
	}
}
