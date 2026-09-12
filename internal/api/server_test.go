package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aluzed/freebox-proxystream/internal/api"
	"github.com/aluzed/freebox-proxystream/internal/cache"
	"github.com/aluzed/freebox-proxystream/internal/config"
	"github.com/aluzed/freebox-proxystream/internal/db"
	"github.com/aluzed/freebox-proxystream/internal/ftpclient"
	"github.com/aluzed/freebox-proxystream/internal/scanner"
	"github.com/aluzed/freebox-proxystream/internal/subs"
	"github.com/aluzed/freebox-proxystream/internal/testftp"
	"github.com/aluzed/freebox-proxystream/internal/thumbnail"
	"github.com/aluzed/freebox-proxystream/internal/transcode"
)

type testEnv struct {
	srv    *httptest.Server
	store  *db.Store
	client *http.Client
	cfg    *config.Config
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()

	tmp := t.TempDir()
	addr, root := testftp.Start(t)
	testftp.WriteFile(t, root, "Films/placeholder.mkv", 1024)

	sdb, err := db.Open(filepath.Join(tmp, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sdb.Close() })
	store := db.NewStore(sdb)

	cfg := &config.Config{
		FTPHost:         addr,
		InternalAddr:    "127.0.0.1:0",
		SessionTTL:      time.Hour,
		DBPath:          filepath.Join(tmp, "app.db"),
		CacheDir:        filepath.Join(tmp, "segments"),
		SubtitleDir:     filepath.Join(tmp, "subs"),
		ThumbDir:        filepath.Join(tmp, "thumbs"),
		CacheMaxBytes:   1 << 30,
		CacheTTL:        time.Hour,
		PreferredLang:   "fr",
		FFmpegBin:       "ffmpeg",
		FFprobeBin:      "ffprobe",
		SegmentDuration: 4,
		X264Preset:      "veryfast",
		X264CRF:         23,
		AudioBitrate:    "128k",
		DefaultHeight:   720,
		Qualities: []config.Quality{
			{Height: 720, Label: "720p", Maxrate: "3M", Bufsize: "6M"},
			{Height: 1080, Label: "1080p", Maxrate: "6M", Bufsize: "12M"},
		},
		EncoderReadaheadSegments: 15,
		EncoderIdleTimeout:       90 * time.Second,
		MaxConcurrentEncoders:    2,
	}

	logger := slog.New(slog.DiscardHandler)
	ftp := ftpclient.New(ftpclient.Config{
		Host: addr, User: testftp.User, Pass: testftp.Pass,
	}, logger)
	t.Cleanup(func() { ftp.Close() })

	thumbs := thumbnail.New(cfg)
	mgr := transcode.NewManager(cfg, store, logger)
	t.Cleanup(func() { mgr.Shutdown("test cleanup", 10*time.Second) })
	s, err := api.New(api.Deps{
		Config:  cfg,
		Store:   store,
		FTP:     ftp,
		Manager: mgr,
		Scanner: scanner.New(cfg, store, ftp, thumbs, logger),
		Janitor: cache.NewJanitor(cfg, store, mgr, logger),
		Subs:    subs.New(cfg),
		Thumbs:  thumbs,
		Log:     logger,
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 30 * time.Second,
	}

	return &testEnv{srv: srv, store: store, client: client, cfg: cfg}
}

func (e *testEnv) get(t *testing.T, path string) *http.Response {
	t.Helper()
	resp, err := e.client.Get(e.srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

func (e *testEnv) postForm(t *testing.T, path string, form url.Values) *http.Response {
	t.Helper()
	resp, err := e.client.PostForm(e.srv.URL+path, form)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// signUp completes first-run setup and leaves the client logged in.
func (e *testEnv) signUp(t *testing.T) {
	t.Helper()
	resp := e.postForm(t, "/setup", url.Values{
		"username": {"alex"}, "password": {"correct horse battery"},
		"confirm": {"correct horse battery"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("setup returned %d: %s", resp.StatusCode, body)
	}
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestHealthIsPublic(t *testing.T) {
	env := newEnv(t)
	resp := env.get(t, "/health")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body(t, resp), "ok") {
		t.Error("health did not report ok")
	}
}

func TestUnauthenticatedAccessIsBlocked(t *testing.T) {
	env := newEnv(t)

	// Pages redirect to the login screen.
	for _, path := range []string{"/", "/settings", "/media/1", "/watch/1"} {
		resp := env.get(t, path)
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("GET %s: status = %d, want 303", path, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
			t.Errorf("GET %s: redirected to %q, want /login", path, loc)
		}
	}

	// The API answers JSON instead.
	for _, path := range []string{"/api/media", "/api/libraries", "/api/status",
		"/api/media/1/master.m3u8", "/api/media/1/seg_00000.ts"} {
		resp := env.get(t, path)
		text := body(t, resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s: status = %d, want 401", path, resp.StatusCode)
		}
		if !strings.Contains(text, "authentication required") {
			t.Errorf("GET %s: body = %s", path, text)
		}
	}
}

func TestSetupThenLoginFlow(t *testing.T) {
	env := newEnv(t)

	// Before an account exists, the login page hands over to setup.
	resp := env.get(t, "/login")
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/setup" {
		t.Errorf("login redirected to %q, want /setup", loc)
	}

	env.signUp(t)

	// Setup logged us in.
	resp = env.get(t, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after setup, GET / = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Setup cannot be replayed to hijack the instance.
	resp = env.postForm(t, "/setup", url.Values{
		"username": {"intruder"}, "password": {"password123"}, "confirm": {"password123"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("second setup returned %d, want 409", resp.StatusCode)
	}

	// Log out, then back in.
	resp = env.postForm(t, "/logout", url.Values{})
	resp.Body.Close()

	resp = env.get(t, "/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("after logout, GET / = %d, want 303", resp.StatusCode)
	}

	resp = env.postForm(t, "/login", url.Values{
		"username": {"alex"}, "password": {"wrong password"},
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad password returned %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	resp = env.postForm(t, "/login", url.Values{
		"username": {"alex"}, "password": {"correct horse battery"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("login returned %d, want 303", resp.StatusCode)
	}

	resp = env.get(t, "/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("after login, GET / = %d", resp.StatusCode)
	}
}

// Template bugs only show up when a page is actually rendered, so render them all.
func TestAllPagesRender(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, err := env.store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	if err != nil {
		t.Fatal(err)
	}
	mediaID, _, err := env.store.UpsertMediaFile(ctx, lib.ID,
		"/Films/Le Fabuleux Destin (2001).mkv", 4<<30, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := env.store.SaveProbe(ctx, mediaID, db.ProbeResult{
		DurationSec: 5349.48,
		VideoCodec:  "hevc",
		Width:       3840,
		Height:      2160,
		FPS:         23.976,
		Audio: []db.AudioStream{
			{Index: 1, AudioIndex: 0, Codec: "ac3", Lang: "fre", Channels: 6, Title: "VF 5.1"},
			{Index: 2, AudioIndex: 1, Codec: "aac", Lang: "eng", Channels: 2},
		},
		Subs: []db.SubStream{
			{Index: 3, SubIndex: 0, Codec: "subrip", Lang: "fre"},
			{Index: 4, SubIndex: 1, Codec: "hdmv_pgs_subtitle", Lang: "eng", BurnInRequired: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	year := 2001
	if err := env.store.SaveMetadata(ctx, mediaID, db.MetadataResult{
		Title: "Le Fabuleux Destin d'Amélie Poulain", Year: &year,
		Overview: "Amélie décide de se mêler de la vie des autres.",
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.SetMediaStatus(ctx, mediaID, db.StatusReady, ""); err != nil {
		t.Fatal(err)
	}
	if err := env.store.SaveProgress(ctx, mediaID, 1800, 0); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		path string
		want []string
	}{
		{"/", []string{"Bibliothèque", "media-grid"}},
		{"/settings/libraries", []string{"Réglages", "Bibliothèques", "Ajouter une bibliothèque"}},
		{"/settings/status", []string{"Réglages", "État du serveur"}},
		{"/settings/account", []string{"Réglages", "Compte", "Changer de mot de passe"}},
		{"/media/1", []string{
			"Le Fabuleux Destin d&#39;Amélie Poulain",
			"Reprendre à",        // progress is mid-film
			"1080p",              // both renditions offered
			"VF 5.1",             // audio picker
			"non pris en charge", // the PGS track is disabled, not hidden
		}},
		{"/watch/1", []string{"player-config", "hls.min.js", "master.m3u8"}},
		{"/ui/media", []string{"Films", "folder-card"}},
		{"/ui/media?library_id=1", []string{"Le Fabuleux Destin", "crumbs"}},
		{"/ui/media?q=fabuleux", []string{"Le Fabuleux Destin"}},
		{"/ui/libraries", []string{"Films", "/Films"}},
		{"/ui/status", []string{"Cache de segments", "Encodeurs actifs"}},
		{"/ui/browse", []string{"browser"}},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp := env.get(t, tc.path)
			text := body(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d\n%s", resp.StatusCode, text)
			}
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Errorf("page does not contain %q", want)
				}
			}
		})
	}
}

// The browser swaps itself with outerHTML, so every response must carry the id
// its own links target. Losing it stops navigation dead after one level.
func TestFTPBrowsingWorksBeyondOneLevel(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)

	root := body(t, env.get(t, "/ui/browse"))
	if !strings.Contains(root, `id="browse"`) {
		t.Fatal(`root listing is missing id="browse", so the next click would have no target`)
	}
	if !strings.Contains(root, "/ui/browse?path=%2FFilms") {
		t.Fatalf("root listing does not link into /Films:\n%s", root)
	}

	// Follow that link, exactly as htmx would.
	sub := body(t, env.get(t, "/ui/browse?path=%2FFilms"))
	if !strings.Contains(sub, `id="browse"`) {
		t.Error(`second level is missing id="browse", browsing would stop here`)
	}
	if !strings.Contains(sub, "placeholder.mkv") {
		t.Errorf("second level does not show the directory contents:\n%s", sub)
	}
	// And it must offer a way back up.
	if !strings.Contains(sub, "Dossier parent") {
		t.Error("second level has no parent link")
	}
}

// Directory names with spaces are the norm on a Freebox ("/Disque dur/Films").
func TestBrowsingHandlesSpacesInPaths(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)

	listing := body(t, env.get(t, "/ui/browse?path=%2FDisque+dur"))
	// The directory does not exist on the test server, but the point is that
	// the path round-trips instead of being truncated at the space.
	if strings.Contains(listing, "Disque</code>") {
		t.Error("path was truncated at the space")
	}
}

func TestPlaylistIsServedBeforeAnyEncoding(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, _ := env.store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	id, _, _ := env.store.UpsertMediaFile(ctx, lib.ID, "/Films/x.mkv", 1<<30, time.Now().UTC())
	env.store.SaveProbe(ctx, id, db.ProbeResult{
		DurationSec: 5349.48, VideoCodec: "h264", Width: 1920, Height: 1080, FPS: 25,
		Audio: []db.AudioStream{{AudioIndex: 0, Codec: "aac", Lang: "fre", Channels: 6}},
	})
	env.store.SetMediaStatus(ctx, id, db.StatusReady, "")

	resp := env.get(t, "/api/media/1/master.m3u8?quality=720&audio=0")
	text := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, text)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Errorf("Content-Type = %q", ct)
	}
	if n := strings.Count(text, "#EXTINF:"); n != 1338 {
		t.Errorf("playlist has %d segments, want 1338", n)
	}
	if !strings.Contains(text, "seg_01337.ts?quality=720&audio=0") {
		t.Error("segment URLs do not carry the variant parameters")
	}
	if !strings.Contains(text, "#EXT-X-ENDLIST") {
		t.Error("playlist is not a finished VOD list")
	}

	// 1080p is offered, and the source allows it.
	resp = env.get(t, "/api/media/1/master.m3u8?quality=1080&audio=0")
	text = body(t, resp)
	if !strings.Contains(text, "quality=1080") {
		t.Error("1080p playlist does not reference the 1080p variant")
	}
}

func TestQualityIsClampedNotRejected(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, _ := env.store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	id, _, _ := env.store.UpsertMediaFile(ctx, lib.ID, "/Films/sd.mkv", 1<<30, time.Now().UTC())
	// A 480p source: neither rendition may upscale it.
	env.store.SaveProbe(ctx, id, db.ProbeResult{
		DurationSec: 100, VideoCodec: "h264", Width: 854, Height: 480, FPS: 25,
		Audio: []db.AudioStream{{AudioIndex: 0, Codec: "aac", Channels: 2}},
	})
	env.store.SetMediaStatus(ctx, id, db.StatusReady, "")

	for _, q := range []string{"720", "1080", "2160", "banana"} {
		resp := env.get(t, "/api/media/1/master.m3u8?quality="+q)
		text := body(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("quality=%s: status %d", q, resp.StatusCode)
		}
		if !strings.Contains(text, "quality=480") {
			t.Errorf("quality=%s did not clamp to the 480p source", q)
		}
	}
}

func TestProgressRoundTrip(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, _ := env.store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	id, _, _ := env.store.UpsertMediaFile(ctx, lib.ID, "/Films/x.mkv", 1<<30, time.Now().UTC())
	env.store.SaveProbe(ctx, id, db.ProbeResult{
		DurationSec: 3600, VideoCodec: "h264", Width: 1920, Height: 1080, FPS: 25,
		Audio: []db.AudioStream{{AudioIndex: 0, Codec: "aac", Channels: 2}},
	})
	env.store.SetMediaStatus(ctx, id, db.StatusReady, "")

	req, _ := http.NewRequest(http.MethodPut, env.srv.URL+"/api/media/1/progress",
		strings.NewReader(`{"position_sec": 1234.5}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT progress = %d, want 204", resp.StatusCode)
	}

	resp = env.get(t, "/api/media/1")
	var view struct {
		Progress struct {
			PositionSec float64 `json:"position_sec"`
		} `json:"progress"`
		DefaultHeight int `json:"default_height"`
		SegmentCount  int `json:"segment_count"`
	}
	if err := json.Unmarshal([]byte(body(t, resp)), &view); err != nil {
		t.Fatal(err)
	}
	if view.Progress.PositionSec != 1234.5 {
		t.Errorf("progress = %v, want 1234.5", view.Progress.PositionSec)
	}
	if view.DefaultHeight != 720 {
		t.Errorf("default height = %d, want 720", view.DefaultHeight)
	}
	if view.SegmentCount != 900 {
		t.Errorf("segment count = %d, want 900", view.SegmentCount)
	}
}

// The seen flag has two ways in, and the one that is written by hand has to
// survive the one that fires on its own ten seconds later.
func TestWatchedFlag(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, _ := env.store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	id, _, _ := env.store.UpsertMediaFile(ctx, lib.ID, "/Films/x.mkv", 1<<30, time.Now().UTC())
	env.store.SaveProbe(ctx, id, db.ProbeResult{
		DurationSec: 1000, VideoCodec: "h264", Width: 1920, Height: 1080, FPS: 25,
		Audio: []db.AudioStream{{AudioIndex: 0, Codec: "aac", Channels: 2}},
	})
	env.store.SetMediaStatus(ctx, id, db.StatusReady, "")

	watched := func() bool {
		t.Helper()
		p, err := env.store.GetProgress(ctx, id)
		if err != nil || p == nil {
			return false
		}
		return p.Watched
	}
	report := func(pos string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut, env.srv.URL+"/api/media/1/progress",
			strings.NewReader(`{"position_sec": `+pos+`}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	// Below the threshold, nothing is claimed.
	report("930")
	if watched() {
		t.Error("93% counted as watched")
	}

	// At the threshold it marks itself.
	report("940")
	if !watched() {
		t.Error("94% did not count as watched")
	}

	// The checkbox wins, in both directions. htmx leaves the field out
	// entirely when the box is unticked.
	resp := env.postForm(t, "/ui/media/1/watched", url.Values{})
	resp.Body.Close()
	if watched() {
		t.Fatal("unticking the checkbox did not clear the flag")
	}

	// And the next position report, still sitting past the end, must not undo
	// that: the player pings every ten seconds.
	report("999")
	if watched() {
		t.Error("a progress ping re-ticked a file marked unseen by hand")
	}

	resp = env.postForm(t, "/ui/media/1/watched", url.Values{"watched": {"on"}})
	resp.Body.Close()
	if !watched() {
		t.Error("ticking the checkbox did not set the flag")
	}

	// A file marked seen by hand keeps its position, so it can still be resumed.
	if p, _ := env.store.GetProgress(ctx, id); p == nil || p.PositionSec != 999 {
		t.Errorf("the checkbox moved the playback position: %+v", p)
	}

	// The detail page renders the state it stored.
	if page := body(t, env.get(t, "/media/1")); !strings.Contains(page, "checked") {
		t.Error("the detail page does not show the file as seen")
	}
}

func TestLibraryCRUDOverJSON(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)

	resp, err := env.client.Post(env.srv.URL+"/api/libraries", "application/json",
		strings.NewReader(`{"ftp_path":"/Films","name":"Films","kind":"movie"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create returned %d: %s", resp.StatusCode, body(t, resp))
	}
	resp.Body.Close()

	// The root is not a valid library, and neither is an unknown kind.
	for _, payload := range []string{
		`{"ftp_path":"/","name":"Root","kind":"movie"}`,
		`{"ftp_path":"/X","name":"X","kind":"anime"}`,
		`{"ftp_path":"/X","name":"","kind":"movie"}`,
	} {
		resp, err := env.client.Post(env.srv.URL+"/api/libraries", "application/json",
			strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("payload %s returned %d, want 400", payload, resp.StatusCode)
		}
	}

	resp = env.get(t, "/api/libraries")
	if !strings.Contains(body(t, resp), `"name":"Films"`) {
		t.Error("library missing from listing")
	}

	req, _ := http.NewRequest(http.MethodDelete, env.srv.URL+"/api/libraries/1", nil)
	resp, err = env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete returned %d, want 204", resp.StatusCode)
	}
}

func TestPasswordChangeInvalidatesSession(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)

	// Wrong current password is rejected.
	resp := env.postForm(t, "/ui/password", url.Values{
		"current_password": {"nope"},
		"new_password":     {"a new long password"},
		"confirm_password": {"a new long password"},
	})
	if text := body(t, resp); !strings.Contains(text, "incorrect") {
		t.Errorf("expected a rejection, got: %s", text)
	}

	resp = env.postForm(t, "/ui/password", url.Values{
		"current_password": {"correct horse battery"},
		"new_password":     {"a new long password"},
		"confirm_password": {"a new long password"},
	})
	text := body(t, resp)
	if !strings.Contains(text, "Mot de passe modifié") {
		t.Fatalf("password change failed: %s", text)
	}

	// Changing the password logs every device out, including this one.
	resp = env.get(t, "/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("session survived a password change: status %d", resp.StatusCode)
	}

	resp = env.postForm(t, "/login", url.Values{
		"username": {"alex"}, "password": {"a new long password"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("login with the new password returned %d", resp.StatusCode)
	}
}

// Settings is split so the password form does not sit next to a cache button.
func TestSettingsIsSplitIntoSubScreens(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)

	// The bare path redirects rather than 404ing on an old bookmark.
	resp := env.get(t, "/settings")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("/settings status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/settings/libraries" {
		t.Errorf("/settings redirected to %q", loc)
	}

	// Each screen carries the sub-nav and only its own content.
	libraries := body(t, env.get(t, "/settings/libraries"))
	account := body(t, env.get(t, "/settings/account"))

	for _, page := range []string{libraries, account} {
		if !strings.Contains(page, "subnav") {
			t.Error("a settings screen is missing the sub-navigation")
		}
	}
	if strings.Contains(libraries, "Changer de mot de passe") {
		t.Error("the password form is still on the libraries screen")
	}
	if strings.Contains(account, "Ajouter une bibliothèque") {
		t.Error("the library form is on the account screen")
	}
	if !strings.Contains(account, "alex") {
		t.Error("the account screen does not show the signed-in user")
	}
}

// The panel explains itself, and quotes the configured values rather than
// hardcoding them into the copy.
func TestStatusPanelExplainsEncoderLifetime(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)

	status := body(t, env.get(t, "/ui/status"))
	if !strings.Contains(status, "Aucun encodage en cours") {
		t.Error("missing the idle encoder state")
	}
	if !strings.Contains(status, "1m30s") {
		t.Errorf("panel does not quote the configured idle timeout:\n%s", status)
	}
	if !strings.Contains(status, "Le cache survit aux redémarrages") {
		t.Error("panel does not explain what clearing the cache costs")
	}
}

// The library mirrors the folder tree the user built on the Freebox. Flattening
// it would throw away their filing.
func TestLibraryFollowsTheFolderHierarchy(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, err := env.store.CreateLibrary(ctx, "/Series", "Séries", db.KindTV)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"/Series/Kaamelott/Saison 1/Kaamelott.S01E01.mkv",
		"/Series/Kaamelott/Saison 1/Kaamelott.S01E02.mkv",
		"/Series/Kaamelott/Saison 2/Kaamelott.S02E01.mkv",
		"/Series/Pokemon/Pokemon.S15E36.mkv",
		"/Series/a_file_at_the_root.mkv",
	} {
		id, _, err := env.store.UpsertMediaFile(ctx, lib.ID, p, 1<<30, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		env.store.SetMediaStatus(ctx, id, db.StatusReady, "")
	}

	// Top of the library: two folders and the loose file, no episodes leaking up.
	top := body(t, env.get(t, "/ui/media?library_id="+strconv.FormatInt(lib.ID, 10)))
	for _, want := range []string{"Kaamelott", "Pokemon", "a_file_at_the_root"} {
		if !strings.Contains(top, want) {
			t.Errorf("library root is missing %q", want)
		}
	}
	if strings.Contains(top, "S01E01") {
		t.Error("episodes from a subfolder leaked into the library root")
	}
	if !strings.Contains(top, "3 médias") {
		t.Errorf("Kaamelott folder does not report its 3 media:\n%s", top)
	}

	// One level down.
	show := body(t, env.get(t, "/ui/media?library_id="+
		strconv.FormatInt(lib.ID, 10)+"&path=Kaamelott"))
	if !strings.Contains(show, "Saison 1") || !strings.Contains(show, "Saison 2") {
		t.Errorf("seasons missing:\n%s", show)
	}
	if strings.Contains(show, "Pokemon") {
		t.Error("a sibling folder leaked into Kaamelott")
	}

	// Two levels down: the episodes themselves, in order.
	season := body(t, env.get(t, "/ui/media?library_id="+
		strconv.FormatInt(lib.ID, 10)+"&path=Kaamelott%2FSaison+1"))
	if !strings.Contains(season, "S01E01") || !strings.Contains(season, "S01E02") {
		t.Errorf("episodes missing:\n%s", season)
	}
	if strings.Contains(season, "S02E01") {
		t.Error("season 2 leaked into season 1")
	}
	// Every level must keep the swap target, or navigation dies after one click.
	for _, level := range []string{top, show, season} {
		if !strings.Contains(level, `id="media-grid"`) {
			t.Error(`a level is missing id="media-grid"`)
		}
	}
	// And a breadcrumb back up.
	if !strings.Contains(season, "Kaamelott") || !strings.Contains(season, "Séries") {
		t.Error("breadcrumb does not lead back up the tree")
	}
}

// "Retour" from a file six folders deep used to land on the library root,
// throwing away the whole descent.
func TestDetailGoesBackToItsOwnFolder(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, err := env.store.CreateLibrary(ctx, "/Series", "Séries", db.KindTV)
	if err != nil {
		t.Fatal(err)
	}
	deep, _, err := env.store.UpsertMediaFile(ctx, lib.ID,
		"/Series/Kaamelott/Saison 1/Kaamelott.S01E01.mkv", 1<<30, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	loose, _, err := env.store.UpsertMediaFile(ctx, lib.ID,
		"/Series/at_the_root.mkv", 1<<30, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	id := strconv.FormatInt(lib.ID, 10)
	cases := []struct {
		media int64
		want  string
	}{
		// html/template escapes the space that url.Values encoded as "+".
		{deep, `href="/?library_id=` + id + `&amp;path=Kaamelott%2FSaison&#43;1"`},
		{loose, `href="/?library_id=` + id + `"`},
	}
	for _, tc := range cases {
		page := body(t, env.get(t, "/media/"+strconv.FormatInt(tc.media, 10)))
		if !strings.Contains(page, tc.want) {
			t.Errorf("media %d: back link is not %s", tc.media, tc.want)
		}
	}
}

// Every action has to say what it did. The outcome fragment can land off
// screen or not at all, so the answer also carries a toast.
func TestActionsReportThroughAToast(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, err := env.store.CreateLibrary(ctx, "/Series", "Séries", db.KindTV)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := env.store.UpsertMediaFile(ctx, lib.ID,
		"/Series/Kaamelott/Kaamelott.S01E01.mkv", 1<<30, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	libID := strconv.FormatInt(lib.ID, 10)
	mediaID := strconv.FormatInt(id, 10)

	cases := []struct {
		name    string
		path    string
		form    url.Values
		level   string
		reload  bool
		refresh bool
	}{
		{"scan", "/ui/libraries/" + libID + "/scan", url.Values{}, "success", false, false},
		{"refresh", "/ui/libraries/" + libID + "/refresh", url.Values{}, "success", false, false},
		{"cache", "/ui/cache/clear", url.Values{}, "success", false, false},
		{"save details", "/ui/media/" + mediaID + "/details",
			url.Values{"title": {"Un titre"}}, "success", true, true},
		{"reset details", "/ui/media/" + mediaID + "/details/reset",
			url.Values{}, "success", true, true},
		{"retry", "/ui/media/" + mediaID + "/retry", url.Values{}, "success", true, true},
		// A rejected action must be as loud as one that worked.
		{"empty title", "/ui/media/" + mediaID + "/details",
			url.Values{"title": {"   "}}, "error", false, false},
		{"bad library", "/ui/libraries/999999/scan", url.Values{}, "error", false, false},
	}

	for _, tc := range cases {
		resp := env.postForm(t, tc.path, tc.form)
		trigger := resp.Header.Get("HX-Trigger")
		resp.Body.Close()

		var events struct {
			Toast struct {
				Level   string `json:"level"`
				Message string `json:"message"`
				Reload  bool   `json:"reload"`
			} `json:"toast"`
		}
		if err := json.Unmarshal([]byte(trigger), &events); err != nil {
			t.Errorf("%s: HX-Trigger is not usable JSON (%q): %v", tc.name, trigger, err)
			continue
		}
		if events.Toast.Message == "" {
			t.Errorf("%s: no toast message in %q", tc.name, trigger)
		}
		if events.Toast.Level != tc.level {
			t.Errorf("%s: toast level = %q, want %q", tc.name, events.Toast.Level, tc.level)
		}
		// A handler that reloads the page has to carry its message across it.
		if events.Toast.Reload != tc.reload {
			t.Errorf("%s: toast reload = %v, want %v", tc.name, events.Toast.Reload, tc.reload)
		}
		if tc.refresh && resp.Header.Get("HX-Refresh") != "true" {
			t.Errorf("%s: expected HX-Refresh alongside the toast", tc.name)
		}
	}
}

// Creating a library both refreshes the list and reports; neither may drop the
// other from the single HX-Trigger header.
func TestTriggersAreMergedNotOverwritten(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)

	resp := env.postForm(t, "/ui/libraries", url.Values{
		"ftp_path": {"/Films"}, "name": {"Films"}, "kind": {"movie"},
	})
	trigger := resp.Header.Get("HX-Trigger")
	resp.Body.Close()

	var events map[string]any
	if err := json.Unmarshal([]byte(trigger), &events); err != nil {
		t.Fatalf("HX-Trigger is not usable JSON (%q): %v", trigger, err)
	}
	if _, ok := events["libraries-changed"]; !ok {
		t.Errorf("libraries-changed was dropped: %q", trigger)
	}
	if _, ok := events["toast"]; !ok {
		t.Errorf("toast was dropped: %q", trigger)
	}
}

// htmx silently drops a request whose hx-target matches nothing: no call, no
// error, nothing in the log. The scan and refresh buttons pointed at #status,
// which only exists on another page, so they did nothing at all for as long as
// they existed. Every target a page can reach has to resolve inside it.
func TestEveryHtmxTargetExistsOnItsPage(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, err := env.store.CreateLibrary(ctx, "/Series", "Séries", db.KindTV)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := env.store.UpsertMediaFile(ctx, lib.ID,
		"/Series/Kaamelott/Kaamelott.S01E01.mkv", 1<<30, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	// A page plus the fragments it pulls in on load: that is the DOM a button
	// on it actually sees.
	pages := map[string][]string{
		"/settings/libraries":                 {"/ui/libraries", "/ui/browse"},
		"/settings/status":                    {"/ui/status"},
		"/settings/account":                   {},
		"/":                                   {"/ui/media"},
		"/media/" + strconv.FormatInt(id, 10): {},
	}

	targetRe := regexp.MustCompile(`hx-target="#([a-zA-Z0-9_-]+)"`)

	for page, fragments := range pages {
		var dom strings.Builder
		dom.WriteString(body(t, env.get(t, page)))
		for _, f := range fragments {
			dom.WriteString(body(t, env.get(t, f)))
		}
		html := dom.String()

		for _, m := range targetRe.FindAllStringSubmatch(html, -1) {
			if !strings.Contains(html, `id="`+m[1]+`"`) {
				t.Errorf("%s: hx-target=%q resolves to nothing, so the request is never sent",
					page, "#"+m[1])
			}
		}
	}
}

// Reading a template is not enough to know a button works: the scan and
// refresh buttons looked perfectly correct and sent nothing. This walks the
// rendered DOM of every page, pulls out every action it can fire, and calls
// each one for real. A route that does not exist, or one that answers without
// telling the user anything, fails here.
func TestEveryActionInTheUIIsWired(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, err := env.store.CreateLibrary(ctx, "/Series", "Séries", db.KindTV)
	if err != nil {
		t.Fatal(err)
	}
	mediaID, _, err := env.store.UpsertMediaFile(ctx, lib.ID,
		"/Series/Kaamelott/Kaamelott.S01E01.mkv", 1<<30, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	pages := map[string][]string{
		"/settings/libraries": {"/ui/libraries", "/ui/browse"},
		"/settings/status":    {"/ui/status"},
		"/settings/account":   {},
		"/":                   {"/ui/media"},
		"/media/" + strconv.FormatInt(mediaID, 10): {},
	}

	attrRe := regexp.MustCompile(`hx-(get|post|put|delete)="([^"]+)"`)
	ajaxRe := regexp.MustCompile(`htmx\.ajax\('([A-Z]+)',\s*'([^']+)'`)

	type action struct{ method, url, page string }
	var actions []action
	seen := map[string]bool{}

	for page, fragments := range pages {
		dom := body(t, env.get(t, page))
		for _, f := range fragments {
			dom += body(t, env.get(t, f))
		}
		for _, m := range attrRe.FindAllStringSubmatch(dom, -1) {
			a := action{strings.ToUpper(m[1]), html.UnescapeString(m[2]), page}
			if !seen[a.method+" "+a.url] {
				seen[a.method+" "+a.url] = true
				actions = append(actions, a)
			}
		}
		for _, m := range ajaxRe.FindAllStringSubmatch(dom, -1) {
			a := action{m[1], html.UnescapeString(m[2]), page}
			if !seen[a.method+" "+a.url] {
				seen[a.method+" "+a.url] = true
				actions = append(actions, a)
			}
		}
	}

	if len(actions) < 10 {
		t.Fatalf("only found %d actions, the extraction is broken", len(actions))
	}
	t.Logf("checking %d distinct actions", len(actions))

	// Deleting a library takes its media with it, so it goes last and nothing
	// after it can be misled by the missing rows.
	sort.SliceStable(actions, func(i, j int) bool {
		return (actions[i].method == "DELETE") != (actions[j].method == "DELETE") &&
			actions[j].method == "DELETE"
	})

	for _, a := range actions {
		req, err := http.NewRequest(a.method, env.srv.URL+a.url, nil)
		if err != nil {
			t.Errorf("%s %s: %v", a.method, a.url, err)
			continue
		}
		req.Header.Set("HX-Request", "true")
		resp, err := env.client.Do(req)
		if err != nil {
			t.Errorf("%s %s (from %s): %v", a.method, a.url, a.page, err)
			continue
		}
		trigger := resp.Header.Get("HX-Trigger")
		status := resp.StatusCode
		resp.Body.Close()

		switch status {
		case http.StatusNotFound:
			t.Errorf("%s %s (from %s): no such route", a.method, a.url, a.page)
		case http.StatusMethodNotAllowed:
			t.Errorf("%s %s (from %s): route exists but not for this method",
				a.method, a.url, a.page)
		}

		// Anything that changes state has to say so; a silent mutation is how
		// this whole class of bug stayed invisible.
		if a.method == "GET" || status >= 400 {
			continue
		}
		if !strings.Contains(trigger, `"toast"`) {
			t.Errorf("%s %s (from %s): changed something without reporting it (HX-Trigger=%q)",
				a.method, a.url, a.page, trigger)
		}
	}
}

// Header values are read as ISO-8859-1 by browsers, so an accented message put
// raw into HX-Trigger comes back as "mÃ©tadonnÃ©es".
func TestToastSurvivesAccents(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, err := env.store.CreateLibrary(ctx, "/Series", "Séries", db.KindTV)
	if err != nil {
		t.Fatal(err)
	}

	resp := env.postForm(t,
		"/ui/libraries/"+strconv.FormatInt(lib.ID, 10)+"/refresh", url.Values{})
	trigger := resp.Header.Get("HX-Trigger")
	resp.Body.Close()

	for i := 0; i < len(trigger); i++ {
		if trigger[i] > 127 {
			t.Fatalf("HX-Trigger carries a raw non-ASCII byte at %d: %q", i, trigger)
		}
	}

	// And it still decodes to the real message on the other side.
	var events struct {
		Toast struct {
			Message string `json:"message"`
		} `json:"toast"`
	}
	if err := json.Unmarshal([]byte(trigger), &events); err != nil {
		t.Fatalf("HX-Trigger is not usable JSON (%q): %v", trigger, err)
	}
	if !strings.Contains(events.Toast.Message, "métadonnées") {
		t.Errorf("message lost its accents: %q", events.Toast.Message)
	}
}

// An underscore is a LIKE wildcard; release names are full of them.
func TestBrowsingDoesNotTreatUnderscoresAsWildcards(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, _ := env.store.CreateLibrary(ctx, "/X", "X", db.KindMovie)
	for _, p := range []string{"/X/a_b/inside.mkv", "/X/axb/other.mkv"} {
		id, _, err := env.store.UpsertMediaFile(ctx, lib.ID, p, 1<<30, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		env.store.SetMediaStatus(ctx, id, db.StatusReady, "")
	}

	listing := body(t, env.get(t, "/ui/media?library_id="+
		strconv.FormatInt(lib.ID, 10)+"&path=a_b"))
	if !strings.Contains(listing, "inside") {
		t.Errorf("the real folder was not listed:\n%s", listing)
	}
	if strings.Contains(listing, "other") {
		t.Error("the underscore matched another folder, so it was used as a wildcard")
	}
}

// Search is deliberately flat: it should find things wherever they are filed.
func TestSearchIsFlatAcrossFolders(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, _ := env.store.CreateLibrary(ctx, "/Series", "Séries", db.KindTV)
	id, _, _ := env.store.UpsertMediaFile(ctx, lib.ID,
		"/Series/Kaamelott/Saison 3/buried deep.mkv", 1<<30, time.Now().UTC())
	env.store.SetMediaStatus(ctx, id, db.StatusReady, "")

	results := body(t, env.get(t, "/ui/media?q=buried"))
	if !strings.Contains(results, "buried deep") {
		t.Errorf("search did not reach into a nested folder:\n%s", results)
	}
	if strings.Contains(results, "folder-card") {
		t.Error("search results should be a flat list, not folders")
	}
}

// Navigation is htmx-driven throughout, not just inside individual widgets.
func TestNavigationIsBoosted(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, _ := env.store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	id, _, _ := env.store.UpsertMediaFile(ctx, lib.ID, "/Films/x.mkv", 1<<30, time.Now().UTC())
	env.store.SaveProbe(ctx, id, db.ProbeResult{
		DurationSec: 3600, VideoCodec: "h264", Width: 1920, Height: 1080, FPS: 25,
		Audio: []db.AudioStream{{AudioIndex: 0, Codec: "aac", Channels: 2}},
	})
	env.store.SetMediaStatus(ctx, id, db.StatusReady, "")

	for _, path := range []string{"/", "/media/1", "/settings/libraries"} {
		if page := body(t, env.get(t, path)); !strings.Contains(page, `hx-boost="true"`) {
			t.Errorf("%s does not boost its navigation", path)
		}
	}

	// The player owns document-level key handlers and an hls.js instance, so
	// swapping the body under it would leave them bound to a dead element.
	player := body(t, env.get(t, "/watch/1"))
	if strings.Contains(player, `<body hx-boost="true"`) {
		t.Error("the player page must not be boosted")
	}
	// And entering it must be a real navigation.
	detail := body(t, env.get(t, "/media/1"))
	if !strings.Contains(detail, `hx-boost="false"`) {
		t.Error("the play form should opt out of boosting")
	}

	// Login is reached without a session and must stay a plain form.
	env.postForm(t, "/logout", url.Values{}).Body.Close()
	if login := body(t, env.get(t, "/login")); strings.Contains(login, `hx-boost="true"`) {
		t.Error("the login page should not be boosted")
	}
}

// A file still being probed must not leave the page frozen.
func TestDetailPagePollsWhileProbing(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, _ := env.store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	id, _, _ := env.store.UpsertMediaFile(ctx, lib.ID, "/Films/new.mkv", 1<<30, time.Now().UTC())

	page := body(t, env.get(t, "/media/1"))
	if !strings.Contains(page, "/ui/media/1/state") || !strings.Contains(page, "every 3s") {
		t.Error("the detail page does not poll while the file is being probed")
	}
	if !strings.Contains(page, "spinner") {
		t.Error("no spinner while analysing")
	}

	// While unprobed, the endpoint keeps returning the waiting state.
	resp := env.get(t, "/ui/media/1/state")
	state := body(t, resp)
	if resp.Header.Get("HX-Refresh") != "" {
		t.Error("refresh was requested before the probe finished")
	}
	if !strings.Contains(state, "spinner") {
		t.Errorf("state fragment has no spinner: %s", state)
	}

	// Once probed, it tells the page to reload so every field updates at once.
	env.store.SaveProbe(ctx, id, db.ProbeResult{
		DurationSec: 60, VideoCodec: "h264", Width: 1920, Height: 1080, FPS: 25,
		Audio: []db.AudioStream{{AudioIndex: 0, Codec: "aac", Channels: 2}},
	})
	env.store.SetMediaStatus(ctx, id, db.StatusReady, "")

	resp = env.get(t, "/ui/media/1/state")
	resp.Body.Close()
	if resp.Header.Get("HX-Refresh") != "true" {
		t.Error("a finished probe did not trigger a refresh")
	}

	// And a settled page stops polling.
	page = body(t, env.get(t, "/media/1"))
	if strings.Contains(page, "/ui/media/1/state") {
		t.Error("the page keeps polling after the probe finished")
	}
}

// One global counter cannot say which library is stuck. The panel breaks it
// down, and libraries must not bleed into each other.
func TestStatusBreaksProgressDownPerLibrary(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	films, _ := env.store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	series, _ := env.store.CreateLibrary(ctx, "/Series", "Series", db.KindTV)

	ready, _, _ := env.store.UpsertMediaFile(ctx, films.ID, "/Films/a.mkv", 1<<30, time.Now().UTC())
	env.store.SetMediaStatus(ctx, ready, db.StatusReady, "")
	env.store.UpsertMediaFile(ctx, series.ID, "/Series/b.mkv", 1<<30, time.Now().UTC())

	panel := body(t, env.get(t, "/ui/status"))
	for _, want := range []string{"Films", "Series", "1 / 1", "0 / 1", "à analyser"} {
		if !strings.Contains(panel, want) {
			t.Errorf("status panel missing %q:\n%s", want, panel)
		}
	}

	// And the JSON API exposes the same shape.
	var view struct {
		Libraries []struct {
			Name    string `json:"name"`
			Total   int    `json:"total"`
			Ready   int    `json:"ready"`
			Pending int    `json:"pending"`
		} `json:"libraries"`
	}
	if err := json.Unmarshal([]byte(body(t, env.get(t, "/api/status"))), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Libraries) != 2 {
		t.Fatalf("api reported %d libraries, want 2", len(view.Libraries))
	}
	for _, l := range view.Libraries {
		switch l.Name {
		case "Films":
			if l.Ready != 1 || l.Pending != 0 {
				t.Errorf("Films = %+v", l)
			}
		case "Series":
			if l.Ready != 0 || l.Pending != 1 {
				t.Errorf("Series = %+v", l)
			}
		}
	}
}

// A file that failed transiently offers a retry and says when it will happen;
// one that is simply not a video says so and does not promise a retry.
func TestFailureStatesAreDistinguished(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, _ := env.store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	flaky, _, _ := env.store.UpsertMediaFile(ctx, lib.ID, "/Films/flaky.mkv", 1<<30, time.Now().UTC())
	broken, _, _ := env.store.UpsertMediaFile(ctx, lib.ID, "/Films/broken.mkv", 1<<30, time.Now().UTC())

	env.store.RecordProbeFailure(ctx, flaky, "ffprobe timed out", false)
	env.store.RecordProbeFailure(ctx, broken, "no video stream in this file", true)

	page := body(t, env.get(t, "/media/1"))
	if !strings.Contains(page, "nouvelle tentative prévue") {
		t.Errorf("a transient failure does not announce a retry:\n%s", page)
	}
	if !strings.Contains(page, "Relancer l'analyse") {
		t.Error("no manual retry button")
	}

	page = body(t, env.get(t, "/media/2"))
	if !strings.Contains(page, "Fichier illisible") {
		t.Errorf("a permanent failure is not stated plainly:\n%s", page)
	}
	if strings.Contains(page, "nouvelle tentative prévue") {
		t.Error("a permanent failure promises a retry that will never come")
	}

	// The manual retry puts it back in the queue whatever the verdict.
	resp := env.postForm(t, "/ui/media/2/retry", url.Values{})
	resp.Body.Close()
	if resp.Header.Get("HX-Refresh") != "true" {
		t.Error("the retry did not refresh the page")
	}
	pending, err := env.store.CountPendingProbes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Errorf("pending = %d, want the retried file queued", pending)
	}
}

// A season folder renders as a list, not as twenty near-identical posters.
func TestSeasonFolderRendersAsAnEpisodeList(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, _ := env.store.CreateLibrary(ctx, "/Series", "Séries", db.KindTV)
	season, ep := 3, 0
	var firstID int64
	for ep = 1; ep <= 4; ep++ {
		id, _, err := env.store.UpsertMediaFile(ctx, lib.ID,
			fmt.Sprintf("/Series/Kaamelott/Saison 3/K.S%02dE%02d.mkv", season, ep),
			1<<30, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if firstID == 0 {
			firstID = id
		}
		env.store.SaveProbe(ctx, id, db.ProbeResult{
			DurationSec: 1500, VideoCodec: "h264", Width: 1920, Height: 1080, FPS: 25,
			Audio: []db.AudioStream{{AudioIndex: 0, Codec: "aac", Channels: 2}},
		})
		s, e := season, ep
		env.store.SaveMetadata(ctx, id, db.MetadataResult{
			Title:  fmt.Sprintf("Kaamelott - S%02dE%02d - Episode %d", s, e, e),
			Season: &s, Episode: &e,
		})
		env.store.SetMediaStatus(ctx, id, db.StatusReady, "")
	}
	// Half-watched, and one played past the threshold.
	env.store.SaveProgress(ctx, firstID, 750, 0)
	env.store.SaveProgress(ctx, firstID+1, 1490, 1500*api.WatchedFraction)

	listing := body(t, env.get(t, "/ui/media?library_id="+
		strconv.FormatInt(lib.ID, 10)+"&path=Kaamelott%2FSaison+3"))

	if !strings.Contains(listing, "episodes") {
		t.Fatalf("season folder did not render as a list:\n%s", listing)
	}
	if strings.Contains(listing, `class="grid"`) {
		t.Error("the poster grid is still being used for episodes")
	}
	for _, want := range []string{"S03E01", "S03E04", "repris à"} {
		if !strings.Contains(listing, want) {
			t.Errorf("episode list missing %q", want)
		}
	}
	// One episode is ticked and one is not: both words ship on every row, so
	// only the checked attribute distinguishes them.
	if got := strings.Count(listing, "checked"); got != 1 {
		t.Errorf("expected exactly one episode ticked as seen, found %d", got)
	}
	// The label already carries the numbering, so it is not repeated in the title.
	if strings.Contains(listing, "Kaamelott - S03E01 - ") {
		t.Error("the episode title still repeats the numbering shown in its label")
	}
}

// A folder of films keeps the poster grid: the decision is made on content,
// not on how the library happens to be labelled.
func TestFilmFolderKeepsThePosterGrid(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	// Declared as a TV library, but the files are standalone films.
	lib, _ := env.store.CreateLibrary(ctx, "/Mixed", "Mixed", db.KindTV)
	for i, name := range []string{"Dune.mkv", "Arrival.mkv", "Sicario.mkv"} {
		id, _, _ := env.store.UpsertMediaFile(ctx, lib.ID, "/Mixed/"+name, 1<<30, time.Now().UTC())
		year := 2016 + i
		env.store.SaveMetadata(ctx, id, db.MetadataResult{Title: name, Year: &year})
		env.store.SetMediaStatus(ctx, id, db.StatusReady, "")
	}

	listing := body(t, env.get(t, "/ui/media?library_id="+strconv.FormatInt(lib.ID, 10)))
	if strings.Contains(listing, `class="episodes"`) {
		t.Error("films were rendered as an episode list")
	}
	if !strings.Contains(listing, "card-link") {
		t.Errorf("the poster grid was not used:\n%s", listing)
	}
}

// A single file is not a season.
func TestSingleFileIsNotAnEpisodeList(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, _ := env.store.CreateLibrary(ctx, "/Series", "Séries", db.KindTV)
	id, _, _ := env.store.UpsertMediaFile(ctx, lib.ID, "/Series/one.mkv", 1<<30, time.Now().UTC())
	s, e := 1, 1
	env.store.SaveMetadata(ctx, id, db.MetadataResult{Title: "one", Season: &s, Episode: &e})
	env.store.SetMediaStatus(ctx, id, db.StatusReady, "")

	listing := body(t, env.get(t, "/ui/media?library_id="+strconv.FormatInt(lib.ID, 10)))
	if strings.Contains(listing, `class="episodes"`) {
		t.Error("a lone file was rendered as an episode list")
	}
}

// A 480p source collapses both renditions onto 480p, so offering "720p" and
// "1080p" would be two names for one identical encode.
func TestQualityChoiceReflectsWhatYouActuallyGet(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, _ := env.store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	sd, _, _ := env.store.UpsertMediaFile(ctx, lib.ID, "/Films/sd.mkv", 1<<30, time.Now().UTC())
	hd, _, _ := env.store.UpsertMediaFile(ctx, lib.ID, "/Films/hd.mkv", 1<<30, time.Now().UTC())

	env.store.SaveProbe(ctx, sd, db.ProbeResult{
		DurationSec: 3600, VideoCodec: "h264", Width: 854, Height: 480, FPS: 25,
		Audio: []db.AudioStream{{AudioIndex: 0, Codec: "aac", Channels: 2}},
	})
	env.store.SaveProbe(ctx, hd, db.ProbeResult{
		DurationSec: 3600, VideoCodec: "h264", Width: 3840, Height: 2160, FPS: 25,
		Audio: []db.AudioStream{{AudioIndex: 0, Codec: "aac", Channels: 2}},
	})
	env.store.SetMediaStatus(ctx, sd, db.StatusReady, "")
	env.store.SetMediaStatus(ctx, hd, db.StatusReady, "")

	// 480p source: one option, disabled, no gibberish about upscaling.
	page := body(t, env.get(t, "/media/1"))
	if !strings.Contains(page, "480p (qualité source)") {
		t.Errorf("the real output height is not stated:\n%s", page)
	}
	if !strings.Contains(page, "disabled") {
		t.Error("a source with a single possible output still offers a choice")
	}
	if strings.Contains(page, "pas d'upscale") {
		t.Error("the old explanatory gibberish is still there")
	}
	if strings.Contains(page, "720p") || strings.Contains(page, "1080p") {
		t.Error("renditions the source cannot produce are still listed")
	}

	// 4K source: two genuinely different outputs, both offered plainly.
	page = body(t, env.get(t, "/media/2"))
	if !strings.Contains(page, ">720p<") || !strings.Contains(page, ">1080p<") {
		t.Errorf("a 4K source should offer both renditions:\n%s", page)
	}
	if strings.Contains(page, "qualité source") {
		t.Error("neither rendition is the 4K source's native height")
	}
}

// A 1080p source: 1080p is its native height, 720p is a real downscale.
func TestNativeHeightIsLabelledAsSuch(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, _ := env.store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	id, _, _ := env.store.UpsertMediaFile(ctx, lib.ID, "/Films/fhd.mkv", 1<<30, time.Now().UTC())
	env.store.SaveProbe(ctx, id, db.ProbeResult{
		DurationSec: 3600, VideoCodec: "h264", Width: 1920, Height: 1080, FPS: 25,
		Audio: []db.AudioStream{{AudioIndex: 0, Codec: "aac", Channels: 2}},
	})
	env.store.SetMediaStatus(ctx, id, db.StatusReady, "")

	page := body(t, env.get(t, "/media/1"))
	if !strings.Contains(page, "1080p (qualité source)") {
		t.Errorf("the native rendition is not marked:\n%s", page)
	}
	if strings.Contains(page, "disabled") {
		t.Error("two distinct outputs exist, the choice should stand")
	}
}

// The synopsis belongs to the user: once written, nothing derived from a
// filename may overwrite it.
func TestUserSynopsisSurvivesRederivation(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, _ := env.store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	id, _, _ := env.store.UpsertMediaFile(ctx, lib.ID,
		"/Films/Nicky.Larson.FRENCH.mkv", 1<<30, time.Now().UTC())
	env.store.SaveMetadata(ctx, id, db.MetadataResult{Title: "Nicky Larson"})
	env.store.SetMediaStatus(ctx, id, db.StatusReady, "")

	resp := env.postForm(t, "/ui/media/1/details", url.Values{
		"title":    {"City Hunter"},
		"overview": {"Ryo Saeba, détective privé, et son marteau."},
	})
	resp.Body.Close()
	if resp.Header.Get("HX-Refresh") != "true" {
		t.Fatal("saving details did not refresh the page")
	}

	// A later automatic pass must not touch it.
	if err := env.store.SaveMetadata(ctx, id, db.MetadataResult{
		Title: "Nicky Larson FRENCH", Overview: "",
	}); err != nil {
		t.Fatal(err)
	}

	m, err := env.store.GetMedia(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.Title == nil || *m.Title != "City Hunter" {
		t.Errorf("title = %v, want the user's", m.Title)
	}
	if m.Overview == nil || !strings.Contains(*m.Overview, "Ryo Saeba") {
		t.Errorf("the synopsis was lost: %v", m.Overview)
	}
	if !m.UserEdited {
		t.Error("the row is not marked as user-edited")
	}

	// The page offers it back for editing.
	page := body(t, env.get(t, "/media/1"))
	if !strings.Contains(page, "City Hunter") || !strings.Contains(page, "Ryo Saeba") {
		t.Error("the page does not show what the user wrote")
	}
	if !strings.Contains(page, "Revenir au titre automatique") {
		t.Error("no way back to the derived title")
	}

	// And handing it back to the automatic naming works.
	resp = env.postForm(t, "/ui/media/1/details/reset", url.Values{})
	resp.Body.Close()
	m, _ = env.store.GetMedia(ctx, id)
	if m.UserEdited {
		t.Error("the row is still marked as user-edited after a reset")
	}
	if m.Overview != nil {
		t.Errorf("the synopsis survived the reset: %v", m.Overview)
	}
}

func TestEmptyTitleIsRejected(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()

	lib, _ := env.store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	id, _, _ := env.store.UpsertMediaFile(ctx, lib.ID, "/Films/x.mkv", 1<<30, time.Now().UTC())
	env.store.SaveMetadata(ctx, id, db.MetadataResult{Title: "Something"})

	resp := env.postForm(t, "/ui/media/1/details", url.Values{"title": {"   "}})
	if text := body(t, resp); !strings.Contains(text, "ne peut pas être vide") {
		t.Errorf("an empty title was accepted: %s", text)
	}
	m, _ := env.store.GetMedia(ctx, id)
	if m.Title == nil || *m.Title != "Something" {
		t.Error("the previous title was clobbered by an invalid submission")
	}
}

func TestFolderGridPollsForNestedPendingMedia(t *testing.T) {
	env := newEnv(t)
	env.signUp(t)
	ctx := context.Background()
	lib, err := env.store.CreateLibrary(ctx, "/Films", "Films", db.KindMovie)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := env.store.UpsertMediaFile(ctx, lib.ID, "/Films/Nested/movie.mkv", 1<<30, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	gridURL := fmt.Sprintf("/ui/media?library_id=%d", lib.ID)
	page := body(t, env.get(t, gridURL))
	if !strings.Contains(page, `hx-trigger="every 10s"`) {
		t.Fatal("folder view does not poll for nested pending files")
	}
	if err := env.store.SetMediaStatus(ctx, id, db.StatusReady, ""); err != nil {
		t.Fatal(err)
	}
	page = body(t, env.get(t, gridURL))
	if strings.Contains(page, `hx-trigger="every 10s"`) {
		t.Fatal("settled view still polls")
	}
	resp := env.postForm(t, fmt.Sprintf("/ui/libraries/%d/scan", lib.ID), url.Values{})
	resp.Body.Close()
	page = body(t, env.get(t, gridURL))
	if !strings.Contains(page, `hx-trigger="every 10s"`) {
		t.Fatal("folder view does not poll for queued scan")
	}
}
