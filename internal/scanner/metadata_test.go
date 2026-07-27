package scanner

import "testing"

func deref(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

func TestParseFilename(t *testing.T) {
	cases := []struct {
		path    string
		title   string
		year    int
		season  int
		episode int
	}{
		{"/Films/The Matrix (1999) 1080p BluRay x264.mkv", "The Matrix", 1999, -1, -1},
		{"/Films/Le.Fabuleux.Destin.d.Amelie.Poulain.2001.MULTi.1080p.mkv",
			"Le Fabuleux Destin d Amelie Poulain", 2001, -1, -1},
		{"/Series/Breaking Bad/S03/Breaking.Bad.S03E07.1080p.WEB-DL.mkv",
			"Breaking Bad", -1, 3, 7},
		{"/Series/Kaamelott/Kaamelott.1x12.FRENCH.mkv", "Kaamelott", -1, 1, 12},
		{"/Films/Dune Part Two 2024 2160p HDR.mkv", "Dune Part Two", 2024, -1, -1},
		{"/Films/no_metadata_at_all.mkv", "no metadata at all", -1, -1, -1},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := ParseFilename(tc.path)
			if got.Title != tc.title {
				t.Errorf("title = %q, want %q", got.Title, tc.title)
			}
			if deref(got.Year) != tc.year {
				t.Errorf("year = %d, want %d", deref(got.Year), tc.year)
			}
			if deref(got.Season) != tc.season {
				t.Errorf("season = %d, want %d", deref(got.Season), tc.season)
			}
			if deref(got.Episode) != tc.episode {
				t.Errorf("episode = %d, want %d", deref(got.Episode), tc.episode)
			}
		})
	}
}

func TestIsVideoFile(t *testing.T) {
	for _, name := range []string{"a.mkv", "b.MP4", "c.avi", "d.m4v", "e.mov", "f.ts", "g.wmv", "h.webm"} {
		if !isVideoFile(name) {
			t.Errorf("%s should be recognised as video", name)
		}
	}
	for _, name := range []string{"a.srt", "b.nfo", "c.jpg", "d.txt", "e"} {
		if isVideoFile(name) {
			t.Errorf("%s should not be recognised as video", name)
		}
	}
}

// go-parse-torrent-name panics on some inputs (it slices a name with a start
// index that can exceed its end index). One oddly named file must not take the
// server down, so the panic is absorbed and our own parsing takes over.
func TestParseSurvivesLibraryPanics(t *testing.T) {
	// Every one of these makes the upstream library panic.
	for _, name := range []string{
		"/Films/2011- Aladdin1x01.mkv",
		"/Films/-_3D.mkv",
		"/Films/2011 AladdinSeason 11x01.mkv",
		"/Films/H264-720pH264x2641080p1080p.mkv",
		"/Films/x264Episode 12011x264 x264.mkv",
		"/Films/2011-BluRayplumesEpisode 1Episode 1MULTi540p.mkv",
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ParseFilename(%q) panicked: %v", name, r)
				}
			}()
			got := ParseFilename(name)
			// It must still produce something usable rather than an empty shell.
			if got.Title == "" {
				t.Errorf("ParseFilename(%q) produced no title", name)
			}
		}()
	}
}

// Once the library bails, our own regexes still have to do their job on a
// normally separated name. "Aladdin1x01" with no separator stays ambiguous and
// is deliberately not guessed at.
func TestFallbackParsingAfterLibraryFailure(t *testing.T) {
	got := ParseFilename("/Films/2011 - Aladdin 1x01.mkv")
	if deref(got.Season) != 1 || deref(got.Episode) != 1 {
		t.Errorf("season/episode = %d/%d, want 1/1", deref(got.Season), deref(got.Episode))
	}
	if deref(got.Year) != 2011 {
		t.Errorf("year = %d, want 2011", deref(got.Year))
	}
}

// A file is recognised as an episode from its own name, not from the label
// someone ticked on the library when they created it.
func TestEpisodeDetectionFollowsTheFileNotTheLibrary(t *testing.T) {
	cases := []struct {
		path   string
		asTV   bool
		reason string
	}{
		{"/Vidéos/Aladdin/Saison 1/Aladdin_-_1x01_-_Les_1001_plumes.avi", true,
			"an episode number means a series, whatever the library says"},
		{"/Vidéos/Pokemon.S15E36.FRENCH.mkv", true, "S15E36 is unambiguous"},
		{"/Vidéos/Aladdin (1992) 1080p.mkv", false, "no episode number, this is the film"},
		{"/Vidéos/Dune Part Two 2024.mkv", false, "a film"},
	}

	for _, tc := range cases {
		p := ParseFilename(tc.path)
		gotTV := p.Season != nil && p.Episode != nil
		if gotTV != tc.asTV {
			t.Errorf("%s: routed as TV=%v, want %v (%s)", tc.path, gotTV, tc.asTV, tc.reason)
		}
	}
}

// Real names from the reference library. The parser has to cope with several
// naming schemes living side by side in one collection.
func TestShowTitleComesFromTheFolder(t *testing.T) {
	cases := []struct {
		path string
		show string
	}{
		// The filename repeats the show, badly; the folder says it once.
		{"/V/Nicky Larson/001 - Nicky Larson - Un cocktail dangereux.avi", "Nicky Larson"},
		{"/V/Funky Cops/Funky Cops Saison 1 EP 01 - up by TakeASuit.mp4", "Funky Cops"},
		// A season folder: the show is one level up.
		{"/V/Detective Conan/Saison 02/Detective Conan 043.avi", "Detective Conan"},
		{"/V/Aladdin/Saison 1/Aladdin_-_1x01_-_Les_1001_plumes.avi", "Aladdin"},
		// Release noise on the folder itself.
		{"/V/Recess.S03.FRENCH.WEB.H264-CUSThOMe/Recess.S03E01.mkv", "Recess"},
		{"/V/Jeanne et Serge.Intégrale/JS 01.avi", "Jeanne et Serge"},
	}
	for _, tc := range cases {
		if got := ShowTitleFromPath(tc.path); got != tc.show {
			t.Errorf("ShowTitleFromPath(%q) = %q, want %q", tc.path, got, tc.show)
		}
	}
}

// Episode numbers hide in several shapes; missing them means no episode title
// and the wrong order in a season list.
func TestEpisodeNumbersInEveryShape(t *testing.T) {
	cases := []struct {
		path    string
		season  int
		episode int
	}{
		{"/V/Aladdin/Saison 1/Aladdin_-_1x01_-_x.avi", 1, 1},
		{"/V/Show/Show.S03E07.mkv", 3, 7},
		{"/V/Funky Cops/Funky Cops Saison 1 EP 01 - up.mp4", 1, 1},
		{"/V/Nicky Larson/001 - Nicky Larson - Titre.avi", -1, 1},
		// The season comes from the folder even when the file omits it.
		{"/V/Conan/Saison 02/Conan Episode 43.avi", 2, 43},
		// Releases separate "Ep" from its number with a dot, not a space.
		// Missing this left whole seasons unnumbered, so shown as cards.
		{"/V/Conan/Saison 02/Detective.Conan.Ep.043.Le.Kidnapping.DVDRip.mkv", 2, 43},
		{"/V/Conan/Saison.02/Detective.Conan.Ep.044.L.Anniversaire.mkv", 2, 44},
	}
	for _, tc := range cases {
		p := ParseFilename(tc.path)
		if deref(p.Season) != tc.season || deref(p.Episode) != tc.episode {
			t.Errorf("%s: S=%d E=%d, want S=%d E=%d",
				tc.path, deref(p.Season), deref(p.Episode), tc.season, tc.episode)
		}
	}
}

// A film must not be dragged into the series path by these looser rules.
func TestFilmsAreStillFilms(t *testing.T) {
	for _, path := range []string{
		"/V/Films/Dune Part Two 2024 2160p.mkv",
		"/V/Films/The Matrix (1999) 1080p BluRay.mkv",
	} {
		p := ParseFilename(path)
		if p.Episode != nil {
			t.Errorf("%s: parsed an episode number %d", path, *p.Episode)
		}
	}
}

// A season list where every row reads "Nicky Larson" is useless: when the
// filename names the episode, that name is what the row must show. When it
// only numbers it, the show name stands in.
func TestEpisodeTitleComesFromTheFilename(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/V/Nicky Larson/001 - Nicky Larson - Un cocktail dangereux.avi", "Un cocktail dangereux"},
		{"/V/Aladdin/Saison 1/Aladdin_-_1x01_-_Les_1001_plumes.avi", "Les 1001 plumes"},
		{"/V/Kaamelott/Kaamelott S01E23 - Les Tartes aux myrtilles.mkv", "Les Tartes aux myrtilles"},
		// Numbering and nothing else: there is no episode title to find.
		{"/V/Recess.S03.FRENCH.WEB.H264-CUSThOMe/Recess.S03E01.mkv", ""},
		{"/V/The Office/Season 2/The.Office.S02E01.1080p.WEB.x264-GRP.mkv", ""},
		{"/V/Detective Conan/Saison 02/Detective Conan 043.avi", ""},
		{"/V/Jeanne et Serge.Intégrale/JS 01.avi", ""},
		// The releaser's signature is not an episode title, in either form.
		{"/V/Funky Cops/Funky Cops Saison 1 EP 01 - up by TakeASuit.mp4", ""},
		{"/V/Docteur Globule/Docteur.Globule.E02.Toutou.Ou.Rien[UpToTen].mp4", "Toutou Ou Rien"},
		// A dot between "Ep" and the number, which is how releases write it.
		{"/V/Detective Conan/Saison 02/Detective.Conan.Ep.043.Le.Kidnapping.De.Conan.Edogawa.DVDRip.x264.AC3.Chap.mkv",
			"Le Kidnapping De Conan Edogawa"},
		// Past the numbering there are only tags, so there is no title here.
		{"/V/Le Bus Magique/S01 - Repack par Davicroquette/Le_Bus_Magique_S01E01-_Repack_par_Davicroquette.mkv", ""},
		{"/V/la vie de famille/saison 4/La Vie De Famille S04E01.VF.mp4", ""},
		{"/V/Detective Conan/Saison 18/Detective.Conan.S18E01.FANSUB.VOSTFR.720p.HDTV.x264-Wawacity.uno.mp4", ""},
		// "vo" and "vf" are tags as whole words only, never inside one.
		{"/V/Show/Show.S01E04 - Le Voyage vers Vfoo.mkv", "Le Voyage vers Vfoo"},
		// A release group signing the end of the name.
		{"/V/Timon/Timon & Pumbaa S01E12 - Piraterie carabinée MTL666.mkv", "Piraterie carabinée"},
	}
	for _, tc := range cases {
		show := ShowTitleFromPath(tc.path)
		if got := episodeTitle(tc.path, show); got != tc.want {
			t.Errorf("episodeTitle(%q, %q) = %q, want %q", tc.path, show, got, tc.want)
		}
	}
}

// Folder names in a real collection carry the whole release string. Nobody
// browsing their own library wants to read it.
func TestFolderLabelStripsReleaseNoise(t *testing.T) {
	cases := []struct {
		name   string
		label  string
		season int
	}{
		{"Pokémon.S04.FRENCH.540p.WEB.H264-BTT", "Pokémon", 4},
		{"Recess.S03.FRENCH.WEB.H264-CUSThOMe", "Recess", 3},
		{"Detective Conan", "Detective Conan", -1},
		{"Saison 02", "Saison 02", 2},
		{"Jeanne et Serge.Intégrale", "Jeanne et Serge", -1},
		{"Funky Cops", "Funky Cops", -1},
	}
	for _, tc := range cases {
		label, season := FolderLabel(tc.name)
		if label != tc.label {
			t.Errorf("FolderLabel(%q) label = %q, want %q", tc.name, label, tc.label)
		}
		if deref(season) != tc.season {
			t.Errorf("FolderLabel(%q) season = %d, want %d", tc.name, deref(season), tc.season)
		}
	}
}

// Cleaning must never leave a tile with no name at all.
func TestFolderLabelNeverEmpty(t *testing.T) {
	for _, name := range []string{"1080p", "FRENCH", "x264", "S01"} {
		if label, _ := FolderLabel(name); label == "" {
			t.Errorf("FolderLabel(%q) returned an empty label", name)
		}
	}
}
