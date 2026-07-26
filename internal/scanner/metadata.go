package scanner

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/apenombre/freebox-proxystream/internal/db"
	ptn "github.com/middelink/go-parse-torrent-name"
)

// Parsed is what the filename alone tells us.
type Parsed struct {
	Title   string
	Year    *int
	Season  *int
	Episode *int
}

var (
	// go-parse-torrent-name misses a few common European release patterns.
	seasonEpisodeRe = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])s(\d{1,2})[\s._-]*e(\d{1,3})(?:[^0-9]|$)`)
	altEpisodeRe    = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(\d{1,2})x(\d{1,3})(?:[^0-9]|$)`)
	// "Funky Cops Saison 1 EP 01", "Saison 02 Episode 3".
	wordSeasonEpRe = regexp.MustCompile(`(?i)sai?son\s*(\d{1,2}).{0,12}?(?:ep|épisode|episode)\s*(\d{1,3})`)
	// "Saison 02" or "Season 2" on its own, usually the folder name.
	wordSeasonRe = regexp.MustCompile(`(?i)(?:^|[^a-z])sai?son\s*(\d{1,2})(?:[^0-9]|$)`)
	// "EP 01" / "Episode 12" with no season stated.
	wordEpisodeRe = regexp.MustCompile(`(?i)(?:^|[^a-z])(?:ep|épisode|episode)\s*(\d{1,3})(?:[^0-9]|$)`)
	// A leading number used as the episode index: "001 - Show - Title".
	leadingNumberRe = regexp.MustCompile(`^(\d{1,3})\s*[-_.]\s*\S`)
	// A bare "S03" token, as in "Recess.S03.FRENCH.WEB".
	seasonTokenRe = regexp.MustCompile(`(?i)[^a-z0-9]s\d{2}(?:[^0-9]|$)`)
	yearRe        = regexp.MustCompile(`(?:^|[^0-9])((?:19|20)\d{2})(?:[^0-9]|$)`)
)

// safeParse contains a bug in go-parse-torrent-name: it derives startIndex and
// endIndex from different pattern matches without checking that the first is
// still below the second, then slices the name with them. Inputs like
// "2011- Aladdin1x01.mkv" make it panic. The library is unmaintained, so the
// panic is absorbed here and the caller falls back to our own parsing rather
// than taking the process down over one oddly named file.
func safeParse(base string) (info *ptn.TorrentInfo) {
	defer func() {
		if recover() != nil {
			info = nil
		}
	}()
	parsed, err := ptn.Parse(base)
	if err != nil {
		return nil
	}
	return parsed
}

// ParseFilename extracts what it can from a release name.
func ParseFilename(ftpPath string) Parsed {
	base := path.Base(ftpPath)
	name := strings.TrimSuffix(base, path.Ext(base))

	var out Parsed
	if info := safeParse(base); info != nil {
		out.Title = cleanTitle(info.Title)
		if info.Year > 0 {
			y := info.Year
			out.Year = &y
		}
		if info.Season > 0 {
			s := info.Season
			out.Season = &s
		}
		if info.Episode > 0 {
			e := info.Episode
			out.Episode = &e
		}
	}

	if out.Season == nil || out.Episode == nil {
		switch {
		case seasonEpisodeRe.MatchString(name):
			m := seasonEpisodeRe.FindStringSubmatch(name)
			out.Season, out.Episode = atoiPtr(m[1]), atoiPtr(m[2])
		case altEpisodeRe.MatchString(name):
			m := altEpisodeRe.FindStringSubmatch(name)
			out.Season, out.Episode = atoiPtr(m[1]), atoiPtr(m[2])
		case wordSeasonEpRe.MatchString(name):
			m := wordSeasonEpRe.FindStringSubmatch(name)
			out.Season, out.Episode = atoiPtr(m[1]), atoiPtr(m[2])
		case wordEpisodeRe.MatchString(name):
			out.Episode = atoiPtr(wordEpisodeRe.FindStringSubmatch(name)[1])
		case leadingNumberRe.MatchString(name):
			// "001 - Show - Title": the index comes first, and the title
			// follows it.
			out.Episode = atoiPtr(leadingNumberRe.FindStringSubmatch(name)[1])
		}
	}

	// A season stated anywhere in the path, most often as the folder name.
	if out.Season == nil {
		if m := wordSeasonRe.FindStringSubmatch(ftpPath); m != nil {
			out.Season = atoiPtr(m[1])
		}
	}
	if out.Year == nil {
		if m := yearRe.FindStringSubmatch(name); m != nil {
			out.Year = atoiPtr(m[1])
		}
	}
	if out.Title == "" {
		out.Title = cleanTitle(fallbackTitle(name))
	}
	// A leading index is not part of the title.
	if m := leadingNumberRe.FindStringSubmatch(out.Title); m != nil {
		out.Title = cleanTitle(strings.TrimPrefix(out.Title, m[1]))
	}
	return out
}

// seasonFolderRe recognises a folder that is a season rather than a show:
// its parent carries the name we want.
var seasonFolderRe = regexp.MustCompile(`(?i)^(?:sai?son|season|s)\s*\d{1,2}$|^(?:int[ée]grale|complete|complet)$`)

// FolderLabel turns a directory name into something readable. Collections are
// full of folders called "Pokémon.S04.FRENCH.540p.WEB.H264-BTT"; the release
// noise is for the downloader, not for someone browsing their own library.
// Returns the cleaned name and the season it mentioned, if any.
func FolderLabel(name string) (string, *int) {
	var season *int
	if m := wordSeasonRe.FindStringSubmatch(name); m != nil {
		season = atoiPtr(m[1])
	} else if m := seasonTokenRe.FindStringSubmatch(" " + name); m != nil {
		season = atoiPtr(strings.TrimLeft(strings.Trim(m[0], " ."), "sS"))
	}

	clean := cleanTitle(fallbackTitle(name))
	if clean == "" {
		// Nothing survived the cleaning: better a raw name than an empty tile.
		return cleanTitle(name), season
	}
	return clean, season
}

// ShowTitleFromPath derives a series name from the directory holding the file.
// For a series the folder is far more reliable than the filename: "Nicky
// Larson" beats "001 - Nicky Larson - Un cocktail dangereux", and the many
// naming schemes in one collection all sit under a correctly named folder.
func ShowTitleFromPath(ftpPath string) string {
	dir := path.Dir(ftpPath)
	name := path.Base(dir)

	// "Detective Conan/Saison 02" -> the show is one level up.
	if seasonFolderRe.MatchString(cleanTitle(name)) {
		name = path.Base(path.Dir(dir))
	}
	if name == "." || name == "/" || name == "" {
		return ""
	}
	return cleanTitle(fallbackTitle(name))
}

// fallbackTitle keeps everything before the first release-noise token.
func fallbackTitle(name string) string {
	lowered := strings.ToLower(name)
	markers := []string{
		"1080p", "720p", "2160p", "480p", "540p", "4k", "bluray", "blu-ray",
		"bdrip", "webrip", "web-dl", "webdl", "hdtv", "dvdrip", "x264", "x265",
		"h264", "h265", "hevc", "xvid", "multi", "vostfr", "truefrench",
		"french", "integrale", "intégrale", "complete", "complet",
	}
	cut := len(name)
	for _, mk := range markers {
		if i := strings.Index(lowered, mk); i > 0 && i < cut {
			cut = i
		}
	}
	for _, re := range []*regexp.Regexp{seasonEpisodeRe, wordSeasonRe, seasonTokenRe} {
		if m := re.FindStringIndex(lowered); m != nil && m[0] > 0 && m[0] < cut {
			cut = m[0]
		}
	}
	return name[:cut]
}

func cleanTitle(s string) string {
	s = strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s)
}

func atoiPtr(s string) *int {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return nil
	}
	return &n
}

// deriveMetadata names a file from its own path. There is no online catalogue
// involved: the folder and the filename are what the owner of the library
// arranged, and a synopsis is theirs to write if they want one.
//
// A row the user has edited is never touched.
func (s *Scanner) deriveMetadata(ctx context.Context, m *db.Media) error {
	if m.UserEdited {
		return nil
	}

	p := ParseFilename(m.FTPPath)
	title := p.Title

	// Several videos in one folder is a season, and the folder is where the
	// show is spelled properly. "Nicky Larson" beats "001 - Nicky Larson -
	// Un cocktail dangereux".
	n, err := s.store.CountVideosInFolder(ctx, m.LibraryID, m.FTPPath)
	if err == nil && n > 1 {
		if show := ShowTitleFromPath(m.FTPPath); show != "" {
			title = show
		}
	}
	if title == "" {
		title = cleanTitle(strings.TrimSuffix(path.Base(m.FTPPath), path.Ext(m.FTPPath)))
	}

	return s.store.SaveMetadata(ctx, m.ID, db.MetadataResult{
		Title:   title,
		Year:    p.Year,
		Season:  p.Season,
		Episode: p.Episode,
	})
}
