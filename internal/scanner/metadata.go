package scanner

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/aluzed/freebox-proxystream/internal/db"
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
	// "Funky Cops Saison 1 EP 01", "Saison 02 Episode 3". The separator before
	// the number is whatever the release used: "Ep 043", "Ep.043", "Ep_043".
	wordSeasonEpRe = regexp.MustCompile(`(?i)sai?son[\s._-]*(\d{1,2}).{0,12}?(?:ep|épisode|episode)[\s._-]*(\d{1,3})`)
	// "Saison 02" or "Season 2" on its own, usually the folder name.
	wordSeasonRe = regexp.MustCompile(`(?i)(?:^|[^a-z])sai?son[\s._-]*(\d{1,2})(?:[^0-9]|$)`)
	// "EP 01" / "Episode 12" with no season stated.
	wordEpisodeRe = regexp.MustCompile(`(?i)(?:^|[^a-z])(?:ep|épisode|episode)[\s._-]*(\d{1,3})(?:[^0-9]|$)`)
	// A bare "E01" with no season, as in "Docteur.Globule.E01.Bienvenue".
	// Title extraction only: go-parse-torrent-name already numbers these, so
	// widening the switch below would change nothing but could misfire.
	bareEpisodeRe = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])e[\s._-]*(\d{1,3})(?:[^0-9]|$)`)
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

var releaseMarkers = []string{
	"1080p", "720p", "2160p", "480p", "540p", "4k", "bluray", "blu-ray",
	"bdrip", "webrip", "web-dl", "webdl", "hdtv", "dvdrip", "x264", "x265",
	"h264", "h265", "hevc", "xvid", "multi", "vostfr", "truefrench",
	"french", "integrale", "intégrale", "complete", "complet",
	"repack", "proper", "remux",
}

// fallbackTitle keeps everything before the first release-noise token.
func fallbackTitle(name string) string {
	lowered := strings.ToLower(name)
	cut := len(name)
	for _, mk := range releaseMarkers {
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
	// Cutting at a release marker can leave a bracket hanging open, as in
	// "Fruits Basket 14[BD". Nothing readable follows it.
	if i := strings.LastIndex(s, "["); i >= 0 && !strings.Contains(s[i:], "]") {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func atoiPtr(s string) *int {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return nil
	}
	return &n
}

// uploaderRe is the signature releasers leave where an episode title would be:
// "Funky Cops Saison 1 EP 01 - up by TakeASuit", "S01E01 - Repack par
// Davicroquette". "par" alone is a common French word, so it only counts
// behind one of these verbs.
var uploaderRe = regexp.MustCompile(
	`(?i)\b(?:up|upload|uploaded|rip|ripped|encoded|repack|remux)\s+(?:by|par)\b`)

// The same signature in its other form, glued to the title: "Toutou Ou
// Rien[UpToTen]". Square brackets only: parentheses carry real title parts.
var bracketTagRe = regexp.MustCompile(`\s*\[[^\]]*\]`)

// Everything past the episode number that is a language or encoding tag
// rather than a title: "La Vie De Famille S04E01.VF" has no episode title.
// Matched as whole words, never as substrings, or "vo" would truncate
// "Le Voyage" and "vf" would be looked for inside real words.
var noiseTokens = map[string]bool{
	"vf": true, "vff": true, "vfq": true, "vfi": true, "vo": true,
	"vof": true, "fansub": true, "fastsub": true, "ia": true,
	"uncensored": true, "custom": true, "uncut": true,
	"vost": true, "vostfr": true, "subfrench": true, "truefrench": true,
	"french": true, "multi": true, "repack": true, "proper": true,
	"remux": true, "integrale": true, "intégrale": true, "complete": true,
	"chap": true, "aac": true, "ac3": true, "dts": true, "avc": true,
	"mp3": true, "x264": true, "x265": true, "h264": true, "h265": true,
	"hevc": true, "xvid": true, "divx": true, "dvdrip": true, "bdrip": true,
	"webrip": true, "hdtv": true, "bluray": true, "web": true, "dl": true,
	"1080p": true, "720p": true, "480p": true, "540p": true, "2160p": true,
}

// groupTagRe is a release group signing the end of the name: "Piraterie
// carabinée MTL666". Capitals glued to digits, which no title word is.
var groupTagRe = regexp.MustCompile(`\s+[A-Z]{2,}\d+$`)

// cutAtNoiseToken keeps what comes before the first tag word.
func cutAtNoiseToken(s string) string {
	fields := strings.Fields(s)
	for i, f := range fields {
		if noiseTokens[strings.ToLower(f)] {
			return strings.Join(fields[:i], " ")
		}
	}
	return s
}

// episodeTitle digs out what the filename says past the numbering, which is
// the episode's own name: "Aladdin_-_1x01_-_Les_1001_plumes" is "Les 1001
// plumes". Parsed.Title cannot answer this, it is truncated at the numbering.
// Returns "" when nothing usable is left, the common case for
// "Show.S01E02.mkv", so the caller can keep the show name.
func episodeTitle(ftpPath, show string) string {
	base := path.Base(ftpPath)
	name := strings.TrimSuffix(base, path.Ext(base))

	rest := ""
	for _, re := range []*regexp.Regexp{
		seasonEpisodeRe, altEpisodeRe, wordSeasonEpRe, wordEpisodeRe,
		bareEpisodeRe, leadingNumberRe,
	} {
		m := re.FindStringSubmatchIndex(name)
		if m == nil {
			continue
		}
		end := m[3] // end of the first capture, then push past any later one
		for i := 2; 2*i+1 < len(m); i++ {
			if e := m[2*i+1]; e > end {
				end = e
			}
		}
		rest = name[end:]
		break
	}

	rest = cleanTitle(bracketTagRe.ReplaceAllString(rest, ""))
	if loc := uploaderRe.FindStringIndex(rest); loc != nil {
		rest = strings.TrimSpace(rest[:loc[0]])
	}
	rest = strings.TrimSpace(groupTagRe.ReplaceAllString(cutAtNoiseToken(rest), ""))
	// The filename usually repeats the show before the episode's own title.
	if len(rest) >= len(show) && strings.EqualFold(rest[:len(show)], show) {
		rest = strings.TrimSpace(rest[len(show):])
	}
	// A leftover of one letter or less is a fragment, not a title.
	if countLetters(rest) < 2 {
		return ""
	}
	return rest
}

func countLetters(s string) int {
	n := 0
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127 {
			n++
		}
	}
	return n
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

	// A numbered file in a folder of several is an episode. The folder spells
	// the show properly, so it wins whenever the file has nothing of its own
	// to say, but a file that carries an episode title keeps it: naming twenty
	// rows "Nicky Larson" tells the reader nothing about which to watch.
	// Without a number it is a film, and a folder of films must not rename
	// them all after the folder.
	if p.Episode != nil {
		n, err := s.store.CountVideosInFolder(ctx, m.LibraryID, m.FTPPath)
		if err == nil && n > 1 {
			if show := ShowTitleFromPath(m.FTPPath); show != "" {
				title = show
				if ep := episodeTitle(m.FTPPath, show); ep != "" {
					title = ep
				}
			}
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
